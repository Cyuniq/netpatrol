package store

import (
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"netpatrol/internal/monitor"
)

func TestRetentionKeepsRecentAndRunningData(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "retention.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	area, err := db.SaveArea(ctx, 0, "Area", "", "192.0.2.0/24")
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.SaveDevice(ctx, 0, DeviceInput{
		Code: "CAM-1", Name: "Camera", AreaID: area, Address: "192.0.2.1", Port: 554, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Minute)
	target := monitor.Target{IP: netip.MustParseAddr("192.0.2.1"), Port: 554}
	times := []time.Time{now.Add(-181 * 24 * time.Hour), now.Add(-3 * time.Hour), now.Add(-30 * time.Minute)}
	for index, at := range times {
		outcome := monitor.Passed
		if index == 1 {
			outcome = monitor.Timeout
		}
		result := monitor.Result{Outcome: outcome, CompletedAt: at}
		if err := db.Record(ctx, []Observation{{DeviceID: id, Version: 1, Round: monitor.Round{
			Target: target, StartedAt: at, CompletedAt: at, EchoSentAt: at, Ping: result, TCP: result,
		}}}); err != nil {
			t.Fatal(err)
		}
	}
	prefix := netip.MustParsePrefix("192.0.2.1/32")
	old, err := db.CreateDiscovery(ctx, area, prefix, []uint16{554}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.InterruptDiscovery(ctx, old, "old task"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.ExecContext(ctx, "UPDATE discovery_jobs SET finished_ns = ? WHERE id = ?",
		now.Add(-25*time.Hour).UnixNano(), old); err != nil {
		t.Fatal(err)
	}
	running, err := db.CreateDiscovery(ctx, area, prefix, []uint16{554}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PruneMonitoring(ctx, now); err != nil {
		t.Fatal(err)
	}
	for table, count := range map[string]int{"ping_minutes": 1, "state_events": 2, "discovery_jobs": 1} {
		var found int
		if err := db.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&found); err != nil || found != count {
			t.Fatalf("%s retained %d records, want %d, error %v", table, found, count, err)
		}
	}
	if job, _, err := db.Discovery(ctx, running); err != nil || job.State != "running" {
		t.Fatalf("running discovery changed: %+v, error %v", job, err)
	}
}

func TestLinkLocalScopeIsPartOfUniqueAddress(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "scopes.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	id, err := db.SaveArea(ctx, 0, "IPv6", "", "fe80::/64")
	if err != nil {
		t.Fatal(err)
	}
	input := DeviceInput{Name: "Camera", AreaID: id, Address: "fe80:0::21%eth0", Port: 554, Enabled: true}
	if _, err := db.SaveDevice(ctx, 0, input); err != nil {
		t.Fatal(err)
	}
	input.Address = "fe80::21%eth0"
	if _, err := db.SaveDevice(ctx, 0, input); err == nil {
		t.Fatal("canonical duplicate accepted")
	}
	input.Address = "fe80::21%eth1"
	if _, err := db.SaveDevice(ctx, 0, input); err != nil {
		t.Fatal(err)
	}
	input.Address = "fe80::21"
	if _, err := db.SaveDevice(ctx, 0, input); err == nil {
		t.Fatal("unscoped link-local address accepted")
	}
}
