package market

import (
	"fmt"
	"math"
	"strings"
	"time"

	"brale/internal/pkg/format"
	"brale/internal/pkg/text"
)

// Candles wraps a slice of Candle for helper methods.
type Candles []Candle

// TimeString formats close time (fallback to open time) in UTC.
func (c Candle) TimeString() string {
	ts := c.CloseTime
	if ts == 0 {
		ts = c.OpenTime
	}
	if ts <= 0 {
		return "-"
	}
	return time.UnixMilli(ts).UTC().Format("01-02 15:04") + "Z"
}

// Snapshot summarizes a window of candles for prompt display.
func (cs Candles) Snapshot(interval, trend string) string {
	if len(cs) == 0 {
		return ""
	}
	first := cs[0]
	last := cs[len(cs)-1]
	base := first.Close
	if base == 0 {
		base = first.Open
	}
	changePct := 0.0
	if base != 0 {
		changePct = (last.Close - base) / base * 100
	}
	low := math.MaxFloat64
	high := -math.MaxFloat64
	for _, bar := range cs {
		if bar.Low < low {
			low = bar.Low
		}
		if bar.High > high {
			high = bar.High
		}
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("close≈%s", format.Float(last.Close, 4)))
	iv := strings.TrimSpace(interval)
	if iv == "" {
		iv = "window"
	}
	if base != 0 {
		sb.WriteString(fmt.Sprintf(" (%+.2f%%/%s)", changePct, iv))
	}
	if low != math.MaxFloat64 && high != -math.MaxFloat64 {
		sb.WriteString(fmt.Sprintf(", 区间 %s–%s", format.Float(low, 4), format.Float(high, 4)))
	}
	if t := strings.TrimSpace(trend); t != "" {
		sb.WriteString(", " + text.Truncate(t, 200))
	}
	return sb.String()
}
