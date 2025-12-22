package freqtrade

import (
	"strconv"
	"strings"
	"time"

	"brale/internal/gateway/database"
	"brale/internal/gateway/exchange"
)

func liveOrderStatusText(status database.LiveOrderStatus) string {
	switch status {
	case database.LiveOrderStatusOpen:
		return "open"
	case database.LiveOrderStatusClosed:
		return "closed"
	case database.LiveOrderStatusPartial:
		return "partial"
	case database.LiveOrderStatusRetrying:
		return "retrying"
	case database.LiveOrderStatusOpening:
		return "opening"
	case database.LiveOrderStatusClosingPartial:
		return "closing_partial"
	case database.LiveOrderStatusClosingFull:
		return "closing_full"
	case database.LiveOrderStatusCanceled:
		return "canceled"
	default:
		return "unknown"
	}
}

func effectiveAmount(entry, amount, stake, leverage float64) float64 {
	// Prioritize actual amount when available, since stake remains at initial value
	// even after partial closures while amount reflects current remaining position.
	if amount > 0 {
		return amount
	}
	if stake > 0 && leverage > 0 && entry > 0 {
		return (stake * leverage) / entry
	}
	return amount
}

func derivePnL(entry, current, amount, stake, leverage float64, side string) (float64, float64) {
	if entry <= 0 || current <= 0 {
		return 0, 0
	}
	amt := effectiveAmount(entry, amount, stake, leverage)
	if amt <= 0 {
		return 0, 0
	}
	dir := 1.0
	if strings.ToLower(side) == "short" {
		dir = -1
	}
	pnlUSD := (current - entry) * amt * dir
	base := stake
	if base <= 0 {
		lev := leverage
		if lev <= 0 {
			lev = 1
		}
		base = entry * amount / lev
	}
	var pnlRatio float64
	if base > 0 {
		pnlRatio = pnlUSD / base
	}
	return pnlUSD, pnlRatio
}

type orderTimes struct {
	openedAt    time.Time
	closedAt    time.Time
	openMillis  int64
	closeMillis int64
	holdingMs   int64
}

func buildOrderTimes(rec database.LiveOrderRecord, nowMillis int64) orderTimes {
	now := time.Now()
	if nowMillis > 0 {
		now = time.UnixMilli(nowMillis)
	}
	var openedAt time.Time
	if rec.StartTime != nil {
		openedAt = *rec.StartTime
	}
	var closedAt time.Time
	if rec.EndTime != nil {
		closedAt = *rec.EndTime
	}
	openAtMillis := timeToMillis(rec.StartTime)
	closeAtMillis := timeToMillis(rec.EndTime)
	holdingMs := positionHoldingMs(openedAt, now, closedAt)
	return orderTimes{
		openedAt:    openedAt,
		closedAt:    closedAt,
		openMillis:  openAtMillis,
		closeMillis: closeAtMillis,
		holdingMs:   holdingMs,
	}
}

func resolveCurrentPrice(rec database.LiveOrderRecord) float64 {
	if cp := valOrZero(rec.CurrentPrice); cp > 0 {
		return cp
	}
	return valOrZero(rec.Price)
}

func resolvePnLForLive(rec database.LiveOrderRecord) (float64, float64) {
	// Prefer PnLUSD/PnLRatio which contains total profit (from Freqtrade's total_profit_abs/ratio)
	// Fall back to CurrentProfitAbs/Ratio for unrealized only
	pnlUSD := valOrZero(rec.PnLUSD)
	pnlRatio := valOrZero(rec.PnLRatio)
	if pnlUSD == 0 {
		pnlUSD = valOrZero(rec.CurrentProfitAbs)
	}
	if pnlRatio == 0 {
		pnlRatio = valOrZero(rec.CurrentProfitRatio)
	}
	return pnlUSD, pnlRatio
}

