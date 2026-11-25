package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"netpatrol/internal/monitor"
	"netpatrol/internal/store"
)

func TestProbeSettingsPagePersistsAndValidatesChanges(t *testing.T) {
	ctx := t.Context()
	file := filepath.Join(t.TempDir(), "probe.db")
	db, err := store.Open(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	settings, err := db.Settings(ctx)
	if err != nil || settings != monitor.DefaultSettings() {
		t.Fatalf("default settings = %+v, error = %v", settings, err)
	}
	router, err := Handler(db, "", nil, nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	send := func(form url.Values) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/settings", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp := httptest.NewRecorder()
		router.ServeHTTP(resp, req)
		return resp
	}
	form := url.Values{
		"fast_interval": {"4"}, "slow_interval": {"30"}, "slow_after": {"40"},
		"ping_timeout": {"500"}, "tcp_timeout": {"1700"},
	}
	if result := send(form); result.Code != http.StatusSeeOther {
		t.Fatalf("save returned %d: %s", result.Code, result.Body.String())
	}
	form.Set("fast_interval", "1")
	if result := send(form); result.Code != http.StatusUnprocessableEntity || !strings.Contains(result.Body.String(), "必须长于") {
		t.Fatalf("invalid settings returned %d: %s", result.Code, result.Body.String())
	}
	reopened, err := store.Open(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.Settings(ctx)
	expected := monitor.Settings{FastInterval: 4 * time.Second, SlowInterval: 30 * time.Second, SlowAfter: 40 * time.Second,
		PingTimeout: 500 * time.Millisecond, TCPTimeout: 1700 * time.Millisecond}
	if err != nil || got != expected {
		t.Fatalf("reopened settings = %+v, error = %v", got, err)
	}
}
