package monitor

import (
	"net/netip"
	"slices"
	"time"
)

type TraceStats struct {
	Sent, Replies, Observed    int
	LossPercent                float64
	Last, Average, Best, Worst time.Duration
}

func (h TraceHop) Stats() TraceStats {
	var stats TraceStats
	var total time.Duration
	var last time.Time
	for _, attempt := range h.Attempts {
		if !attempt.Sent {
			continue
		}
		stats.Sent++
		if attempt.Result.Outcome != LocalError {
			stats.Observed++
		}
		if !attempt.Address.IsValid() {
			continue
		}
		rtt := attempt.Result.RTT
		stats.Replies++
		total += rtt
		if stats.Replies == 1 || rtt < stats.Best {
			stats.Best = rtt
		}
		stats.Worst = max(stats.Worst, rtt)
		if !attempt.Result.CompletedAt.Before(last) {
			last, stats.Last = attempt.Result.CompletedAt, rtt
		}
	}
	if stats.Observed != 0 {
		stats.LossPercent = float64(stats.Observed-stats.Replies) * 100 / float64(stats.Observed)
	}
	if stats.Replies != 0 {
		stats.Average = total / time.Duration(stats.Replies)
	}
	return stats
}

func (h *TraceHop) markIncomplete(now time.Time) {
	for i := range h.Attempts {
		attempt := &h.Attempts[i]
		if attempt.Result.Outcome == Timeout && attempt.Result.CompletedAt.After(now) {
			attempt.Result.Outcome = LocalError
		}
	}
}

func (h TraceHop) Addresses() []netip.Addr {
	var addresses []netip.Addr
	if h.Address.IsValid() {
		addresses = append(addresses, h.Address)
	}
	for _, attempt := range h.Attempts {
		if attempt.Address.IsValid() && !slices.Contains(addresses, attempt.Address) {
			addresses = append(addresses, attempt.Address)
		}
	}
	slices.SortFunc(addresses, netip.Addr.Compare)
	return addresses
}

func (h *TraceHop) selectResult() {
	for _, attempt := range h.Attempts {
		current := traceOutcomeRank(h.Result.Outcome)
		next := traceOutcomeRank(attempt.Result.Outcome)
		if next > current || (next == current && attempt.Address.Compare(h.Address) < 0) {
			h.Address, h.Result = attempt.Address, attempt.Result
		}
	}
}

func traceOutcomeRank(outcome Outcome) int {
	switch outcome {
	case Passed:
		return 5
	case HopReply:
		return 4
	case Unreachable:
		return 3
	case LocalError:
		return 2
	case Timeout:
		return 1
	default:
		return 0
	}
}
