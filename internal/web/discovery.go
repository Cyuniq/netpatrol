package web

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"sort"
	"strconv"

	"netpatrol/internal/discovery"
	"netpatrol/internal/monitor"
	"netpatrol/internal/store"
)

type discoveryPage struct {
	Enabled         bool
	Areas           []store.Area
	AreaID          int64
	Networks        []netip.Prefix
	Prefix, Message string
	Ports           string
	Plan            discovery.Plan
}

func (app *application) showDiscovery(w http.ResponseWriter, r *http.Request) {
	app.renderDiscovery(w, r, http.StatusOK, discoveryPage{AreaID: positiveID(r.URL.Query().Get("area")), Ports: "554"})
}

func (app *application) renderDiscovery(w http.ResponseWriter, r *http.Request, status int, data discoveryPage) {
	var err error
	data.Areas, err = app.store.Areas(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	data.Enabled = app.discovery.Enabled()
	var networks []netip.Prefix
	if data.AreaID == 0 {
		networks, err = app.store.Networks(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
	}
	for _, area := range data.Areas {
		if area.ID == data.AreaID {
			networks = area.Networks
			break
		}
	}
	for _, prefix := range networks {
		if prefix.Addr().Is4() {
			data.Networks = append(data.Networks, prefix)
		}
	}
	app.render(w, "discovery", status, data)
}

func (app *application) discoveryForm(w http.ResponseWriter, r *http.Request) (discoveryPage, bool) {
	if !app.discovery.Enabled() {
		http.Error(w, "网段发现未启用", http.StatusForbidden)
		return discoveryPage{}, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "表单无效", http.StatusBadRequest)
		return discoveryPage{}, false
	}
	ports := r.PostForm.Get("ports")
	if ports == "" {
		ports = r.PostForm.Get("port")
	}
	return discoveryPage{AreaID: positiveID(r.PostForm.Get("area")), Prefix: r.PostForm.Get("prefix"), Ports: ports}, true
}

func (app *application) previewDiscovery(w http.ResponseWriter, r *http.Request) {
	data, ok := app.discoveryForm(w, r)
	if !ok {
		return
	}
	plan, err := app.discovery.Preview(data.Prefix, data.Ports, discoveryAccess(r))
	if err != nil {
		data.Message = err.Error()
		app.renderDiscovery(w, r, http.StatusUnprocessableEntity, data)
		return
	}
	data.Prefix = plan.Prefix.String()
	data.Ports = plan.PortText
	data.Plan = plan
	app.renderDiscovery(w, r, http.StatusOK, data)
}

func (app *application) startDiscovery(w http.ResponseWriter, r *http.Request) {
	data, ok := app.discoveryForm(w, r)
	if !ok {
		return
	}
	id, err := app.discovery.Start(r.Context(), data.AreaID, data.Prefix, data.Ports, discoveryAccess(r))
	if err != nil {
		status := http.StatusUnprocessableEntity
		if errors.Is(err, store.ErrDiscoveryBusy) {
			status = http.StatusConflict
		}
		data.Message = err.Error()
		app.renderDiscovery(w, r, status, data)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/discovery/%d", id), http.StatusSeeOther)
}

type candidateRow struct {
	store.Candidate
	State         string
	Both          bool
	PreferredPort uint16
}

type discoveryResult struct {
	Job     store.DiscoveryJob
	Rows    []candidateRow
	Areas   []store.Area
	Message string
}

func (app *application) discoveryJob(w http.ResponseWriter, r *http.Request) {
	app.renderDiscoveryJob(w, r, http.StatusOK, "")
}

func (app *application) renderDiscoveryJob(w http.ResponseWriter, r *http.Request, status int, message string) {
	job, candidates, err := app.store.Discovery(r.Context(), positiveID(r.PathValue("id")))
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Probe.IP.Compare(candidates[j].Probe.IP) < 0
	})
	data := discoveryResult{Job: job, Message: message}
	if job.State != "running" {
		data.Areas, err = app.store.Areas(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
	}
	for _, row := range candidates {
		var preferred uint16
		var open bool
		incomplete := row.Probe.Ping.Outcome == monitor.LocalError
		for _, port := range row.Probe.Ports {
			incomplete = incomplete || port.Result.Outcome == monitor.LocalError
			if preferred == 0 || !open && port.Result.Outcome == monitor.Passed {
				preferred = port.Port
			}
			open = open || port.Result.Outcome == monitor.Passed
		}
		both := row.Probe.Ping.Outcome == monitor.Passed && open
		state := "部分异常"
		switch {
		case both:
			state = "健康"
		case incomplete:
			state = "结果不完整"
		}
		data.Rows = append(data.Rows, candidateRow{Candidate: row, State: state, Both: both, PreferredPort: preferred})
	}
	w.Header().Set("Vary", "HX-Request")
	if r.Header.Get("HX-Request") == "true" {
		app.render(w, "discovery-result", status, data)
		return
	}
	app.render(w, "discovery-job", status, data)
}

func (app *application) confirmDiscovery(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "表单无效", http.StatusBadRequest)
		return
	}
	addresses := r.PostForm["selected"]
	if len(addresses) == 0 || len(addresses) > 65536 {
		app.renderDiscoveryJob(w, r, http.StatusUnprocessableEntity, "请勾选要添加的候选设备。")
		return
	}
	var selected []store.DiscoverySelection
	seen := make(map[string]bool)
	for _, address := range addresses {
		if !seen[address] {
			raw := r.PostForm.Get("port." + address)
			port, err := strconv.ParseUint(raw, 10, 16)
			if raw != "" && (err != nil || port == 0) {
				app.renderDiscoveryJob(w, r, http.StatusUnprocessableEntity, "纳管端口无效。")
				return
			}
			selected = append(selected, store.DiscoverySelection{Address: address, Name: r.PostForm.Get("name." + address), Port: uint16(port)})
			seen[address] = true
		}
	}
	added, existing, err := app.store.AddDiscovered(r.Context(), positiveID(r.PathValue("id")), positiveID(r.PostForm.Get("area")), selected)
	if err != nil {
		app.renderDiscoveryJob(w, r, http.StatusUnprocessableEntity, err.Error())
		return
	}
	app.renderDiscoveryJob(w, r, http.StatusOK, fmt.Sprintf("新增 %d 台，已存在 %d 台。", added, existing))
}
