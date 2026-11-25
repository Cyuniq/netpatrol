package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// Backup uses a SQLite snapshot, including commits currently in the WAL.
func (s *Store) Backup(ctx context.Context, destination string) error {
	path, err := filepath.Abs(destination)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		os.Remove(path)
		return err
	}
	// SQLite can open an empty destination created exclusively above.
	if _, err := s.reader.ExecContext(ctx, "VACUUM INTO ?", path); err != nil {
		os.Remove(path)
		return fmt.Errorf("创建 SQLite 快照: %w", err)
	}
	file, err = os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}
