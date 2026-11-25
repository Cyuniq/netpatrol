package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"netpatrol/internal/monitor"
	"netpatrol/internal/store"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := runApplication(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, ready func()) (runErr error) {
	ipText := flag.String("ip", "", "IPv4 or IPv6 address, including %interface for link-local IPv6")
	port := flag.Int("port", 554, "TCP port")
	once := flag.Bool("once", false, "probe once and exit")
	listen := flag.String("listen", "127.0.0.1:8080", "HTTP listen address in service mode")
	dbPath := flag.String("db", "netpatrol.db", "SQLite database in service mode")
	maxInFlight := flag.Int("max-in-flight", 1200, "maximum concurrent monitoring rounds")
	discoveryLimit := flag.Int("discovery-limit", 1024, "maximum IPv4 addresses per discovery task")
	backupTo := flag.String("backup-to", "", "write a consistent database snapshot to a new file and exit")
	envFile := flag.String("env-file", "", "read Web settings from a KEY=VALUE configuration file")
	logFile := flag.String("log-file", "", "append service logs to this file")
	flag.Parse()

	if *logFile != "" {
		file, err := os.OpenFile(*logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		output := log.Writer()
		log.SetOutput(file)
		defer func() {
			if runErr != nil {
				log.Print(runErr)
			}
			log.SetOutput(output)
			file.Close()
		}()
	}
	if *backupTo != "" {
		if _, err := os.Stat(*dbPath); err != nil {
			return err
		}
		db, err := store.Open(ctx, *dbPath)
		if err != nil {
			return err
		}
		defer db.Close()
		return db.Backup(ctx, *backupTo)
	}
	if *ipText == "" && !*once {
		config, err := loadWebConfig(*envFile)
		if err != nil {
			return err
		}
		return serve(ctx, *listen, *dbPath, *maxInFlight, *discoveryLimit, config, ready)
	}
	target, err := monitor.ParseTarget(*ipText, *port)
	if err != nil {
		return err
	}
	prober := monitor.NewProber()
	defer prober.Close()
	v4Err, v6Err := prober.ICMPErrors()
	for _, err := range []error{v4Err, v6Err} {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	}
	encoder := json.NewEncoder(os.Stdout)
	snapshot := monitor.NewSnapshot(time.Now())
	var lossWindow monitor.LossWindow
	for ctx.Err() == nil {
		round := prober.Probe(ctx, target)
		if ctx.Err() != nil {
			return nil
		}
		if err := report(encoder, &snapshot, &lossWindow, round); err != nil {
			return err
		}
		if *once {
			return nil
		}
		next := round.StartedAt.Add(snapshot.Interval)
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		// A suspended process can miss a full scheduled slot. Record one
		// missing observation and resume at the next slot without replaying it.
		if now := time.Now(); now.Sub(next) >= snapshot.Interval {
			missed := monitor.Round{Target: target, CompletedAt: now}
			if err := report(encoder, &snapshot, &lossWindow, missed); err != nil {
				return err
			}
			remainder := now.Sub(next) % snapshot.Interval
			timer.Reset(snapshot.Interval - remainder)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil
			case <-timer.C:
			}
		}
	}
	return nil
}

func report(encoder *json.Encoder, snapshot *monitor.Snapshot, window *monitor.LossWindow, round monitor.Round) error {
	event := snapshot.Apply(round)
	window.Record(round)
	loss := window.At(round.CompletedAt)
	record := struct {
		monitor.Snapshot
		Loss        monitor.Loss   `json:"ping_loss"`
		Lost        int            `json:"ping_lost"`
		LossPercent *float64       `json:"ping_loss_percent"`
		Event       *monitor.Event `json:"event,omitempty"`
	}{Snapshot: *snapshot, Loss: loss, Lost: loss.Lost(), Event: event}
	if percent, ok := loss.Percent(); ok {
		record.LossPercent = &percent
	}
	return encoder.Encode(record)
}
