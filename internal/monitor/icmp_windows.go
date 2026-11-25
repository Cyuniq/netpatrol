package monitor

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	ipHelper        = windows.NewLazySystemDLL("iphlpapi.dll")
	icmpCreateFile  = ipHelper.NewProc("IcmpCreateFile")
	icmp6CreateFile = ipHelper.NewProc("Icmp6CreateFile")
	icmpCloseHandle = ipHelper.NewProc("IcmpCloseHandle")
	icmpSendEcho    = ipHelper.NewProc("IcmpSendEcho")
	icmp6SendEcho   = ipHelper.NewProc("Icmp6SendEcho2")
)

type Prober struct {
	v4Err, v6Err error
}

func NewProber() *Prober {
	return &Prober{v4Err: checkICMP(false), v6Err: checkICMP(true)}
}

func (p *Prober) ICMPErrors() (error, error) { return p.v4Err, p.v6Err }

func (*Prober) Close() {}

func (p *Prober) probePing(ctx context.Context, ip netip.Addr, duration time.Duration) (Result, time.Time) {
	err := p.v4Err
	if ip.Is6() {
		err = p.v6Err
	}
	if err != nil {
		return failure(err), time.Time{}
	}
	attempt, sentAt := windowsEcho(ctx, ip, 0, duration)
	return attempt.Result, sentAt
}

func openICMP(ipv6 bool) (uintptr, error) {
	create := icmpCreateFile
	if ipv6 {
		create = icmp6CreateFile
	}
	if err := create.Find(); err != nil {
		return 0, err
	}
	handle, _, err := create.Call()
	if handle == uintptr(windows.InvalidHandle) {
		return 0, fmt.Errorf("%s: %w", create.Name, err)
	}
	return handle, nil
}

func checkICMP(ipv6 bool) error {
	handle, err := openICMP(ipv6)
	if err == nil {
		icmpCloseHandle.Call(handle)
	}
	return err
}

// IP Helper uses IP_OPTION_INFORMATION32 on Windows x64. This API owns
// request matching and supports echo requests without a Winsock raw socket.
type echoOptions struct {
	TTL, TOS, Flags, OptionsSize byte
	OptionsData                  uint32
}

// IP Helper returns a packed IPV6_ADDRESS_EX (port, flow, address, scope)
// with two trailing alignment bytes before the status and RTT fields.
type echoReply6 struct {
	address     [28]byte
	status, rtt uint32
}

type echoReply4 struct {
	address     [4]byte
	status, rtt uint32
}

