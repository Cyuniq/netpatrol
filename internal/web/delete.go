package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"netpatrol/internal/store"
)

type deletionPage struct {
	Kind, Name, Details, Consequence    string
	Submit, Back, BlockingLink, Blocked string
}

func (app *application) removeResource(w http.ResponseWriter, r *http.Request, resource string) {
	id := positiveID(r.PathValue("id"))
	if id == 0 {
		http.NotFound(w, r)
		return
	}
	page, remove, err := app.deletionDetails(r.Context(), resource, id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	if r.Method == http.MethodGet {
		app.render(w, "delete", http.StatusOK, page)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1024)
	if err := r.ParseForm(); err != nil || r.PostForm.Get("confirm") != "yes" {
		http.Error(w, "请在确认页面提交删除", http.StatusBadRequest)
		return
	}
	if page.Blocked == "" {
		err = remove(r.Context(), id)
		switch {
		case err == nil:
			http.Redirect(w, r, "/"+resource, http.StatusSeeOther)
			return
		case errors.Is(err, sql.ErrNoRows):
			http.NotFound(w, r)
			return
		case errors.Is(err, store.ErrAreaInUse):
			page.Blocked = err.Error()
			page.BlockingLink = fmt.Sprintf("/devices?area=%d", id)
		default:
			fail(w, err)
			return
		}
	}
	app.render(w, "delete", http.StatusConflict, page)
}

func (app *application) deletionDetails(ctx context.Context, resource string, id int64) (deletionPage, func(context.Context, int64) error, error) {
	page := deletionPage{Submit: fmt.Sprintf("/%s/%d/delete", resource, id)}
	switch resource {
	case "devices":
		page.Kind, page.Back = "设备", fmt.Sprintf("/devices/%d", id)
		page.Consequence = "该设备的最新探测结果、丢包统计、状态记录和延迟历史会一并删除。"
		rows, _, err := app.store.DeviceViews(ctx, store.DeviceFilter{DeviceID: id}, time.Now(), 1, 0)
		if err == nil {
			if len(rows) == 0 {
				err = sql.ErrNoRows
			} else {
				page.Name, page.Details = rows[0].Name, rows[0].Target.IP.String()
			}
		}
		return page, app.store.DeleteDevice, err
	case "paths":
		page.Kind, page.Back = "路径目标", fmt.Sprintf("/paths/%d", id)
		page.Consequence = "该目标的持续检测会停止，最近结果、路径变化记录和延迟历史会一并删除。"
		path, err := app.store.PathWatch(ctx, id)
		page.Name, page.Details = path.Name, path.Address
		return page, app.store.DeletePath, err
	case "areas":
		page.Kind, page.Back = "区域", "/areas"
		page.Consequence = "该区域的网段关联会解除，已登记的网段和设备状态历史仍可查看。"
		name, devices, err := app.store.AreaDeleteInfo(ctx, id)
		page.Name = name
		if devices > 0 {
			page.Blocked = fmt.Sprintf("区域内还有 %d 台设备，请先调整设备归属。", devices)
			page.BlockingLink = fmt.Sprintf("/devices?area=%d", id)
		}
		return page, app.store.DeleteArea, err
	default:
		return page, nil, sql.ErrNoRows
	}
}
