package freqtrade

import (
	"encoding/json"
	"strings"
	"time"

	"brale/internal/gateway/database"
)

func tradeToLiveRecord(tr *Trade) database.LiveOrderRecord {
	if tr == nil {
		return database.LiveOrderRecord{}
	}
	now := time.Now()

	symbol := freqtradePairToSymbol(tr.Pair)
	isOpen := tr.IsOpen

	if !isOpen {
		closeDate := strings.TrimSpace(tr.CloseDate)
		if closeDate == "" {
			isOpen = true
		}
	}
	rec := initLiveRecord(tr, symbol, now, isOpen)
	rec = applyAmounts(tr, rec, isOpen)
	rec = applyPricing(tr, rec)
	rec = applyTimestamps(tr, rec)
	rec = applyPnLFields(tr, rec, isOpen)

	rec.LastStatusSync = ptrTime(time.Now())
	if raw, err := json.Marshal(tr); err == nil {
		rec.RawData = string(raw)
	}
	return rec
}

// initLiveRecord seeds a record with defaults; status starts open unless already closed.
func initLiveRecord(tr *Trade, symbol string, now time.Time, isOpen bool) database.LiveOrderRecord {
	status := database.LiveOrderStatusOpen
	if !isOpen {
		status = database.LiveOrderStatusClosed
	}
	return database.LiveOrderRecord{
		FreqtradeID: tr.ID,
		Symbol:      symbol,
		Side:        normalizeTradeSide(tr),
		Status:      status,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
}

func applyAmounts(tr *Trade, rec database.LiveOrderRecord, isOpen bool) database.LiveOrderRecord {
	initAmt := resolveInitialAmount(tr)
	rec = applyAmountFields(rec, initAmt, tr.Amount, isOpen)
	rec = applyStakeAndValue(tr, rec)
	rec = ensureNotionalAndStake(tr, rec)
	return rec
}

func applyPricing(tr *Trade, rec database.LiveOrderRecord) database.LiveOrderRecord {
	if tr.OpenRate != 0 {
		rec.Price = ptrFloat(tr.OpenRate)
	}
	if tr.Leverage != 0 {
		rec.Leverage = ptrFloat(tr.Leverage)
	}
	return rec
}

func applyTimestamps(tr *Trade, rec database.LiveOrderRecord) database.LiveOrderRecord {
	if openAt := parseFreqtradeTime(tr.OpenDate); !openAt.IsZero() {
		rec.StartTime = ptrTime(openAt)
		rec.CreatedAt = openAt
	}
	if closeAt := parseFreqtradeTime(tr.CloseDate); !closeAt.IsZero() {
		rec.EndTime = ptrTime(closeAt)
		rec.UpdatedAt = closeAt
		if rec.Status == database.LiveOrderStatusOpen {
			rec.Status = database.LiveOrderStatusClosed
		}
	}
	return rec
}

func applyPnLFields(tr *Trade, rec database.LiveOrderRecord, isOpen bool) database.LiveOrderRecord {
	if isOpen {
		rec = applyOpenPnL(tr, rec)
	} else {
		rec = applyClosedPnL(tr, rec)
	}
	return rec
}

func applyOpenPnL(tr *Trade, rec database.LiveOrderRecord) database.LiveOrderRecord {
	if rate := firstNonZero(tr.CurrentRate, tr.OpenRate); rate != 0 {
		rec.CurrentPrice = ptrFloat(rate)
	}
	if tr.ProfitRatio != 0 {
		rec.CurrentProfitRatio = ptrFloat(tr.ProfitRatio)
		rec.UnrealizedPnLRatio = ptrFloat(tr.ProfitRatio)
	}
	if tr.ProfitAbs != 0 {
		rec.CurrentProfitAbs = ptrFloat(tr.ProfitAbs)
		rec.UnrealizedPnLUSD = ptrFloat(tr.ProfitAbs)
	}
	// Use TotalProfitAbs (realized + unrealized) for PnL display to match Freqtrade UI
	if tr.TotalProfitAbs != 0 {
		rec.PnLUSD = ptrFloat(tr.TotalProfitAbs)
	}
	if tr.TotalProfitRatio != 0 {
		rec.PnLRatio = ptrFloat(tr.TotalProfitRatio)
	}
	if tr.RealizedProfit != 0 {
		rec.RealizedPnLUSD = ptrFloat(tr.RealizedProfit)
		if rec.StakeAmount != nil && *rec.StakeAmount != 0 {
			ratio := tr.RealizedProfit / *rec.StakeAmount
			rec.RealizedPnLRatio = ptrFloat(ratio)
		}
	}
	return rec
}

func applyClosedPnL(tr *Trade, rec database.LiveOrderRecord) database.LiveOrderRecord {
	if tr.CloseRate != 0 {
		rec.CurrentPrice = ptrFloat(tr.CloseRate)
	}
	if tr.CloseProfit != 0 {
		rec.PnLRatio = ptrFloat(tr.CloseProfit)
	}
	if tr.CloseProfitAbs != 0 {
		rec.PnLUSD = ptrFloat(tr.CloseProfitAbs)
	}
	return rec
}

func normalizeTradeSide(tr *Trade) string {
	if tr == nil {
		return "long"
	}
	side := strings.ToLower(strings.TrimSpace(tr.Side))
	if side == "" {
		if tr.IsShort {
			return "short"
		}
		return "long"
	}
	switch {
	case strings.Contains(side, "short"), strings.Contains(side, "sell"):
		return "short"
	default:
		return "long"
	}
}

func ptrTime(t time.Time) *time.Time {
	return &t
}

func resolveInitialAmount(tr *Trade) float64 {
	if tr.AmountRequested != 0 {
		return tr.AmountRequested
	}
	return tr.Amount
}

func applyAmountFields(rec database.LiveOrderRecord, initAmt, amt float64, isOpen bool) database.LiveOrderRecord {
	if initAmt != 0 {
		rec.InitialAmount = ptrFloat(initAmt)
	}
	if amt != 0 {
		rec.Amount = ptrFloat(amt)
		if !isOpen {
			rec.ClosedAmount = ptrFloat(amt)
		}
	}
	if isOpen && initAmt > 0 && amt > 0 {
		closed := initAmt - amt
		if closed > 0 {
			rec.ClosedAmount = ptrFloat(closed)
		}
	}
	return rec
}

func applyStakeAndValue(tr *Trade, rec database.LiveOrderRecord) database.LiveOrderRecord {
	if stake := tr.StakeAmount; stake != 0 {
		rec.StakeAmount = ptrFloat(stake)
		rec.PositionValue = ptrFloat(stake)
	}
	if tr.OpenTradeValue != 0 {
		rec.PositionValue = ptrFloat(tr.OpenTradeValue)
		if tr.Leverage != 0 {
			stake := tr.OpenTradeValue / tr.Leverage
			if stake != 0 {
				rec.StakeAmount = ptrFloat(stake)
			}
		}
	}
	return rec
}

func ensureNotionalAndStake(tr *Trade, rec database.LiveOrderRecord) database.LiveOrderRecord {
	if rec.PositionValue == nil && tr.Amount != 0 && tr.OpenRate != 0 {
		notional := tr.Amount * tr.OpenRate
		if tr.Leverage != 0 {
			notional *= tr.Leverage
		}
		rec.PositionValue = ptrFloat(notional)
	}
	if rec.StakeAmount == nil && rec.PositionValue != nil && tr.Leverage != 0 {
		stake := *rec.PositionValue / tr.Leverage
		rec.StakeAmount = &stake
	}
	return rec
}
