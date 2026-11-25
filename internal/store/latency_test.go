package store

import (
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"netpatrol/internal/monitor"
)

func TestDeviceLatencyAggregatesAcceptedObservationsAndRetainsHours(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "latency.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	area, err := db.SaveArea(ctx, 0, "Lab", "", "")
	if err != nil {
		t.Fatal(err)
	}
	input := DeviceInput{Name: "Gateway", Address: "192.0.2.1", Port: 443, AreaID: area, Enabled: true}
	id, err := db.SaveDevice(ctx, 0, input)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
	target := monitor.Target{IP: netip.MustParseAddr(input.Address), Port: uint16(input.Port)}
	two := int64(2 * time.Millisecond)
	four := int64(4 * time.Millisecond)
	one := Observation{DeviceID: id, Version: 1, Round: monitor.Round{
		Target: target, CompletedAt: start.Add(time.Second),
		Ping: monitor.Result{Outcome: monitor.Passed, RTT: time.Duration(two)},
		TCP:  monitor.Result{Outcome: monitor.Timeout},
	}}
	next := one
	next.Round.CompletedAt = start.Add(30 * time.Second)
	next.Round.Ping.RTT = time.Duration(four)
	next.Round.TCP.Outcome = monitor.Refused
	local := one
	local.Round.CompletedAt = start.Add(time.Minute)
	local.Round.Ping.Outcome = monitor.LocalError
	local.Round.TCP.Outcome = ""
	if err := db.Record(ctx, []Observation{one, next, next, local}); err != nil {
		t.Fatal(err)
	}
	window := LatencyWindow{From: start, To: start.Add(2 * time.Minute)}
	curves, step, err := db.DeviceLatency(ctx, id, window, window.To)
	if err != nil || step != 60 || len(curves[0].Points) != 2 || len(curves[1].Points) != 1 {
		t.Fatalf("minute result: %+v step=%d err=%v", curves, step, err)
	}
	ping, tcp := curves[0].Points[0], curves[1].Points[0]
	if ping.LatencyStats != (LatencyStats{Replies: 2, Total: two + four, Min: two, Max: four}) ||
		curves[0].Points[1].LatencyStats != (LatencyStats{Errors: 1}) ||
		tcp.LatencyStats != (LatencyStats{Timeouts: 1, Failures: 1}) {
		t.Fatalf("minutes: ping=%+v tcp=%+v", curves[0].Points, curves[1].Points)
	}
	after := start.Add(25 * time.Hour)
	if err := db.PruneLatency(ctx, after); err != nil {
		t.Fatal(err)
	}
	curves, step, err = db.DeviceLatency(ctx, id, window, after)
	if err != nil || step != 3600 || len(curves[0].Points) != 1 ||
		curves[0].Points[0].LatencyStats != (LatencyStats{Replies: 2, Errors: 1, Total: two + four, Min: two, Max: four}) {
		t.Fatalf("hour result: %+v step=%d err=%v", curves, step, err)
	}
	var fine int
	if err := db.reader.QueryRowContext(ctx, "SELECT count(*) FROM device_latency WHERE step = 60").Scan(&fine); err != nil || fine != 0 {
		t.Fatalf("fine latency remains: %d (%v)", fine, err)
	}
	input.Port = 80
	if _, err := db.SaveDevice(ctx, id, input); err != nil {
		t.Fatal(err)
	}
	curves, _, err = db.DeviceLatency(ctx, id, window, after)
	if err != nil || len(curves[0].Points) != 1 || len(curves[1].Points) != 1 ||
		len(curves[1].Points[0].TCPPorts) != 1 || curves[1].Points[0].TCPPorts[0].Port != 443 {
		t.Fatalf("a port edit hid the retained hourly data: %+v (%v)", curves, err)
	}
}

