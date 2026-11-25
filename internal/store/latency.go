package store

import (
	"context"
	"database/sql"
	"net/netip"
	"time"
)

const (
	LatencyRetention     = 30 * 24 * time.Hour
	latencyFineRetention = 24 * time.Hour
	maxLatencyPoints     = 1200
)

type LatencyStats struct {
	Replies  int64 `json:"r,omitempty"`
	Timeouts int64 `json:"t,omitempty"`
	Failures int64 `json:"f,omitempty"`
	Errors   int64 `json:"e,omitempty"`
	Total    int64 `json:"sum,omitempty"`
	Min      int64 `json:"min,omitempty"`
	Max      int64 `json:"max,omitempty"`
}

func (s *LatencyStats) addRTT(rtt time.Duration) {
	ns := int64(rtt)
	s.merge(LatencyStats{Replies: 1, Total: ns, Min: ns, Max: ns})
}

func (s *LatencyStats) merge(other LatencyStats) {
	if other.Replies > 0 {
		if s.Replies == 0 || other.Min < s.Min {
			s.Min = other.Min
		}
		s.Max = max(s.Max, other.Max)
	}
	s.Replies += other.Replies
	s.Timeouts += other.Timeouts
	s.Failures += other.Failures
	s.Errors += other.Errors
	s.Total += other.Total
}

type LatencyPoint struct {
	At int64
	LatencyStats
	OtherReplies int64
	Checks       int64
	Unreached    int64
	LocalChecks  int64
	TCPPorts     []TCPPortLatency
}

type TCPPortLatency struct {
	Port uint16
	LatencyStats
}

type LatencySeries struct {
	Name   string
	Points []LatencyPoint
}

type LatencyWindow struct {
	From, To time.Time
}

func (w LatencyWindow) step(minimum int64) int64 {
	for _, step := range []int64{60, 120, 300, 600, 900, 1800, 3600, 7200, 14400, 28800, 43200, 86400} {
		if step >= minimum && bucketAt(w.To.Unix(), step)-bucketAt(w.From.Unix(), step) < maxLatencyPoints*step {
			return step
		}
	}
	return 86400
}

func bucketAt(unix, step int64) int64 {
	return unix / step * step
}

func recordDeviceLatency(ctx context.Context, tx *sql.Tx, observation Observation) error {
	round := observation.Round
	if round.Ping.Outcome == "" && round.TCP.Outcome == "" {
		return nil
	}
	at := round.CompletedAt.Unix()
	_, err := tx.ExecContext(ctx, `
		WITH probes(protocol, outcome, rtt) AS (VALUES ('ping', @ping, @pingRTT), ('tcp', @tcp, @tcpRTT)),
		     steps(step) AS (VALUES (60), (3600))
		INSERT INTO device_latency
		  (device_id, address, port, step, at, protocol, replies, timeouts, failures, errors, total_ns, min_ns, max_ns)
		SELECT @id, @address, @port, step, @at / step * step, protocol, outcome = 'passed',
		  outcome = 'timeout', outcome IN ('unreachable', 'refused'),
		  outcome = 'local_error', CASE WHEN outcome = 'passed' THEN rtt ELSE 0 END,
		  CASE WHEN outcome = 'passed' THEN rtt END, CASE WHEN outcome = 'passed' THEN rtt END
		FROM steps CROSS JOIN probes WHERE outcome <> ''
		ON CONFLICT (device_id, address, port, step, at, protocol) DO UPDATE SET
		  replies = replies + excluded.replies, timeouts = timeouts + excluded.timeouts,
		  failures = failures + excluded.failures, errors = errors + excluded.errors, total_ns = total_ns + excluded.total_ns,
		  min_ns = COALESCE(MIN(min_ns, excluded.min_ns), min_ns, excluded.min_ns),
		  max_ns = COALESCE(MAX(max_ns, excluded.max_ns), max_ns, excluded.max_ns)
	`, sql.Named("id", observation.DeviceID), sql.Named("address", round.Target.IP.String()), sql.Named("port", round.Target.Port),
		sql.Named("at", at), sql.Named("ping", round.Ping.Outcome), sql.Named("pingRTT", int64(round.Ping.RTT)),
		sql.Named("tcp", round.TCP.Outcome), sql.Named("tcpRTT", int64(round.TCP.RTT)))
	return err
}

