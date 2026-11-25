package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"netpatrol/internal/monitor"
	"netpatrol/internal/store"
	"netpatrol/internal/telemetry"
)

type Prober interface {
	ProbeWithSettings(context.Context, monitor.Target, monitor.Settings) monitor.Round
}

type repository interface {
	Devices(context.Context) ([]store.Device, error)
	Settings(context.Context) (monitor.Settings, error)
	Record(context.Context, []store.Observation) error
	PruneMonitoring(context.Context, time.Time) error
}

type queuedDevice struct {
	device   store.Device
	next     time.Time
	snapshot monitor.Snapshot
}

func (d *queuedDevice) advance(round monitor.Round, settings monitor.Settings) {
	d.snapshot.ApplyWithSettings(round, settings)
	d.next = d.next.Add(d.snapshot.Interval)
	if !d.next.After(round.CompletedAt) {
		skipped := round.CompletedAt.Sub(d.next)/d.snapshot.Interval + 1
		d.next = d.next.Add(skipped * d.snapshot.Interval)
	}
}

// A stable phase avoids launching a fleet all at once on restart.
func firstDue(id int64, now time.Time, interval time.Duration) time.Time {
	slots := uint64(interval / (50 * time.Millisecond))
	hash := uint64(id) * 11400714819323198485
	phase := time.Duration((hash>>32)%slots) * 50 * time.Millisecond
	due := now.Truncate(interval).Add(phase)
	if due.Before(now) {
		due = due.Add(interval)
	}
	return due
}

func Run(ctx context.Context, repo repository, prober Prober, maxInFlight int, saturated *atomic.Bool, metrics *telemetry.Metrics) error {
	if maxInFlight < 1 {
		return errors.New("探测并发数至少为 1")
	}
	ctx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	defer func() {
		cancel()
		workers.Wait()
	}()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	results := make(chan store.Observation, maxInFlight)
	inFlight := make(map[int64]struct{})
	devices := make(map[int64]*queuedDevice)
	var order []store.Device
	settings := monitor.DefaultSettings()
	var nextSync, nextPrune time.Time
	batch := make([]store.Observation, 0, 128)

	for ctx.Err() == nil {
		now := time.Now()
		if !now.Before(nextSync) {
			current, err := repo.Settings(ctx)
			if err != nil {
				return fmt.Errorf("读取探测设置: %w", err)
			}
			rows, err := repo.Devices(ctx)
			if err != nil {
				return fmt.Errorf("读取设备配置: %w", err)
			}
			order = rows
			devices = reconcile(devices, rows, now, current)
			if current != settings {
				for id, device := range devices {
					if _, busy := inFlight[id]; !busy {
						device.reschedule(now, current)
					}
				}
				settings = current
			}
			nextSync = now.Add(time.Second)
		}
		if !now.Before(nextPrune) {
			if err := repo.PruneMonitoring(ctx, now); err != nil {
				return fmt.Errorf("清理历史记录: %w", err)
			}
			nextPrune = now.Add(time.Hour)
		}
		batch = batch[:0]
	drain:
		for len(batch) < cap(batch) {
			select {
			case result := <-results:
				batch = append(batch, result)
				if d := devices[result.DeviceID]; d != nil && d.device.Version == result.Version {
					d.advance(result.Round, settings)
				}
			default:
				break drain
			}
		}
		for _, asset := range order {
			d := devices[asset.ID]
			if d == nil || now.Before(d.next) {
				continue
			}
			if _, busy := inFlight[asset.ID]; busy {
				continue
			}
			if now.Sub(d.next) >= d.snapshot.Interval {
				if len(batch) < cap(batch) {
					round := monitor.Round{Target: asset.Target, CompletedAt: now}
					metrics.ResultQueued(asset.ID, now, 0, true)
					batch = append(batch, store.Observation{DeviceID: asset.ID, Version: asset.Version, Round: round})
					d.advance(round, settings)
				}
				continue
			}
			if len(inFlight) < maxInFlight {
				due := d.next
				current := settings
				inFlight[asset.ID] = struct{}{}
				workers.Go(func() {
					round := prober.ProbeWithSettings(ctx, asset.Target, current)
					if ctx.Err() == nil {
						metrics.ResultQueued(asset.ID, round.CompletedAt, round.StartedAt.Sub(due), false)
						results <- store.Observation{DeviceID: asset.ID, Version: asset.Version, Round: round}
					}
				})
			}
		}
		saturated.Store(len(inFlight) >= maxInFlight || len(batch) == cap(batch))
		metrics.Scheduler(len(devices), len(inFlight))
		if len(batch) != 0 {
			if err := repo.Record(ctx, batch); err != nil {
				return fmt.Errorf("保存探测结果: %w", err)
			}
			written := time.Now()
			for _, result := range batch {
				delete(inFlight, result.DeviceID)
				var interval time.Duration
				if d := devices[result.DeviceID]; d != nil && d.device.Version == result.Version {
					interval = d.snapshot.ActualInterval
				}
				metrics.ResultWritten(result.DeviceID, written, interval)
			}
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
	return nil
}

func (d *queuedDevice) reschedule(now time.Time, settings monitor.Settings) {
	interval := settings.Interval(d.snapshot.BothFailedSince, d.snapshot.LastRound.CompletedAt)
	if interval != d.snapshot.Interval {
		d.snapshot.Interval = interval
		d.next = firstDue(d.device.ID, now, interval)
	}
}

func reconcile(previous map[int64]*queuedDevice, assets []store.Device, now time.Time, settings monitor.Settings) map[int64]*queuedDevice {
	devices := make(map[int64]*queuedDevice, len(assets))
	for _, asset := range assets {
		if !asset.Enabled {
			continue
		}
		if existing := previous[asset.ID]; existing != nil && existing.device.Version == asset.Version {
			devices[asset.ID] = existing
		} else {
			snapshot := monitor.NewSnapshot(now)
			snapshot.Interval = settings.FastInterval
			devices[asset.ID] = &queuedDevice{
				device: asset, next: firstDue(asset.ID, now, settings.FastInterval), snapshot: snapshot,
			}
		}
	}
	return devices
}
