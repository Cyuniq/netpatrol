package store

import (
	"encoding/json"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"netpatrol/internal/monitor"
)

func TestOpenEnablesWALAndForeignKeys(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "net patrol #1.db")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, "INSERT INTO area_networks VALUES (99, 99)"); err == nil {
		t.Fatal("accepted a network without its parent area")
	}
	if _, err := store.db.ExecContext(ctx, "INSERT INTO areas(name) VALUES ('East gate')"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var name, mode string
	if err := store.db.QueryRowContext(ctx, "SELECT name FROM areas WHERE id = 1").Scan(&name); err != nil || name != "East gate" {
		t.Fatalf("reopened name = %q, error = %v", name, err)
	}
	if err := store.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal mode = %q, error = %v", mode, err)
	}
}

func TestRecordRejectsDuplicateAndOldConfiguration(t *testing.T) {
	ctx := t.Context()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "observations.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO areas(id, name) VALUES (1, 'East gate');
		INSERT INTO devices(id, code, name, area_id, address, port)
		VALUES (1, 'CAM-1', 'Camera', 1, '192.0.2.21', 554);
	`); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
	target := monitor.Target{IP: netip.MustParseAddr("192.0.2.21"), Port: 554}
	round := monitor.Round{
		Target: target, StartedAt: at, CompletedAt: at.Add(time.Second), EchoSentAt: at,
		Ping: monitor.Result{Outcome: monitor.Timeout, CompletedAt: at.Add(time.Second)},
		TCP:  monitor.Result{Outcome: monitor.Refused, CompletedAt: at},
	}
	first := Observation{DeviceID: 1, Version: 1, Round: round}
	if err := s.Record(ctx, []Observation{first, first}); err != nil {
		t.Fatal(err)
	}
	loss, err := s.Loss(ctx, 1, at)
	if err != nil || loss.Sent != 1 || loss.Replies != 0 {
		t.Fatalf("after duplicate, loss = %+v, error = %v", loss, err)
	}
	var events int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM state_events").Scan(&events); err != nil || events != 1 {
		t.Fatalf("initial event count = %d, error = %v", events, err)
	}
	restart := at.Add(11 * time.Minute)
	if err := s.ResetMonitoring(ctx, restart); err != nil {
		t.Fatal(err)
	}
	resumed := first
	resumed.Round.StartedAt = restart
	resumed.Round.CompletedAt = restart.Add(time.Second)
	resumed.Round.EchoSentAt = restart
	if err := s.Record(ctx, []Observation{resumed}); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := s.db.QueryRowContext(ctx, "SELECT snapshot FROM latest_results WHERE device_id = 1").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	var snapshot monitor.Snapshot
	if err := json.Unmarshal([]byte(stored), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Interval != monitor.FastInterval || !snapshot.BothFailedSince.Equal(resumed.Round.CompletedAt) {
		t.Fatalf("failure continuity included downtime: %+v", snapshot)
	}
	if loss, err := s.Loss(ctx, 1, restart); err != nil || loss.Sent != 2 {
		t.Fatalf("loss after restart = %+v, error = %v", loss, err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE devices SET version = 2, port = 8554 WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	late := first
	late.Round.CompletedAt = restart.Add(11 * time.Second)
	if err := s.Record(ctx, []Observation{late}); err != nil {
		t.Fatal(err)
	}
	loss, err = s.Loss(ctx, 1, restart)
	if err != nil || loss.Sent != 0 {
		t.Fatalf("after configuration change, loss = %+v, error = %v", loss, err)
	}
	var version int
	if err := s.db.QueryRowContext(ctx, "SELECT version FROM latest_results WHERE device_id = 1").Scan(&version); err != nil || version != 1 {
		t.Fatalf("late result changed the stored version to %d, error = %v", version, err)
	}
}

func TestAssetNormalizationAndVersion(t *testing.T) {
	ctx := t.Context()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "assets.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	areaID, err := s.SaveArea(ctx, 0, "东门", "接入交换机", "192.0.2.21/24, 192.0.2.0/24\n2001:db8::/64")
	if err != nil {
		t.Fatal(err)
	}
	areas, err := s.Areas(ctx)
	if err != nil || len(areas) != 1 || len(areas[0].Networks) != 2 || areas[0].Networks[0].String() != "192.0.2.0/24" {
		t.Fatalf("normalized areas = %+v, error = %v", areas, err)
	}
	input := DeviceInput{Code: "CAM-1", Name: "东门入口", Address: "::ffff:192.0.2.21", Port: 554, Enabled: true}
	deviceID, err := s.SaveDevice(ctx, 0, input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveDevice(ctx, 0, DeviceInput{
		Name: "重复", Address: "192.0.2.21", Port: 8554, Enabled: true,
	}); err == nil {
		t.Fatal("accepted duplicate canonical address")
	}
	input.Name = "东门入口球机"
	if _, err := s.SaveDevice(ctx, deviceID, input); err != nil {
		t.Fatal(err)
	}
	devices, err := s.Devices(ctx)
	if err != nil || len(devices) != 1 || devices[0].Version != 1 || devices[0].AreaID != areaID {
		t.Fatalf("devices after rename = %+v, error = %v", devices, err)
	}
	input.Port = 8554
	if _, err := s.SaveDevice(ctx, deviceID, input); err != nil {
		t.Fatal(err)
	}
	devices, err = s.Devices(ctx)
	if err != nil || len(devices) != 1 || devices[0].Version != 2 || devices[0].Target.Port != 8554 {
		t.Fatalf("devices after port change = %+v, error = %v", devices, err)
	}
}
