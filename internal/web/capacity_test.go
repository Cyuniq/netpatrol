package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"netpatrol/internal/discovery"
	"netpatrol/internal/monitor"
	"netpatrol/internal/scheduler"
	"netpatrol/internal/store"
	"netpatrol/internal/telemetry"
)

// Run this separately from the ordinary test suite. Probe outcomes are
// simulated while scheduling, SQLite, discovery and HTTP run normally.
func TestCapacity5000(t *testing.T) {
	mode := os.Getenv("NETPATROL_LOADTEST")
	if mode == "" {
		t.Skip("set NETPATROL_LOADTEST=short or full")
	}
	failureDuration := 28 * time.Second
	if mode == "full" {
		failureDuration = 12 * time.Minute
	} else if mode != "short" {
		t.Fatalf("unsupported NETPATROL_LOADTEST %q", mode)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	path := filepath.Join(t.TempDir(), "capacity.db")
	db, err := store.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cameraArea, err := db.SaveArea(ctx, 0, "Fleet", "", "198.18.0.0/15")
	if err != nil {
		t.Fatal(err)
	}
	discoveryArea, err := db.SaveArea(ctx, 0, "Discovery", "", "192.0.2.0/24")
	if err != nil {
		t.Fatal(err)
	}
	const devices = 5000
	rows := make([]store.ImportRow, devices)
	for i := range rows {
		rows[i] = store.ImportRow{Line: i + 2, Area: "Fleet", DeviceInput: store.DeviceInput{
			Code: fmt.Sprintf("CAM-%05d", i), Name: fmt.Sprintf("Camera %05d", i),
			Address: fmt.Sprintf("198.18.%d.%d", (i+1)/256, (i+1)%256), Port: 554,
		}}
	}
	plan, err := db.PreviewImport(ctx, rows, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, commit, err := db.ConfirmImport(ctx, rows, false, plan); err != nil || !commit {
		t.Fatalf("seed: committed %v, error %v", commit, err)
	}

	var failAll atomic.Bool
	prober := discoveryTestProber(func(ctx context.Context, target monitor.Target) monitor.Round {
		start := time.Now()
		pingDone := start.Add(time.Millisecond)
		outcome := monitor.Passed
		delay := 2 * time.Millisecond
		if target.IP.As4()[0] == 198 && failAll.Load() {
			pingDone = start.Add(time.Second)
			outcome, delay = monitor.Timeout, 2*time.Second
		}
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
		case <-timer.C:
		}
		end := time.Now()
		return monitor.Round{Target: target, StartedAt: start, CompletedAt: end, EchoSentAt: start,
			Ping: monitor.Result{Outcome: outcome, CompletedAt: pingDone},
			TCP:  monitor.Result{Outcome: outcome, CompletedAt: end},
		}
	})
	metrics := new(telemetry.Metrics)
	var saturated atomic.Bool
	discover, err := discovery.New(ctx, db, prober, "192.0.2.0/24", 1024, func() bool { return !saturated.Load() })
	if err != nil {
		t.Fatal(err)
	}
	defer discover.Close()
	handler, err := Handler(db, "capacity-test", nil, discover, metrics, false)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()

	runDone := make(chan error, 1)
	go func() { runDone <- scheduler.Run(ctx, db, prober, 1200, &saturated, metrics) }()
	defer func() {
		cancel()
		if err := <-runDone; err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	}()
	request := func(route string) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+route, nil)
		if err != nil {
			return err
		}
		req.SetBasicAuth("admin", "capacity-test")
		req.Header.Set("HX-Request", "true")
		response, err := server.Client().Do(req)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("%s returned %d", route, response.StatusCode)
		}
		_, err = io.Copy(io.Discard, response.Body)
		return err
	}
	var workers sync.WaitGroup
	problem := make(chan error, 3)
	runEvery := func(interval time.Duration, task func() error) {
		workers.Go(func() {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if err := task(); err != nil && ctx.Err() == nil {
						problem <- err
						return
					}
				}
			}
		})
	}
	runEvery(300*time.Millisecond, func() error { return request("/devices?area=1&page=2") })
	runEvery(5*time.Second, func() error { return request("/exports/status.csv?area=1") })
	runEvery(9*time.Second, func() error {
		_, err := discover.Start(ctx, discoveryArea, "192.0.2.0/26", "554", discovery.Access{})
		if errors.Is(err, store.ErrDiscoveryBusy) {
			return nil
		}
		return err
	})
	defer func() { cancel(); workers.Wait() }()

	diskSize := func(suffix string) int64 {
		if stat, err := os.Stat(path + suffix); err == nil {
			return stat.Size()
		}
		return 0
	}
	runPhase := func(name string, duration time.Duration, expected monitor.State) telemetry.Snapshot {
		t.Helper()
		for elapsed := time.Duration(0); elapsed < duration; {
			step := min(5*time.Second, duration-elapsed)
			select {
			case err := <-problem:
				t.Fatal(err)
			case <-time.After(step):
			}
			elapsed += step
			snapshot := metrics.Snapshot(time.Now())
			result, err := json.Marshal(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("phase=%s elapsed=%s db_bytes=%d wal_bytes=%d %s", name, elapsed, diskSize(""), diskSize("-wal"), result)
		}
		groups, err := db.AreaStats(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, group := range groups {
			if group.ID == cameraArea {
				if group.Counts[expected] != devices {
					t.Fatalf("%s: states %v", name, group.Counts)
				}
				break
			}
		}
		snapshot := metrics.Snapshot(time.Now())
		if snapshot.ScheduledDevices != devices || snapshot.Missed != 0 {
			t.Fatalf("%s: missing schedules: %+v", name, snapshot)
		}
		return snapshot
	}
	runPhase("healthy", 18*time.Second, monitor.Healthy)
	failAll.Store(true)
	failure := runPhase("timeout", failureDuration, monitor.Down)
	if mode == "full" && failure.Interval.P95MS < 50000 {
		t.Fatalf("slow intervals were not observed: %+v", failure.Interval)
	}
	failAll.Store(false)
	recoveryDuration := 20 * time.Second
	if mode == "full" {
		recoveryDuration = 75 * time.Second
	}
	recovered := runPhase("recovery", recoveryDuration, monitor.Healthy)
	if recovered.Interval.P95MS > 11000 {
		t.Fatalf("fast interval did not recover: %+v", recovered.Interval)
	}
}
