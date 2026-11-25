package monitor

import (
	"errors"
	"time"
)

type Settings struct {
	PingTimeout, TCPTimeout               time.Duration
	FastInterval, SlowInterval, SlowAfter time.Duration
}

func DefaultSettings() Settings {
	return Settings{
		PingTimeout: time.Second, TCPTimeout: 2 * time.Second,
		FastInterval: FastInterval, SlowInterval: SlowInterval, SlowAfter: SlowAfter,
	}
}

func (s Settings) Validate() error {
	if s.PingTimeout < 100*time.Millisecond || s.PingTimeout > 30*time.Second {
		return errors.New("Ping 超时必须在 100～30,000 毫秒之间")
	}
	if s.TCPTimeout < 100*time.Millisecond || s.TCPTimeout > time.Minute {
		return errors.New("TCP 超时必须在 100～60,000 毫秒之间")
	}
	if s.FastInterval < time.Second || s.FastInterval > time.Hour {
		return errors.New("常规间隔必须在 1～3,600 秒之间")
	}
	if s.FastInterval <= max(s.PingTimeout, s.TCPTimeout) {
		return errors.New("常规间隔必须长于 Ping 和 TCP 超时")
	}
	if s.SlowInterval < s.FastInterval || s.SlowInterval > 24*time.Hour {
		return errors.New("降频间隔必须不短于常规间隔，且不超过 86,400 秒")
	}
	if s.SlowAfter < time.Second || s.SlowAfter > 24*time.Hour {
		return errors.New("降频门槛必须在 1～86,400 秒之间")
	}
	return nil
}

func (s Settings) Interval(failedSince, at time.Time) time.Duration {
	if !failedSince.IsZero() && at.Sub(failedSince) >= s.SlowAfter {
		return s.SlowInterval
	}
	return s.FastInterval
}
