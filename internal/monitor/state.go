package monitor

import (
	"net/netip"
	"time"
)

type Target struct {
	IP   netip.Addr `json:"ip"`
	Port uint16     `json:"port"`
}

type Outcome string

const (
	Passed      Outcome = "passed"
	Timeout     Outcome = "timeout"
	Unreachable Outcome = "unreachable"
	Refused     Outcome = "refused"
	LocalError  Outcome = "local_error"
)

type Result struct {
	Outcome     Outcome       `json:"outcome"`
	Detail      string        `json:"detail,omitempty"`
	CompletedAt time.Time     `json:"completed_at"`
	RTT         time.Duration `json:"rtt_ns,omitempty"`
}

type Round struct {
	Target      Target    `json:"target"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
	EchoSentAt  time.Time `json:"echo_sent_at,omitzero"`
	Ping        Result    `json:"ping"`
	TCP         Result    `json:"tcp"`
}

type State string

const (
	Unknown  State = "unknown"
	Healthy  State = "healthy"
	Partial  State = "partial"
	Down     State = "down"
	Disabled State = "disabled"
)

const (
	FastInterval = 10 * time.Second
	SlowInterval = 60 * time.Second
	SlowAfter    = 10 * time.Minute
)

type Snapshot struct {
	State           State         `json:"state"`
	Since           time.Time     `json:"since,omitzero"`
	BothFailedSince time.Time     `json:"both_failed_since,omitzero"`
	Interval        time.Duration `json:"interval_ns"`
	ActualInterval  time.Duration `json:"actual_interval_ns,omitempty"`
	LastRound       Round         `json:"last_round"`
	LastProbeAt     time.Time     `json:"last_probe_at,omitzero"`
	PingLastSuccess time.Time     `json:"ping_last_success,omitzero"`
	TCPLastSuccess  time.Time     `json:"tcp_last_success,omitzero"`
	Evaluated       bool          `json:"-"`
}

type Event struct {
	At       time.Time `json:"at"`
	Previous State     `json:"previous,omitempty"`
	Current  State     `json:"current"`
	Round    Round     `json:"round"`
}

func NewSnapshot(at time.Time) Snapshot {
	return Snapshot{State: Unknown, Since: at, Interval: FastInterval}
}

func (s Snapshot) LastProbeTime() time.Time {
	if s.LastProbeAt.IsZero() && !s.LastRound.StartedAt.IsZero() {
		return s.LastRound.CompletedAt
	}
	return s.LastProbeAt
}

func (s *Snapshot) Apply(round Round) *Event {
	return s.ApplyWithSettings(round, DefaultSettings())
}

func (s *Snapshot) ApplyWithSettings(round Round, settings Settings) *Event {
	if !s.LastRound.CompletedAt.IsZero() && !round.CompletedAt.After(s.LastRound.CompletedAt) {
		return nil
	}
	s.ActualInterval = 0
	if !s.LastRound.StartedAt.IsZero() && !round.StartedAt.IsZero() {
		s.ActualInterval = round.StartedAt.Sub(s.LastRound.StartedAt)
	}
	s.LastProbeAt = s.LastProbeTime()
	if !round.StartedAt.IsZero() {
		s.LastProbeAt = round.CompletedAt
	}
	if round.Ping.Outcome == Passed {
		s.PingLastSuccess = round.Ping.CompletedAt
	}
	if round.TCP.Outcome == Passed {
		s.TCPLastSuccess = round.TCP.CompletedAt
	}
	s.LastRound = round

	state := classify(round)
	if state == Down {
		if s.BothFailedSince.IsZero() {
			s.BothFailedSince = round.CompletedAt
		}
	} else {
		s.BothFailedSince = time.Time{}
	}
	s.Interval = settings.Interval(s.BothFailedSince, round.CompletedAt)
	if state == s.State {
		return nil
	}
	previous := s.State
	s.State = state
	s.Since = round.CompletedAt
	if !s.Evaluated {
		s.Evaluated = true
		previous = ""
	}
	return &Event{At: round.CompletedAt, Previous: previous, Current: state, Round: round}
}

func classify(round Round) State {
	ping, tcp := round.Ping.Outcome, round.TCP.Outcome
	if tcp == Passed {
		return Healthy
	}
	if ping == "" || ping == LocalError || tcp == "" || tcp == LocalError {
		return Unknown
	}
	if ping == Passed {
		return Partial
	}
	return Down
}

type Loss struct {
	Sent    int `json:"sent"`
	Replies int `json:"replies"`
}

func (l Loss) Lost() int {
	return l.Sent - l.Replies
}

func (l Loss) Percent() (float64, bool) {
	if l.Sent == 0 {
		return 0, false
	}
	return float64(l.Lost()) * 100 / float64(l.Sent), true
}

type minuteBucket struct {
	minute time.Time
	loss   Loss
}

type LossWindow struct {
	buckets [60]minuteBucket
}

func (w *LossWindow) Record(round Round) {
	if round.EchoSentAt.IsZero() {
		return
	}
	minute := round.EchoSentAt.UTC().Truncate(time.Minute)
	index := (minute.Unix()/60%60 + 60) % 60
	bucket := &w.buckets[index]
	if bucket.minute.After(minute) {
		return
	}
	if bucket.minute != minute {
		*bucket = minuteBucket{minute: minute}
	}
	bucket.loss.Sent++
	if round.Ping.Outcome == Passed {
		bucket.loss.Replies++
	}
}

func (w *LossWindow) At(at time.Time) Loss {
	current := at.UTC().Truncate(time.Minute)
	oldest := current.Add(-59 * time.Minute)
	var result Loss
	for _, bucket := range w.buckets {
		if bucket.minute.Before(oldest) || bucket.minute.After(current) {
			continue
		}
		result.Sent += bucket.loss.Sent
		result.Replies += bucket.loss.Replies
	}
	return result
}