func windowsEcho(ctx context.Context, ip netip.Addr, ttl byte, duration time.Duration) (TraceAttempt, time.Time) {
	localFailure := func(err error) (TraceAttempt, time.Time) {
		return TraceAttempt{Result: failure(err)}, time.Time{}
	}
	timeout, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	if err := timeout.Err(); err != nil {
		return localFailure(err)
	}
	destination := windows.RawSockaddrInet6{Family: windows.AF_INET6}
	if ip.Is6() {
		destination.Addr = ip.As16()
		if zone := ip.Zone(); zone != "" {
			index, err := strconv.ParseUint(zone, 10, 32)
			if err != nil {
				iface, err := net.InterfaceByName(zone)
				if err != nil {
					return localFailure(err)
				}
				index = uint64(iface.Index)
			}
			destination.Scope_id = uint32(index)
		}
	}
	handle, err := openICMP(ip.Is6())
	if err != nil {
		return localFailure(err)
	}
	defer icmpCloseHandle.Call(handle)

	deadline, _ := timeout.Deadline()
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return localFailure(context.DeadlineExceeded)
	}
	millis := uintptr((remaining + time.Millisecond - 1) / time.Millisecond)
	var options *echoOptions
	if ttl != 0 {
		options = &echoOptions{TTL: ttl}
	}
	payload := [16]byte{'N', 'e', 't', 'P', 'a', 't', 'r', 'o', 'l'}
	// Provide space for a reply header, the echoed payload and ICMP errors.
	var reply [32]uint64
	var count uintptr
	var callErr error
	var status, rtt uint32
	var address netip.Addr
	sentAt := time.Now()
	if ip.Is6() {
		source := windows.RawSockaddrInet6{Family: windows.AF_INET6}
		count, _, callErr = icmp6SendEcho.Call(handle, 0, 0, 0,
			uintptr(unsafe.Pointer(&source)), uintptr(unsafe.Pointer(&destination)),
			uintptr(unsafe.Pointer(&payload)), uintptr(len(payload)), uintptr(unsafe.Pointer(options)),
			uintptr(unsafe.Pointer(&reply)), unsafe.Sizeof(reply), millis)
		result := (*echoReply6)(unsafe.Pointer(&reply[0]))
		status, rtt, address = result.status, result.rtt, netip.AddrFrom16([16]byte(result.address[6:22]))
		if address.IsLinkLocalUnicast() && ip.Zone() != "" {
			address = address.WithZone(ip.Zone())
		}
	} else {
		ipBytes := ip.As4()
		count, _, callErr = icmpSendEcho.Call(handle, uintptr(binary.LittleEndian.Uint32(ipBytes[:])),
			uintptr(unsafe.Pointer(&payload)), uintptr(len(payload)), uintptr(unsafe.Pointer(options)),
			uintptr(unsafe.Pointer(&reply)), unsafe.Sizeof(reply), millis)
		result := (*echoReply4)(unsafe.Pointer(&reply[0]))
		status, rtt, address = result.status, result.rtt, netip.AddrFrom4(result.address)
	}
	completed := time.Now()
	if err := ctx.Err(); err != nil {
		return localFailure(err)
	}
	if count == 0 {
		var code syscall.Errno
		if !errors.As(callErr, &code) || code == 0 {
			return localFailure(errors.New("Windows ICMP 未返回结果"))
		}
		status = uint32(code)
	}
	outcome, detail := icmpOutcome(status)
	if outcome != Passed && outcome != HopReply && outcome != Unreachable || address.IsUnspecified() {
		address = netip.Addr{}
	}
	if outcome == Passed && (!address.IsValid() || address.WithZone("") != ip.WithZone("")) {
		return localFailure(errors.New("Windows ICMP 返回了非目标地址"))
	}
	result := Result{CompletedAt: completed, Outcome: outcome, Detail: detail}
	if address.IsValid() {
		result.RTT = time.Duration(rtt) * time.Millisecond
	}
	sent := count > 0 || outcome == Timeout
	if !sent {
		sentAt = time.Time{}
	}
	return TraceAttempt{Address: address, Result: result, Sent: sent}, sentAt
}

func icmpOutcome(status uint32) (Outcome, string) {
	switch status {
	case 0:
		return Passed, ""
	case 11010, uint32(windows.ERROR_TIMEOUT): // IP_REQ_TIMED_OUT
		return Timeout, ""
	case 11013, 11041: // IP_TTL_EXPIRED_TRANSIT, IP_TIME_EXCEEDED
		return HopReply, ""
	case 11002, 11003, 11004, 11005, 11012, 11040: // destination unreachable / no route
		return Unreachable, fmt.Sprintf("ICMP 不可达（代码 %d）", status)
	default:
		return LocalError, fmt.Sprintf("Windows ICMP 错误 %d", status)
	}
}

func Trace(ctx context.Context, ip netip.Addr) (TraceResult, error) {
	var trace TraceResult
	for hop := 1; hop <= TraceMaxHops; hop++ {
		if err := ctx.Err(); err != nil {
			return trace, err
		}
		result := TraceHop{Number: hop, Attempts: make([]TraceAttempt, TraceQueries)}
		var batch sync.WaitGroup
		for i := range result.Attempts {
			batch.Go(func() {
				timer := time.NewTimer(time.Duration(i) * traceSpacing)
				defer timer.Stop()
				select {
				case <-ctx.Done():
					result.Attempts[i].Result = failure(ctx.Err())
				case <-timer.C:
					result.Attempts[i], _ = windowsEcho(ctx, ip, byte(hop), traceTimeout)
				}
			})
		}
		batch.Wait()
		result.selectResult()
		trace.Hops = append(trace.Hops, result)
		if err := ctx.Err(); err != nil {
			return trace, err
		}
		switch result.Result.Outcome {
		case Passed:
			trace.Reached = true
			return trace, nil
		case LocalError, Unreachable:
			return trace, nil
		}
	}
	return trace, nil
}
