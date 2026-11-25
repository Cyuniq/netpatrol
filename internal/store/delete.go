package store

import (
	"context"
	"database/sql"
	"errors"
)

var ErrAreaInUse = errors.New("区域内还有设备，请先调整设备归属")

func (s *Store) AreaDeleteInfo(ctx context.Context, id int64) (string, int, error) {
	var name string
	var devices int
	err := s.reader.QueryRowContext(ctx, `
		SELECT name, (SELECT COUNT(*) FROM devices WHERE area_id = areas.id)
		FROM areas WHERE id = ?`, id).Scan(&name, &devices)
	return name, devices, err
}

func (s *Store) DeleteArea(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var used bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM devices WHERE area_id = ?)", id).Scan(&used); err != nil {
		return err
	}
	if used {
		return ErrAreaInUse
	}
	if _, err := tx.ExecContext(ctx, "UPDATE discovery_jobs SET preferred_area_id = NULL WHERE preferred_area_id = ?", id); err != nil {
		return err
	}
	return deleteObject(ctx, tx, "area", "DELETE FROM areas WHERE id = ?", id)
}

func (s *Store) DeleteDevice(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, query := range []string{
		"DELETE FROM latest_results WHERE device_id = ?",
		"DELETE FROM ping_minutes WHERE device_id = ?",
		"DELETE FROM state_events WHERE device_id = ?",
	} {
		if _, err := tx.ExecContext(ctx, query, id); err != nil {
			return err
		}
	}
	return deleteObject(ctx, tx, "device", "DELETE FROM devices WHERE id = ?", id)
}

func (s *Store) DeletePath(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return deleteObject(ctx, tx, "path", "DELETE FROM probe_paths WHERE id = ?", id)
}

func deleteObject(ctx context.Context, tx *sql.Tx, kind, query string, id int64) error {
	result, err := tx.ExecContext(ctx, query, id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return sql.ErrNoRows
	}
	// Preserve IDs consumed by deleted objects so stale requests and pending
	// probe results cannot find a replacement at the same ID.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO deleted_id_floors(kind, last_id) VALUES (?, ?)
		ON CONFLICT(kind) DO UPDATE SET last_id = MAX(excluded.last_id, deleted_id_floors.last_id)
	`, kind, id); err != nil {
		return err
	}
	return tx.Commit()
}