func TestDeviceTrendContinuesAcrossPortChanges(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "ports.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	area, err := db.SaveArea(ctx, 0, "Lab", "", "")
	if err != nil {
		t.Fatal(err)
	}
	input := DeviceInput{Name: "Gateway", Address: "192.0.2.1", Port: 443, AreaID: area, Enabled: true}
	id, err := db.SaveDevice(ctx, 0, input)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
	old := monitor.Target{IP: netip.MustParseAddr(input.Address), Port: 443}
	updated := monitor.Target{IP: old.IP, Port: 8443}
	sample := func(version int64, target monitor.Target, at time.Duration, tcp monitor.Outcome, rtt time.Duration) Observation {
		return Observation{DeviceID: id, Version: version, Round: monitor.Round{
			Target: target, CompletedAt: start.Add(at),
			Ping: monitor.Result{Outcome: monitor.Passed, RTT: time.Millisecond},
			TCP:  monitor.Result{Outcome: tcp, RTT: rtt},
		}}
	}
	if err := db.Record(ctx, []Observation{
		sample(1, old, time.Second, monitor.Passed, 4*time.Millisecond),
		sample(1, old, time.Minute+time.Second, monitor.Passed, 6*time.Millisecond),
	}); err != nil {
		t.Fatal(err)
	}
	input.Port = int(updated.Port)
	if _, err := db.SaveDevice(ctx, id, input); err != nil {
		t.Fatal(err)
	}
	window := LatencyWindow{From: start, To: start.Add(2*time.Minute + 10*time.Second)}
	curves, _, err := db.DeviceLatency(ctx, id, window, window.To)
	if err != nil || len(curves[1].Points) != 2 {
		t.Fatalf("old samples vanished before the first new observation: %+v (%v)", curves, err)
	}
	if err := db.Record(ctx, []Observation{
		sample(1, old, time.Minute+2*time.Second, monitor.Timeout, 0), // queued before edit
		sample(2, updated, time.Minute+3*time.Second, monitor.Passed, 10*time.Millisecond),
		sample(2, updated, 2*time.Minute+time.Second, monitor.Timeout, 0),
	}); err != nil {
		t.Fatal(err)
	}
	curves, _, err = db.DeviceLatency(ctx, id, window, window.To)
	if err != nil || len(curves[0].Points) != 3 || len(curves[1].Points) != 3 {
		t.Fatalf("wrong timeline after the new port sampled: %+v (%v)", curves, err)
	}
	tcp := curves[1].Points
	if len(tcp[0].TCPPorts) != 1 || tcp[0].TCPPorts[0].Port != old.Port ||
		len(tcp[1].TCPPorts) != 2 || tcp[1].TCPPorts[0].Port != old.Port || tcp[1].TCPPorts[1].Port != updated.Port ||
		tcp[1].TCPPorts[0].Total != int64(6*time.Millisecond) || tcp[1].TCPPorts[1].Total != int64(10*time.Millisecond) ||
		tcp[1].Timeouts != 0 || len(tcp[2].TCPPorts) != 1 || tcp[2].TCPPorts[0].Port != updated.Port || tcp[2].Timeouts != 1 {
		t.Fatalf("wrong per-port breakdown: %+v", tcp)
	}
	page, _, err := db.DeviceList(ctx, DeviceFilter{DeviceID: id}, window.To, 50, 0,
		ProbeTrendWindow{Duration: 3 * time.Minute, Step: time.Minute})
	if err != nil || len(page) != 1 || len(page[0].TCPTrend) != 3 || len(page[0].PingTrend) != 3 {
		t.Fatalf("wrong list history: %+v (%v)", page, err)
	}
	trend := page[0].TCPTrend
	if trend[0].RTTMillis == nil || *trend[0].RTTMillis != 4 ||
		trend[1].RTTMillis == nil || *trend[1].RTTMillis != 8 || trend[1].Timeout ||
		trend[2].RTTMillis != nil || !trend[2].Timeout || page[0].Snapshot.LastRound.Target != updated {
		t.Fatalf("wrong list points or current target: %+v", page[0])
	}
	input.Address = "192.0.2.2"
	if _, err := db.SaveDevice(ctx, id, input); err != nil {
		t.Fatal(err)
	}
	curves, _, err = db.DeviceLatency(ctx, id, window, window.To)
	if err != nil || len(curves[0].Points) != 0 || len(curves[1].Points) != 0 {
		t.Fatalf("latency crossed into a different IP: %+v (%v)", curves, err)
	}
}

