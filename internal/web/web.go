package web

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"netpatrol/internal/discovery"
	"netpatrol/internal/monitor"
	"netpatrol/internal/store"
	"netpatrol/internal/telemetry"
)

//go:embed templates/*.html static/*
var files embed.FS

var states = []monitor.State{monitor.Healthy, monitor.Partial, monitor.Down, monitor.Unknown, monitor.Disabled}

type application struct {
	store     *store.Store
	pages     *template.Template
	previews  previewCache
	discovery *discovery.Manager
	pathMu    sync.Mutex
	lastPath  time.Time
}

func Handler(db *store.Store, password string, probeErrors []string, discover *discovery.Manager, metrics *telemetry.Metrics, noAuth bool, noAuthSources ...netip.Prefix) (http.Handler, error) {
	if !noAuth && password == "" {
		return nil, errors.New("请设置 NETPATROL_ADMIN_PASSWORD")
	}
	pages, err := template.New("").Funcs(template.FuncMap{
		"state": stateLabel, "result": resultLabel, "when": formatTime, "duration": formatDuration,
		"age": stateAge, "loss": lossLabel,
		"nowMillis": func() int64 { return time.Now().UnixMilli() },
		"rtt":       func(d time.Duration) string { return fmt.Sprintf("%.1f ms", float64(d)/float64(time.Millisecond)) },
		"linked":    func(networks []netip.Prefix, prefix netip.Prefix) bool { return slices.Contains(networks, prefix) },
		"trend": func(points []store.ProbeTrendMinute) (string, error) {
			encoded, err := json.Marshal(points)
			return string(encoded), err
		},
	}).ParseFS(files, "templates/*.html")
	if err != nil {
		return nil, err
	}
	app := &application{store: db, pages: pages, discovery: discover}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/devices", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /devices", func(w http.ResponseWriter, r *http.Request) {
		if metrics != nil {
			start := time.Now()
			defer func() { metrics.PageLatency(time.Since(start)) }()
		}
		app.devices(w, r, probeErrors)
	})
	if metrics != nil {
		mux.HandleFunc("GET /diagnostics.json", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(metrics.Snapshot(time.Now()))
		})
	}
	mux.HandleFunc("GET /devices/new", app.newDevice)
	mux.HandleFunc("GET /imports/csv", app.showImport)
	mux.HandleFunc("GET /static/template.csv", downloadAssetTemplate)
	mux.HandleFunc("POST /imports/csv/preview", app.previewImport)
	mux.HandleFunc("POST /imports/csv/confirm", app.confirmImport)
	mux.HandleFunc("GET /discovery", app.showDiscovery)
	mux.HandleFunc("POST /discovery/preview", app.previewDiscovery)
	mux.HandleFunc("POST /discovery", app.startDiscovery)
	mux.HandleFunc("GET /discovery/{id}", app.discoveryJob)
	mux.HandleFunc("POST /discovery/{id}/confirm", app.confirmDiscovery)
	mux.HandleFunc("GET /exports/{kind}", app.exportDevices)
	mux.HandleFunc("POST /devices", app.saveDevice)
	mux.HandleFunc("GET /devices/{id}", app.device)
	mux.HandleFunc("GET /devices/{id}/latency", app.latency(false))
	mux.HandleFunc("POST /devices/{id}", app.saveDevice)
	mux.HandleFunc("GET /paths", app.paths)
	mux.HandleFunc("POST /paths", app.savePath)
	mux.HandleFunc("GET /paths/{id}", app.path)
	mux.HandleFunc("POST /paths/{id}", app.savePath)
	mux.HandleFunc("POST /paths/{id}/probe", app.tracePath)
	mux.HandleFunc("GET /paths/{id}/results", app.pathResults)
	mux.HandleFunc("GET /paths/{id}/latency", app.latency(true))
	mux.HandleFunc("GET /areas", app.areas)
	mux.HandleFunc("POST /areas", app.saveArea)
	mux.HandleFunc("GET /networks", app.networks)
	mux.HandleFunc("POST /networks", app.saveNetwork)
	mux.HandleFunc("GET /events", app.events)
	mux.HandleFunc("GET /settings", app.settings)
	mux.HandleFunc("POST /settings", app.saveSettings)
	for _, resource := range []string{"devices", "paths", "areas"} {
		for _, method := range []string{"GET", "POST"} {
			mux.HandleFunc(method+" /"+resource+"/{id}/delete", func(w http.ResponseWriter, r *http.Request) {
				app.removeResource(w, r, resource)
			})
		}
	}
	assets, err := fs.Sub(files, "static")
	if err != nil {
		return nil, err
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(assets)))

	protected := http.NewCrossOriginProtection().Handler(mux)
	digest := sha256.Sum256([]byte(password))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		access := directRequestAccess(r)
		listed := requestAllowsNoAuth(access, noAuthSources)
		if noAuth && (len(noAuthSources) == 0 || access.Local || listed) {
			access.Trusted = listed
		} else {
			if password == "" {
				http.Error(w, "此来源地址不在免登录名单内", http.StatusForbidden)
				return
			}
			user, pass, ok := r.BasicAuth()
			actual := sha256.Sum256([]byte(pass))
			if !ok || user != "admin" || subtle.ConstantTimeCompare(actual[:], digest[:]) != 1 {
				w.Header().Set("WWW-Authenticate", `Basic realm="NetPatrol", charset="UTF-8"`)
				http.Error(w, "需要管理员登录", http.StatusUnauthorized)
				return
			}
			access.Trusted = true
		}
		protected.ServeHTTP(w, withDiscoveryAccess(r, access))
	}), nil
}

