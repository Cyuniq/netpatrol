package web

import (
	"bytes"
	"crypto/rand"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"

	"netpatrol/internal/store"
)

const maxCSVBytes = 4 << 20
const maxCSVRows = 10000

type importPreview struct {
	created time.Time
	rows    []store.ImportRow
	entries []store.ImportEntry
	update  bool
}

type previewCache struct {
	mu      sync.Mutex
	pending map[string]importPreview
}

func (cache *previewCache) put(preview importPreview) string {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.pending == nil {
		cache.pending = make(map[string]importPreview)
	}
	var oldest string
	for token, entry := range cache.pending {
		if time.Since(entry.created) >= 20*time.Minute {
			delete(cache.pending, token)
		} else if oldest == "" || entry.created.Before(cache.pending[oldest].created) {
			oldest = token
		}
	}
	if len(cache.pending) >= 8 {
		delete(cache.pending, oldest)
	}
	token := rand.Text()
	preview.created = time.Now()
	cache.pending[token] = preview
	return token
}

func (cache *previewCache) take(token string) (importPreview, bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	preview, ok := cache.pending[token]
	delete(cache.pending, token)
	return preview, ok && time.Since(preview.created) < 20*time.Minute
}

type importPage struct {
	Entries                                     []store.ImportEntry
	Added, Updated, Skipped, Errors, Applicable int
	Update, Committed                           bool
	Token, Message                              string
}

func (app *application) showImport(w http.ResponseWriter, _ *http.Request) {
	app.render(w, "import", http.StatusOK, importPage{})
}

func downloadAssetTemplate(w http.ResponseWriter, _ *http.Request) {
	data, err := files.ReadFile("static/template.csv")
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="netpatrol-template.csv"`)
	w.Write(data)
}

func (app *application) previewImport(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxCSVBytes+(64<<10))
	if err := r.ParseMultipartForm(maxCSVBytes); err != nil {
		app.render(w, "import", http.StatusBadRequest, importPage{Message: "请上传不超过 4 MiB 的 CSV 文件"})
		return
	}
	defer r.MultipartForm.RemoveAll()
	file, _, err := r.FormFile("file")
	if err != nil {
		app.render(w, "import", http.StatusBadRequest, importPage{Message: "请选择 CSV 文件"})
		return
	}
	defer file.Close()
	rows, err := parseAssetCSV(file)
	update := r.PostForm.Get("mode") == "update"
	if err != nil {
		app.render(w, "import", http.StatusUnprocessableEntity, importPage{Update: update, Message: err.Error()})
		return
	}
	entries, err := app.store.PreviewImport(r.Context(), rows, update)
	if err != nil {
		fail(w, err)
		return
	}
	app.reviewImport(w, importPreview{rows: rows, entries: entries, update: update}, http.StatusOK, "", false)
}

func (app *application) confirmImport(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 2<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "表单无效", http.StatusBadRequest)
		return
	}
	preview, ok := app.previews.take(r.PostForm.Get("token"))
	if !ok {
		app.render(w, "import", http.StatusConflict, importPage{Message: "预览已过期或已提交，请重新上传"})
		return
	}
	entries, committed, err := app.store.ConfirmImport(r.Context(), preview.rows, preview.update, preview.entries)
	if err != nil {
		fail(w, err)
		return
	}
	preview.entries = entries
	if !committed {
		app.reviewImport(w, preview, http.StatusConflict, "预览涉及的资产或区域已变化，请核对最新结果。", false)
		return
	}
	app.reviewImport(w, preview, http.StatusOK, "已提交本次导入。", true)
}

func (app *application) reviewImport(w http.ResponseWriter, preview importPreview, status int, message string, committed bool) {
	data := importPage{Entries: preview.entries, Update: preview.update, Message: message, Committed: committed}
	for _, row := range data.Entries {
		switch row.Action {
		case store.ImportAdd:
			data.Added++
		case store.ImportUpdate:
			data.Updated++
		case store.ImportSkip:
			data.Skipped++
		case store.ImportError:
			data.Errors++
		}
	}
	data.Applicable = data.Added + data.Updated
	if !committed && data.Applicable != 0 {
		data.Token = app.previews.put(preview)
	}
	app.render(w, "import", status, data)
}

func parseAssetCSV(file io.Reader) ([]store.ImportRow, error) {
	data, err := io.ReadAll(io.LimitReader(file, maxCSVBytes+1))
	if err != nil {
		return nil, fmt.Errorf("读取 CSV: %w", err)
	}
	if len(data) > maxCSVBytes {
		return nil, errors.New("CSV 文件超过 4 MiB")
	}
	data = bytes.TrimPrefix(data, []byte(csvUTF8BOM))
	// GB18030 multibyte characters never include comma, quote or newline bytes.
	// Read CSV records first so a UTF-8 header and legacy-encoded rows can coexist.
	reader := csv.NewReader(bytes.NewReader(data))
	reader.FieldsPerRecord = -1
	header, err := reader.Read()
	header = trimCSVExtraColumns(header)
	legacy := !slices.Equal(header, assetHeader)
	if err != nil || (legacy && (!decodeLegacyCSVFields(header) || !slices.Equal(header, assetHeader))) {
		return nil, fmt.Errorf("CSV 表头应为 %s，支持 UTF-8 和 GBK／GB18030 编码", strings.Join(assetHeader, ","))
	}
	var rows []store.ImportRow
	for {
		fields, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if len(rows) >= maxCSVRows {
			return nil, fmt.Errorf("CSV 最多包含 %d 条设备记录", maxCSVRows)
		}
		if err != nil {
			var parseErr *csv.ParseError
			if !errors.As(err, &parseErr) {
				return nil, err
			}
			rows = append(rows, store.ImportRow{Line: parseErr.StartLine, Error: fmt.Sprintf("CSV 格式错误，检查第 %d 行第 %d 列的引号", parseErr.Line, parseErr.Column)})
			continue
		}
		line, _ := reader.FieldPos(0)
		fields = trimCSVExtraColumns(fields)
		if len(fields) != len(assetHeader) {
			rows = append(rows, store.ImportRow{Line: line, Error: fmt.Sprintf("应有 %d 列，实际有 %d 列", len(assetHeader), len(fields))})
			continue
		}
		if (legacy || slices.ContainsFunc(fields, invalidUTF8)) && !decodeLegacyCSVFields(fields) {
			rows = append(rows, store.ImportRow{Line: line, Error: "无法识别此行的字符编码，请检查原文件"})
			continue
		}
		port, _ := strconv.Atoi(strings.TrimSpace(fields[4]))
		rows = append(rows, store.ImportRow{Line: line, Area: fields[2], DeviceInput: store.DeviceInput{
			Code: fields[0], Name: fields[1], Address: fields[3], Port: port,
		}})
	}
	if len(rows) == 0 {
		return nil, errors.New("CSV 没有设备记录")
	}
	return rows, nil
}

func trimCSVExtraColumns(fields []string) []string {
	for len(fields) > len(assetHeader) && strings.TrimSpace(fields[len(fields)-1]) == "" {
		fields = fields[:len(fields)-1]
	}
	return fields
}

func invalidUTF8(value string) bool {
	return !utf8.ValidString(value)
}

func decodeLegacyCSVFields(fields []string) bool {
	decoder := simplifiedchinese.GB18030.NewDecoder()
	for index, field := range fields {
		decoded, err := decoder.String(field)
		if err != nil || strings.ContainsRune(decoded, utf8.RuneError) {
			return false
		}
		fields[index] = decoded
	}
	return true
}
