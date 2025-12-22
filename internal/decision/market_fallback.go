package decision

import (
	"encoding/json"
	"strings"

	"brale/internal/market"
)

// augmentMarketData ensures provider prompts总能看到账户与价格：优先使用实时 Market 数据，
// 若缺失则从指标快照或 K 线提取最近收盘价做兜底。
func augmentMarketData(base map[string]MarketData, ctxs []AnalysisContext) map[string]MarketData {
	if len(base) == 0 && len(ctxs) == 0 {
		return base
	}
	out := make(map[string]MarketData, len(base)+len(ctxs))
	for sym, data := range base {
		key := strings.ToUpper(strings.TrimSpace(sym))
		if key == "" {
			continue
		}
		if data.Symbol == "" {
			data.Symbol = key
		}
		out[key] = data
	}
	addPrice := func(sym string, price float64) {
		if price <= 0 {
			return
		}
		key := strings.ToUpper(strings.TrimSpace(sym))
		if key == "" {
			return
		}
		existing := out[key]
		if existing.Symbol == "" {
			existing.Symbol = key
		}
		// 以实时 Market 为主，仅在缺失时补全。
		if existing.Price <= 0 {
			existing.Price = price
			out[key] = existing
		}
	}
	for _, ac := range ctxs {
		if price, ok := extractIndicatorPrice(ac.IndicatorJSON); ok {
			addPrice(ac.Symbol, price)
		}
		if price, ok := extractKlineClose(ac.KlineJSON); ok {
			addPrice(ac.Symbol, price)
		}
	}
	if len(out) == 0 {
		return base
	}
	return out
}

func extractIndicatorPrice(raw string) (float64, bool) {
	if strings.TrimSpace(raw) == "" {
		return 0, false
	}
	var payload struct {
		Market struct {
			Symbol       string  `json:"symbol"`
			CurrentPrice float64 `json:"current_price"`
		} `json:"market"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return 0, false
	}
	if payload.Market.CurrentPrice > 0 {
		return payload.Market.CurrentPrice, true
	}
	return 0, false
}

func extractKlineClose(raw string) (float64, bool) {
	if strings.TrimSpace(raw) == "" {
		return 0, false
	}
	var candles []market.Candle
	if err := json.Unmarshal([]byte(raw), &candles); err != nil || len(candles) == 0 {
		return 0, false
	}
	last := candles[len(candles)-1]
	if last.Close > 0 {
		return last.Close, true
	}
	return 0, false
}
