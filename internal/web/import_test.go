package web

import (
	"bytes"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/text/encoding/simplifiedchinese"

	"netpatrol/internal/store"
)

func TestCSVTemplateDownloadHasUTF8BOM(t *testing.T) {
	handler, err := Handler(nil, "", nil, nil, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/static/template.csv", nil))
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "text/csv; charset=utf-8" ||
		!strings.HasPrefix(response.Body.String(), csvUTF8BOM+strings.Join(assetHeader, ",")+"\r\n") {
		t.Fatalf("template download: status %d, headers %v", response.Code, response.Header())
	}
	if rows, err := parseAssetCSV(response.Body); err != nil || len(rows) == 0 {
		t.Fatalf("exported template failed to parse: %v", err)
	}
}

func TestCSVImportSupportsGBKAndMixedHeader(t *testing.T) {
	encode := func(s string) string {
		t.Helper()
		data, err := simplifiedchinese.GBK.NewEncoder().String(s)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	header := strings.Join(assetHeader, ",") + ",\r\n"
	legacyRow := encode("CAM-1,\"相机,入口\n第二行\",东门,192.0.2.1,554,\r\n")
	for _, input := range []string{header + legacyRow, csvUTF8BOM + header + legacyRow, encode(header) + legacyRow} {
		rows, err := parseAssetCSV(strings.NewReader(input))
		if err != nil || len(rows) != 1 || rows[0].Error != "" || rows[0].Name != "相机,入口\n第二行" ||
			rows[0].Area != "东门" || rows[0].Port != 554 || rows[0].Line != 2 {
			t.Fatalf("legacy CSV import: %+v (%v)", rows, err)
		}
	}
	input := header + "CAM-1,入口,东门,192.0.2.1,554,意外内容\r\n" + "CAM-2,\xff,东门,192.0.2.2,554\r\n"
	rows, err := parseAssetCSV(strings.NewReader(input))
	if err != nil || len(rows) != 2 || !strings.Contains(rows[0].Error, "实际有 6 列") ||
		!strings.Contains(rows[1].Error, "字符编码") {
		t.Fatalf("unexpected columns or invalid bytes silently accepted: %+v (%v)", rows, err)
	}
}

func TestCSVImportRevalidatesPreview(t *testing.T) {
	ctx := t.Context()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "import.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	area, err := db.SaveArea(ctx, 0, "东门", "", "192.0.2.0/24")
	if err != nil {
		t.Fatal(err)
	}
	write := func(code, name, address string) {
		t.Helper()
		_, err := db.SaveDevice(ctx, 0, store.DeviceInput{Code: code, Name: name, AreaID: area, Address: address, Port: 554})
		if err != nil {
			t.Fatal(err)
		}
	}
	write("CAM-1", "原名称", "192.0.2.1")
	handler, err := Handler(db, "test", nil, nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	send := func(path, contentType string, body *bytes.Buffer) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, path, body)
		request.SetBasicAuth("admin", "test")
		request.Header.Set("Content-Type", contentType)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}
	var buffer bytes.Buffer
	upload := multipart.NewWriter(&buffer)
	file, err := upload.CreateFormFile("file", "devices.csv")
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintln(file, "\ufeff"+strings.Join(assetHeader, ","))
	fmt.Fprintln(file, "CAM-1,新名称,东门,192.0.2.1,8554")
	fmt.Fprintln(file, `CAM-2,"相机,入口",,192.0.2.2,554`)
	fmt.Fprintln(file, "CAM-3,端口错误,东门,192.0.2.3,0")
	fmt.Fprintln(file, "CAM-4,重复一,东门,192.0.2.4,554")
	fmt.Fprintln(file, "CAM-4,重复二,东门,192.0.2.5,554")
	fmt.Fprintln(file, "CAM-5,错列")
	if err := upload.WriteField("mode", "update"); err != nil {
		t.Fatal(err)
	}
	if err := upload.Close(); err != nil {
		t.Fatal(err)
	}
	preview := send("/imports/csv/preview", upload.FormDataContentType(), &buffer)
	if preview.Code != http.StatusOK || !strings.Contains(preview.Body.String(), "确认导入 2 台") ||
		!strings.Contains(preview.Body.String(), "错误 4") {
		t.Fatalf("preview = %d %s", preview.Code, preview.Body.String())
	}
	if rows, err := db.Devices(ctx); err != nil || len(rows) != 1 || rows[0].Target.Port != 554 {
		t.Fatalf("preview wrote assets: %+v, %v", rows, err)
	}
	write("MANUAL-2", "手动纳管", "192.0.2.2")
	confirm := func(result *httptest.ResponseRecorder) *httptest.ResponseRecorder {
		token := regexp.MustCompile(`name="token" value="([^"]+)"`).FindStringSubmatch(result.Body.String())
		if len(token) != 2 {
			t.Fatal("confirmation token absent")
		}
		form := url.Values{"token": {token[1]}}
		return send("/imports/csv/confirm", "application/x-www-form-urlencoded", bytes.NewBufferString(form.Encode()))
	}
	obsolete := confirm(preview)
	if obsolete.Code != http.StatusConflict || !strings.Contains(obsolete.Body.String(), "确认导入 1 台") {
		t.Fatalf("stale preview = %d %s", obsolete.Code, obsolete.Body.String())
	}
	if rows, err := db.Devices(ctx); err != nil || len(rows) != 2 || rows[0].Target.Port != 554 {
		t.Fatalf("stale plan wrote assets: %+v, %v", rows, err)
	}
	result := confirm(obsolete)
	if result.Code != http.StatusOK || !strings.Contains(result.Body.String(), "已提交本次导入") {
		t.Fatalf("confirm = %d %s", result.Code, result.Body.String())
	}
	if rows, err := db.Devices(ctx); err != nil || len(rows) != 2 || rows[0].Target.Port != 8554 ||
		rows[0].Enabled || rows[0].Version != 2 || rows[1].Code != "MANUAL-2" {
		t.Fatalf("committed assets = %+v, %v", rows, err)
	}
	if replay := confirm(obsolete); replay.Code != http.StatusConflict {
		t.Fatalf("replayed confirmation = %d", replay.Code)
	}
}

func TestPreviewFiveThousandCSVRows(t *testing.T) {
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "scale.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.SaveArea(t.Context(), 0, "IPv6", "", "2001:db8::/64"); err != nil {
		t.Fatal(err)
	}
	var data strings.Builder
	fmt.Fprintln(&data, strings.Join(assetHeader, ","))
	for i := 1; i <= 5000; i++ {
		fmt.Fprintf(&data, "CAM-%d,相机,,2001:db8::%x,554\n", i, i)
	}
	rows, err := parseAssetCSV(strings.NewReader(data.String()))
	if err != nil || len(rows) != 5000 {
		t.Fatalf("parsed %d, error = %v", len(rows), err)
	}
	entries, err := db.PreviewImport(t.Context(), rows, false)
	if err != nil || len(entries) != 5000 {
		t.Fatalf("planned %d, error = %v", len(entries), err)
	}
	for _, entry := range entries {
		if entry.Action != store.ImportAdd || entry.Area != "IPv6" {
			t.Fatalf("unexpected preview entry %+v", entry)
		}
	}
}
