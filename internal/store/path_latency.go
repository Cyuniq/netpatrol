package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/netip"
	"slices"

	"netpatrol/internal/monitor"
)

type traceLatency struct {
	Reached bool              `json:"reached,omitempty"`
	Local   bool              `json:"local,omitempty"`
	Target  LatencyStats      `json:"target"`
	Hops    []traceHopLatency `json:"hops,omitempty"`
}

type traceHopLatency struct {
	Number     int                         `json:"n"`
	Stats      LatencyStats                `json:"stats"`
	Responders map[netip.Addr]LatencyStats `json:"responders,omitempty"`
}

func summarizeTrace(sample PathSample) traceLatency {
	trace := sample.Result
	summary := traceLatency{Reached: trace.Reached, Local: sample.Message != ""}
	for _, hop := range trace.Hops {
		point := traceHopLatency{Number: hop.Number, Responders: make(map[netip.Addr]LatencyStats)}
		for _, attempt := range hop.Attempts {
			switch {
			case attempt.Sent && attempt.Address.IsValid():
				point.Stats.addRTT(attempt.Result.RTT)
				if attempt.Result.Outcome == monitor.Passed {
					summary.Target.addRTT(attempt.Result.RTT)
				}
				responder := point.Responders[attempt.Address]
				responder.addRTT(attempt.Result.RTT)
				point.Responders[attempt.Address] = responder
			case attempt.Result.Outcome == monitor.LocalError:
				point.Stats.Errors++
			case attempt.Sent && attempt.Result.Outcome == monitor.Timeout:
				point.Stats.Timeouts++
			case attempt.Sent:
				point.Stats.Failures++
			}
		}
		if hop.Result.Outcome == monitor.Passed {
			// A successful destination response identifies the hop at which
			// target RTT can be measured. Other responders share its TTL.
			summary.Target.Timeouts = point.Stats.Timeouts
			summary.Target.Failures = point.Stats.Failures
			summary.Target.Errors = point.Stats.Errors
		}
		summary.Hops = append(summary.Hops, point)
	}
	if len(trace.Hops) > 0 && trace.Hops[len(trace.Hops)-1].Result.Outcome == monitor.LocalError {
		summary.Local = true
	}
	if !summary.Reached && summary.Local {
		summary.Target.Errors++
	}
	return summary
}

type latencyWriter interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func recordPathLatency(ctx context.Context, writer latencyWriter, id int64, sample PathSample) error {
	summary, err := json.Marshal(summarizeTrace(sample))
	if err != nil {
		return err
	}
	_, err = writer.ExecContext(ctx, `
		INSERT INTO path_latency(path_id, address, at_ns, sample)
		SELECT id, address, ?, ? FROM probe_paths WHERE id = ? AND address = ?
		ON CONFLICT (path_id, address, at_ns) DO NOTHING
	`, sample.Completed.UnixNano(), string(summary), id, sample.Address.String())
	return err
}

func (s *Store) RecordManualPathLatency(ctx context.Context, id int64, sample PathSample) error {
	return recordPathLatency(ctx, s.db, id, sample)
}

func (s *Store) PathLatency(ctx context.Context, id int64, w LatencyWindow, selected LatencyHop) (LatencySeries, []LatencyHop, int64, error) {
	var address string
	var interval int64
	if err := s.reader.QueryRowContext(ctx, "SELECT address, interval_seconds FROM probe_paths WHERE id = ?", id).Scan(&address, &interval); err != nil {
		return LatencySeries{}, nil, 0, err
	}
	rows, err := s.reader.QueryContext(ctx, `
		SELECT at_ns, sample FROM path_latency WHERE path_id = ? AND address = ? AND at_ns >= ? AND at_ns < ? ORDER BY at_ns
	`, id, address, w.From.UnixNano(), w.To.UnixNano())
	if err != nil {
		return LatencySeries{}, nil, 0, err
	}
	defer rows.Close()
	// Match regular path sampling where possible, while leaving room for
	// multiple manual observations when inspecting a narrow time range.
	step := w.step(min(interval, max(60, int64(w.To.Sub(w.From).Seconds()/2))))
	series := LatencySeries{Name: "目标端"}
	if selected.Number > 0 {
		series.Name = "跳点"
	}
	available := make(map[LatencyHop]struct{})
	for rows.Next() {
		var at int64
		var encoded []byte
		if err := rows.Scan(&at, &encoded); err != nil {
			return series, nil, 0, err
		}
		var sample traceLatency
		if err := json.Unmarshal(encoded, &sample); err != nil {
			return series, nil, 0, err
		}
		point := LatencyPoint{At: bucketAt(at/1e9, step)}
		for _, hop := range sample.Hops {
			available[LatencyHop{Number: hop.Number}] = struct{}{}
			for responder := range hop.Responders {
				available[LatencyHop{Number: hop.Number, Address: responder}] = struct{}{}
			}
			if hop.Number == selected.Number {
				point.LatencyStats = hop.Stats
				if selected.Address.IsValid() {
					responder := hop.Responders[selected.Address]
					point.OtherReplies = hop.Stats.Replies - responder.Replies
					point.Replies, point.Total, point.Min, point.Max = responder.Replies, responder.Total, responder.Min, responder.Max
				}
				point.Checks = 1
			}
		}
		if selected.Number == 0 {
			point.Checks, point.LatencyStats = 1, sample.Target
			if !sample.Reached && !sample.Local {
				point.Unreached = 1
			}
			if !sample.Reached && sample.Local {
				point.LocalChecks = 1
			}
		}
		if point.Checks == 0 {
			continue
		}
		last := len(series.Points) - 1
		if last >= 0 && series.Points[last].At == point.At {
			merged := &series.Points[last]
			merged.LatencyStats.merge(point.LatencyStats)
			merged.OtherReplies += point.OtherReplies
			merged.Checks += point.Checks
			merged.Unreached += point.Unreached
			merged.LocalChecks += point.LocalChecks
		} else {
			series.Points = append(series.Points, point)
		}
	}
	options := make([]LatencyHop, 0, len(available))
	for hop := range available {
		options = append(options, hop)
	}
	slices.SortFunc(options, func(a, b LatencyHop) int {
		if a.Number != b.Number {
			return a.Number - b.Number
		}
		return a.Address.Compare(b.Address)
	})
	return series, options, step, rows.Err()
}
