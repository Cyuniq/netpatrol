package web

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"netpatrol/internal/monitor"
)

type settingField struct {
	Key, Label, Unit, Value string
	Min, Max                int
	duration                *time.Duration
	scale                   time.Duration
}

func settingFields(settings *monitor.Settings) []settingField {
	fields := []settingField{
		{Key: "fast_interval", Label: "常规探测间隔", Unit: "秒", Min: 1, Max: 3600, duration: &settings.FastInterval, scale: time.Second},
		{Key: "slow_interval", Label: "降频后的探测间隔", Unit: "秒", Min: 1, Max: 86400, duration: &settings.SlowInterval, scale: time.Second},
		{Key: "slow_after", Label: "双失败持续多久后降频", Unit: "秒", Min: 1, Max: 86400, duration: &settings.SlowAfter, scale: time.Second},
		{Key: "ping_timeout", Label: "Ping 超时", Unit: "毫秒", Min: 100, Max: 30000, duration: &settings.PingTimeout, scale: time.Millisecond},
		{Key: "tcp_timeout", Label: "TCP 超时", Unit: "毫秒", Min: 100, Max: 60000, duration: &settings.TCPTimeout, scale: time.Millisecond},
	}
	for i := range fields {
		fields[i].Value = strconv.FormatInt(int64(*fields[i].duration/fields[i].scale), 10)
	}
	return fields
}

type settingsPage struct {
	Fields []settingField
	Saved  bool
	Error  string
}

func (app *application) settings(w http.ResponseWriter, r *http.Request) {
	settings, err := app.store.Settings(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	app.render(w, "settings", http.StatusOK, settingsPage{
		Fields: settingFields(&settings), Saved: r.URL.Query().Get("saved") == "1",
	})
}

func (app *application) saveSettings(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "表单无效", http.StatusBadRequest)
		return
	}
	var settings monitor.Settings
	fields := settingFields(&settings)
	var validation error
	for i := range fields {
		field := &fields[i]
		field.Value = r.PostForm.Get(field.Key)
		value, err := strconv.Atoi(field.Value)
		if err != nil || value < field.Min || value > field.Max {
			if validation == nil {
				validation = fmt.Errorf("%s必须在 %d～%d %s之间", field.Label, field.Min, field.Max, field.Unit)
			}
			continue
		}
		*field.duration = time.Duration(value) * field.scale
	}
	if validation == nil {
		validation = settings.Validate()
	}
	if validation != nil {
		app.render(w, "settings", http.StatusUnprocessableEntity, settingsPage{Fields: fields, Error: validation.Error()})
		return
	}
	if err := app.store.SaveSettings(r.Context(), settings); err != nil {
		fail(w, err)
		return
	}
	http.Redirect(w, r, "/settings?saved=1", http.StatusSeeOther)
}