func (app *application) render(w http.ResponseWriter, name string, status int, data any) {
	var result bytes.Buffer
	if err := app.pages.ExecuteTemplate(&result, name, data); err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	result.WriteTo(w)
}

func fail(w http.ResponseWriter, err error) {
	log.Printf("web: %v", err)
	http.Error(w, "暂时无法完成请求", http.StatusInternalServerError)
}

type areaCountsPanel struct {
	Stats    []store.AreaCounts
	States   []monitor.State
	TrendKey string
	PageSize int
}

func (p areaCountsPanel) AreaURL(id int64, state monitor.State) string {
	return deviceURL(store.DeviceFilter{AreaID: id, State: state}, 1, p.PageSize, p.TrendKey)
}

type deviceList struct {
	areaCountsPanel
	Areas                          []store.Area
	Rows                           []store.DeviceView
	Filter                         store.DeviceFilter
	Trend                          deviceTrendRange
	TrendRanges                    []deviceTrendRange
	Path                           string
	Page, Pages, Total             int
	PageSizes                      []int
	PageLinks                      []devicePageLink
	FirstRow, LastRow              int
	Now                            time.Time
	Previous, Next, Refresh, Clear string
	AssetExport, StatusExport      string
	ProbeErrors                    []string
}

func listFilter(query url.Values) store.DeviceFilter {
	filter := store.DeviceFilter{AreaID: positiveID(query.Get("area")), Keyword: strings.TrimSpace(query.Get("q"))}
	for _, state := range states {
		if string(state) == query.Get("state") {
			filter.State = state
		}
	}
	return filter
}

func deviceURL(filter store.DeviceFilter, page, size int, trendKey string) string {
	query := make(url.Values)
	if filter.AreaID > 0 {
		query.Set("area", strconv.FormatInt(filter.AreaID, 10))
	}
	if filter.State != "" {
		query.Set("state", string(filter.State))
	}
	if filter.Keyword != "" {
		query.Set("q", filter.Keyword)
	}
	if page > 1 {
		query.Set("page", strconv.Itoa(page))
	}
	if size > 0 && size != defaultDevicePageSize {
		query.Set("page_size", strconv.Itoa(size))
	}
	if trendKey != "" {
		query.Set("trend", trendKey)
	}
	if len(query) == 0 {
		return "/devices"
	}
	return "/devices?" + query.Encode()
}

