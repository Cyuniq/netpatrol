package monitor

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"
)

func (p *Prober) Probe(ctx context.Context, target Target) Round {
	return p.ProbeWithSettings(ctx, target, DefaultSettings())
}

func (p *Prober) ProbeWithSettings(ctx context.Context, target Target, settings Settings) Round {
	round := Round{Target: target, StartedAt: time.Now()}
	var wait sync.WaitGroup
	wait.Go(func() {
		round.Ping, round.EchoSentAt = p.probePing(ctx, target.IP, settings.PingTimeout)
	})
	wait.Go(func() {
		round.TCP = probeTCP(ctx, target, settings.TCPTimeout)
	})
	wait.Wait()
	round.CompletedAt = time.Now()
	return round
}

type DiscoveryProbe struct {
	IP    netip.Addr  `json:"ip"`
	Ping  Result      `json:"ping"`
	Ports []PortProbe `json:"ports"`
}

type PortProbe struct {
	Port   uint16 `json:"port"`
	Result Result `json:"result"`
}

func (p *Prober) ProbeDiscovery(ctx context.Context, ip netip.Addr, ports []uint16, settings Settings) DiscoveryProbe {
	observation := DiscoveryProbe{IP: ip, Ports: make([]PortProbe, len(ports))}
	var wait sync.WaitGroup
	wait.Go(func() {
		observation.Ping, _ = p.probePing(ctx, ip, settings.PingTimeout)
	})
	for i, port := range ports {
		wait.Go(func() {
			observation.Ports[i] = PortProbe{Port: port, Result: probeTCP(ctx, Target{IP: ip, Port: port}, settings.TCPTimeout)}
		})
	}
	wait.Wait()
	return observation
}

func probeTCP(ctx context.Context, target Target, duration time.Duration) Result {
	timeout, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	start := time.Now()
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(timeout, "tcp", net.JoinHostPort(target.IP.String(), strconv.Itoa(int(target.Port))))
	if err != nil {
		return failure(err)
	}
	end := time.Now()
	conn.Close()
	return Result{Outcome: Passed, CompletedAt: end, RTT: end.Sub(start)}
}

func failure(err error) Result {
	outcome := LocalError
	var networkError net.Error
	switch {
	case errors.Is(err, connectionRefused):
		outcome = Refused
	case errors.Is(err, hostUnreachable), errors.Is(err, networkUnreachable):
		outcome = Unreachable
	case errors.As(err, &networkError) && networkError.Timeout():
		outcome = Timeout
	}
	return Result{Outcome: outcome, Detail: err.Error(), CompletedAt: time.Now()}
}

func quotedEcho(proto int, packet []byte) (netip.Addr, uint32, []byte, bool) {
	var ip netip.Addr
	var echoType byte
	switch {
	case proto == 1 && len(packet) >= 20 && packet[0]>>4 == 4 && packet[9] == 1:
		offset := int(packet[0]&15) * 4
		if offset < 20 || len(packet) < offset+8 {
			return netip.Addr{}, 0, nil, false
		}
		ip = netip.AddrFrom4([4]byte(packet[16:20]))
		packet, echoType = packet[offset:], 8
	case proto == 58 && len(packet) >= 48 && packet[0]>>4 == 6 && packet[6] == 58:
		ip = netip.AddrFrom16([16]byte(packet[24:40]))
		packet, echoType = packet[40:], 128
	default:
		return netip.Addr{}, 0, nil, false
	}
	if packet[0] != echoType || packet[1] != 0 {
		return netip.Addr{}, 0, nil, false
	}
	return ip, binary.BigEndian.Uint32(packet[4:8]), packet[8:min(len(packet), 24)], true
}
