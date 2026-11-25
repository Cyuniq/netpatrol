//go:build !windows

package monitor

import "syscall"

const (
	connectionRefused  = syscall.ECONNREFUSED
	hostUnreachable    = syscall.EHOSTUNREACH
	networkUnreachable = syscall.ENETUNREACH
)