func initAPIPositionFromLive(rec database.LiveOrderRecord, times orderTimes, currentPrice, pnlUSD, pnlRatio float64) exchange.APIPosition {
	return exchange.APIPosition{
		TradeID:            rec.FreqtradeID,
		Symbol:             rec.Symbol,
		Side:               rec.Side,
		EntryPrice:         valOrZero(rec.Price),
		Amount:             valOrZero(rec.Amount),
		InitialAmount:      valOrZero(rec.InitialAmount),
		Stake:              valOrZero(rec.StakeAmount),
		Leverage:           valOrZero(rec.Leverage),
		PositionValue:      valOrZero(rec.PositionValue),
		OpenedAt:           times.openMillis,
		HoldingMs:          times.holdingMs,
		CurrentPrice:       currentPrice,
		PnLRatio:           pnlRatio,
		PnLUSD:             pnlUSD,
		UnrealizedPnLRatio: valOrZero(rec.UnrealizedPnLRatio),
		UnrealizedPnLUSD:   valOrZero(rec.UnrealizedPnLUSD),
		RealizedPnLRatio:   valOrZero(rec.RealizedPnLRatio),
		RealizedPnLUSD:     valOrZero(rec.RealizedPnLUSD),
		Status:             liveOrderStatusText(rec.Status),
	}
}

func computePositionValue(out *exchange.APIPosition) {
	ref := out.CurrentPrice
	if ref <= 0 {
		ref = out.EntryPrice
	}
	if ref > 0 {
		amt := effectiveAmount(out.EntryPrice, out.Amount, out.Stake, out.Leverage)
		if amt > 0 {
			out.PositionValue = amt * ref
		}
	} else if out.Stake > 0 && out.Leverage > 0 {
		out.PositionValue = out.Stake * out.Leverage
	}
}

func deriveBaseStake(out exchange.APIPosition) float64 {
	baseStake := out.Stake
	if baseStake > 0 || out.EntryPrice <= 0 {
		return baseStake
	}
	lev := out.Leverage
	if lev <= 0 {
		lev = 1
	}
	switch {
	case out.InitialAmount > 0:
		baseStake = out.EntryPrice * out.InitialAmount / lev
	case out.Amount > 0:
		baseStake = out.EntryPrice * out.Amount / lev
	}
	return baseStake
}

func fillPnL(out *exchange.APIPosition, isClosed bool, baseStake, pnlUSD, pnlRatio, derivedUSD, derivedRatio float64) {
	if out == nil {
		return
	}
	if !isClosed {
		fillUnrealizedPnL(out, pnlUSD, pnlRatio, derivedUSD, derivedRatio)
	}
	fillRealizedPnL(out, isClosed, derivedUSD, derivedRatio)
	if !isClosed {
		syncUnrealizedPnL(out, baseStake)
		return
	}
	ensureClosedPnLRatio(out, baseStake)
}

func fillUnrealizedPnL(out *exchange.APIPosition, pnlUSD, pnlRatio, derivedUSD, derivedRatio float64) {
	if out.UnrealizedPnLUSD != 0 || out.UnrealizedPnLRatio != 0 {
		return
	}
	switch {
	case pnlUSD != 0 || pnlRatio != 0:
		out.UnrealizedPnLUSD = pnlUSD
		out.UnrealizedPnLRatio = pnlRatio
	case derivedUSD != 0 || derivedRatio != 0:
		out.UnrealizedPnLUSD = derivedUSD
		out.UnrealizedPnLRatio = derivedRatio
	}
}

func fillRealizedPnL(out *exchange.APIPosition, isClosed bool, derivedUSD, derivedRatio float64) {
	if out.PnLUSD != 0 || out.PnLRatio != 0 {
		return
	}
	if derivedUSD == 0 && derivedRatio == 0 {
		return
	}
	out.PnLUSD = derivedUSD
	out.PnLRatio = derivedRatio
	if isClosed {
		return
	}
	if out.UnrealizedPnLUSD == 0 {
		out.UnrealizedPnLUSD = derivedUSD
	}
	if out.UnrealizedPnLRatio == 0 {
		out.UnrealizedPnLRatio = derivedRatio
	}
}

