package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"netpatrol/internal/store"
	"netpatrol/internal/telemetry"
)

func TestDiagnosticsRequiresAdminAndMeasuresList(t *testing.T) {
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "view.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	metrics := new(telemetry.Metrics)
	metrics.Scheduler(5, 3)
	metrics.ResultQueued(3, time.Now(), 20*time.Millisecond, false)
	server, err := Handler(db, "test", nil, nil, metrics, false)
	if err != nil {
		t.Fatal(err)
	}
	get := func(path string, authenticated bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if authenticated {
			req.SetBasicAuth("admin", "test")
		}
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		return rec
	}
	if rec := get("/diagnostics.json", false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated diagnostics status = %d", rec.Code)
	}
	if rec := get("/devices", true); rec.Code != http.StatusOK {
		t.Fatalf("devices status = %d", rec.Code)
	}
	rec := get("/diagnostics.json", true)
	var state telemetry.Snapshot
	if err := json.NewDecoder(rec.Body).Decode(&state); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || state.ScheduledDevices != 5 || state.QueueDepth != 1 ||
		state.Dispatch.P95MS != 20 || state.Page.Samples != 1 {
		t.Fatalf("status=%d snapshot=%+v", rec.Code, state)
	}
}

func TestAnonymousModeAllowsManagement(t *testing.T) {
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "anonymous.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server, err := Handler(db, "", nil, nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/areas", strings.NewReader("name=Gate&networks=192.0.2.0%2F24"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("WWW-Authenticate") != "" {
		t.Fatalf("anonymous area creation = %d", rec.Code)
	}
	areas, err := db.Areas(t.Context())
	if err != nil || len(areas) != 1 || areas[0].Name != "Gate" {
		t.Fatalf("anonymous area creation: areas=%v error=%v", areas, err)
	}
}