func (app *application) devices(w http.ResponseWriter, r *http.Request, probeErrors []string) {
	ctx := r.Context()
	query := r.URL.Query()
	filter := listFilter(query)
	trend := selectedDeviceTrend(query.Get("trend"))
	trendKey := trend.Key
	if trendKey == defaultDeviceTrend.Key {
		trendKey = ""
	}
	size := selectedDevicePageSize(query.Get("page_size"))
	now := time.Now()
	page := int(max(1, min(positiveID(query.Get("page")), 1_000_000)))
	rows, count, err := app.store.DeviceList(ctx, filter, now, size, (page-1)*size, trend.Window)
	if err != nil {
		fail(w, err)
		return
	}
	stats, err := app.store.AreaStats(ctx)
	if err != nil {
		fail(w, err)
		return
	}
	pages := max(0, count-1)/size + 1
	page = min(page, pages)
	pageURL := func(n int) string {
		return deviceURL(filter, n, size, trendKey)
	}
	data := deviceList{areaCountsPanel: areaCountsPanel{Stats: stats, States: states, TrendKey: trendKey, PageSize: size},
		Filter: filter, Rows: rows, Trend: trend, TrendRanges: deviceTrendRanges, Page: page, Pages: pages,
		PageSizes: devicePageSizes, PageLinks: devicePageLinks(page, pages, pageURL), Total: count,
		Now: now, Refresh: pageURL(page), Clear: deviceURL(store.DeviceFilter{}, 1, size, trendKey), ProbeErrors: probeErrors}
	if count > 0 {
		data.FirstRow = (page-1)*size + 1
		data.LastRow = data.FirstRow + len(rows) - 1
	}
	exportQuery := strings.TrimPrefix(deviceURL(filter, 1, 0, ""), "/devices")
	data.AssetExport = "/exports/assets.csv" + exportQuery
	data.StatusExport = "/exports/status.csv" + exportQuery
	if page > 1 {
		data.Previous = pageURL(page - 1)
	}
	if page < pages {
		data.Next = pageURL(page + 1)
	}
	w.Header().Set("Vary", "HX-Request")
	if r.Header.Get("HX-Request") == "true" {
		app.render(w, "device-results", http.StatusOK, data)
		return
	}
	data.Areas, err = app.store.Areas(ctx)
	if err != nil {
		fail(w, err)
		return
	}
	for _, area := range data.Areas {
		if area.ID == filter.AreaID {
			data.Path = area.Path
		}
	}
	app.render(w, "devices", http.StatusOK, data)
}

func positiveID(raw string) int64 {
	value, _ := strconv.ParseInt(raw, 10, 64)
	return max(value, 0)
}

func stateLabel(state monitor.State) string {
	switch state {
	case monitor.Healthy:
		return "正常"
	case monitor.Partial:
		return "部分异常"
	case monitor.Down:
		return "不可达"
	case monitor.Disabled:
		return "停用"
	case monitor.Unknown:
		return "未知"
	default:
		return "—"
	}
}

func resultLabel(r monitor.Result) string {
	switch r.Outcome {
	case monitor.Passed:
		return fmt.Sprintf("通过 · %.1f ms", float64(r.RTT)/float64(time.Millisecond))
	case monitor.Timeout:
		return "超时"
	case monitor.Refused:
		return "连接拒绝"
	case monitor.Unreachable:
		return "不可达"
	case monitor.LocalError:
		return "本机错误"
	default:
		return "—"
	}
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Local().Format("2006-01-02 15:04:05 -07:00")
}

func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "—"
	}
	return d.Round(time.Second).String()
}

func stateAge(now, since time.Time) string {
	if since.IsZero() {
		return "—"
	}
	return max(0, now.Sub(since)).Truncate(time.Second).String()
}

func lossLabel(loss monitor.Loss) string {
	if percentage, ok := loss.Percent(); ok {
		return fmt.Sprintf("丢 %d / 发 %d · %.2f%%", loss.Lost(), loss.Sent, percentage)
	}
	return "无数据"
}