func syncUnrealizedPnL(out *exchange.APIPosition, baseStake float64) {
	if out.UnrealizedPnLUSD == 0 {
		return
	}
	out.PnLUSD = out.UnrealizedPnLUSD
	if baseStake <= 0 {
		return
	}
	out.PnLRatio = out.PnLUSD / baseStake
	if out.UnrealizedPnLRatio == 0 {
		out.UnrealizedPnLRatio = out.UnrealizedPnLUSD / baseStake
	}
}

func ensureClosedPnLRatio(out *exchange.APIPosition, baseStake float64) {
	if out.PnLRatio != 0 || baseStake <= 0 || out.PnLUSD == 0 {
		return
	}
	out.PnLRatio = out.PnLUSD / baseStake
}

func finalizeClosure(out *exchange.APIPosition, closeAtMillis int64, currentPrice float64) {
	if closeAtMillis > 0 {
		out.ClosedAt = closeAtMillis
		out.ExitPrice = currentPrice
	}
}

func syncOpenOrderPnL(out *exchange.APIPosition, rec database.LiveOrderRecord, baseStake float64) {
	realizedUSD, realizedRatio := resolveRealizedPnL(rec, out, baseStake)
	totalUSD, totalRatio := resolveTotalPnL(rec, out, baseStake, realizedUSD, realizedRatio)

	if out.RealizedPnLUSD == 0 && realizedUSD != 0 {
		out.RealizedPnLUSD = realizedUSD
	}
	if out.RealizedPnLRatio == 0 && realizedRatio != 0 {
		out.RealizedPnLRatio = realizedRatio
	}

	out.PnLUSD = totalUSD
	if totalRatio != 0 {
		out.PnLRatio = totalRatio
	}
}

func resolveRealizedPnL(rec database.LiveOrderRecord, out *exchange.APIPosition, baseStake float64) (float64, float64) {
	realizedUSD := valOrZero(rec.RealizedPnLUSD)
	realizedRatio := valOrZero(rec.RealizedPnLRatio)
	totalUSD := valOrZero(rec.PnLUSD)

	if realizedUSD == 0 && totalUSD != 0 && out.UnrealizedPnLUSD != 0 {
		realizedUSD = totalUSD - out.UnrealizedPnLUSD
	}
	if realizedRatio == 0 && baseStake > 0 && realizedUSD != 0 {
		realizedRatio = realizedUSD / baseStake
	}
	return realizedUSD, realizedRatio
}

func resolveTotalPnL(rec database.LiveOrderRecord, out *exchange.APIPosition, baseStake, realizedUSD, realizedRatio float64) (float64, float64) {
	totalUSD := valOrZero(rec.PnLUSD)
	totalRatio := valOrZero(rec.PnLRatio)

	if totalUSD == 0 && (realizedUSD != 0 || out.UnrealizedPnLUSD != 0) {
		totalUSD = realizedUSD + out.UnrealizedPnLUSD
	}
	if totalRatio == 0 {
		switch {
		case baseStake > 0 && totalUSD != 0:
			totalRatio = totalUSD / baseStake
		default:
			totalRatio = out.UnrealizedPnLRatio + realizedRatio
		}
	}
	return totalUSD, totalRatio
}

