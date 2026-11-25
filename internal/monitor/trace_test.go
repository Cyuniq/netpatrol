package monitor

import (
	"encoding/binary"
	"net/netip"
	"testing"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

func TestTraceMatchesQuotedEchoForBothFamilies(t *testing.T) {
	const key = 0x91f20003
	nonce := []byte("payload")
	tests := []struct {
		target, source           string
		proto, offset            int
		hopType, unreachableType icmp.Type
	}{
		{"192.0.2.9", "192.0.2.1", 1, 20, ipv4.ICMPTypeTimeExceeded, ipv4.ICMPTypeDestinationUnreachable},
		{"2001:db8::9", "2001:db8::1", 58, 40, ipv6.ICMPTypeTimeExceeded, ipv6.ICMPTypeDestinationUnreachable},
	}
	for _, test := range tests {
		t.Run(test.target, func(t *testing.T) {
			target, source := netip.MustParseAddr(test.target), netip.MustParseAddr(test.source)
			quote := make([]byte, test.offset+8+len(nonce))
			if test.proto == 1 {
				quote[0], quote[9], quote[test.offset] = 0x45, 1, 8
				copy(quote[16:20], target.AsSlice())
			} else {
				quote[0], quote[6], quote[test.offset] = 0x60, 58, 128
				copy(quote[24:40], target.AsSlice())
			}
			binary.BigEndian.PutUint32(quote[test.offset+4:], key)
			copy(quote[test.offset+8:], nonce)
			reply := &icmp.Message{Type: test.hopType, Body: &icmp.TimeExceeded{Data: quote}}
			if got, _ := traceReply(test.proto, reply, source, target, key, nonce); got != HopReply {
				t.Fatalf("time-exceeded outcome = %q", got)
			}
			if got, _ := traceReply(test.proto, reply, source, target, key+1, nonce); got != "" {
				t.Fatal("accepted quoted reply from another hop")
			}
			if got, _ := traceReply(test.proto, reply, source, source, key, nonce); got != "" {
				t.Fatal("accepted quoted reply for a different destination")
			}
			reply = &icmp.Message{Type: test.unreachableType, Body: &icmp.DstUnreach{Data: quote[:test.offset+8]}}
			if got, _ := traceReply(test.proto, reply, source, target, key, nonce); got != Unreachable {
				t.Fatalf("truncated destination-unreachable outcome = %q", got)
			}
		})
	}
}

func TestTraceStatisticsCountResponsesAndExcludeIncompleteAttempts(t *testing.T) {
	now := time.Now()
	first := netip.MustParseAddr("192.0.2.2")
	second := netip.MustParseAddr("192.0.2.1")
	hop := TraceHop{Attempts: []TraceAttempt{
		{Sent: true, Address: first, Result: Result{Outcome: HopReply, CompletedAt: now.Add(time.Second), RTT: 14 * time.Millisecond}},
		{Sent: true, Result: Result{Outcome: Timeout}},
		{Sent: true, Address: second, Result: Result{Outcome: Unreachable, CompletedAt: now, RTT: 4 * time.Millisecond}},
	}}
	hop.selectResult()
	if hop.Address != first || hop.Result.Outcome != HopReply {
		t.Fatalf("selected reply = %+v", hop)
	}
	stats := hop.Stats()
	if stats.Sent != 3 || stats.Observed != 3 || stats.Replies != 2 || stats.LossPercent < 33.3 ||
		stats.LossPercent > 33.4 || stats.Best != 4*time.Millisecond || stats.Average != 9*time.Millisecond ||
		stats.Worst != 14*time.Millisecond || stats.Last != stats.Worst {
		t.Fatalf("hop stats = %+v", stats)
	}
	if addresses := hop.Addresses(); len(addresses) != 2 || addresses[0] != second || addresses[1] != first {
		t.Fatalf("responders = %v", addresses)
	}
	hop.Attempts[1].Result = Result{Outcome: Timeout, CompletedAt: now.Add(time.Second)}
	hop.markIncomplete(now)
	stats = hop.Stats()
	if stats.Sent != 3 || stats.Observed != 2 || stats.LossPercent != 0 {
		t.Fatalf("incomplete attempt counted as loss: %+v", stats)
	}
}
