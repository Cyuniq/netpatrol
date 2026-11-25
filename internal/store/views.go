package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"netpatrol/internal/monitor"
)

type DeviceFilter struct {
	AreaID   int64
	DeviceID int64
	State    monitor.State
	Keyword  string
}

type DeviceView struct {
	Device
	AreaName  string
	AreaPath  string
	Snapshot  monitor.Snapshot
	Loss      monitor.Loss
	PingTrend []ProbeTrendMinute
	TCPTrend  []ProbeTrendMinute
}

type AreaCounts struct {
	ID     int64
	Name   string
	Counts map[monitor.State]int
}

const deviceJoin = `FROM devices d JOIN areas a ON a.id = d.area_id
  LEFT JOIN latest_results r ON r.device_id = d.id AND r.version = d.version`

const effectiveState = `CASE WHEN d.enabled = 0 THEN 'disabled' ELSE COALESCE(r.state, 'unknown') END`

func filterDevices(filter DeviceFilter) (string, []any) {
	var terms []string
	var args []any
	if filter.AreaID != 0 {
		terms = append(terms, "d.area_id = ?")
		args = append(args, filter.AreaID)
	}
	if filter.DeviceID != 0 {
		terms = append(terms, "d.id = ?")
		args = append(args, filter.DeviceID)
	}
	if filter.State != "" {
		terms = append(terms, effectiveState+" = ?")
		args = append(args, filter.State)
	}
	if filter.Keyword != "" {
		terms = append(terms, "(instr(lower(d.name), lower(?)) > 0 OR instr(lower(d.code), lower(?)) > 0 OR instr(lower(d.address), lower(?)) > 0)")
		args = append(args, filter.Keyword, filter.Keyword, filter.Keyword)
	}
	if len(terms) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(terms, " AND "), args
}

func (s *Store) DeviceViews(ctx context.Context, filter DeviceFilter, at time.Time, limit, offset int) ([]DeviceView, int, error) {
	return s.deviceViews(ctx, filter, at, limit, offset, ProbeTrendWindow{}, false)
}

func (s *Store) DeviceList(ctx context.Context, filter DeviceFilter, at time.Time, limit, offset int, trend ProbeTrendWindow) ([]DeviceView, int, error) {
	return s.deviceViews(ctx, filter, at, limit, offset, trend, true)
}

func (s *Store) deviceViews(ctx context.Context, filter DeviceFilter, at time.Time, limit, offset int, trend ProbeTrendWindow, clampOffset bool) ([]DeviceView, int, error) {
	where, args := filterDevices(filter)
	tx, err := s.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()
	var total int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) "+deviceJoin+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if clampOffset && limit > 0 {
		// Use the same snapshot as the rows when a live filter loses a page.
		offset = min(offset, max(0, total-1)/limit*limit)
	}
	views, err := readDeviceViews(ctx, tx, where, args, at, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	if trend.Duration > 0 && len(views) > 0 {
		if err := readProbeTrends(ctx, tx, views, at, trend); err != nil {
			return nil, 0, err
		}
	}
	return views, total, tx.Commit()
}

// VisitDeviceViews streams batches from one database snapshot so live status
// changes cannot move devices between pages during an export.
func (s *Store) VisitDeviceViews(ctx context.Context, filter DeviceFilter, at time.Time, visit func(DeviceView) error) error {
	where, args := filterDevices(filter)
	tx, err := s.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	const batchSize = 200
	for offset := 0; ; offset += batchSize {
		batch, err := readDeviceViews(ctx, tx, where, args, at, batchSize, offset)
		if err != nil {
			return err
		}
		for _, device := range batch {
			if err := visit(device); err != nil {
				return err
			}
		}
		if len(batch) < batchSize {
			return tx.Commit()
		}
	}
}

