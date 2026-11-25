package telemetry

import (
	"testing"
	"time"
)

func TestPendingWritesAndPercentiles(t *testing.T) {
	start := time.Now()
	var metrics Metrics
	metrics.Scheduler(20, 2)
	for id := int64(1); id <= 20; id++ {
		offset := time.Duration(id) * time.Millisecond
		metrics.ResultQueued(id, start.Add(offset), offset, false)
		metrics.PageLatency(offset)
	}
	metrics.ResultWritten(1, start.Add(50*time.Millisecond), 10*time.Second)
	at := start.Add(100 * time.Millisecond)
	metrics.ResultQueued(99, at, 0, true)
	snapshot := metrics.Snapshot(at)
	if snapshot.Completed != 20 || snapshot.Missed != 1 || snapshot.QueueDepth != 20 ||
		snapshot.OldestWriteMS != 98 || snapshot.Write.P95MS != 49 {
		t.Fatalf("incorrect queue snapshot: %+v", snapshot)
	}
	if snapshot.Dispatch.P95MS != 19 || snapshot.Page.P95MS != 19 || snapshot.Interval.P95MS != 10000 {
		t.Fatalf("incorrect timing snapshot: %+v", snapshot)
	}
}
