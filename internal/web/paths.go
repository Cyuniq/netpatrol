package web

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"netpatrol/internal/monitor"
	"netpatrol/internal/store"
)

type manualPath struct {
	Started time.Time
	Result  monitor.TraceResult
	Message string
}

type pathPage struct {
	Watch  store.PathWatch
	Paths  []store.PathWatch
	Manual *manualPath
	Form   *store.PathInput
	Error  string
}

func (p pathPage) Edit() store.PathInput {
	if p.Form != nil {
		return *p.Form
	}
	return p.Watch.PathInput
}

func (app *application) paths(w http.ResponseWriter, r *http.Request) {
	app.renderPaths(w, r, http.StatusOK, pathPage{Watch: store.PathWatch{
		PathInput: store.PathInput{IntervalSeconds: store.DefaultPathIntervalSeconds},
	}})
}

func (app *application) renderPaths(w http.ResponseWriter, r *http.Request, status int, page pathPage) {
	var err error
	page.Paths, err = app.store.ListPaths(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	app.render(w, "paths", status, page)
}

func (app *application) loadPath(w http.ResponseWriter, r *http.Request) (store.PathWatch, bool) {
	id := positiveID(r.PathValue("id"))
	if id == 0 {
		http.NotFound(w, r)
		return store.PathWatch{}, false
	}
	watch, err := app.store.PathWatch(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return watch, false
	}
	if err != nil {
		fail(w, err)
		return watch, false
	}
	return watch, true
}

func (app *application) path(w http.ResponseWriter, r *http.Request) {
	if watch, ok := app.loadPath(w, r); ok {
		app.render(w, "path", http.StatusOK, pathPage{Watch: watch})
	}
}

func (app *application) pathResults(w http.ResponseWriter, r *http.Request) {
	watch, ok := app.loadPath(w, r)
	if !ok {
		return
	}
	w.Header().Set("Vary", "HX-Request")
	if r.Header.Get("HX-Request") != "true" {
		http.Redirect(w, r, fmt.Sprintf("/paths/%d#path-watch-results", watch.ID), http.StatusSeeOther)
		return
	}
	app.render(w, "path-watch-results", http.StatusOK, watch)
}

func (app *application) savePath(w http.ResponseWriter, r *http.Request) {
	id := positiveID(r.PathValue("id"))
	if r.PathValue("id") != "" && id == 0 {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "表单无效", http.StatusBadRequest)
		return
	}
	interval, _ := strconv.Atoi(r.PostForm.Get("interval"))
	input := store.PathInput{Name: r.PostForm.Get("name"), Address: r.PostForm.Get("address"),
		Enabled: r.PostForm.Get("enabled") == "on", IntervalSeconds: interval}
	updated, err := app.store.SavePath(r.Context(), id, input, time.Now())
	if err == nil {
		http.Redirect(w, r, fmt.Sprintf("/paths/%d", updated), http.StatusSeeOther)
		return
	}
	if id == 0 {
		app.renderPaths(w, r, http.StatusUnprocessableEntity, pathPage{
			Watch: store.PathWatch{PathInput: input}, Error: err.Error(),
		})
		return
	}
	if watch, ok := app.loadPath(w, r); ok {
		app.render(w, "path", http.StatusUnprocessableEntity, pathPage{Watch: watch, Form: &input, Error: err.Error()})
	}
}

func (app *application) tracePath(w http.ResponseWriter, r *http.Request) {
	watch, ok := app.loadPath(w, r)
	if !ok {
		return
	}
	address, err := netip.ParseAddr(watch.Address)
	if err != nil {
		fail(w, err)
		return
	}
	page := pathPage{Watch: watch, Manual: app.runTrace(r, address)}
	if r.Context().Err() != nil {
		return
	}
	if !page.Manual.Started.IsZero() {
		err := app.store.RecordManualPathLatency(r.Context(), watch.ID, store.PathSample{
			Address: address, Started: page.Manual.Started, Completed: time.Now(),
			Result: page.Manual.Result, Message: page.Manual.Message,
		})
		if err != nil {
			fail(w, err)
			return
		}
		w.Header().Set("HX-Trigger", "path-latency-recorded")
	}
	w.Header().Set("Vary", "HX-Request")
	if r.Header.Get("HX-Request") == "true" {
		app.render(w, "manual-path", http.StatusOK, page)
		return
	}
	app.render(w, "path", http.StatusOK, page)
}

func (app *application) runTrace(r *http.Request, address netip.Addr) *manualPath {
	if !app.pathMu.TryLock() {
		return &manualPath{Message: "另一个路径探测正在运行，请稍后重试。"}
	}
	defer app.pathMu.Unlock()
	if time.Since(app.lastPath) < time.Second {
		return &manualPath{Message: "路径探测请求过于频繁，请稍后重试。"}
	}
	app.lastPath = time.Now()
	result, err := monitor.Trace(r.Context(), address)
	path := &manualPath{Started: app.lastPath, Result: result}
	if err != nil {
		path.Message = err.Error()
	}
	return path
}
