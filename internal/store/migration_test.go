package store

import (
	"database/sql"
	"net/netip"
	"path/filepath"
	"slices"
	"testing"

	"netpatrol/internal/monitor"
)

func TestLegacyNetworkAndDiscoveryMigration(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacy.ExecContext(ctx, `
		CREATE TABLE areas(id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, path TEXT NOT NULL);
		CREATE TABLE area_networks(area_id INTEGER REFERENCES areas(id), prefix TEXT NOT NULL, PRIMARY KEY(area_id,prefix));
		CREATE TABLE discovery_jobs(
			id INTEGER PRIMARY KEY AUTOINCREMENT, area_id INTEGER NOT NULL REFERENCES areas(id), prefix TEXT NOT NULL,
			port INTEGER NOT NULL, state TEXT NOT NULL, created_ns INTEGER NOT NULL, finished_ns INTEGER,
			total INTEGER NOT NULL, completed INTEGER NOT NULL, local_errors INTEGER NOT NULL, reason TEXT NOT NULL
		);
		CREATE UNIQUE INDEX discovery_one_running ON discovery_jobs(state) WHERE state='running';
		CREATE INDEX discovery_retention ON discovery_jobs(finished_ns);
		CREATE TABLE discovery_candidates(
			job_id INTEGER REFERENCES discovery_jobs(id) ON DELETE CASCADE, address TEXT NOT NULL,
			result TEXT NOT NULL, PRIMARY KEY(job_id,address)
		);
		INSERT INTO areas VALUES (1,'East',''),(2,'West','');
		INSERT INTO area_networks VALUES (1,'192.0.2.0/24'),(1,'198.51.100.0/24'),(2,'192.0.2.0/24');
		INSERT INTO discovery_jobs VALUES (5,1,'192.0.2.1/32',554,'done',1,2,1,1,0,'');
		UPDATE sqlite_sequence SET seq=7 WHERE name='discovery_jobs';
		INSERT INTO discovery_candidates VALUES (5,'192.0.2.1',
		  '{"target":{"ip":"192.0.2.1","port":554},"ping":{"outcome":"passed"},"tcp":{"outcome":"refused"}}');
	`)
	if closeErr := legacy.Close(); err != nil || closeErr != nil {
		t.Fatalf("legacy fixture: %v %v", err, closeErr)
	}
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	networks, err := db.Networks(ctx)
	if err != nil || len(networks) != 2 {
		t.Fatalf("network catalog = %v, error = %v", networks, err)
	}
	areas, err := db.Areas(ctx)
	if err != nil || len(areas) != 2 || len(areas[0].Networks) != 2 || len(areas[1].Networks) != 1 {
		t.Fatalf("area associations = %v, error = %v", areas, err)
	}
	if _, err := db.SaveArea(ctx, 1, "East", "", ""); err != nil {
		t.Fatal(err)
	}
	if networks, err := db.Networks(ctx); err != nil || len(networks) != 2 {
		t.Fatalf("unlink removed network catalog item: %v, error = %v", networks, err)
	}
	job, candidates, err := db.Discovery(ctx, 5)
	if err != nil || job.PreferredAreaID != 1 || !slices.Equal(job.Ports, []uint16{554}) ||
		len(candidates) != 1 || candidates[0].Probe.Ping.Outcome != monitor.Passed ||
		candidates[0].Probe.Ports[0].Result.Outcome != monitor.Refused {
		t.Fatalf("legacy job = %+v, candidates = %+v, error = %v", job, candidates, err)
	}
	id, err := db.CreateDiscovery(ctx, 0, netip.MustParsePrefix("192.0.2.10/32"), []uint16{22, 554}, 1)
	if err != nil || id != 8 {
		t.Fatalf("new job ID = %d, error = %v", id, err)
	}
}
