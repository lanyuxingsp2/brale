package freqtrade

import (
	"strings"
	"time"
)

var freqtradeQuoteSuffixes = []string{
	"USDT", "USDC", "BUSD", "FDUSD", "TUSD",
	"DAI", "BTC", "ETH", "BNB", "BETH",
	"EUR", "GBP", "AUD", "BRL", "TRY", "IDR",
}

func formatFreqtradePair(symbol string) string {
	symbol = strings.TrimSpace(symbol)
	if symbol == "" {
		return ""
	}
	if strings.Contains(symbol, "/") {
		return strings.ToUpper(symbol)
	}
	up := strings.ToUpper(symbol)
	for _, suf := range freqtradeQuoteSuffixes {
		if strings.HasSuffix(up, suf) && len(up) > len(suf) {
			return up[:len(up)-len(suf)] + "/" + suf
		}
	}
	return up
}

func freqtradePairToSymbol(pair string) string {
	pair = strings.TrimSpace(pair)
	if pair == "" {
		return ""
	}
	upper := strings.ToUpper(pair)
	return strings.ReplaceAll(upper, "/", "")
}

func freqtradeKey(symbol, side string) string {
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	side = strings.ToLower(strings.TrimSpace(side))
	if symbol == "" || side == "" {
		return ""
	}
	return symbol + "#" + side
}

func deriveSide(action string) string {
	switch action {
	case "open_long", "close_long":
		return "long"
	case "open_short", "close_short":
		return "short"
	default:
		return ""
	}
}

func clampCloseRatio(ratio float64) float64 {
	if ratio < 0 {
		return 0
	}
	if ratio > 1 {
		return 1
	}
	return ratio
}

func parseFreqtradeTime(raw string) time.Time {
	layouts := []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05"}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, strings.TrimSpace(raw)); err == nil {
			return t
		}
	}
	return time.Time{}
}

func firstNonZero(vals ...float64) float64 {
	for _, v := range vals {
		if v != 0 {
			return v
		}
	}
	return 0
}

func freqtradeAction(side string, closing bool) string {
	switch side {
	case "long":
		if closing {
			return "close_long"
		}
		return "open_long"
	case "short":
		if closing {
			return "close_short"
		}
		return "open_short"
	default:
		return ""
	}
}
