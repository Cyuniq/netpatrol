package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

type ProbeTrendWindow struct {
	Duration time.Duration
	Step     time.Duration
}

type ProbeTrendMinute struct {
	RTTMillis *float64 `json:"ms,omitempty"`
	Timeout   bool     `json:"timeout,omitempty"`
}

func readProbeTrends(ctx context.Context, tx *sql.Tx, views []DeviceView, at time.Time, window ProbeTrendWindow) error {
	deviceIDs := make([]int64, 0, len(views))
	byID := make(map[int64]*DeviceView, len(views))
	for i := range views {
		id := views[i].ID
		deviceIDs = append(deviceIDs, id)
		byID[id] = &views[i]
	}
	ids, err := json.Marshal(deviceIDs)
	if err != nil {
		return err
	}
	sourceStep := int64(60)
	if window.Duration >= latencyFineRetention {
		sourceStep = 3600
	}
	last := bucketAt(at.Unix(), sourceStep)
	first := last - int64(window.Duration/time.Second) + sourceStep
	stepSeconds := int64(window.Step / time.Second)
	bucketCount := int(window.Duration / window.Step)
	// Start all curves at the same source bucket and use sample counts when
	// averaging RTT over multiple buckets. A single timeout marks the interval.
	rows, err := tx.QueryContext(ctx, `
		SELECT h.device_id, h.protocol, (h.at - @first) / @step AS bucket,
		       SUM(h.replies), SUM(h.total_ns), SUM(h.timeouts)
		FROM json_each(@ids) j
		CROSS JOIN devices d
		CROSS JOIN device_latency h
		WHERE d.id = j.value AND h.device_id = d.id AND h.address = d.address
		  AND h.step = @source AND h.at BETWEEN @first AND @last
		  AND (h.replies > 0 OR h.timeouts > 0)
		GROUP BY h.device_id, h.protocol, bucket
	`, sql.Named("ids", string(ids)), sql.Named("first", first), sql.Named("last", last),
		sql.Named("step", stepSeconds), sql.Named("source", sourceStep))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, bucket, replies, total, timeouts int64
		var protocol string
		if err := rows.Scan(&id, &protocol, &bucket, &replies, &total, &timeouts); err != nil {
			return err
		}
		view := byID[id]
		trend := &view.PingTrend
		if protocol == "tcp" {
			trend = &view.TCPTrend
		}
		if *trend == nil {
			*trend = make([]ProbeTrendMinute, bucketCount)
		}
		point := &(*trend)[bucket]
		point.Timeout = timeouts > 0
		if replies > 0 {
			ms := float64(total) / float64(replies) / float64(time.Millisecond)
			point.RTTMillis = &ms
		}
	}
	return rows.Err()
}
