package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"netpatrol/internal/discovery"
	"netpatrol/internal/monitor"
	"netpatrol/internal/store"
)

type discoveryTestProber func(context.Context, monitor.Target) monitor.Round

func (p discoveryTestProber) Probe(ctx context.Context, target monitor.Target) monitor.Round {
	return p(ctx, target)
}

func (p discoveryTestProber) ProbeWithSettings(ctx context.Context, target monitor.Target, _ monitor.Settings) monitor.Round {
	return p(ctx, target)
}

func (p discoveryTestProber) ProbeDiscovery(ctx context.Context, ip netip.Addr, ports []uint16, _ monitor.Settings) monitor.DiscoveryProbe {
	result := monitor.DiscoveryProbe{IP: ip}
	for _, port := range ports {
		round := p(ctx, monitor.Target{IP: ip, Port: port})
		result.Ping = round.Ping
		result.Ports = append(result.Ports, monitor.PortProbe{Port: port, Result: round.TCP})
	}
	return result
}

func TestDiscoveryHTTPWorkflow(t *testing.T) {
	ctx := t.Context()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "discover.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	area, err := db.SaveArea(ctx, 0, "Test area", "", "192.0.2.0/24, 198.51.100.0/24")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveDevice(ctx, 0, store.DeviceInput{
		Code: "EXISTING", Name: "Original name", AreaID: area, Address: "192.0.2.5", Port: 8554,
	}); err != nil {
		t.Fatal(err)
	}
	handle := make(chan struct{})
	prober := discoveryTestProber(func(ctx context.Context, target monitor.Target) monitor.Round {
		select {
		case <-ctx.Done():
		case <-handle:
		}
		ping, tcp := monitor.Timeout, monitor.Refused
		switch target.IP.As4()[3] {
		case 1, 5:
			ping, tcp = monitor.Passed, monitor.Passed
		case 2:
			ping = monitor.Passed
		case 3:
			ping, tcp = monitor.LocalError, monitor.Passed
		case 6:
			ping = monitor.LocalError
		}
		if target.Port == 445 && target.IP.As4()[3] != 1 {
			tcp = monitor.Refused
		}
		now := time.Now()
		return monitor.Round{Target: target, CompletedAt: now,
			Ping: monitor.Result{Outcome: ping, CompletedAt: now}, TCP: monitor.Result{Outcome: tcp, CompletedAt: now}}
	})
	manager, err := discovery.New(ctx, db, prober, "192.0.2.0/24", 254, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	for prefix, count := range map[string]int{"192.0.2.1/24": 254, "192.0.2.0/31": 2, "192.0.2.1/32": 1} {
		plan, err := manager.Preview(prefix, "554, 445 554", discovery.Access{})
		if err != nil || plan.Total != count {
			t.Fatalf("%s preview = %+v, %v", prefix, plan, err)
		}
	}
	if _, err := manager.Preview("198.51.100.0/29", "554", discovery.Access{}); err == nil {
		t.Fatal("accepted a network outside the configured allowlist")
	}
	if _, err := manager.Preview("198.51.100.0/29", "554", discovery.Access{Trusted: true}); err != nil {
		t.Fatalf("trusted client could not select a different IPv4 subnet: %v", err)
	}
	if _, err := manager.Preview("192.0.2.0/23", "554", discovery.Access{}); err == nil {
		t.Fatal("accepted a network exceeding the configured limit")
	}

	rest, err := Handler(db, "test", nil, manager, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	request := func(path string, form url.Values, fragment bool) *httptest.ResponseRecorder {
		t.Helper()
		method := http.MethodGet
		if form != nil {
			method = http.MethodPost
		}
		req := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		req.SetBasicAuth("admin", "test")
		if form != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		if fragment {
			req.Header.Set("HX-Request", "true")
		}
		rec := httptest.NewRecorder()
		rest.ServeHTTP(rec, req)
		return rec
	}
	form := url.Values{"prefix": {"192.0.2.3/29"}, "ports": {"554, 445 554"}}
	preview := request("/discovery/preview", form, false)
	if preview.Code != 200 || !strings.Contains(preview.Body.String(), "192.0.2.0/29") ||
		!strings.Contains(preview.Body.String(), "预计探测 6 个地址") || !strings.Contains(preview.Body.String(), "554, 445") {
		t.Fatalf("preview: %d %s", preview.Code, preview.Body.String())
	}
	start := request("/discovery", form, false)
	location := start.Header().Get("Location")
	if start.Code != 303 || location != "/discovery/1" {
		t.Fatalf("start: %d %q", start.Code, location)
	}
	if duplicate := request("/discovery", form, false); duplicate.Code != 409 {
		t.Fatalf("parallel discovery: %d", duplicate.Code)
	}
	progress := request(location, nil, true)
	if progress.Code != 200 || !strings.Contains(progress.Body.String(), `hx-trigger="every 2s"`) ||
		strings.Contains(progress.Body.String(), `data-discovery-selection`) {
		t.Fatalf("running job: %d %s", progress.Code, progress.Body.String())
	}
	close(handle)
	deadline := time.Now().Add(5 * time.Second)
	var job store.DiscoveryJob
	for time.Now().Before(deadline) {
		job, _, err = db.Discovery(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		if job.State == "done" {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if job.Completed != 6 || job.CandidateCount != 4 || job.LocalErrors != 2 || job.State != "done" {
		t.Fatalf("completed job = %+v", job)
	}
	result := request(location, nil, true)
	if result.Code != 200 || !strings.Contains(result.Body.String(), "结果不完整") ||
		!strings.Contains(result.Body.String(), "连接拒绝") || !strings.Contains(result.Body.String(), "已添加") ||
		strings.Contains(result.Body.String(), `hx-trigger`) {
		t.Fatalf("result: %d %s", result.Code, result.Body.String())
	}
	invalidSelection := request(location+"/confirm", url.Values{
		"area": {"1"}, "selected": {"192.0.2.1", "192.0.2.4"}, "port.192.0.2.1": {"445"},
	}, false)
	if invalidSelection.Code != 422 {
		t.Fatalf("accepted an undiscovered address: %d", invalidSelection.Code)
	}
	if devices, _ := db.Devices(ctx); len(devices) != 1 {
		t.Fatal("invalid request wrote part of the selection")
	}
	badPort := request(location+"/confirm", url.Values{
		"area": {"1"}, "selected": {"192.0.2.1"}, "port.192.0.2.1": {"3389"},
	}, false)
	if badPort.Code != 422 || !strings.Contains(badPort.Body.String(), "必须为本次探测的端口之一") {
		t.Fatalf("accepted an untested port: %d", badPort.Code)
	}
	added := url.Values{"area": {"1"}, "selected": {"192.0.2.1", "192.0.2.5"}, "name.192.0.2.1": {"Edited name"}, "port.192.0.2.1": {"445"}}
	for i, message := range []string{"新增 1 台，已存在 1 台", "新增 0 台，已存在 2 台"} {
		response := request(location+"/confirm", added, false)
		if response.Code != 200 || !strings.Contains(response.Body.String(), message) {
			t.Fatalf("confirm %d: %d %s", i, response.Code, response.Body.String())
		}
	}
	devices, err := db.Devices(ctx)
	if err != nil || len(devices) != 2 || devices[0].Name != "Original name" ||
		devices[1].Name != "Edited name" || devices[1].Target.Port != 445 {
		t.Fatalf("device list: %+v %v", devices, err)
	}
	if loss, err := db.Loss(ctx, devices[1].ID, time.Now()); err != nil || loss.Sent != 0 {
		t.Fatalf("formal statistics included discovery: %+v %v", loss, err)
	}
	id, err := db.CreateDiscovery(ctx, area, netip.MustParsePrefix("192.0.2.1/32"), []uint16{554}, 1)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := discovery.New(ctx, db, prober, "192.0.2.0/24", 254, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if result := request(fmt.Sprintf("/discovery/%d", id), nil, false); result.Code != 200 ||
		!strings.Contains(result.Body.String(), "服务重启，任务中断") {
		t.Fatalf("after restart: %d %s", result.Code, result.Body.String())
	}
}
