package store

import (
	"database/sql"
	"errors"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"netpatrol/internal/monitor"
)

func TestAreaRemovalKeepsMovedDeviceHistory(t *testing.T) {
	ctx := t.Context()
	file := filepath.Join(t.TempDir(), "delete.db")
	db, err := Open(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	from, err := db.SaveArea(ctx, 0, "Original", "", "192.0.2.0/24")
	if err != nil {
		t.Fatal(err)
	}
	to, err := db.SaveArea(ctx, 0, "New area", "", "")
	if err != nil {
		t.Fatal(err)
	}
	input := DeviceInput{Name: "Camera", Address: "192.0.2.1", Port: 554, AreaID: from, Enabled: true}
	device, err := db.SaveDevice(ctx, 0, input)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	observation := Observation{DeviceID: device, Version: 1, Round: monitor.Round{
		Target:    monitor.Target{IP: netip.MustParseAddr(input.Address), Port: 554},
		StartedAt: now, CompletedAt: now, EchoSentAt: now,
		Ping: monitor.Result{Outcome: monitor.Passed}, TCP: monitor.Result{Outcome: monitor.Passed},
	}}
	if err := db.Record(ctx, []Observation{observation}); err != nil {
		t.Fatal(err)
	}
	// Give the fixture the foreign key that existed before area deletion.
	if _, err := db.db.ExecContext(ctx, `
		ALTER TABLE state_events RENAME TO events_fixture;
		CREATE TABLE state_events (
			id INTEGER PRIMARY KEY, device_id INTEGER NOT NULL REFERENCES devices(id),
			area_id INTEGER NOT NULL REFERENCES areas(id), area_name TEXT NOT NULL,
			version INTEGER NOT NULL, at_ns INTEGER NOT NULL, event TEXT NOT NULL
		) STRICT;
		INSERT INTO state_events SELECT * FROM events_fixture;
		DROP TABLE events_fixture;`); err != nil {
		t.Fatal(err)
	}
	migrated, err := Open(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	if err := migrated.DeleteArea(ctx, from); !errors.Is(err, ErrAreaInUse) {
		t.Fatalf("nonempty area deletion = %v", err)
	}
	input.AreaID = to
	if _, err := migrated.SaveDevice(ctx, device, input); err != nil {
		t.Fatal(err)
	}
	job, err := migrated.CreateDiscovery(ctx, from, netip.MustParsePrefix("192.0.2.0/24"), []uint16{554}, 254)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrated.DeleteArea(ctx, from); err != nil {
		t.Fatal(err)
	}
	events, err := migrated.Events(ctx, from, device, time.Time{}, time.Time{}, 50, 0)
	if err != nil || len(events) != 1 || events[0].AreaName != "Original" {
		t.Fatalf("moved device history = %+v, %v", events, err)
	}
	networks, err := migrated.Networks(ctx)
	if err != nil || len(networks) != 1 {
		t.Fatalf("network catalog = %v, %v", networks, err)
	}
	discovery, _, err := migrated.Discovery(ctx, job)
	if err != nil || discovery.PreferredAreaID != 0 {
		t.Fatalf("discovery lost or stale area retained: %+v, %v", discovery, err)
	}
	if err := migrated.DeleteDevice(ctx, device); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"latest_results", "ping_minutes", "state_events"} {
		var count int
		if err := migrated.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s count = %d, %v", table, count, err)
		}
	}
	replacement, err := migrated.SaveDevice(ctx, 0, input)
	if err != nil || replacement <= device {
		t.Fatalf("replacement = %d, %v", replacement, err)
	}
	if err := migrated.Record(ctx, []Observation{observation}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := migrated.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM latest_results").Scan(&count); err != nil || count != 0 {
		t.Fatalf("old observation reached replacement: count=%d %v", count, err)
	}
}

func TestPathRemovalClearsChangesAndAvoidsIDReuse(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "path-delete.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	input := PathInput{Name: "Gateway", Address: "192.0.2.1", Enabled: true, IntervalSeconds: 60}
	now := time.Now()
	id, err := db.SavePath(ctx, 0, input, now)
	if err != nil {
		t.Fatal(err)
	}
	due, err := db.DuePathChecks(ctx, now, 2)
	if err != nil || len(due) != 1 {
		t.Fatalf("due checks = %+v, %v", due, err)
	}
	sample := PathSample{Address: due[0].Address, Started: now, Completed: now}
	if err := db.RecordPathCheck(ctx, due[0], sample); err != nil {
		t.Fatal(err)
	}
	if err := db.DeletePath(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PathWatch(ctx, id); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted path error = %v", err)
	}
	replacement, err := db.SavePath(ctx, 0, input, now)
	if err != nil || replacement <= id {
		t.Fatalf("path replacement = %d, %v", replacement, err)
	}
	if err := db.RecordPathCheck(ctx, due[0], sample); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM probe_path_changes").Scan(&count); err != nil || count != 0 {
		t.Fatalf("path changes after deletion = %d, %v", count, err)
	}
}
