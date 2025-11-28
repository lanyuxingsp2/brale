package freqtrade

import "strings"

// TierPriceQuote 携带最近价格及高低点，供自动触发判断使用。
type TierPriceQuote struct {
	Last float64
	High float64
	Low  float64
}

func (q TierPriceQuote) isEmpty() bool {
	return q.Last == 0 && q.High == 0 && q.Low == 0
}

// priceForStopLoss 返回最能代表止损触发的价格（做多看低点，做空看高点）。
func priceForStopLoss(side string, quote TierPriceQuote, stop float64) (float64, bool) {
	if stop <= 0 || quote.isEmpty() || quote.Last <= 0 {
		return 0, false
	}
	switch strings.ToLower(strings.TrimSpace(side)) {
	case "long":
		price := quote.Last
		return price, price <= stop
	case "short":
		price := quote.Last
		return price, price >= stop
	default:
		return 0, false
	}
}

// priceForTakeProfit 返回止盈触发的参考价格（做多看高点，做空看低点）。
func priceForTakeProfit(side string, quote TierPriceQuote, tp float64) (float64, bool) {
	if tp <= 0 || quote.isEmpty() || quote.Last <= 0 {
		return 0, false
	}
	switch strings.ToLower(strings.TrimSpace(side)) {
	case "long":
		price := quote.Last
		return price, price >= tp
	case "short":
		price := quote.Last
		return price, price <= tp
	default:
		return 0, false
	}
}

// priceForTierTrigger 复用止盈逻辑（tier 也是价格触达触发）。
func priceForTierTrigger(side string, quote TierPriceQuote, target float64) (float64, bool) {
	return priceForTakeProfit(side, quote, target)
}

func ratioOrDefault(val float64, def float64) float64 {
	if val > 0 {
		return val
	}
	return def
}
