package format

import (
	"fmt"
	"math"
	"strings"
	"time"
)

func Percent(val float64) string {
	if val == 0 {
		return "0%"
	}
	return fmt.Sprintf("%.0f%%", val*100)
}

func Float(val float64, decimals int) string {
	if decimals < 0 {
		decimals = 4
	}
	out := fmt.Sprintf("%.*f", decimals, val)
	out = strings.TrimRight(strings.TrimRight(out, "0"), ".")
	if out == "" {
		return "0"
	}
	return out
}

func VolumeSlice(volumes []float64) string {
	if len(volumes) == 0 {
		return "[]"
	}
	parts := make([]string, len(volumes))
	for i, v := range volumes {
		parts[i] = fmt.Sprintf("%.0f", v)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func Duration(ms int64) string {
	if ms <= 0 {
		return "-"
	}
	d := time.Duration(ms) * time.Millisecond
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	if h > 0 {
		return fmt.Sprintf("%dh%dm", h, m)
	}
	if m > 0 {
		return fmt.Sprintf("%dm%ds", m, d/time.Second)
	}
	return fmt.Sprintf("%ds", d/time.Second)
}

func RangeSummary(bars []float64) (float64, float64) {
	low := math.MaxFloat64
	high := -math.MaxFloat64
	for _, v := range bars {
		if v < low {
			low = v
		}
		if v > high {
			high = v
		}
	}
	return low, high
}
