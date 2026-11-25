package monitor

import (
	"math"
	"testing"
	"time"
)

func sample(at time.Time, ping, tcp Outcome) Round {
	return Round{
		StartedAt: at.Add(-time.Second), CompletedAt: at,
		Ping: Result{Outcome: ping, CompletedAt: at},
		TCP:  Result{Outcome: tcp, CompletedAt: at},
	}
}

func TestFailureThresholdAndRecovery(t *testing.T) {
	start := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
	snapshot := NewSnapshot(start)
	event := snapshot.Apply(sample(start, Timeout, Refused))
	if event == nil || event.Previous != "" || event.Current != Down {
		t.Fatalf("first dual failure = %+v", event)
	}
	for seconds := 10; seconds <= 600; seconds += 10 {
		at := start.Add(time.Duration(seconds) * time.Second)
		event := snapshot.Apply(sample(at, Timeout, Timeout))
		if event != nil || !snapshot.Since.Equal(start) || !snapshot.BothFailedSince.Equal(start) {
			t.Fatalf("persistent failure at %s changed state", at)
		}
		expected := FastInterval
		if seconds == 600 {
			expected = SlowInterval
		}
		if snapshot.Interval != expected {
			t.Fatalf("interval at %ds = %s, want %s", seconds, snapshot.Interval, expected)
		}
	}
	recovery := start.Add(11 * time.Minute)
	event = snapshot.Apply(sample(recovery, Timeout, Passed))
	if event == nil || event.Previous != Down || event.Current != Healthy ||
		snapshot.Interval != FastInterval || !snapshot.BothFailedSince.IsZero() ||
		snapshot.TCPLastSuccess != recovery {
		t.Fatalf("one-sided recovery = %+v, snapshot = %+v", event, snapshot)
	}
	event = snapshot.Apply(sample(recovery.Add(FastInterval), Passed, Passed))
	if event != nil || snapshot.State != Healthy || !snapshot.Since.Equal(recovery) {
		t.Fatalf("Ping recovery changed an already healthy device: %+v, snapshot = %+v", event, snapshot)
	}
	event = snapshot.Apply(sample(recovery.Add(2*FastInterval), Passed, Refused))
	if event == nil || event.Current != Partial {
		t.Fatalf("TCP-only failure = %+v", event)
	}
}

func TestMissingSampleBreaksFailureContinuity(t *testing.T) {
	start := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
	snapshot := NewSnapshot(start)
	if event := snapshot.Apply(sample(start, Timeout, LocalError)); event != nil || snapshot.State != Unknown {
		t.Fatalf("invalid initial round generated a transition: %+v", event)
	}
	firstFailure := start.Add(FastInterval)
	snapshot.Apply(sample(firstFailure, Timeout, Refused))
	gap := firstFailure.Add(SlowAfter - FastInterval)
	event := snapshot.Apply(sample(gap, Passed, LocalError))
	if event == nil || event.Current != Unknown || !snapshot.BothFailedSince.IsZero() ||
		snapshot.LastRound.Ping.Outcome != Passed {
		t.Fatalf("missing observation = %+v, snapshot = %+v", event, snapshot)
	}
	next := gap.Add(FastInterval)
	snapshot.Apply(sample(next, Timeout, Refused))
	if snapshot.State != Down || snapshot.BothFailedSince != next || snapshot.Interval != FastInterval {
		t.Fatalf("failure after a gap = %+v", snapshot)
	}
}

func TestTCPReachabilityDoesNotDependOnPing(t *testing.T) {
	for _, ping := range []Outcome{Timeout, Unreachable, LocalError, "", Passed} {
		if got := classify(sample(time.Now(), ping, Passed)); got != Healthy {
			t.Errorf("Ping %q with successful TCP = %s, want %s", ping, got, Healthy)
		}
	}
}

func TestLossWindow(t *testing.T) {
	start := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
	var window LossWindow
	for i := range 360 {
		sent := start.Add(time.Duration(i) * FastInterval)
		round := sample(sent.Add(time.Second), Passed, Passed)
		round.EchoSentAt = sent
		if i < 3 {
			round.Ping.Outcome = Timeout
		}
		window.Record(round)
	}
	check := start.Add(time.Hour - time.Second)
	window.Record(sample(check, LocalError, Passed))
	loss := window.At(check)
	percent, ok := loss.Percent()
	if loss.Sent != 360 || loss.Lost() != 3 || !ok || math.Abs(percent-0.833333333) > 0.000001 {
		t.Fatalf("loss = %+v, percent = %f", loss, percent)
	}
	next := start.Add(time.Hour)
	round := sample(next.Add(time.Second), Passed, Passed)
	round.EchoSentAt = next
	window.Record(round)
	if loss := window.At(next); loss.Sent != 355 || loss.Lost() != 0 {
		t.Fatalf("rolling into next minute = %+v", loss)
	}
	if _, ok := window.At(next.Add(time.Hour)).Percent(); ok {
		t.Fatal("expired window returned a percentage")
	}
}

func TestScheduledSkipPreservesLastProbeTime(t *testing.T) {
	start := time.Now()
	snapshot := NewSnapshot(start)
	snapshot.Apply(sample(start.Add(time.Second), Passed, Passed))
	skipped := start.Add(20 * time.Second)
	event := snapshot.Apply(Round{CompletedAt: skipped})
	if event == nil || event.Current != Unknown || !snapshot.LastProbeTime().Equal(start.Add(time.Second)) {
		t.Fatalf("skipped schedule replaced last real probe: event=%+v snapshot=%+v", event, snapshot)
	}
}
