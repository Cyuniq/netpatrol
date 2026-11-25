//go:build !windows

package monitor

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

type traceSocket struct {
	conn        *icmp.PacketConn
	protocol    int
	request     icmp.Type
	setHopLimit func(int) error
}

func openTrace(ip netip.Addr) (traceSocket, error) {
	if ip.Is6() {
		conn, err := icmp.ListenPacket("ip6:ipv6-icmp", "::")
		if err != nil {
			return traceSocket{}, err
		}
		return traceSocket{conn: conn, protocol: 58, request: ipv6.ICMPTypeEchoRequest, setHopLimit: conn.IPv6PacketConn().SetHopLimit}, nil
	}
	conn, err := icmp.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		return traceSocket{}, err
	}
	return traceSocket{conn: conn, protocol: 1, request: ipv4.ICMPTypeEcho, setHopLimit: conn.IPv4PacketConn().SetTTL}, nil
}

// Trace sends a short batch per hop on its own socket, so setting the
// outgoing hop limit cannot change the regular monitoring probes.
func Trace(ctx context.Context, ip netip.Addr) (TraceResult, error) {
	socket, err := openTrace(ip)
	if err != nil {
		return TraceResult{}, fmt.Errorf("打开 ICMP 路径探测套接字: %w", err)
	}
	defer socket.conn.Close()
	stop := context.AfterFunc(ctx, func() { socket.conn.Close() })
	defer stop()

	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return TraceResult{}, err
	}
	id := binary.BigEndian.Uint16(nonce[:2])
	buffer := make([]byte, 65535)
	var trace TraceResult
	for hop := 1; hop <= TraceMaxHops; hop++ {
		if err := ctx.Err(); err != nil {
			return trace, err
		}
		result, failed := socket.probe(ip, id, hop, nonce[:], buffer)
		trace.Hops = append(trace.Hops, result)
		switch result.Result.Outcome {
		case Passed:
			trace.Reached = true
			return trace, nil
		case LocalError, Unreachable:
			return trace, nil
		}
		if failed {
			return trace, nil
		}
	}
	return trace, nil
}

func (s traceSocket) probe(ip netip.Addr, id uint16, hop int, nonce, buffer []byte) (TraceHop, bool) {
	observation := TraceHop{Number: hop, Attempts: make([]TraceAttempt, 0, TraceQueries)}
	if err := s.setHopLimit(hop); err != nil {
		observation.Result = failure(err)
		return observation, true
	}
	var starts [TraceQueries]time.Time
	for index := range TraceQueries {
		seq := (hop-1)*TraceQueries + index + 1
		start := time.Now()
		if err := s.send(ip, id, seq, nonce, start); err != nil {
			observation.markIncomplete(time.Now())
			observation.Attempts = append(observation.Attempts, TraceAttempt{Result: failure(err)})
			observation.selectResult()
			return observation, true
		}
		starts[index] = start
		observation.Attempts = append(observation.Attempts, TraceAttempt{
			Sent: true, Result: Result{Outcome: Timeout, CompletedAt: start.Add(traceTimeout)},
		})
		wait := traceSpacing
		if index == TraceQueries-1 {
			wait = traceTimeout
		}
		if err := s.collect(ip, id, nonce, buffer, &observation, starts, start.Add(wait)); err != nil {
			observation.markIncomplete(time.Now())
			observation.Attempts = append(observation.Attempts, TraceAttempt{Result: failure(err)})
			observation.selectResult()
			return observation, true
		}
	}
	observation.selectResult()
	return observation, false
}

func (s traceSocket) send(ip netip.Addr, id uint16, seq int, nonce []byte, at time.Time) error {
	message := icmp.Message{Type: s.request, Body: &icmp.Echo{ID: int(id), Seq: seq, Data: nonce}}
	packet, err := message.Marshal(nil)
	if err != nil {
		return err
	}
	if err := s.conn.SetWriteDeadline(at.Add(traceTimeout)); err != nil {
		return err
	}
	_, err = s.conn.WriteTo(packet, &net.IPAddr{IP: ip.AsSlice(), Zone: ip.Zone()})
	return err
}

func (s traceSocket) collect(ip netip.Addr, id uint16, nonce, buffer []byte, hop *TraceHop, starts [TraceQueries]time.Time, until time.Time) error {
	if err := s.conn.SetReadDeadline(until); err != nil {
		return err
	}
	for {
		if !time.Now().Before(until) || len(hop.Attempts) == TraceQueries && hop.Stats().Replies == TraceQueries {
			return nil
		}
		n, peer, err := s.conn.ReadFrom(buffer)
		at := time.Now()
		if err != nil {
			if failure(err).Outcome == Timeout {
				return nil
			}
			return err
		}
		message, err := icmp.ParseMessage(s.protocol, buffer[:n])
		addr, ok := peer.(*net.IPAddr)
		if err != nil || !ok {
			continue
		}
		source, ok := netip.AddrFromSlice(addr.IP)
		if !ok {
			continue
		}
		source = source.Unmap().WithZone(addr.Zone)
		for i := range hop.Attempts {
			attempt := &hop.Attempts[i]
			if attempt.Result.Outcome != Timeout || at.Sub(starts[i]) > traceTimeout {
				continue
			}
			seq := (hop.Number-1)*TraceQueries + i + 1
			key := uint32(id)<<16 | uint32(seq)
			outcome, detail := traceReply(s.protocol, message, source, ip, key, nonce)
			if outcome != "" {
				attempt.Address = source
				attempt.Result = Result{Outcome: outcome, Detail: detail, CompletedAt: at, RTT: at.Sub(starts[i])}
				break
			}
		}
	}
}
