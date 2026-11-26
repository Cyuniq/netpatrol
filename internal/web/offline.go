package web

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"time"
)

func (app *application) exportOffline(w http.ResponseWriter, r *http.Request) {
	if !app.exportMu.TryLock() {
		http.Error(w, "已有离线包正在导出，请稍后重试", http.StatusConflict)
		return
	}
	defer app.exportMu.Unlock()
	// Large snapshots can outlast the ordinary page write timeout.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	dir, err := os.MkdirTemp("", "netpatrol-offline-*")
	if err != nil {
		fail(w, err)
		return
	}
	defer os.RemoveAll(dir)
	at := time.Now()
	dbPath := filepath.Join(dir, "netpatrol.db")
	if err := app.store.Backup(r.Context(), dbPath); err != nil {
		fail(w, err)
		return
	}
	output, err := os.CreateTemp(dir, "bundle-*.zip")
	if err != nil {
		fail(w, err)
		return
	}
	defer output.Close()
	if err := app.writeOfflineArchive(r.Context(), output, dbPath, at); err != nil {
		fail(w, err)
		return
	}
	name := "netpatrol-offline-" + at.UTC().Format("20060102T150405Z") + ".zip"
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	http.ServeContent(w, r, name, at, output)
}

// Stop compressing a large database if the requesting browser goes away.
type snapshotReader struct {
	ctx  context.Context
	file *os.File
}

func (r snapshotReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.file.Read(p)
}

func (app *application) writeOfflineArchive(ctx context.Context, output io.Writer, dbPath string, at time.Time) error {
	z := zip.NewWriter(output)
	page, err := z.Create("index.html")
	if err != nil {
		return err
	}
	if err := app.pages.ExecuteTemplate(page, "offline", at.UnixMilli()); err != nil {
		return err
	}
	// sql.js 1.17.0 includes the SQLite WASM module in its JS file so the
	// browser can open the archive on file:// without fetching a WASM URL.
	for _, source := range []string{
		"offline/viewer.js", "offline/queries.js", "offline/sql-wasm-inline.js", "offline/sql-js-LICENSE.txt",
		"static/app.css", "static/echarts.min.js", "static/latency.js", "static/echarts-LICENSE.txt", "static/echarts-NOTICE.txt",
	} {
		data, err := files.ReadFile(source)
		if err != nil {
			return err
		}
		file, err := z.Create("assets/" + path.Base(source))
		if err != nil {
			return err
		}
		if _, err := file.Write(data); err != nil {
			return err
		}
	}
	db, err := os.Open(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	file, err := z.Create("netpatrol.db")
	if err != nil {
		return err
	}
	if _, err := io.Copy(file, snapshotReader{ctx: ctx, file: db}); err != nil {
		return err
	}
	return z.Close()
}
