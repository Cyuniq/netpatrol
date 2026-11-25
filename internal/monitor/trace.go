package monitor

import (
	"bytes"
	"fmt"
	"net/netip"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

const (
	TraceMaxHops = 24
	TraceQueries = 3
	traceTimeout = time.Second
	traceSpacing = 250 * time.Millisecond
)

type TraceHop struct {
	Number   int
	Address  netip.Addr
	Result   Result
	Attempts []TraceAttempt `json:",omitempty"`
}

type TraceAttempt struct {
	Address netip.Addr
	Result  Result
	Sent    bool
}

type TraceResult struct {
	Hops    []TraceHop
	Reached bool
}

const HopReply Outcome = "hop_reply"

func traceReply(proto int, msg *icmp.Message, source, target netip.Addr, key uint32, nonce []byte) (Outcome, string) {
	var quoted []byte
	var outcome Outcome
	switch msg.Type {
	case ipv4.ICMPTypeEchoReply, ipv6.ICMPTypeEchoReply:
		echo, ok := msg.Body.(*icmp.Echo)
		if ok && msg.Code == 0 && source.WithZone("") == target.WithZone("") &&
			uint32(echo.ID)<<16|uint32(echo.Seq) == key && bytes.Equal(echo.Data, nonce) {
			return Passed, ""
		}
	case ipv4.ICMPTypeTimeExceeded, ipv6.ICMPTypeTimeExceeded:
		if message, ok := msg.Body.(*icmp.TimeExceeded); ok && msg.Code == 0 {
			outcome, quoted = HopReply, message.Data
		}
	case ipv4.ICMPTypeDestinationUnreachable, ipv6.ICMPTypeDestinationUnreachable:
		if message, ok := msg.Body.(*icmp.DstUnreach); ok {
			outcome, quoted = Unreachable, message.Data
		}
	}
	ip, id, data, ok := quotedEcho(proto, quoted)
	if !ok || ip != target.WithZone("") || id != key || !bytes.HasPrefix(nonce, data) {
		return "", ""
	}
	if outcome == Unreachable {
		return outcome, fmt.Sprintf("ICMP 不可达（代码 %d）", msg.Code)
	}
	return outcome, ""
}
