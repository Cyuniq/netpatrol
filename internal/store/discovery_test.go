package store

import (
	"net/netip"
	"path/filepath"
	"testing"

	"netpatrol/internal/monitor"
)

func TestConcurrentCSVAndDiscoveryConfirmSameAddress(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "batch.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	area, err := db.SaveArea(ctx, 0, "Area", "", "192.0.2.0/24")
	if err != nil {
		t.Fatal(err)
	}
	rows := []ImportRow{{Line: 2, Area: "Area", DeviceInput: DeviceInput{
		Code: "CSV-1", Name: "CSV name", Address: "192.0.2.1", Port: 8554,
	}}}
	preview, err := db.PreviewImport(ctx, rows, false)
	if err != nil || len(preview) != 1 || preview[0].Action != ImportAdd {
		t.Fatalf("preview %v: %v", preview, err)
	}
	job, err := db.CreateDiscovery(ctx, area, netip.MustParsePrefix("192.0.2.1/32"), []uint16{554}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RecordDiscovery(ctx, job, monitor.DiscoveryProbe{
		IP: netip.MustParseAddr("192.0.2.1"), Ping: monitor.Result{Outcome: monitor.Passed},
		Ports: []monitor.PortProbe{{Port: 554}},
	}); err != nil {
		t.Fatal(err)
	}
	type importResult struct {
		committed bool
		err       error
	}
	type discoveryResult struct {
		added, existing int
		err             error
	}
	start := make(chan struct{})
	imported := make(chan importResult, 1)
	discovered := make(chan discoveryResult, 1)
	go func() {
		<-start
		_, committed, err := db.ConfirmImport(ctx, rows, false, preview)
		imported <- importResult{committed, err}
	}()
	go func() {
		<-start
		added, existing, err := db.AddDiscovered(ctx, job, area, []DiscoverySelection{{Address: "192.0.2.1", Name: "Discovery name"}})
		discovered <- discoveryResult{added, existing, err}
	}()
	close(start)
	fromCSV, fromDiscovery := <-imported, <-discovered
	if fromCSV.err != nil || fromDiscovery.err != nil {
		t.Fatalf("CSV error: %v, discovery error: %v", fromCSV.err, fromDiscovery.err)
	}
	devices, err := db.Devices(ctx)
	if err != nil || len(devices) != 1 {
		t.Fatalf("found %d devices: %v", len(devices), err)
	}
	if fromCSV.committed {
		if fromDiscovery.added != 0 || fromDiscovery.existing != 1 || devices[0].Name != "CSV name" ||
			devices[0].Target.Port != 8554 {
			t.Fatalf("discovery replaced CSV configuration: %+v, %+v", fromDiscovery, devices[0])
		}
	} else if fromDiscovery.added != 1 || devices[0].Name != "Discovery name" || devices[0].Target.Port != 554 {
		t.Fatalf("CSV replaced discovery configuration: %+v, %+v", fromDiscovery, devices[0])
	}
}
