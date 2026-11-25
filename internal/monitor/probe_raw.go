//go:build !windows

package monitor

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

type Prober struct {
	v4, v6       *echoSocket
	v4Err, v6Err error
}

func NewProber() *Prober {
	p := &Prober{}
	p.v4, p.v4Err = openEcho("ip4:icmp", "0.0.0.0", 1, ipv4.ICMPTypeEcho, ipv4.ICMPTypeEchoReply, ipv4.ICMPTypeDestinationUnreachable)
	p.v6, p.v6Err = openEcho("ip6:ipv6-icmp", "::", 58, ipv6.ICMPTypeEchoRequest, ipv6.ICMPTypeEchoReply, ipv6.ICMPTypeDestinationUnreachable)
	return p
}

func (p *Prober) ICMPErrors() (error, error) {
	return p.v4Err, p.v6Err
}

func (p *Prober) Close() {
	if p.v4 != nil {
		p.v4.conn.Close()
	}
	if p.v6 != nil {
		p.v6.conn.Close()
	}
}

func (p *Prober) probePing(ctx context.Context, ip netip.Addr, duration time.Duration) (Result, time.Time) {
	socket, err := p.v4, p.v4Err
	if ip.Is6() {
		socket, err = p.v6, p.v6Err
	}
	if err != nil {
		return failure(err), time.Time{}
	}
	timeout, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	return socket.ping(timeout, ip)
}

type echoReply struct {
	at      time.Time
	outcome Outcome
	detail  string
}

type echoRequest struct {
	ip   netip.Addr
	data [16]byte
	done chan echoReply
}

type echoSocket struct {
	conn                        *icmp.PacketConn
	proto                       int
	request, reply, unreachable icmp.Type
	mu                          sync.Mutex
	pending                     map[uint32]*echoRequest
	readError                   error
	closed                      chan struct{}
}

func openEcho(network, bind string, proto int, request, reply, unreachable icmp.Type) (*echoSocket, error) {
	conn, err := icmp.ListenPacket(network, bind)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", network, err)
	}
	s := &echoSocket{
		conn: conn, proto: proto, request: request, reply: reply, unreachable: unreachable,
		pending: make(map[uint32]*echoRequest), closed: make(chan struct{}),
	}
	go s.receive()
	return s, nil
}

func (s *echoSocket) ping(ctx context.Context, ip netip.Addr) (Result, time.Time) {
	if err := ctx.Err(); err != nil {
		return failure(err), time.Time{}
	}
	request := &echoRequest{ip: ip.WithZone(""), done: make(chan echoReply, 1)}
	var key uint32
	for {
		if _, err := rand.Read(request.data[:]); err != nil {
			return failure(err), time.Time{}
		}
		key = binary.BigEndian.Uint32(request.data[:4])
		s.mu.Lock()
		if s.readError != nil {
			err := s.readError
			s.mu.Unlock()
			return failure(err), time.Time{}
		}
		if s.pending[key] == nil {
			s.pending[key] = request
			s.mu.Unlock()
			break
		}
		s.mu.Unlock()
	}
	defer func() {
		s.mu.Lock()
		delete(s.pending, key)
		s.mu.Unlock()
	}()
	message := icmp.Message{
		Type: s.request,
		Body: &icmp.Echo{ID: int(key >> 16), Seq: int(key & 0xffff), Data: request.data[:]},
	}
	packet, err := message.Marshal(nil)
	if err != nil {
		return failure(err), time.Time{}
	}
	sentAt := time.Now()
	if _, err := s.conn.WriteTo(packet, &net.IPAddr{IP: ip.AsSlice(), Zone: ip.Zone()}); err != nil {
		return failure(err), time.Time{}
	}
	select {
	case result := <-request.done:
		if deadline, ok := ctx.Deadline(); ok && result.at.After(deadline) {
			return failure(context.DeadlineExceeded), sentAt
		}
		r := Result{Outcome: result.outcome, Detail: result.detail, CompletedAt: result.at}
		if r.Outcome == Passed {
			r.RTT = result.at.Sub(sentAt)
		}
		return r, sentAt
	case <-s.closed:
		s.mu.Lock()
		err := s.readError
		s.mu.Unlock()
		return failure(err), sentAt
	case <-ctx.Done():
		return failure(ctx.Err()), sentAt
	}
}

func (s *echoSocket) receive() {
	buffer := make([]byte, 65535)
	for {
		n, peer, err := s.conn.ReadFrom(buffer)
		at := time.Now()
		if err != nil {
			s.mu.Lock()
			s.readError = err
			s.mu.Unlock()
			close(s.closed)
			return
		}
		message, err := icmp.ParseMessage(s.proto, buffer[:n])
		if err != nil {
			continue
		}
		switch message.Type {
		case s.reply:
			echo, ok := message.Body.(*icmp.Echo)
			source, addrOK := peer.(*net.IPAddr)
			if ok && addrOK && message.Code == 0 {
				ip, _ := netip.AddrFromSlice(source.IP)
				s.deliver(uint32(echo.ID)<<16|uint32(echo.Seq), ip.Unmap(), echo.Data, echoReply{at: at, outcome: Passed})
			}
		case s.unreachable:
			if body, ok := message.Body.(*icmp.DstUnreach); ok {
				ip, key, data, valid := quotedEcho(s.proto, body.Data)
				if valid {
					s.deliver(key, ip, data, echoReply{at: at, outcome: Unreachable, detail: fmt.Sprintf("ICMP destination unreachable (code %d)", message.Code)})
				}
			}
		}
	}
}

func (s *echoSocket) deliver(key uint32, ip netip.Addr, data []byte, result echoReply) {
	s.mu.Lock()
	defer s.mu.Unlock()
	request := s.pending[key]
	if request == nil || request.ip != ip {
		return
	}
	if result.outcome == Passed && !bytes.Equal(request.data[:], data) {
		return
	}
	if result.outcome == Unreachable && len(data) > 0 && !bytes.HasPrefix(request.data[:], data) {
		return
	}
	select {
	case request.done <- result:
	default:
	}
}
