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

func TestEditAndDeleteDevicePathAndArea(t *testing.T) {
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "remove.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	router, err := Handler(db, "", nil, nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, endpoint string, form url.Values, status int) string {
		t.Helper()
		req := httptest.NewRequest(method, endpoint, strings.NewReader(form.Encode()))
		if method == http.MethodPost {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		res := httptest.NewRecorder()
		router.ServeHTTP(res, req)
		if res.Code != status {
			t.Fatalf("%s %s = %d, want %d: %s", method, endpoint, res.Code, status, res.Body.String())
		}
		return res.Body.String()
	}
	get := func(endpoint string, status int) string { return request(http.MethodGet, endpoint, nil, status) }
	post := func(endpoint string, form url.Values, status int) string {
		return request(http.MethodPost, endpoint, form, status)
	}
	contains := func(page string, want ...string) {
		t.Helper()
		for _, item := range want {
			if !strings.Contains(page, item) {
				t.Fatalf("page missing %q", item)
			}
		}
	}
	post("/areas", url.Values{"name": {"Old area"}}, 303)
	post("/areas", url.Values{"id": {"1"}, "name": {"Edited area"}, "path": {"Edited route"}}, 303)
	contains(get("/areas?edit=1", 200), `id="area-1" class="card panel area-edit" open`, "Edited route")
	post("/areas", url.Values{"name": {"New area"}}, 303)
	device := url.Values{"name": {"Old device"}, "address": {"192.0.2.1"}, "port": {"554"}, "area": {"1"}}
	post("/devices", device, 303)
	contains(get("/devices", 200), "/devices/1#edit-device", "/devices/1/delete")
	confirm := url.Values{"confirm": {"yes"}}
	contains(get("/areas/1/delete", 200), "区域内还有 1 台设备")
	post("/areas/1/delete", confirm, 409)
	device.Set("area", "2")
	device.Set("name", "Edited device")
	post("/devices/1", device, 303)
	contains(get("/devices/1", 200), "Edited device", "New area")
	post("/areas/1/delete", confirm, 303)
	get("/areas/1/delete", 404)

	path := url.Values{"name": {"Old path"}, "address": {"192.0.2.2"}, "interval": {"300"}}
	post("/paths", path, 303)
	path.Set("name", "Edited path")
	path.Set("interval", "600")
	post("/paths/1", path, 303)
	contains(get("/paths", 200), "Edited path", "600 秒", "/paths/1#edit-path", "/paths/1/delete")
	for _, resource := range []string{"paths", "devices"} {
		entry := "/" + resource + "/1"
		contains(get(entry+"/delete", 200), "确认删除")
		get(entry, 200)
		post(entry+"/delete", nil, 400)
		post(entry+"/delete", confirm, 303)
		get(entry, 404)
	}
}
