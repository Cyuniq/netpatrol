package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"netpatrol/internal/monitor"
)

const (
	DefaultPathIntervalSeconds = 300
	MinPathIntervalSeconds     = 60
	MaxPathIntervalSeconds     = 86400
)

type PathInput struct {
	Name, Address   string
	Enabled         bool
	IntervalSeconds int
}

type PathWatch struct {
	ID int64
	PathInput
	NextCheck time.Time
	Latest    *PathSample
	Changes   []PathSample
}

type PathTask struct {
	ID, Generation int64
	Address        netip.Addr
}

type PathSample struct {
	Address   netip.Addr          `json:"address"`
	Started   time.Time           `json:"started"`
	Completed time.Time           `json:"completed"`
	Result    monitor.TraceResult `json:"result"`
	Message   string              `json:"message,omitempty"`
}

func migratePathWatches(ctx context.Context, tx *sql.Tx) error {
	var legacy bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM sqlite_schema WHERE type = 'table' AND name = 'path_watch')
	`).Scan(&legacy); err != nil || !legacy {
		return err
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO probe_paths(id, name, address, enabled, interval_seconds, next_check_ns, signature, latest)
		SELECT d.id, d.name, d.address, w.enabled AND d.enabled, w.interval_seconds,
		  CASE WHEN w.observed_version = d.version THEN w.next_check_ns ELSE 0 END,
		  CASE WHEN w.observed_version = d.version THEN w.signature END,
		  CASE WHEN w.observed_version = d.version THEN w.latest END
		FROM path_watch w JOIN devices d ON d.id = w.device_id;
		INSERT INTO probe_path_changes(id, path_id, sample)
		SELECT c.id, c.device_id, c.sample FROM path_changes c JOIN probe_paths p ON p.id = c.device_id;
		DROP TABLE path_changes;
		DROP TABLE path_watch;
	`)
	return err
}

func (s *Store) SavePath(ctx context.Context, id int64, input PathInput, now time.Time) (int64, error) {
	if input.IntervalSeconds < MinPathIntervalSeconds || input.IntervalSeconds > MaxPathIntervalSeconds {
		return 0, fmt.Errorf("持续检测间隔必须在 %d～%d 秒之间", MinPathIntervalSeconds, MaxPathIntervalSeconds)
	}
	ip, err := monitor.ParseAddress(input.Address)
	if err != nil {
		return 0, fmt.Errorf("目标 IP 无效: %w", err)
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		name = ip.String()
	}
	params := []any{sql.Named("name", name), sql.Named("address", ip.String()), sql.Named("enabled", input.Enabled),
		sql.Named("interval", input.IntervalSeconds), sql.Named("now", now.UnixNano())}
	if id == 0 {
		err := s.db.QueryRowContext(ctx, `
			INSERT INTO probe_paths(id, name, address, enabled, interval_seconds, next_check_ns)
			VALUES (MAX(COALESCE((SELECT MAX(id) FROM probe_paths), 0),
			  COALESCE((SELECT last_id FROM deleted_id_floors WHERE kind = 'path'), 0)) + 1,
			  @name, @address, @enabled, @interval, @now)
			ON CONFLICT(address) DO NOTHING RETURNING id
		`, params...).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, errors.New("该 IP 已登记为路径目标")
		}
		return id, err
	}
	params = append(params, sql.Named("id", id))
	err = s.db.QueryRowContext(ctx, `
		UPDATE probe_paths SET name = @name, address = @address, enabled = @enabled, interval_seconds = @interval,
		  next_check_ns = CASE WHEN address <> @address OR enabled <> @enabled OR interval_seconds <> @interval
		    THEN @now ELSE next_check_ns END,
		  generation = generation + (address <> @address OR enabled <> @enabled OR interval_seconds <> @interval),
		  signature = CASE WHEN address <> @address THEN NULL ELSE signature END,
		  latest = CASE WHEN address <> @address THEN NULL ELSE latest END
		WHERE id = @id RETURNING id
	`, params...).Scan(&id)
	return id, err
}