func TestPathLatencyRecordsStableRoutesAndKeepsResponderSamplesSeparate(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "path-latency.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	start := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
	addr := netip.MustParseAddr("192.0.2.10")
	first := netip.MustParseAddr("192.0.2.1")
	second := netip.MustParseAddr("192.0.2.2")
	id, err := db.SavePath(ctx, 0, PathInput{Address: addr.String(), Enabled: true, IntervalSeconds: 60}, start)
	if err != nil {
		t.Fatal(err)
	}
	sample := PathSample{Address: addr, Started: start, Completed: start.Add(time.Second), Result: monitor.TraceResult{
		Reached: true, Hops: []monitor.TraceHop{
			{Number: 1, Address: first, Result: monitor.Result{Outcome: monitor.HopReply}, Attempts: []monitor.TraceAttempt{
				{Sent: true, Address: first, Result: monitor.Result{Outcome: monitor.HopReply, RTT: time.Millisecond}},
				{Sent: true, Address: second, Result: monitor.Result{Outcome: monitor.HopReply, RTT: 5 * time.Millisecond}},
				{Sent: true, Result: monitor.Result{Outcome: monitor.Timeout}},
			}},
			{Number: 2, Address: addr, Result: monitor.Result{Outcome: monitor.Passed}, Attempts: []monitor.TraceAttempt{
				{Sent: true, Address: addr, Result: monitor.Result{Outcome: monitor.Passed, RTT: 10 * time.Millisecond}},
			}},
		},
	}}
	task := PathTask{ID: id, Generation: 1, Address: addr}
	for i := range 3 {
		sample.Completed = start.Add(time.Duration(i)*time.Minute + time.Second)
		if err := db.RecordPathCheck(ctx, task, sample); err != nil {
			t.Fatal(err)
		}
	}
	watch, err := db.PathWatch(ctx, id)
	if err != nil || len(watch.Changes) != 1 {
		t.Fatalf("changes=%+v err=%v", watch.Changes, err)
	}
	sample.Completed = start.Add(10 * time.Second)
	sample.Result.Hops[0].Attempts[1].Result.RTT = 7 * time.Millisecond
	if err := db.RecordManualPathLatency(ctx, id, sample); err != nil {
		t.Fatal(err)
	}
	window := LatencyWindow{From: start, To: start.Add(3 * time.Minute)}
	curve, hops, step, err := db.PathLatency(ctx, id, window, LatencyHop{Number: 1, Address: second})
	if err != nil || step != 60 || len(hops) != 5 || len(curve.Points) != 3 {
		t.Fatalf("path curve=%+v hops=%+v step=%d err=%v", curve, hops, step, err)
	}
	point := curve.Points[0]
	if point.Replies != 2 || point.OtherReplies != 2 || point.Timeouts != 2 || point.Checks != 2 ||
		point.Total != int64(12*time.Millisecond) || point.Min != int64(5*time.Millisecond) || point.Max != int64(7*time.Millisecond) {
		t.Fatalf("mixed responders or lost a sample: %+v", point)
	}
	curve, _, _, err = db.PathLatency(ctx, id, window, LatencyHop{})
	if err != nil || len(curve.Points) != 3 || curve.Points[0].Total != int64(20*time.Millisecond) {
		t.Fatalf("target latency: %+v (%v)", curve, err)
	}
	if err := db.DeletePath(ctx, id); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.reader.QueryRowContext(ctx, "SELECT count(*) FROM path_latency").Scan(&count); err != nil || count != 0 {
		t.Fatalf("deleted path left %d samples (%v)", count, err)
	}
}
