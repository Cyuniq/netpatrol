package web

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"netpatrol/internal/discovery"
	"netpatrol/internal/store"
)

func TestConfiguredNoAuthSources(t *testing.T) {
	source := netip.MustParsePrefix("203.0.113.2/32")
	handler, err := Handler(nil, "admin-test", nil, nil, nil, true, source)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		peer, host, forwarded string
		auth                  bool
		status                int
	}{
		{"203.0.113.2:40200", "example.test", "", false, http.StatusSeeOther},
		{"203.0.113.3:40200", "example.test", "", false, http.StatusUnauthorized},
		{"203.0.113.3:40200", "example.test", "", true, http.StatusSeeOther},
		{"203.0.113.3:40200", "example.test", "203.0.113.2", false, http.StatusUnauthorized},
		{"127.0.0.1:40200", "127.0.0.1:8080", "", false, http.StatusSeeOther},
		{"127.0.0.1:40200", "example.test", "203.0.113.2", false, http.StatusUnauthorized},
	}
	for _, test := range tests {
		req := httptest.NewRequest(http.MethodGet, "http://"+test.host+"/", nil)
		req.RemoteAddr = test.peer
		if test.forwarded != "" {
			req.Header.Set("X-Forwarded-For", test.forwarded)
		}
		if test.auth {
			req.SetBasicAuth("admin", "admin-test")
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != test.status {
			t.Errorf("%+v: status %d, want %d", test, w.Code, test.status)
		}
	}

	handler, err = Handler(nil, "", nil, nil, nil, true, source)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "http://example.test/", nil)
	req.RemoteAddr = "203.0.113.3:40200"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("unlisted source without a password fallback: status %d", w.Code)
	}
}

func TestNoAuthSourceCanPreviewManuallySelectedNetwork(t *testing.T) {
	ctx := t.Context()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "discovery-access.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	manager, err := discovery.New(ctx, db, nil, "", 1024, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	host := "http://example.test"
	source := netip.MustParsePrefix("203.0.113.2/32")
	for _, test := range []struct {
		sources []netip.Prefix
		status  int
	}{
		{nil, http.StatusUnprocessableEntity},
		{[]netip.Prefix{source}, http.StatusOK},
	} {
		handler, err := Handler(db, "", nil, manager, nil, true, test.sources...)
		if err != nil {
			t.Fatal(err)
		}
		get := httptest.NewRequest(http.MethodGet, host+"/discovery", nil)
		get.RemoteAddr = "203.0.113.2:40200"
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, get)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `name="prefix"`) {
			t.Fatalf("discovery form missing without startup allowlist: status %d", w.Code)
		}
		form := url.Values{"prefix": {"198.51.100.0/29"}, "ports": {"554"}}
		post := httptest.NewRequest(http.MethodPost, host+"/discovery/preview", strings.NewReader(form.Encode()))
		post.RemoteAddr = get.RemoteAddr
		post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w = httptest.NewRecorder()
		handler.ServeHTTP(w, post)
		if w.Code != test.status {
			t.Fatalf("sources %v: preview status %d, want %d", test.sources, w.Code, test.status)
		}
	}
}