func (s *Store) DeviceLatency(ctx context.Context, id int64, w LatencyWindow, now time.Time) ([]LatencySeries, int64, error) {
	tx, err := s.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()
	var address string
	if err := tx.QueryRowContext(ctx, "SELECT address FROM devices WHERE id = ?", id).Scan(&address); err != nil {
		return nil, 0, err
	}
	sourceStep := int64(60)
	if w.From.Unix() < bucketAt(now.Add(-latencyFineRetention).Unix(), sourceStep) {
		sourceStep = 3600
	}
	step := w.step(sourceStep)
	if step == 3600 {
		sourceStep = step
	}
	// Device samples are stored in complete buckets. Include every source bucket
	// that intersects the requested range, including its two edges.
	// An IP edit still separates different endpoints. Within the current IP,
	// retain samples collected on every port and identify them in the TCP tooltip.
	rows, err := tx.QueryContext(ctx, `
		SELECT protocol, at / @step * @step AS bucket, CASE WHEN protocol = 'tcp' THEN port ELSE 0 END AS tcp_port,
		       SUM(replies), SUM(timeouts), SUM(failures),
		       SUM(errors), SUM(total_ns), COALESCE(MIN(min_ns), 0), COALESCE(MAX(max_ns), 0)
		FROM device_latency
		WHERE device_id = @id AND address = @address AND step = @source
		  AND at >= @start AND at < @end
		GROUP BY protocol, bucket, tcp_port ORDER BY bucket, tcp_port
	`, sql.Named("id", id), sql.Named("address", address), sql.Named("source", sourceStep),
		sql.Named("step", step), sql.Named("start", bucketAt(w.From.Unix(), sourceStep)),
		sql.Named("end", w.To.Add(-time.Nanosecond).Unix()+1))
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	series := []LatencySeries{{Name: "Ping"}, {Name: "TCP"}}
	for rows.Next() {
		var protocol string
		var point LatencyPoint
		var port uint16
		if err := rows.Scan(&protocol, &point.At, &port, &point.Replies, &point.Timeouts, &point.Failures,
			&point.Errors, &point.Total, &point.Min, &point.Max); err != nil {
			return nil, 0, err
		}
		if protocol == "ping" {
			series[0].Points = append(series[0].Points, point)
			continue
		}
		tcp := &series[1].Points
		if len(*tcp) == 0 || (*tcp)[len(*tcp)-1].At != point.At {
			*tcp = append(*tcp, LatencyPoint{At: point.At})
		}
		bucket := &(*tcp)[len(*tcp)-1]
		bucket.LatencyStats.merge(point.LatencyStats)
		bucket.TCPPorts = append(bucket.TCPPorts, TCPPortLatency{Port: port, LatencyStats: point.LatencyStats})
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return series, step, tx.Commit()
}

type LatencyHop struct {
	Number  int        `json:"number"`
	Address netip.Addr `json:"address,omitzero"`
}

func (s *Store) PruneLatency(ctx context.Context, at time.Time) error {
	// Limit each write transaction so a long period of downtime does not
	// put all expired history ahead of fresh observations in the write queue.
	for step, retention := range map[int64]time.Duration{60: latencyFineRetention, 3600: LatencyRetention} {
		if _, err := s.db.ExecContext(ctx, `
			DELETE FROM device_latency WHERE (device_id, address, port, step, at, protocol) IN (
			  SELECT device_id, address, port, step, at, protocol FROM device_latency
			  WHERE step = ? AND at < ? ORDER BY at LIMIT 15000
			)
		`, step, bucketAt(at.Add(-retention).Unix(), step)); err != nil {
			return err
		}
	}
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM path_latency WHERE (path_id, address, at_ns) IN (
		  SELECT path_id, address, at_ns FROM path_latency WHERE at_ns < ? ORDER BY at_ns LIMIT 15000
		)
	`, at.Add(-LatencyRetention).UnixNano())
	return err
}
