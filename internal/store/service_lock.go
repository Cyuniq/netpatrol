package store

import (
	"fmt"
	"os"
	"path/filepath"
)

// LockService holds exclusive ownership of the scheduler for a database.
// The lock file stays in place so concurrent starts always lock the same file.
func LockService(database string) (*os.File, error) {
	absolute, err := filepath.Abs(database)
	if err != nil {
		return nil, err
	}
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		absolute = resolved
	} else if parent, err := filepath.EvalSymlinks(filepath.Dir(absolute)); err == nil {
		absolute = filepath.Join(parent, filepath.Base(absolute))
	}
	file, err := os.OpenFile(absolute+".service-lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := exclusiveLock(file); err != nil {
		file.Close()
		return nil, fmt.Errorf("无法取得监测服务锁 %s: %w", absolute, err)
	}
	return file, nil
}
