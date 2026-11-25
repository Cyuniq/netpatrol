package monitor

import "golang.org/x/sys/windows"

const (
	connectionRefused  = windows.WSAECONNREFUSED
	hostUnreachable    = windows.WSAEHOSTUNREACH
	networkUnreachable = windows.WSAENETUNREACH
)
