package telemetry

import (
	"slices"
	"sync"
	"time"
)

const sampleLimit = 4096

type series struct {
	values [sampleLimit]time.Duration
	next   int
	count  int
}

func (s *series) record(value time.Duration) {
	s.values[s.next] = max(value, 0)
	s.next = (s.next + 1) % len(s.values)
	s.count = min(s.count+1, len(s.values))
}

type Timing struct {
	Samples int     `json:"samples"`
	P95MS   float64 `json:"p95_ms"`
}

func percentile(values []time.Duration) Timing {
	if len(values) == 0 {
		return Timing{}
	}
	slices.Sort(values)
	rank := (len(values)*95 + 99) / 100
	return Timing{Samples: len(values), P95MS: milliseconds(values[rank-1])}
}

type Snapshot struct {
	ScheduledDevices int     `json:"scheduled_devices"`
	InFlight         int     `json:"in_flight_rounds"`
	Completed        uint64  `json:"completed_rounds"`
	Missed           uint64  `json:"missing_rounds"`
	QueueDepth       int     `json:"write_queue_depth"`
	OldestWriteMS    float64 `json:"oldest_pending_write_ms"`
	Dispatch         Timing  `json:"dispatch_delay"`
	Interval         Timing  `json:"actual_probe_interval"`
	Write            Timing  `json:"result_write_delay"`
	Page             Timing  `json:"devices_page_latency"`
}

type Metrics struct {
	mu                              sync.Mutex
	scheduled, inFlight             int
	completed, missed               uint64
	pending                         map[int64]time.Time
	dispatch, interval, write, page series
}

func (m *Metrics) Scheduler(scheduled, inFlight int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scheduled, m.inFlight = scheduled, inFlight
}

func (m *Metrics) ResultQueued(device int64, at time.Time, dispatchDelay time.Duration, missed bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pending == nil {
		m.pending = make(map[int64]time.Time)
	}
	m.pending[device] = at
	if missed {
		m.missed++
	} else {
		m.completed++
		m.dispatch.record(dispatchDelay)
	}
}

func (m *Metrics) ResultWritten(device int64, at time.Time, interval time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if queued, exists := m.pending[device]; exists {
		m.write.record(at.Sub(queued))
		delete(m.pending, device)
	}
	if interval > 0 {
		m.interval.record(interval)
	}
}

func (m *Metrics) PageLatency(took time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.page.record(took)
}

func (m *Metrics) Snapshot(at time.Time) Snapshot {
	m.mu.Lock()
	state := Snapshot{ScheduledDevices: m.scheduled, InFlight: m.inFlight, Completed: m.completed, Missed: m.missed,
		QueueDepth: len(m.pending)}
	for _, start := range m.pending {
		state.OldestWriteMS = max(state.OldestWriteMS, milliseconds(at.Sub(start)))
	}
	dispatch := slices.Clone(m.dispatch.values[:m.dispatch.count])
	interval := slices.Clone(m.interval.values[:m.interval.count])
	write := slices.Clone(m.write.values[:m.write.count])
	page := slices.Clone(m.page.values[:m.page.count])
	m.mu.Unlock()
	state.Dispatch = percentile(dispatch)
	state.Interval = percentile(interval)
	state.Write = percentile(write)
	state.Page = percentile(page)
	return state
}

func milliseconds(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}
