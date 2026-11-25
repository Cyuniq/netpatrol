package scheduler

import (
	"testing"
	"time"

	"netpatrol/internal/monitor"
	"netpatrol/internal/store"
)

func TestStableFleetPhases(t *testing.T) {
	start := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
	phases := map[time.Time]int{}
	for id := int64(1); id <= 5000; id++ {
		due := firstDue(id, start, monitor.FastInterval)
		if due.Before(start) || !due.Before(start.Add(monitor.FastInterval)) {
			t.Fatalf("device %d due outside initial period: %s", id, due)
		}
		if !firstDue(id, start.Add(time.Hour), monitor.FastInterval).Equal(due.Add(time.Hour)) {
			t.Fatalf("device %d has an unstable phase", id)
		}
		phases[due]++
	}
	if len(phases) != 200 {
		t.Fatalf("fleet used %d of 200 phases", len(phases))
	}
	for _, count := range phases {
		if count > 40 {
			t.Fatalf("fleet launches %d devices in one 50ms phase", count)
		}
	}
}

func TestConfigurationVersionResetsScheduling(t *testing.T) {
	start := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
	device := store.Device{ID: 1, Enabled: true, Version: 1}
	settings := monitor.DefaultSettings()
	old := &queuedDevice{device: device, next: start.Add(time.Minute), snapshot: monitor.Snapshot{
		State: monitor.Down, Interval: monitor.SlowInterval, BothFailedSince: start.Add(-time.Hour),
	}}
	entries := reconcile(map[int64]*queuedDevice{1: old}, []store.Device{device}, start, settings)
	if entries[1] != old {
		t.Fatal("unchanged configuration lost schedule")
	}
	device.Version++
	entries = reconcile(entries, []store.Device{device}, start, settings)
	next := entries[1]
	if next == old || next.snapshot.State != monitor.Unknown || next.snapshot.Interval != monitor.FastInterval {
		t.Fatalf("changed configuration retained previous evaluation: %+v", next)
	}
	device.Enabled = false
	if entries := reconcile(entries, []store.Device{device}, start, settings); len(entries) != 0 {
		t.Fatal("disabled device remains scheduled")
	}
}

func TestSettingsRescheduleExistingDeviceWithoutResettingFailure(t *testing.T) {
	start := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
	oldSettings := monitor.DefaultSettings()
	round := monitor.Round{
		StartedAt: start, CompletedAt: start.Add(time.Second),
		Ping: monitor.Result{Outcome: monitor.Timeout}, TCP: monitor.Result{Outcome: monitor.Timeout},
	}
	device := queuedDevice{device: store.Device{ID: 1}, next: start, snapshot: monitor.NewSnapshot(start)}
	device.advance(round, oldSettings)
	round.StartedAt = start.Add(10 * time.Second)
	round.CompletedAt = round.StartedAt.Add(time.Second)
	device.advance(round, oldSettings)
	settings := oldSettings
	settings.SlowAfter = 9 * time.Second
	settings.SlowInterval = 22 * time.Second
	now := start.Add(12 * time.Second)
	device.reschedule(now, settings)
	if device.snapshot.Interval != settings.SlowInterval || device.snapshot.State != monitor.Down ||
		!device.snapshot.BothFailedSince.Equal(start.Add(time.Second)) ||
		device.next.Before(now) || !device.next.Before(now.Add(settings.SlowInterval)) {
		t.Fatalf("changed timing reset state or kept old interval: %+v", device)
	}
	settings.SlowAfter = 30 * time.Second
	settings.FastInterval = 4 * time.Second
	device.reschedule(now, settings)
	if device.snapshot.Interval != settings.FastInterval || !device.next.Before(now.Add(settings.FastInterval)) {
		t.Fatalf("did not return to the configured fast schedule: %+v", device)
	}
}
