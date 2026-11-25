package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"netpatrol/internal/store"
)

func TestPathTargetCanBeCreatedWithoutDeviceOrArea(t *testing.T) {
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "paths.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	handler, err := Handler(db, "", nil, nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	send := func(method, path string, form url.Values) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp := httptest.NewRecorder()
		handler.ServeHTTP(resp, req)
		return resp
	}
	initial := send(http.MethodGet, "/paths", nil)
	if initial.Code != 200 || !strings.Contains(initial.Body.String(), "添加路径目标") {
		t.Fatalf("initial path page = %d %s", initial.Code, initial.Body.String())
	}
	created := send(http.MethodPost, "/paths", url.Values{"name": {"Gateway"}, "address": {"192.0.2.1"}, "interval": {"60"}, "enabled": {"on"}})
	if created.Code != 303 || created.Header().Get("Location") != "/paths/1" {
		t.Fatalf("create path = %d %s", created.Code, created.Body.String())
	}
	detail := send(http.MethodGet, "/paths/1", nil)
	if detail.Code != 200 || !strings.Contains(detail.Body.String(), `hx-get="/paths/1/results"`) ||
		!strings.Contains(detail.Body.String(), "立即检测") {
		t.Fatalf("path details = %d %s", detail.Code, detail.Body.String())
	}
}
