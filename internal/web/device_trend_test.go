package web

import (
	"encoding/json"
	"html"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"netpatrol/internal/monitor"
	"netpatrol/internal/store"
)

func TestDeviceListShowsSeparateProbeTrendsAndTreatsPingTimeoutAsNormal(t *testing.T) {
	ctx := t.Context()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "trends.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	areaID, err := db.SaveArea(ctx, 0, "Lab", "", "")
	if err != nil {
		t.Fatal(err)
	}
	target, err := monitor.ParseTarget("192.0.2.1", 443)
	if err != nil {
		t.Fatal(err)
	}
	deviceID, err := db.SaveDevice(ctx, 0, store.DeviceInput{
		Name: "Windows", Address: target.IP.String(), Port: int(target.Port), Enabled: true, AreaID: areaID,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Minute)
	var observations []store.Observation
	add := func(offset time.Duration, ping, tcp monitor.Outcome, tcpRTT time.Duration) {
		at := now.Add(offset)
		observations = append(observations, store.Observation{DeviceID: deviceID, Version: 1, Round: monitor.Round{
			Target: target, StartedAt: at, CompletedAt: at, EchoSentAt: at,
			Ping: monitor.Result{Outcome: ping, RTT: 11 * time.Millisecond},
			TCP:  monitor.Result{Outcome: tcp, RTT: tcpRTT},
		}})
	}
	add(-26*time.Hour, monitor.Timeout, monitor.Passed, 4*time.Millisecond)
	add(-31*time.Minute, monitor.Timeout, monitor.Timeout, 0)
	add(-5*time.Minute, monitor.Passed, monitor.Refused, 0)
	add(-4*time.Minute, monitor.Timeout, monitor.Timeout, 0)
	add(-3*time.Minute, monitor.Passed, monitor.Timeout, 0)
	add(-3*time.Minute+time.Second, monitor.Passed, monitor.Passed, 2*time.Millisecond)
	add(-3*time.Minute+2*time.Second, monitor.Timeout, monitor.Passed, 6*time.Millisecond)
	add(-time.Minute, monitor.Timeout, monitor.Passed, 5*time.Millisecond)
	if err := db.Record(ctx, observations); err != nil {
		t.Fatal(err)
	}
	handler, err := Handler(db, "", nil, nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	pattern := regexp.MustCompile(`(?s)data-protocol="(ping|tcp)">.*?data-probe-trend="([^"]+)"`)
	wantTimeouts := map[string]int{"ping": 3, "tcp": 2}
	wantMixedMean := map[string]float64{"ping": 11, "tcp": 4}
	for _, fragment := range []bool{false, true} {
		req := httptest.NewRequest(http.MethodGet, "/devices?state=healthy", nil)
		if fragment {
			req.Header.Set("HX-Request", "true")
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		body := w.Body.String()
		if w.Code != 200 || !strings.Contains(body, `class="status healthy">正常</span>`) ||
			!strings.Contains(body, `/devices/1#latency-history`) {
			t.Fatalf("fragment=%v status=%d, device or trend link missing", fragment, w.Code)
		}
		matches := pattern.FindAllStringSubmatch(body, -1)
		if len(matches) != 2 || matches[0][1] != "ping" || matches[1][1] != "tcp" {
			t.Fatal("expected separate Ping and TCP trend cells")
		}
		for _, match := range matches {
			protocol := match[1]
			var points []store.ProbeTrendMinute
			if err := json.Unmarshal([]byte(html.UnescapeString(match[2])), &points); err != nil {
				t.Fatal(err)
			}
			var timeouts, successful, mixed int
			for _, point := range points {
				if point.Timeout {
					timeouts++
				}
				if point.RTTMillis != nil {
					successful++
					if point.Timeout {
						mixed++
						if *point.RTTMillis != wantMixedMean[protocol] {
							t.Errorf("%s mixed bucket mean = %v ms", protocol, *point.RTTMillis)
						}
					}
				}
			}
			if len(points) != 30 || timeouts != wantTimeouts[protocol] || successful != 2 || mixed != 1 {
				t.Fatalf("%s has %d points, %d timeout buckets, %d successful buckets and %d mixed buckets",
					protocol, len(points), timeouts, successful, mixed)
			}
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/devices?state=healthy&trend=7d", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, `value="7d" selected`) ||
		!strings.Contains(body, `hx-get="/devices?state=healthy&amp;trend=7d"`) ||
		!strings.Contains(body, `/devices?area=1&amp;state=healthy&amp;trend=7d`) ||
		!strings.Contains(body, "每点 4 小时") {
		t.Fatal("the page did not retain the selected long range in its controls and refresh links")
	}
	matches := pattern.FindAllStringSubmatch(body, -1)
	if len(matches) != 2 || matches[1][1] != "tcp" {
		t.Fatal("TCP history missing")
	}
	var coarse []store.ProbeTrendMinute
	if err := json.Unmarshal([]byte(html.UnescapeString(matches[1][2])), &coarse); err != nil {
		t.Fatal(err)
	}
	var buckets []store.ProbeTrendMinute
	for _, point := range coarse {
		if point.RTTMillis != nil || point.Timeout {
			buckets = append(buckets, point)
		}
	}
	if len(coarse) != 42 || len(buckets) != 2 || buckets[0].RTTMillis == nil || *buckets[0].RTTMillis != 4 ||
		buckets[0].Timeout || !buckets[1].Timeout || buckets[1].RTTMillis == nil ||
		math.Abs(*buckets[1].RTTMillis-13.0/3) > 0.000001 {
		t.Fatalf("long range failed to preserve older samples or aggregate timeout/RTT: %+v", buckets)
	}
}
