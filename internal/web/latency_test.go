package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"netpatrol/internal/monitor"
	"netpatrol/internal/store"
)

func TestRelativeLatencyWindow(t *testing.T) {
	now := time.Date(2026, 11, 25, 15, 0, 0, 0, time.FixedZone("CST", 8*3600))
	for _, minutes := range []int{1, 15, 60, 43200} {
		window, err := latencyWindow(url.Values{"minutes": {strconv.Itoa(minutes)}}, now)
		if err != nil || !window.To.Equal(now) || window.To.Sub(window.From) != time.Duration(minutes)*time.Minute {
			t.Errorf("%d minutes: window=%+v, err=%v", minutes, window, err)
		}
	}
	for _, query := range []string{"minutes=", "minutes=0", "minutes=-1", "minutes=43201", "minutes=1.5",
		"minutes=15&from=2026-11-25T06%3A00%3A00Z"} {
		input, parseErr := url.ParseQuery(query)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		if _, err := latencyWindow(input, now); err == nil {
			t.Errorf("accepted invalid range %q", query)
		}
	}
}

func TestDeviceLateBrowserClockAndFreshSample(t *testing.T) {
	ctx := t.Context()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	area, err := db.SaveArea(ctx, 0, "Lab", "", "")
	if err != nil {
		t.Fatal(err)
	}
	target, err := monitor.ParseTarget("192.0.2.1", 443)
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.SaveDevice(ctx, 0, store.DeviceInput{
		Name: "Fresh target", AreaID: area, Address: target.IP.String(), Port: int(target.Port), Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	round := monitor.Round{Target: target, StartedAt: now.Add(-11 * time.Second), CompletedAt: now.Add(-10 * time.Second),
		Ping: monitor.Result{Outcome: monitor.Passed, RTT: time.Millisecond},
		TCP:  monitor.Result{Outcome: monitor.Passed, RTT: 2 * time.Millisecond}}
	if err := db.Record(ctx, []store.Observation{{DeviceID: id, Version: 1, Round: round}}); err != nil {
		t.Fatal(err)
	}
	handler, err := Handler(db, "", nil, nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	request := func(query string) latencyResponse {
		t.Helper()
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/devices/"+strconv.FormatInt(id, 10)+"/latency?"+query, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("latency returned %d: %s", w.Code, w.Body.String())
		}
		var data latencyResponse
		if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
			t.Fatal(err)
		}
		return data
	}
	relative := request("minutes=15")
	if relative.To < now.UnixMilli() || relative.To-relative.From != int64(15*time.Minute/time.Millisecond) ||
		len(relative.Series) != 2 || len(relative.Series[0].Points) != 1 || len(relative.Series[1].Points) != 1 ||
		relative.Series[1].Points[0].RTT == nil {
		t.Fatalf("fresh sample missing from server-relative window: %+v", relative)
	}
	end := now.Add(-27 * time.Minute)
	fixed := request(url.Values{
		"from": {end.Add(-time.Hour).Format(time.RFC3339Nano)},
		"to":   {end.Format(time.RFC3339Nano)},
	}.Encode())
	if fixed.To != end.UnixMilli() || len(fixed.Series) != 2 || len(fixed.Series[1].Points) != 0 {
		t.Fatalf("fixed window changed: %+v", fixed)
	}
}
