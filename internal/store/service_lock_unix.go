//go:build !windows

package store

import (
	"os"

	"golang.org/x/sys/unix"
)

func exclusiveLock(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}