func (s *Store) ListPaths(ctx context.Context) ([]PathWatch, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT id,name,address,enabled,interval_seconds,next_check_ns FROM probe_paths ORDER BY id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var paths []PathWatch
	for rows.Next() {
		var path PathWatch
		var next int64
		if err := rows.Scan(&path.ID, &path.Name, &path.Address, &path.Enabled, &path.IntervalSeconds, &next); err != nil {
			return nil, err
		}
		if next > 0 && path.Enabled {
			path.NextCheck = time.Unix(0, next)
		}
		paths = append(paths, path)
	}
	return paths, rows.Err()
}

func (s *Store) PathWatch(ctx context.Context, id int64) (PathWatch, error) {
	watch := PathWatch{ID: id}
	var next int64
	var latest sql.NullString
	err := s.reader.QueryRowContext(ctx, `
		SELECT name, address, enabled, interval_seconds, next_check_ns, latest FROM probe_paths WHERE id = ?
	`, id).Scan(&watch.Name, &watch.Address, &watch.Enabled, &watch.IntervalSeconds, &next, &latest)
	if err != nil {
		return watch, err
	}
	if next > 0 && watch.Enabled {
		watch.NextCheck = time.Unix(0, next)
	}
	if latest.Valid {
		watch.Latest = new(PathSample)
		if err := json.Unmarshal([]byte(latest.String), watch.Latest); err != nil {
			return watch, err
		}
	}
	rows, err := s.reader.QueryContext(ctx, `
		SELECT sample FROM probe_path_changes WHERE path_id = ? ORDER BY id DESC LIMIT 50
	`, id)
	if err != nil {
		return watch, err
	}
	defer rows.Close()
	for rows.Next() {
		var sample PathSample
		var encoded string
		if err := rows.Scan(&encoded); err != nil {
			return watch, err
		}
		if err := json.Unmarshal([]byte(encoded), &sample); err != nil {
			return watch, err
		}
		watch.Changes = append(watch.Changes, sample)
	}
	return watch, rows.Err()
}

func (s *Store) DuePathChecks(ctx context.Context, now time.Time, limit int) ([]PathTask, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT id, generation, address FROM probe_paths WHERE enabled = 1 AND next_check_ns <= ?
		ORDER BY next_check_ns, id LIMIT ?
	`, now.UnixNano(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tasks []PathTask
	for rows.Next() {
		var task PathTask
		var address string
		if err := rows.Scan(&task.ID, &task.Generation, &address); err != nil {
			return nil, err
		}
		task.Address, err = netip.ParseAddr(address)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

func (s *Store) RecordPathCheck(ctx context.Context, task PathTask, sample PathSample) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var previous sql.NullString
	var interval int64
	err = tx.QueryRowContext(ctx, `
		SELECT signature, interval_seconds FROM probe_paths WHERE id = ? AND generation = ? AND enabled = 1
	`, task.ID, task.Generation).Scan(&previous, &interval)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := recordPathLatency(ctx, tx, task.ID, sample); err != nil {
		return err
	}
	encoded, err := json.Marshal(sample)
	if err != nil {
		return err
	}
	signature := pathSignature(sample)
	next := sample.Completed.Add(time.Duration(interval) * time.Second)
	if _, err := tx.ExecContext(ctx, `
		UPDATE probe_paths SET signature = ?, latest = ?, next_check_ns = ? WHERE id = ?
	`, signature, string(encoded), next.UnixNano(), task.ID); err != nil {
		return err
	}
	if previous.String != signature {
		if _, err := tx.ExecContext(ctx, "INSERT INTO probe_path_changes(path_id, sample) VALUES (?, ?)",
			task.ID, string(encoded)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM probe_path_changes WHERE id IN (
			  SELECT id FROM probe_path_changes WHERE path_id = ? ORDER BY id DESC LIMIT -1 OFFSET 50
			)`, task.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func pathSignature(sample PathSample) string {
	digest := sha256.New()
	fmt.Fprintln(digest, sample.Address, sample.Result.Reached, sample.Message != "")
	for _, hop := range sample.Result.Hops {
		fmt.Fprintln(digest, hop.Number, hop.Address, hop.Result.Outcome)
		if addresses := hop.Addresses(); len(addresses) > 1 {
			fmt.Fprintln(digest, addresses)
		}
	}
	return hex.EncodeToString(digest.Sum(nil))
}
