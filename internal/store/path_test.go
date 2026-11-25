package store

import (
	"encoding/json"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"netpatrol/internal/monitor"
)

func TestPathWatchSchedulesAndRecordsVisibleChanges(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "paths.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
	due := func(at time.Time, count int) []PathTask {
		t.Helper()
		tasks, err := db.DuePathChecks(ctx, at, 2)
		if err != nil || len(tasks) != count {
			t.Fatalf("due at %s = %+v, error = %v", at, tasks, err)
		}
		return tasks
	}
	due(now, 0)
	input := PathInput{Name: "Gateway", Address: "192.0.2.10", Enabled: true, IntervalSeconds: 60}
	id, err := db.SavePath(ctx, 0, input, now)
	if err != nil {
		t.Fatal(err)
	}
	task := due(now, 1)[0]
	completed := now.Add(time.Second)
	sample := PathSample{Address: task.Address, Started: now, Completed: completed, Result: monitor.TraceResult{Reached: true,
		Hops: []monitor.TraceHop{{Number: 1, Address: netip.MustParseAddr("192.0.2.1"), Result: monitor.Result{Outcome: monitor.HopReply}},
			{Number: 2, Address: task.Address, Result: monitor.Result{Outcome: monitor.Passed}}},
	}}
	if err := db.RecordPathCheck(ctx, task, sample); err != nil {
		t.Fatal(err)
	}
	next := completed.Add(time.Minute)
	due(next.Add(-time.Nanosecond), 0)
	task = due(next, 1)[0]
	sample.Completed = next
	sample.Result.Hops[0].Result.RTT += time.Millisecond
	if err := db.RecordPathCheck(ctx, task, sample); err != nil {
		t.Fatal(err)
	}
	watch, err := db.PathWatch(ctx, id)
	if err != nil || watch.Latest == nil || !watch.Latest.Completed.Equal(next) || len(watch.Changes) != 1 {
		t.Fatalf("RTT updated without route change: %+v, %v", watch, err)
	}
	sample.Completed = next.Add(time.Minute)
	task = due(sample.Completed, 1)[0]
	sample.Result.Hops[0].Address = netip.MustParseAddr("192.0.2.2")
	if err := db.RecordPathCheck(ctx, task, sample); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	watch, err = reopened.PathWatch(ctx, id)
	if err != nil || !watch.Enabled || len(watch.Changes) != 2 || !watch.NextCheck.Equal(sample.Completed.Add(time.Minute)) {
		t.Fatalf("persistent path watch = %+v, error = %v", watch, err)
	}
	input.Enabled = false
	if _, err := db.SavePath(ctx, id, input, sample.Completed); err != nil {
		t.Fatal(err)
	}
	sample.Completed = sample.Completed.Add(time.Minute)
	sample.Result.Hops[0].Address = netip.MustParseAddr("192.0.2.3")
	if err := db.RecordPathCheck(ctx, task, sample); err != nil {
		t.Fatal(err)
	}
	due(sample.Completed, 0)
	watch, err = db.PathWatch(ctx, id)
	if err != nil || watch.Enabled || !watch.Latest.Completed.Before(sample.Completed) || len(watch.Changes) != 2 {
		t.Fatalf("disabled watch accepted old result: %+v, %v", watch, err)
	}
}

func TestDevicePathWatchMigrationKeepsScheduleAndHistory(t *testing.T) {
	ctx := t.Context()
	file := filepath.Join(t.TempDir(), "legacy-path.db")
	db, err := Open(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	area, err := db.SaveArea(ctx, 0, "Legacy", "", "")
	if err != nil {
		t.Fatal(err)
	}
	device, err := db.SaveDevice(ctx, 0, DeviceInput{AreaID: area, Name: "Legacy target", Address: "192.0.2.3", Port: 554, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	timestamp := time.Now().UTC()
	sample := PathSample{Address: netip.MustParseAddr("192.0.2.3"), Started: timestamp, Completed: timestamp,
		Result: monitor.TraceResult{Reached: true}}
	encoded, err := json.Marshal(sample)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.db.ExecContext(ctx, `
		CREATE TABLE path_watch (
		  device_id INTEGER PRIMARY KEY REFERENCES devices(id), enabled INTEGER NOT NULL,
		  interval_seconds INTEGER NOT NULL, generation INTEGER NOT NULL, next_check_ns INTEGER NOT NULL,
		  observed_version INTEGER, signature TEXT, latest TEXT
		);
		CREATE TABLE path_changes (
		  id INTEGER PRIMARY KEY, device_id INTEGER REFERENCES devices(id), sample TEXT NOT NULL
		);
		INSERT INTO path_watch VALUES (?,1,123,1,?,1,'signature',?);
		INSERT INTO path_changes(id,device_id,sample) SELECT 1, device_id, latest FROM path_watch;
	`, device, timestamp.Add(123*time.Second).UnixNano(), string(encoded))
	if err != nil {
		t.Fatal(err)
	}
	migrated, err := Open(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	watch, err := migrated.PathWatch(ctx, device)
	if err != nil || !watch.Enabled || watch.Name != "Legacy target" || watch.Address != "192.0.2.3" ||
		watch.IntervalSeconds != 123 || watch.Latest == nil || !watch.Latest.Result.Reached ||
		len(watch.Changes) != 1 || !watch.NextCheck.Equal(timestamp.Add(123*time.Second)) {
		t.Fatalf("migrated path watch: %+v error=%v", watch, err)
	}
	if _, err := migrated.SaveDevice(ctx, device, DeviceInput{AreaID: area, Name: "Other", Address: "192.0.2.4", Port: 554}); err != nil {
		t.Fatal(err)
	}
	paths, err := migrated.ListPaths(ctx)
	if err != nil || len(paths) != 1 || !paths[0].Enabled || paths[0].Address != "192.0.2.3" {
		t.Fatalf("device edits affected the independent target: %+v error=%v", paths, err)
	}
}
