package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"netpatrol/internal/discovery"
	"netpatrol/internal/monitor"
	"netpatrol/internal/scheduler"
	"netpatrol/internal/store"
	"netpatrol/internal/telemetry"
	"netpatrol/internal/web"
)

func serve(ctx context.Context, address, database string, maxInFlight, discoveryLimit int, config webConfig, ready func()) error {
	if maxInFlight < 1 {
		return errors.New("探测并发数必须大于零")
	}
	noAuthSources, err := parseNoAuthSources(config.noAuthCIDRs)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	defer listener.Close()
	lock, err := store.LockService(database)
	if err != nil {
		return err
	}
	defer lock.Close()
	db, err := store.Open(ctx, database)
	if err != nil {
		return err
	}
	defer db.Close()
	prober := monitor.NewProber()
	defer prober.Close()
	v4, v6 := prober.ICMPErrors()
	var warnings []string
	for _, err := range []error{v4, v6} {
		if err != nil {
			warnings = append(warnings, err.Error())
			log.Print(err)
		}
	}
	var saturated atomic.Bool
	metrics := new(telemetry.Metrics)
	discover, err := discovery.New(ctx, db, prober, config.discoveryCIDRs, discoveryLimit, func() bool {
		return !saturated.Load()
	})
	if err != nil {
		return err
	}
	defer discover.Close()
	handler, err := web.Handler(db, config.adminPassword, warnings, discover, metrics, config.noAuth == "1", noAuthSources...)
	if err != nil {
		return err
	}
	if err := db.ResetMonitoring(ctx, time.Now()); err != nil {
		return fmt.Errorf("重置监测状态: %w", err)
	}
	server := &http.Server{
		Handler: handler, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 60 * time.Second,
	}
	service, cancel := context.WithCancel(ctx)
	defer cancel()
	monitorDone := make(chan error, 1)
	httpDone := make(chan error, 1)
	pathDone := make(chan struct{})
	latencyDone := make(chan struct{})
	go func() { monitorDone <- scheduler.Run(service, db, prober, maxInFlight, &saturated, metrics) }()
	go func() {
		defer close(pathDone)
		scheduler.RunPathChecks(service, db, &saturated)
	}()
	go func() {
		defer close(latencyDone)
		scheduler.RunLatencyPruning(service, db)
	}()
	go func() { httpDone <- server.Serve(listener) }()
	log.Printf("网巡监听 %s", listener.Addr())
	ready()
	var exitErr error
	monitorStopped := false
	select {
	case <-ctx.Done():
	case exitErr = <-httpDone:
		if errors.Is(exitErr, http.ErrServerClosed) {
			exitErr = nil
		}
	case exitErr = <-monitorDone:
		monitorStopped = true
		if errors.Is(exitErr, context.Canceled) && ctx.Err() != nil {
			exitErr = nil
		}
	}
	cancel()
	<-pathDone
	<-latencyDone
	shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err := server.Shutdown(shutdown); err != nil {
		server.Close()
		exitErr = errors.Join(exitErr, err)
	}
	if !monitorStopped {
		if err := <-monitorDone; err != nil && !errors.Is(err, context.Canceled) {
			exitErr = errors.Join(exitErr, err)
		}
	}
	return exitErr
}
