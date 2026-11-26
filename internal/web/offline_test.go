package web

import (
	"archive/zip"
	"bytes"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"netpatrol/internal/store"
)

func TestOfflineBundleIncludesUnfilteredSnapshotAndBrowserAssets(t *testing.T) {
	ctx := t.Context()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	area, err := db.SaveArea(ctx, 0, "测试区域", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, addr := range []string{"192.0.2.1", "192.0.2.2"} {
		if _, err := db.SaveDevice(ctx, 0, store.DeviceInput{Name: addr, Address: addr, Port: 554, AreaID: area}); err != nil {
			t.Fatal(err)
		}
	}
	handler, err := Handler(db, "secret", nil, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/exports/offline?q=192.0.2.1", nil)
	denied := httptest.NewRecorder()
	handler.ServeHTTP(denied, req)
	if denied.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated export: %d", denied.Code)
	}
	req.SetBasicAuth("admin", "secret")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != http.StatusOK || res.Header().Get("Content-Type") != "application/zip" {
		t.Fatalf("download failed: %d %s", res.Code, res.Body.String())
	}
	z, err := zip.NewReader(bytes.NewReader(res.Body.Bytes()), int64(res.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	page, err := z.Open("index.html")
	if err != nil {
		t.Fatal(err)
	}
	defer page.Close()
	html, err := io.ReadAll(page)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"assets/app.css", "assets/sql-wasm-inline.js", "assets/echarts.min.js", "assets/latency.js", "assets/queries.js", "assets/viewer.js"} {
		if _, err := fs.Stat(z, path); err != nil || !strings.Contains(string(html), path) {
			t.Fatalf("missing offline resource %s", path)
		}
	}
	entry, err := z.Open("netpatrol.db")
	if err != nil {
		t.Fatal(err)
	}
	defer entry.Close()
	raw, err := io.ReadAll(entry)
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "copy.db")
	if err := os.WriteFile(dest, raw, 0600); err != nil {
		t.Fatal(err)
	}
	copyDB, err := store.Open(ctx, dest)
	if err != nil {
		t.Fatal(err)
	}
	defer copyDB.Close()
	_, count, err := copyDB.DeviceViews(ctx, store.DeviceFilter{}, time.Now(), 10, 0)
	if err != nil || count != 2 {
		t.Fatalf("exported devices: %d, err=%v", count, err)
	}
}
