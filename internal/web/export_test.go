package web

import (
	"encoding/csv"
	"fmt"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"netpatrol/internal/monitor"
	"netpatrol/internal/store"
)

func TestExportFilterAcrossBatches(t *testing.T) {
	ctx := t.Context()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "export.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	area, err := db.SaveArea(ctx, 0, "East", " +1", "192.0.2.0/24")
	if err != nil {
		t.Fatal(err)
	}
	for index := 1; index <= 205; index++ {
		name := fmt.Sprintf("Camera %03d", index)
		if index == 1 {
			name = "=Camera, first"
		}
		_, err := db.SaveDevice(ctx, 0, store.DeviceInput{Name: name, AreaID: area,
			Address: fmt.Sprintf("192.0.2.%d", index), Port: 554, Enabled: index != 205})
		if err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	if err := db.Record(ctx, []store.Observation{{DeviceID: 1, Version: 1, Round: monitor.Round{
		Target:    monitor.Target{IP: netip.MustParseAddr("192.0.2.1"), Port: 554},
		StartedAt: now, CompletedAt: now, EchoSentAt: now,
		Ping: monitor.Result{Outcome: monitor.Passed, CompletedAt: now},
		TCP:  monitor.Result{Outcome: monitor.Passed, CompletedAt: now},
	}}}); err != nil {
		t.Fatal(err)
	}
	handler, err := Handler(db, "local-test-password", nil, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	get := func(path string, fragment bool) string {
		t.Helper()
		req := httptest.NewRequest("GET", path, nil)
		req.SetBasicAuth("admin", "local-test-password")
		if fragment {
			req.Header.Set("HX-Request", "true")
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("%s = %d, body = %q", path, w.Code, w.Body.String())
		}
		return w.Body.String()
	}
	exported := get("/exports/assets.csv?area=1&q=Camera&page=4", false)
	assetText, hasBOM := strings.CutPrefix(exported, csvUTF8BOM)
	rows, err := csv.NewReader(strings.NewReader(assetText)).ReadAll()
	if !hasBOM || err != nil || len(rows) != 206 || rows[1][1] != "=Camera, first" {
		t.Fatalf("asset export: %d rows, error=%v", len(rows), err)
	}
	inputs, err := parseAssetCSV(strings.NewReader(exported))
	if err != nil {
		t.Fatal(err)
	}
	planned, err := db.PreviewImport(ctx, inputs, true)
	if err != nil || len(planned) != 205 {
		t.Fatalf("re-imported %d assets, error=%v", len(planned), err)
	}
	for _, entry := range planned {
		if entry.Action != store.ImportSkip {
			t.Fatalf("round-trip changed an asset: %+v", entry)
		}
	}
	report := strings.TrimPrefix(get("/exports/status.csv?state=healthy", false), "\ufeff")
	rows, err = csv.NewReader(strings.NewReader(report)).ReadAll()
	if err != nil || len(rows) != 2 || rows[1][1] != "'=Camera, first" ||
		rows[1][5] != "' +1" || !strings.Contains(rows[1][7], "T") {
		t.Fatalf("status export: %v, error=%v", rows, err)
	}
	fragment := get("/devices?area=1&q=Camera&page=2&trend=6h", true)
	if strings.Contains(fragment, "<html") || !strings.Contains(fragment, `hx-get="/devices?area=1&amp;page=2&amp;q=Camera&amp;trend=6h"`) ||
		!strings.Contains(fragment, `href="/devices?area=1&amp;q=Camera&amp;trend=6h#device-list" rel="prev">上一页`) ||
		!strings.Contains(fragment, "Camera 051") || strings.Contains(fragment, "Camera 050") {
		t.Fatalf("unexpected page 2 fragment")
	}

	fragment = get("/devices?area=1&state=unknown&q=Camera&page=6&page_size=20&trend=6h", true)
	for _, want := range []string{
		`hx-get="/devices?area=1&amp;page=6&amp;page_size=20&amp;q=Camera&amp;state=unknown&amp;trend=6h"`,
		`href="/devices?area=1&amp;page=11&amp;page_size=20&amp;q=Camera&amp;state=unknown&amp;trend=6h#device-list"`,
		`aria-current="page" aria-label="第 6 页"`,
		`name="page" type="number" min="1" max="11"`,
		`class="pagination-gap"`,
		`显示第 101–120 台 · 共 203 台`,
		"Camera 102",
		"Camera 121",
	} {
		if !strings.Contains(fragment, want) {
			t.Errorf("custom pagination missing %q", want)
		}
	}
	if strings.Contains(fragment, "Camera 101") || strings.Contains(fragment, "Camera 122") {
		t.Fatal("custom page contains a device outside its range")
	}

	fragment = get("/devices?area=1&state=unknown&page=999&page_size=20", true)
	if !strings.Contains(fragment, `hx-get="/devices?area=1&amp;page=11&amp;page_size=20&amp;state=unknown"`) ||
		!strings.Contains(fragment, "显示第 201–203 台 · 共 203 台") ||
		!strings.Contains(fragment, "Camera 204") || strings.Contains(fragment, `rel="next"`) {
		t.Fatal("an out-of-range page did not show the last page of a filtered list")
	}

	fragment = get("/devices?q=no-match&page=999&page_size=2000", true)
	if !strings.Contains(fragment, `hx-get="/devices?q=no-match"`) ||
		!strings.Contains(fragment, `value="50" selected`) ||
		!strings.Contains(fragment, "没有匹配的设备") || strings.Contains(fragment, `name="page"`) {
		t.Fatal("empty list did not reset the page and fall back to the default page size")
	}
}