func readDeviceViews(ctx context.Context, tx *sql.Tx, where string, args []any, at time.Time, limit, offset int) ([]DeviceView, error) {
	settings, err := readSettings(ctx, tx)
	if err != nil {
		return nil, err
	}
	minute := at.UTC().Truncate(time.Minute).Unix() / 60
	params := append([]any{minute - 59, minute, minute - 59, minute}, args...)
	params = append(params, limit, offset)
	rows, err := tx.QueryContext(ctx, `
		SELECT d.id, d.code, d.name, d.area_id, d.address, d.port, d.enabled, d.version,
		  a.name, a.path, r.snapshot, `+effectiveState+`,
		  COALESCE((SELECT SUM(sent) FROM ping_minutes WHERE device_id = d.id AND version = d.version AND minute BETWEEN ? AND ?), 0),
		  COALESCE((SELECT SUM(replies) FROM ping_minutes WHERE device_id = d.id AND version = d.version AND minute BETWEEN ? AND ?), 0)
		`+deviceJoin+where+" ORDER BY d.id LIMIT ? OFFSET ?", params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var views []DeviceView
	for rows.Next() {
		var view DeviceView
		var address string
		var port int
		var snapshot sql.NullString
		var state monitor.State
		err := rows.Scan(&view.ID, &view.Code, &view.Name, &view.AreaID, &address, &port, &view.Enabled,
			&view.Version, &view.AreaName, &view.AreaPath, &snapshot, &state, &view.Loss.Sent, &view.Loss.Replies)
		if err != nil {
			return nil, err
		}
		view.Target, err = monitor.ParseTarget(address, port)
		if err != nil {
			return nil, err
		}
		if snapshot.Valid {
			if err := json.Unmarshal([]byte(snapshot.String), &view.Snapshot); err != nil {
				return nil, err
			}
		}
		view.Snapshot.State = state
		if view.Enabled {
			view.Snapshot.Interval = settings.Interval(view.Snapshot.BothFailedSince, view.Snapshot.LastRound.CompletedAt)
		} else {
			view.Snapshot.Interval = 0
		}
		views = append(views, view)
	}
	return views, rows.Err()
}

func (s *Store) AreaStats(ctx context.Context) ([]AreaCounts, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT a.id, a.name, `+effectiveState+`, COUNT(d.id)
		FROM areas a LEFT JOIN devices d ON d.area_id = a.id
		LEFT JOIN latest_results r ON r.device_id = d.id AND r.version = d.version
		GROUP BY a.id, a.name, 3 ORDER BY a.name, a.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var groups []AreaCounts
	for rows.Next() {
		var id int64
		var name string
		var state monitor.State
		var count int
		if err := rows.Scan(&id, &name, &state, &count); err != nil {
			return nil, err
		}
		if len(groups) == 0 || groups[len(groups)-1].ID != id {
			groups = append(groups, AreaCounts{ID: id, Name: name, Counts: make(map[monitor.State]int)})
		}
		groups[len(groups)-1].Counts[state] = count
	}
	return groups, rows.Err()
}

type EventView struct {
	DeviceID   int64
	DeviceName string
	AreaName   string
	monitor.Event
}

func (s *Store) Events(ctx context.Context, area, device int64, from, through time.Time, limit, offset int) ([]EventView, error) {
	var terms []string
	var args []any
	if area > 0 {
		terms = append(terms, "e.area_id = ?")
		args = append(args, area)
	}
	if device > 0 {
		terms = append(terms, "e.device_id = ?")
		args = append(args, device)
	}
	if !from.IsZero() {
		terms = append(terms, "e.at_ns >= ?")
		args = append(args, from.UnixNano())
	}
	if !through.IsZero() {
		terms = append(terms, "e.at_ns < ?")
		args = append(args, through.UnixNano())
	}
	query := "SELECT e.device_id, d.name, e.area_name, e.event FROM state_events e JOIN devices d ON d.id = e.device_id"
	if len(terms) != 0 {
		query += " WHERE " + strings.Join(terms, " AND ")
	}
	args = append(args, limit, offset)
	rows, err := s.reader.QueryContext(ctx, query+" ORDER BY e.at_ns DESC, e.id DESC LIMIT ? OFFSET ?", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []EventView
	for rows.Next() {
		var view EventView
		var encoded string
		if err := rows.Scan(&view.DeviceID, &view.DeviceName, &view.AreaName, &encoded); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(encoded), &view.Event); err != nil {
			return nil, err
		}
		events = append(events, view)
	}
	return events, rows.Err()
}
