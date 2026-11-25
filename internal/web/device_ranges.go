package web

import (
	"time"

	"netpatrol/internal/store"
)

type deviceTrendRange struct {
	Key, Label, StepLabel string
	Window                store.ProbeTrendWindow
}

var defaultDeviceTrend = deviceTrendRange{
	"30m", "近 30 分钟", "1 分钟", store.ProbeTrendWindow{Duration: 30 * time.Minute, Step: time.Minute},
}

var deviceTrendRanges = []deviceTrendRange{
	{"15m", "近 15 分钟", "1 分钟", store.ProbeTrendWindow{Duration: 15 * time.Minute, Step: time.Minute}},
	defaultDeviceTrend,
	{"1h", "近 1 小时", "1 分钟", store.ProbeTrendWindow{Duration: time.Hour, Step: time.Minute}},
	{"6h", "近 6 小时", "5 分钟", store.ProbeTrendWindow{Duration: 6 * time.Hour, Step: 5 * time.Minute}},
	{"24h", "近 24 小时", "1 小时", store.ProbeTrendWindow{Duration: 24 * time.Hour, Step: time.Hour}},
	{"7d", "近 7 天", "4 小时", store.ProbeTrendWindow{Duration: 7 * 24 * time.Hour, Step: 4 * time.Hour}},
	{"30d", "近 30 天", "12 小时", store.ProbeTrendWindow{Duration: store.LatencyRetention, Step: 12 * time.Hour}},
}

func selectedDeviceTrend(key string) deviceTrendRange {
	for _, trend := range deviceTrendRanges {
		if trend.Key == key {
			return trend
		}
	}
	// Unknown links use the same fallback as a fresh visit.
	return defaultDeviceTrend
}
