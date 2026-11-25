package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"time"

	"netpatrol/internal/monitor"
	"netpatrol/internal/store"
)

type latencyValues struct {
	RTT      *[3]float64 `json:"rtt,omitempty"`
	Replies  int64       `json:"replies"`
	Timeouts int64       `json:"timeouts,omitempty"`
	Failures int64       `json:"failures,omitempty"`
	Errors   int64       `json:"errors,omitempty"`
}

func latencyDisplay(s store.LatencyStats) latencyValues {
	v := latencyValues{Replies: s.Replies, Timeouts: s.Timeouts, Failures: s.Failures, Errors: s.Errors}
	if s.Replies > 0 {
		const ms = float64(time.Millisecond)
		v.RTT = &[3]float64{float64(s.Min) / ms, float64(s.Total) / float64(s.Replies) / ms, float64(s.Max) / ms}
	}
	return v
}

type latencyPort struct {
	Port uint16 `json:"port"`
	latencyValues
}

type latencyPoint struct {
	At int64 `json:"at"`
	latencyValues
	OtherReplies int64         `json:"otherReplies,omitempty"`
	Checks       int64         `json:"checks,omitempty"`
	Unreached    int64         `json:"unreached,omitempty"`
	LocalChecks  int64         `json:"localChecks,omitempty"`
	TCPPorts     []latencyPort `json:"tcpPorts,omitempty"`
}

type latencySeries struct {
	Name   string         `json:"name"`
	Points []latencyPoint `json:"points"`
}

type latencyResponse struct {
	From   int64              `json:"from"`
	To     int64              `json:"to"`
	Step   int64              `json:"step"`
	Series []latencySeries    `json:"series"`
	Hops   []store.LatencyHop `json:"hops,omitempty"`
}

func latencyWindow(query url.Values, now time.Time) (store.LatencyWindow, error) {
	if query.Has("minutes") {
		minutes, err := strconv.Atoi(query.Get("minutes"))
		if err != nil || minutes < 1 || minutes > int(store.LatencyRetention/time.Minute) ||
			query.Has("from") || query.Has("to") {
			return store.LatencyWindow{}, errors.New("最近时长须在 1 分钟到 30 天之间")
		}
		return store.LatencyWindow{From: now.Add(-time.Duration(minutes) * time.Minute), To: now}, nil
	}
	fromText, toText := query.Get("from"), query.Get("to")
	if fromText == "" && toText == "" {
		return store.LatencyWindow{From: now.Add(-time.Hour), To: now}, nil
	}
	from, fromErr := time.Parse(time.RFC3339Nano, fromText)
	to, toErr := time.Parse(time.RFC3339Nano, toText)
	if fromErr != nil || toErr != nil || from.Unix() < 0 || to.Year() >= 2100 ||
		!from.Before(to) || to.Sub(from) > store.LatencyRetention {
		return store.LatencyWindow{}, errors.New("请选择有效的起止时间，单次查询最长 30 天")
	}
	return store.LatencyWindow{From: from, To: to}, nil
}

func latencyHop(query url.Values) (store.LatencyHop, error) {
	var selected store.LatencyHop
	hop, responder := query.Get("hop"), query.Get("responder")
	if hop == "" && responder == "" {
		return selected, nil
	}
	number, err := strconv.Atoi(hop)
	if err != nil || number < 1 || number > monitor.TraceMaxHops {
		return selected, errors.New("跳点无效")
	}
	selected.Number = number
	if responder != "" {
		selected.Address, err = netip.ParseAddr(responder)
		if err != nil {
			return selected, errors.New("回应地址无效")
		}
	}
	return selected, nil
}

func (app *application) latency(path bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := positiveID(r.PathValue("id"))
		if id == 0 {
			http.NotFound(w, r)
			return
		}
		now := time.Now()
		window, err := latencyWindow(r.URL.Query(), now)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		data := latencyResponse{From: window.From.UnixMilli(), To: window.To.UnixMilli()}
		var series []store.LatencySeries
		if path {
			hop, err := latencyHop(r.URL.Query())
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			var selected store.LatencySeries
			selected, data.Hops, data.Step, err = app.store.PathLatency(ctx, id, window, hop)
			if handleLatencyError(w, r, err) {
				return
			}
			series = []store.LatencySeries{selected}
		} else {
			series, data.Step, err = app.store.DeviceLatency(ctx, id, window, now)
			if handleLatencyError(w, r, err) {
				return
			}
		}
		for _, item := range series {
			curve := latencySeries{Name: item.Name, Points: make([]latencyPoint, 0, len(item.Points))}
			for _, p := range item.Points {
				point := latencyPoint{At: p.At * 1000, latencyValues: latencyDisplay(p.LatencyStats),
					OtherReplies: p.OtherReplies, Checks: p.Checks, Unreached: p.Unreached, LocalChecks: p.LocalChecks}
				for _, port := range p.TCPPorts {
					point.TCPPorts = append(point.TCPPorts, latencyPort{Port: port.Port, latencyValues: latencyDisplay(port.LatencyStats)})
				}
				curve.Points = append(curve.Points, point)
			}
			data.Series = append(data.Series, curve)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(data)
	}
}

func handleLatencyError(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, sql.ErrNoRows):
		http.NotFound(w, r)
	default:
		fail(w, err)
	}
	return true
}
