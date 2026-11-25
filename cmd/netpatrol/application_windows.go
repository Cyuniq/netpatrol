package main

import (
	"context"
	"errors"

	"golang.org/x/sys/windows/svc"
)

func runApplication(ctx context.Context) error {
	service, err := svc.IsWindowsService()
	if err != nil {
		return err
	}
	if !service {
		return run(ctx, func() {})
	}
	return svc.Run("NetPatrol", windowsService{ctx: ctx})
}

type windowsService struct{ ctx context.Context }

func (w windowsService) Execute(_ []string, requests <-chan svc.ChangeRequest, statuses chan<- svc.Status) (bool, uint32) {
	ctx, cancel := context.WithCancel(w.ctx)
	defer cancel()
	status := svc.Status{State: svc.StartPending, WaitHint: 30000}
	statuses <- status
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, func() { close(ready) })
	}()
	for {
		select {
		case <-ready:
			ready = nil
			if status.State == svc.StartPending {
				status = svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
				statuses <- status
			}
		case change := <-requests:
			switch change.Cmd {
			case svc.Interrogate:
				statuses <- status
			case svc.Stop, svc.Shutdown:
				status = svc.Status{State: svc.StopPending, WaitHint: 30000}
				statuses <- status
				cancel()
			}
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				return true, 1
			}
			return false, 0
		}
	}
}