func liveOrderToAPIPosition(rec database.LiveOrderRecord, nowMillis int64) exchange.APIPosition {
	times := buildOrderTimes(rec, nowMillis)
	currentPrice := resolveCurrentPrice(rec)
	pnlUSD, pnlRatio := resolvePnLForLive(rec)
	out := initAPIPositionFromLive(rec, times, currentPrice, pnlUSD, pnlRatio)

	computePositionValue(&out)
	baseStake := deriveBaseStake(out)
	derivedUSD, derivedRatio := derivePnL(out.EntryPrice, out.CurrentPrice, out.Amount, out.Stake, out.Leverage, out.Side)
	fillPnL(&out, rec.Status == database.LiveOrderStatusClosed, baseStake, pnlUSD, pnlRatio, derivedUSD, derivedRatio)
	if rec.Status != database.LiveOrderStatusClosed {
		syncOpenOrderPnL(&out, rec, baseStake)
	}

	out.RemainingRatio = remainingRatio(rec)
	finalizeClosure(&out, times.closeMillis, currentPrice)
	return out
}

func exchangePositionToAPIPosition(pos exchange.Position, nowMillis int64) exchange.APIPosition {
	now := time.Now()
	if nowMillis > 0 {
		now = time.UnixMilli(nowMillis)
	}

	closedAt := time.Time{}
	closeAtMillis := int64(0)
	if !pos.IsOpen && !pos.UpdatedAt.IsZero() {
		closedAt = pos.UpdatedAt
		closeAtMillis = closedAt.UnixMilli()
	}
	holdingMs := positionHoldingMs(pos.OpenedAt, now, closedAt)

	tradeID, _ := strconv.Atoi(pos.ID)
	currentPrice := pos.CurrentPrice
	if currentPrice == 0 {
		currentPrice = pos.EntryPrice
	}

	out := exchange.APIPosition{
		TradeID:            tradeID,
		Symbol:             pos.Symbol,
		Side:               pos.Side,
		EntryPrice:         pos.EntryPrice,
		Amount:             pos.Amount,
		InitialAmount:      pos.InitialAmount,
		Stake:              pos.StakeAmount,
		Leverage:           pos.Leverage,
		OpenedAt:           pos.OpenedAt.UnixMilli(),
		HoldingMs:          holdingMs,
		StopLoss:           pos.StopLoss,
		TakeProfit:         pos.TakeProfit,
		CurrentPrice:       currentPrice,
		UnrealizedPnLRatio: pos.UnrealizedPnLRatio,
		UnrealizedPnLUSD:   pos.UnrealizedPnL,
		Status:             "open",
		PnLRatio:           pos.UnrealizedPnLRatio,
		PnLUSD:             pos.UnrealizedPnL,
	}
	if !pos.IsOpen {
		out.Status = "closed"
		out.ClosedAt = closeAtMillis
		out.ExitPrice = currentPrice
	}

	computePositionValue(&out)
	if out.InitialAmount > 0 && out.Amount > 0 {
		out.RemainingRatio = out.Amount / out.InitialAmount
	}

	derivedUSD, derivedRatio := derivePnL(out.EntryPrice, out.CurrentPrice, out.Amount, out.Stake, out.Leverage, out.Side)
	baseStake := deriveBaseStake(out)
	fillPnL(&out, !pos.IsOpen, baseStake, out.PnLUSD, out.PnLRatio, derivedUSD, derivedRatio)
	return out
}

func positionHoldingMs(openedAt time.Time, now time.Time, closedAt time.Time) int64 {
	if openedAt.IsZero() {
		return 0
	}
	end := now
	if !closedAt.IsZero() {
		end = closedAt
	}
	d := end.Sub(openedAt)
	if d < 0 {
		return 0
	}
	return d.Milliseconds()
}

// remainingRatio estimates remaining position size versus initial.
// If Amount is missing but ClosedAmount exists, it derives remaining = initial - closed.
func remainingRatio(rec database.LiveOrderRecord) float64 {
	initAmt := valOrZero(rec.InitialAmount)
	if initAmt <= 0 {
		return 0
	}

	amt := valOrZero(rec.Amount)
	if amt > 0 {
		return amt / initAmt
	}

	if rec.ClosedAmount != nil {
		remaining := initAmt - valOrZero(rec.ClosedAmount)
		if remaining < 0 {
			remaining = 0
		}
		return remaining / initAmt
	}
	return 0
}
