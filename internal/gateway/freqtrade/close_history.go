package freqtrade

import (
	"encoding/json"
	"sort"
	"strings"

	"brale/internal/gateway/database"
	"brale/internal/gateway/exchange"
)

func attachCloseHistory(pos *exchange.APIPosition, rec database.LiveOrderRecord) {
	if pos == nil {
		return
	}
	raw := strings.TrimSpace(rec.RawData)
	if raw == "" {
		return
	}
	var trade Trade
	if err := json.Unmarshal([]byte(raw), &trade); err != nil {
		return
	}
	if len(trade.Orders) == 0 {
		return
	}
	history := buildCloseHistory(trade.Orders, pos)
	if len(history) == 0 {
		return
	}
	pos.CloseHistory = history
}

func buildCloseHistory(orders []TradeOrder, pos *exchange.APIPosition) []exchange.APIOrder {
	if len(orders) == 0 || pos == nil {
		return nil
	}
	exitSide := exitOrderSide(pos.Side)
	out := make([]exchange.APIOrder, 0, len(orders))
	for _, ord := range orders {
		side := strings.ToLower(strings.TrimSpace(ord.FTOrderSide))
		tag := strings.ToLower(strings.TrimSpace(ord.FTOrderTag))
		if !isExitOrder(side, tag, exitSide) {
			continue
		}
		filled := ord.Filled
		if filled == 0 {
			filled = ord.Amount
		}
		if filled == 0 && !strings.EqualFold(strings.TrimSpace(ord.Status), "closed") && ord.OrderFilledTimestamp <= 0 {
			continue
		}
		price := firstNonZero(ord.SafePrice, ord.Price)
		if price == 0 && filled > 0 && ord.Cost > 0 {
			price = ord.Cost / filled
		}
		openedAt := normalizeUnixMillis(ord.OrderTimestamp)
		filledAt := normalizeUnixMillis(ord.OrderFilledTimestamp)
		pnlUSD, pnlRatio := computeOrderPnL(pos, price, filled)
		out = append(out, exchange.APIOrder{
			OrderID:   ord.OrderID,
			Side:      side,
			OrderType: ord.OrderType,
			Tag:       ord.FTOrderTag,
			Amount:    ord.Amount,
			Filled:    filled,
			Price:     price,
			Cost:      ord.Cost,
			Status:    ord.Status,
			IsOpen:    ord.IsOpen,
			OpenedAt:  openedAt,
			FilledAt:  filledAt,
			PnLUSD:    pnlUSD,
			PnLRatio:  pnlRatio,
		})
	}
	if len(out) == 0 {
		return nil
	}
	sort.Slice(out, func(i, j int) bool {
		return orderSortKey(out[i]) > orderSortKey(out[j])
	})
	return out
}

func orderSortKey(ord exchange.APIOrder) int64 {
	if ord.FilledAt > 0 {
		return ord.FilledAt
	}
	return ord.OpenedAt
}

func exitOrderSide(side string) string {
	s := strings.ToLower(strings.TrimSpace(side))
	switch {
	case strings.Contains(s, "short"):
		return "buy"
	case strings.Contains(s, "long"):
		return "sell"
	case strings.Contains(s, "buy"):
		return "sell"
	case strings.Contains(s, "sell"):
		return "buy"
	default:
		return ""
	}
}

func isExitOrder(side, tag, exitSide string) bool {
	if tag != "" && isExitTag(tag) {
		return true
	}
	if exitSide == "" || side == "" {
		return false
	}
	return side == exitSide
}

func isExitTag(tag string) bool {
	if tag == "" {
		return false
	}
	if strings.Contains(tag, "entry") {
		return false
	}
	switch {
	case strings.Contains(tag, "exit"),
		strings.Contains(tag, "stop"),
		strings.Contains(tag, "take"),
		strings.Contains(tag, "tp"),
		strings.Contains(tag, "sl"),
		strings.Contains(tag, "close"):
		return true
	default:
		return false
	}
}

func computeOrderPnL(pos *exchange.APIPosition, price, filled float64) (float64, float64) {
	if pos == nil || price == 0 || filled == 0 || pos.EntryPrice == 0 {
		return 0, 0
	}
	dir := 1.0
	if strings.Contains(strings.ToLower(pos.Side), "short") {
		dir = -1
	}
	pnlUSD := (price - pos.EntryPrice) * filled * dir
	lev := pos.Leverage
	if lev <= 0 {
		lev = 1
	}
	base := pos.EntryPrice * filled / lev
	if base <= 0 {
		return pnlUSD, 0
	}
	return pnlUSD, pnlUSD / base
}
