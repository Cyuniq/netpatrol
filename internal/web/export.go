package web

import (
	"encoding/csv"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"netpatrol/internal/monitor"
	"netpatrol/internal/store"
)

var assetHeader = []string{"设备编号", "名称", "区域", "IP", "TCP端口"}

const csvUTF8BOM = "\xef\xbb\xbf"

var statusHeader = []string{
	"区域路径", "状态", "状态开始时间", "Ping结果", "Ping失败原因", "Ping RTT(ms)",
	"TCP结果", "TCP失败原因", "TCP耗时(ms)", "近一小时丢包数", "近一小时发送数",
	"近一小时丢包率", "实际间隔(秒)", "最后探测时间",
}

func (app *application) exportDevices(w http.ResponseWriter, r *http.Request) {
	report := r.PathValue("kind") == "status.csv"
	switch r.PathValue("kind") {
	case "assets.csv", "status.csv":
	default:
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", r.PathValue("kind")))
	if _, err := w.Write([]byte(csvUTF8BOM)); err != nil {
		return
	}
	writer := csv.NewWriter(w)
	header := append([]string(nil), assetHeader...)
	if report {
		header = append(header, statusHeader...)
	}
	if err := writer.Write(header); err != nil {
		return
	}
	err := app.store.VisitDeviceViews(r.Context(), listFilter(r.URL.Query()), time.Now(), func(device store.DeviceView) error {
		code, name, area := device.Code, device.Name, device.AreaName
		if report {
			code, name, area = safeExcel(code), safeExcel(name), safeExcel(area)
		}
		row := []string{code, name, area, device.Target.IP.String(), strconv.Itoa(int(device.Target.Port))}
		if report {
			row = append(row, reportFields(device)...)
		}
		return writer.Write(row)
	})
	writer.Flush()
	if err != nil || writer.Error() != nil {
		log.Printf("csv export: read=%v write=%v", err, writer.Error())
	}
}

func reportFields(device store.DeviceView) []string {
	snapshot := device.Snapshot
	ping, tcp := snapshot.LastRound.Ping, snapshot.LastRound.TCP
	var loss, interval string
	if percent, ok := device.Loss.Percent(); ok {
		loss = fmt.Sprintf("%.2f%%", percent)
	}
	if snapshot.ActualInterval > 0 {
		interval = strconv.FormatFloat(snapshot.ActualInterval.Seconds(), 'f', 3, 64)
	}
	return []string{
		safeExcel(device.AreaPath), stateLabel(snapshot.State), csvTime(snapshot.Since),
		csvOutcome(ping), safeExcel(ping.Detail), csvRTT(ping),
		csvOutcome(tcp), safeExcel(tcp.Detail), csvRTT(tcp),
		strconv.Itoa(device.Loss.Lost()), strconv.Itoa(device.Loss.Sent), loss, interval,
		csvTime(snapshot.LastProbeTime()),
	}
}

func csvOutcome(result monitor.Result) string {
	if result.Outcome == monitor.Passed {
		return "通过"
	}
	if result.Outcome == "" {
		return ""
	}
	return resultLabel(result)
}

func csvTime(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.Local().Format(time.RFC3339)
}

func csvRTT(result monitor.Result) string {
	if result.Outcome != monitor.Passed {
		return ""
	}
	return fmt.Sprintf("%.3f", float64(result.RTT)/float64(time.Millisecond))
}

func safeExcel(value string) string {
	trimmed := strings.TrimLeftFunc(value, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r) || r == '\ufeff'
	})
	first, _ := utf8.DecodeRuneInString(value)
	meaningful, _ := utf8.DecodeRuneInString(trimmed)
	if first == '\t' || first == '\r' || first == '\n' || strings.ContainsRune("=+-@＝＋－＠", meaningful) {
		return "'" + value
	}
	return value
}
