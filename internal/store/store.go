package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

type Store struct {
	db     *sql.DB
	reader *sql.DB
}

func Open(ctx context.Context, path string) (*Store, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dsnPath := filepath.ToSlash(absolute)
	if filepath.VolumeName(absolute) != "" && !strings.HasPrefix(dsnPath, "/") {
		dsnPath = "/" + dsnPath
	}
	pragmas := make(url.Values)
	pragmas.Add("_pragma", "busy_timeout(5000)")
	pragmas.Add("_pragma", "foreign_keys(ON)")
	dsn := url.URL{Scheme: "file", Path: dsnPath, RawQuery: pragmas.Encode()}
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := initialize(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	pragmas.Set("mode", "ro")
	dsn.RawQuery = pragmas.Encode()
	reader, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		db.Close()
		return nil, err
	}
	reader.SetMaxOpenConns(8)
	if err := reader.PingContext(ctx); err != nil {
		reader.Close()
		db.Close()
		return nil, err
	}
	return &Store{db: db, reader: reader}, nil
}

func initialize(ctx context.Context, db *sql.DB) error {
	var version string
	if err := db.QueryRowContext(ctx, "SELECT sqlite_version()").Scan(&version); err != nil {
		return err
	}
	var major, minor, patch int
	if _, err := fmt.Sscanf(version, "%d.%d.%d", &major, &minor, &patch); err != nil {
		return fmt.Errorf("parse SQLite version %q: %w", version, err)
	}
	if major < 3 || (major == 3 && (minor < 51 || (minor == 51 && patch < 3))) {
		return fmt.Errorf("SQLite %s lacks the required WAL-reset fix", version)
	}
	var mode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
		return err
	}
	if mode != "wal" {
		return fmt.Errorf("SQLite returned journal mode %q, expected WAL", mode)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := initializeSchema(ctx, tx); err != nil {
		return fmt.Errorf("initialize schema: %w", err)
	}
	return tx.Commit()
}

func initializeSchema(ctx context.Context, tx *sql.Tx) error {
	var eventAreaFK bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM pragma_foreign_key_list('state_events') WHERE "from" = 'area_id')
	`).Scan(&eventAreaFK); err != nil {
		return err
	}
	if eventAreaFK {
		if _, err := tx.ExecContext(ctx, `
			ALTER TABLE state_events RENAME TO state_events_v1;
			DROP INDEX IF EXISTS state_events_device_time;
			DROP INDEX IF EXISTS state_events_area_time;
			DROP INDEX IF EXISTS state_events_time;
		`); err != nil {
			return err
		}
	}
	var legacyNetworks bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM pragma_table_info('area_networks') WHERE name = 'prefix')
	`).Scan(&legacyNetworks); err != nil {
		return err
	}
	if legacyNetworks {
		if _, err := tx.ExecContext(ctx, "ALTER TABLE area_networks RENAME TO area_networks_v1"); err != nil {
			return err
		}
	}
	var legacy bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM pragma_table_info('discovery_jobs') WHERE name = 'port')
	`).Scan(&legacy); err != nil {
		return err
	}
	if legacy {
		if _, err := tx.ExecContext(ctx, `
			ALTER TABLE discovery_candidates RENAME TO discovery_candidates_v1;
			ALTER TABLE discovery_jobs RENAME TO discovery_jobs_v1;
			DROP INDEX IF EXISTS discovery_one_running;
			DROP INDEX IF EXISTS discovery_retention;
		`); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, schema); err != nil {
		return err
	}
	if eventAreaFK {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO state_events(id, device_id, area_id, area_name, version, at_ns, event)
			SELECT id, device_id, area_id, area_name, version, at_ns, event FROM state_events_v1;
			DROP TABLE state_events_v1;
		`); err != nil {
			return err
		}
	}
	if err := migratePathWatches(ctx, tx); err != nil {
		return err
	}
	if legacyNetworks {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO networks(prefix) SELECT DISTINCT prefix FROM area_networks_v1;
			INSERT INTO area_networks(area_id, network_id)
			SELECT a.area_id, n.id FROM area_networks_v1 a JOIN networks n ON n.prefix = a.prefix;
			DROP TABLE area_networks_v1;
		`); err != nil {
			return err
		}
	}
	if !legacy {
		return nil
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO discovery_jobs(id, preferred_area_id, prefix, ports, state, created_ns, finished_ns, total, completed, local_errors, reason)
		SELECT id, area_id, prefix, json_array(port), state, created_ns, finished_ns, total, completed, local_errors, reason
		FROM discovery_jobs_v1;
		INSERT INTO discovery_candidates(job_id, address, result)
		SELECT c.job_id, c.address, json_object('ip', c.address, 'ping', json_extract(c.result, '$.ping'),
		  'ports', json_array(json_object('port', j.port, 'result', json_extract(c.result, '$.tcp'))))
		FROM discovery_candidates_v1 c JOIN discovery_jobs_v1 j ON j.id = c.job_id;
		INSERT INTO sqlite_sequence(name, seq)
		SELECT 'discovery_jobs', seq FROM sqlite_sequence
		WHERE name = 'discovery_jobs_v1' AND NOT EXISTS (SELECT 1 FROM sqlite_sequence WHERE name = 'discovery_jobs');
		UPDATE sqlite_sequence SET seq = MAX(seq, COALESCE(
		  (SELECT seq FROM sqlite_sequence WHERE name = 'discovery_jobs_v1'), 0))
		WHERE name = 'discovery_jobs';
		DROP TABLE discovery_candidates_v1;
		DROP TABLE discovery_jobs_v1;
	`)
	return err
}

func (s *Store) Close() error {
	return errors.Join(s.reader.Close(), s.db.Close())
}
