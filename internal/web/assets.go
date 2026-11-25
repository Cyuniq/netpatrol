package web

import (
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"netpatrol/internal/store"
)

type devicePage struct {
	ID     int64
	Input  store.DeviceInput
	View   store.DeviceView
	Areas  []store.Area
	Events []store.EventView
	Now    time.Time
	Error  string
}

func (app *application) loadDevice(r *http.Request, id int64) (devicePage, bool, error) {
	data := devicePage{ID: id, Now: time.Now(), Input: store.DeviceInput{Port: 554, Enabled: true}}
	ctx := r.Context()
	var err error
	data.Areas, err = app.store.Areas(ctx)
	if err != nil || id == 0 {
		return data, id == 0, err
	}
	rows, _, err := app.store.DeviceViews(ctx, store.DeviceFilter{DeviceID: id}, data.Now, 1, 0)
	if err != nil || len(rows) == 0 {
		return data, false, err
	}
	data.View = rows[0]
	d := rows[0]
	data.Input = store.DeviceInput{Code: d.Code, Name: d.Name, AreaID: d.AreaID,
		Address: d.Target.IP.String(), Port: int(d.Target.Port), Enabled: d.Enabled}
	data.Events, err = app.store.Events(ctx, 0, id, time.Time{}, time.Time{}, 25, 0)
	return data, true, err
}

func (app *application) newDevice(w http.ResponseWriter, r *http.Request) {
	data, _, err := app.loadDevice(r, 0)
	if err != nil {
		fail(w, err)
		return
	}
	data.Input.AreaID = positiveID(r.URL.Query().Get("area"))
	app.render(w, "device", http.StatusOK, data)
}

func (app *application) device(w http.ResponseWriter, r *http.Request) {
	id := positiveID(r.PathValue("id"))
	if id == 0 {
		http.NotFound(w, r)
		return
	}
	data, found, err := app.loadDevice(r, id)
	if err != nil {
		fail(w, err)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	app.render(w, "device", http.StatusOK, data)
}

func (app *application) saveDevice(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "表单无效", http.StatusBadRequest)
		return
	}
	id := positiveID(r.PathValue("id"))
	if r.PathValue("id") != "" && id == 0 {
		http.NotFound(w, r)
		return
	}
	port, _ := strconv.Atoi(r.PostForm.Get("port"))
	input := store.DeviceInput{Code: r.PostForm.Get("code"), Name: r.PostForm.Get("name"),
		AreaID: positiveID(r.PostForm.Get("area")), Address: r.PostForm.Get("address"),
		Port: port, Enabled: r.PostForm.Get("enabled") == "on"}
	updated, err := app.store.SaveDevice(r.Context(), id, input)
	if err == nil {
		http.Redirect(w, r, fmt.Sprintf("/devices/%d", updated), http.StatusSeeOther)
		return
	}
	data, found, loadErr := app.loadDevice(r, id)
	if loadErr != nil {
		fail(w, loadErr)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	if id != 0 {
		input.Code = data.Input.Code
	}
	data.Input, data.Error = input, err.Error()
	app.render(w, "device", http.StatusUnprocessableEntity, data)
}

type areaPage struct {
	areaCountsPanel
	Areas      []store.Area
	ID         int64
	EditID     int64
	Name, Path string
	Networks   []netip.Prefix
	Selected   map[string]bool
	Error      string
}

func (app *application) showAreas(w http.ResponseWriter, r *http.Request, status int, data areaPage) {
	var err error
	data.Areas, err = app.store.Areas(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	data.Stats, err = app.store.AreaStats(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	data.Networks, err = app.store.Networks(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	data.States = states
	app.render(w, "areas", status, data)
}

func (app *application) areas(w http.ResponseWriter, r *http.Request) {
	app.showAreas(w, r, http.StatusOK, areaPage{EditID: positiveID(r.URL.Query().Get("edit"))})
}

func (app *application) saveArea(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "表单无效", http.StatusBadRequest)
		return
	}
	data := areaPage{ID: positiveID(r.PostForm.Get("id")), Name: r.PostForm.Get("name"),
		Path: r.PostForm.Get("path")}
	networks := r.PostForm["networks"]
	if _, err := app.store.SaveArea(r.Context(), data.ID, data.Name, data.Path, strings.Join(networks, " ")); err != nil {
		data.Error = err.Error()
		data.Selected = make(map[string]bool)
		for _, prefix := range networks {
			data.Selected[prefix] = true
		}
		app.showAreas(w, r, http.StatusUnprocessableEntity, data)
		return
	}
	http.Redirect(w, r, "/areas", http.StatusSeeOther)
}

func (app *application) networks(w http.ResponseWriter, r *http.Request) {
	app.showNetworks(w, r, http.StatusOK, "", "")
}

func (app *application) showNetworks(w http.ResponseWriter, r *http.Request, status int, prefix, message string) {
	networks, err := app.store.Networks(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	app.render(w, "networks", status, struct {
		Networks        []netip.Prefix
		Prefix, Message string
	}{networks, prefix, message})
}

func (app *application) saveNetwork(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "表单无效", http.StatusBadRequest)
		return
	}
	prefix := r.PostForm.Get("prefix")
	if err := app.store.AddNetwork(r.Context(), prefix); err != nil {
		app.showNetworks(w, r, http.StatusUnprocessableEntity, prefix, err.Error())
		return
	}
	http.Redirect(w, r, "/networks", http.StatusSeeOther)
}

func (app *application) events(w http.ResponseWriter, r *http.Request) {
	const size = 50
	query := r.URL.Query()
	page := int(max(1, min(positiveID(query.Get("page")), 1_000_000)))
	area, device := positiveID(query.Get("area")), positiveID(query.Get("device"))
	from, err := parseDate(query.Get("from"))
	if err != nil {
		http.Error(w, "开始日期无效", http.StatusBadRequest)
		return
	}
	through, err := parseDate(query.Get("through"))
	if err != nil || !through.IsZero() && through.Before(from) {
		http.Error(w, "结束日期无效", http.StatusBadRequest)
		return
	}
	if !through.IsZero() {
		through = through.AddDate(0, 0, 1)
	}
	rows, err := app.store.Events(r.Context(), area, device, from, through, size+1, (page-1)*size)
	if err != nil {
		fail(w, err)
		return
	}
	areas, err := app.store.Areas(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	devices, err := app.store.Devices(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	pageURL := func(page int) string {
		params := make(url.Values)
		for _, key := range []string{"area", "device", "from", "through"} {
			if value := query.Get(key); value != "" {
				params.Set(key, value)
			}
		}
		params.Set("page", strconv.Itoa(page))
		return "/events?" + params.Encode()
	}
	data := struct {
		Rows                          []store.EventView
		Areas                         []store.Area
		Devices                       []store.Device
		Area, Device                  int64
		From, Through, Previous, Next string
	}{Rows: rows, Areas: areas, Devices: devices, Area: area, Device: device, From: query.Get("from"), Through: query.Get("through")}
	if len(rows) > size {
		data.Rows = rows[:size]
		data.Next = pageURL(page + 1)
	}
	if page > 1 {
		data.Previous = pageURL(page - 1)
	}
	app.render(w, "events", http.StatusOK, data)
}

func parseDate(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	return time.ParseInLocation("2006-01-02", value, time.Local)
}
