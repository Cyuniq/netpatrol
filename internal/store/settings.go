package store

import (
	"context"
	"database/sql"
	"errors"

	"netpatrol/internal/monitor"
)

type settingsReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func (s *Store) Settings(ctx context.Context) (monitor.Settings, error) {
	return readSettings(ctx, s.reader)
}

func readSettings(ctx context.Context, db settingsReader) (monitor.Settings, error) {
	var settings monitor.Settings
	err := db.QueryRowContext(ctx, `
		SELECT ping_timeout_ns, tcp_timeout_ns, fast_interval_ns, slow_interval_ns, slow_after_ns
		FROM probe_settings WHERE id = 1`,
	).Scan(&settings.PingTimeout, &settings.TCPTimeout, &settings.FastInterval, &settings.SlowInterval, &settings.SlowAfter)
	if errors.Is(err, sql.ErrNoRows) {
		return monitor.DefaultSettings(), nil
	}
	if err != nil {
		return settings, err
	}
	return settings, settings.Validate()
}

func (s *Store) SaveSettings(ctx context.Context, settings monitor.Settings) error {
	if err := settings.Validate(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO probe_settings VALUES (1, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
		  ping_timeout_ns=excluded.ping_timeout_ns, tcp_timeout_ns=excluded.tcp_timeout_ns,
		  fast_interval_ns=excluded.fast_interval_ns, slow_interval_ns=excluded.slow_interval_ns,
		  slow_after_ns=excluded.slow_after_ns`,
		settings.PingTimeout, settings.TCPTimeout, settings.FastInterval, settings.SlowInterval, settings.SlowAfter,
	)
	return err
}
