package store

import (
	"path/filepath"
	"testing"
	"time"

	"netpatrol/internal/monitor"
)

func TestBackupRestoresAssetsAndMonitoringHistory(t *testing.T) {
	ctx := t.Context()
	source := filepath.Join(t.TempDir(), "live.db")
	backup := filepath.Join(t.TempDir(), "snapshot.db")
	live, err := Open(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	area, err := live.SaveArea(ctx, 0, "East", "Switch", "192.0.2.0/24")
	if err != nil {
		t.Fatal(err)
	}
	id, err := live.SaveDevice(ctx, 0, DeviceInput{Code: "CAM1", Name: "Loading dock", AreaID: area,
		Address: "192.0.2.21", Port: 554, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	target, err := monitor.ParseTarget("192.0.2.21", 554)
	if err != nil {
		t.Fatal(err)
	}
	round := monitor.Round{Target: target, StartedAt: now, CompletedAt: now, EchoSentAt: now,
		Ping: monitor.Result{Outcome: monitor.Passed, CompletedAt: now},
		TCP:  monitor.Result{Outcome: monitor.Refused, CompletedAt: now},
	}
	if err := live.Record(ctx, []Observation{{DeviceID: id, Version: 1, Round: round}}); err != nil {
		t.Fatal(err)
	}
	if err := live.Backup(ctx, backup); err != nil {
		t.Fatal(err)
	}
	if err := live.Backup(ctx, backup); err == nil {
		t.Fatal("backup overwrote an existing destination")
	}
	restored, err := Open(ctx, backup)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	devices, _, err := restored.DeviceViews(ctx, DeviceFilter{}, now, 50, 0)
	if err != nil || len(devices) != 1 || devices[0].Code != "CAM1" ||
		devices[0].AreaPath != "Switch" || devices[0].Snapshot.State != monitor.Partial ||
		devices[0].Loss.Sent != 1 || devices[0].Loss.Replies != 1 {
		t.Fatalf("restored data = %+v, %v", devices, err)
	}
	events, err := restored.Events(ctx, area, id, time.Time{}, time.Time{}, 50, 0)
	if err != nil || len(events) != 1 || events[0].Current != monitor.Partial {
		t.Fatalf("restored events = %+v, %v", events, err)
	}
}

func TestServiceLockRejectsSecondScheduler(t *testing.T) {
	path := filepath.Join(t.TempDir(), "live.db")
	first, err := LockService(path)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := LockService(path); err == nil {
		first.Close()
		second.Close()
		t.Fatal("second scheduler acquired lock")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	replacement, err := LockService(path)
	if err != nil {
		t.Fatal(err)
	}
	replacement.Close()
}
