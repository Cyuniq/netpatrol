package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"netpatrol/internal/monitor"
)

type Observation struct {
	DeviceID int64
	Version  int64
	Round    monitor.Round
}

// Record commits accepted rounds in order. A late result from replaced or
// disabled configuration has no effect on the new device state.
func (s *Store) Record(ctx context.Context, observations []Observation) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	settings, err := readSettings(ctx, tx)
	if err != nil {
		return err
	}
	for _, observation := range observations {
		if err := recordRound(ctx, tx, observation, settings); err != nil {
			return fmt.Errorf("record device %d: %w", observation.DeviceID, err)
		}
	}
	return tx.Commit()
}

type savedSnapshot struct {
	monitor.Snapshot
	Evaluated bool `json:"evaluated"`
}

func recordRound(ctx context.Context, tx *sql.Tx, observation Observation, settings monitor.Settings) error {
	round := observation.Round
	if round.CompletedAt.IsZero() {
		return errors.New("round has no completion timestamp")
	}
	var areaID, completed int64
	var areaName string
	var previous sql.NullString
	err := tx.QueryRowContext(ctx, `
		SELECT d.area_id, a.name, COALESCE(r.completed_ns, 0), r.snapshot
		FROM devices d JOIN areas a ON a.id = d.area_id
		LEFT JOIN latest_results r ON r.device_id = d.id AND r.version = d.version
		WHERE d.id = ? AND d.version = ? AND d.enabled = 1 AND d.address = ? AND d.port = ?`,
		observation.DeviceID, observation.Version, round.Target.IP.String(), round.Target.Port,
	).Scan(&areaID, &areaName, &completed, &previous)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	at := round.CompletedAt.UnixNano()
	if previous.Valid && at <= completed {
		return nil
	}
	state := savedSnapshot{Snapshot: monitor.NewSnapshot(round.CompletedAt)}
	if previous.Valid {
		if err := json.Unmarshal([]byte(previous.String), &state); err != nil {
			return err
		}
		state.Snapshot.Evaluated = state.Evaluated
	}
	event := state.Snapshot.ApplyWithSettings(round, settings)
	state.Evaluated = state.Snapshot.Evaluated
	encoded, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO latest_results(device_id, version, state, state_since_ns, completed_ns, snapshot)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (device_id) DO UPDATE SET version = excluded.version, state = excluded.state,
		  state_since_ns = excluded.state_since_ns, completed_ns = excluded.completed_ns, snapshot = excluded.snapshot`,
		observation.DeviceID, observation.Version, state.State, state.Since.UnixNano(), at, string(encoded),
	); err != nil {
		return err
	}
	if err := recordDeviceLatency(ctx, tx, observation); err != nil {
		return err
	}
	if !round.EchoSentAt.IsZero() {
		minute := round.EchoSentAt.UTC().Truncate(time.Minute).Unix() / 60
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ping_minutes(device_id, version, minute, sent, replies)
			VALUES (?, ?, ?, 1, ?)
			ON CONFLICT (device_id, version, minute) DO UPDATE
			SET sent = sent + 1, replies = replies + excluded.replies`,
			observation.DeviceID, observation.Version, minute, round.Ping.Outcome == monitor.Passed,
		); err != nil {
			return err
		}
	}
	if event == nil {
		return nil
	}
	encoded, err = json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO state_events(device_id, area_id, area_name, version, at_ns, event)
		VALUES (?, ?, ?, ?, ?, ?)`,
		observation.DeviceID, areaID, areaName, observation.Version, at, string(encoded),
	)
	return err
}

func (s *Store) Loss(ctx context.Context, deviceID int64, at time.Time) (monitor.Loss, error) {
	var loss monitor.Loss
	minute := at.UTC().Truncate(time.Minute).Unix() / 60
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(m.sent), 0), COALESCE(SUM(m.replies), 0)
		FROM devices d JOIN ping_minutes m ON m.device_id = d.id AND m.version = d.version
		WHERE d.id = ? AND m.minute BETWEEN ? AND ?`, deviceID, minute-59, minute,
	).Scan(&loss.Sent, &loss.Replies)
	return loss, err
}

// ResetMonitoring starts a fresh evaluation after the service was offline.
// The latest accepted timestamp remains as the duplicate guard.
func (s *Store) ResetMonitoring(ctx context.Context, at time.Time) error {
	initial := monitor.NewSnapshot(at)
	settings, err := s.Settings(ctx)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		UPDATE latest_results
		SET state = ?, state_since_ns = ?, snapshot = json_set(
		  json_remove(snapshot, '$.both_failed_since', '$.actual_interval_ns'),
		  '$.state', ?, '$.since', ?, '$.interval_ns', ?, '$.last_round', json('{}'),
		  '$.last_probe_at', COALESCE(json_extract(snapshot, '$.last_probe_at'),
		    CASE WHEN json_extract(snapshot, '$.last_round.started_at') <> '0001-01-01T00:00:00Z'
		      THEN json_extract(snapshot, '$.last_round.completed_at') END)
		)
		WHERE EXISTS (
		  SELECT 1 FROM devices d WHERE d.id = device_id AND d.enabled = 1 AND d.version = latest_results.version
		)`,
		initial.State, at.UnixNano(), initial.State, at.Format(time.RFC3339Nano), settings.FastInterval,
	)
	return err
}

func (s *Store) PruneMonitoring(ctx context.Context, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	minute := at.Add(-2*time.Hour).UTC().Truncate(time.Minute).Unix() / 60
	if _, err := tx.ExecContext(ctx, "DELETE FROM ping_minutes WHERE minute < ?", minute); err != nil {
		return err
	}
	cutoff := at.Add(-180 * 24 * time.Hour).UnixNano()
	if _, err := tx.ExecContext(ctx, "DELETE FROM state_events WHERE at_ns < ?", cutoff); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM discovery_jobs WHERE finished_ns < ?", at.Add(-24*time.Hour).UnixNano()); err != nil {
		return err
	}
	return tx.Commit()
}
