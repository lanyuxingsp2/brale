package freqtrade

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"brale/internal/gateway/database"
	"brale/internal/logger"
)

func (m *Manager) evaluateTiers(ctx context.Context, priceFn func(symbol string) TierPriceQuote) {
	if m.posRepo == nil || priceFn == nil {
		return
	}
	positions := m.cachePositions()
	if len(positions) == 0 {
		positions = m.refreshCache(ctx)
	}
	for _, p := range positions {
		symbol := strings.ToUpper(strings.TrimSpace(p.Order.Symbol))
		if symbol == "" {
			continue
		}
		if m.hasPendingExit(p.Order.FreqtradeID) {
			continue
		}
		if p.Order.Status != database.LiveOrderStatusOpen && p.Order.Status != database.LiveOrderStatusPartial {
			continue
		}
		if p.Tiers.IsPlaceholder || !hasCompleteTier(p.Tiers) {
			continue
		}
		quote := priceFn(symbol)
		if quote.isEmpty() || quote.Last <= 0 {
			m.reportMissingPrice(symbol)
			continue
		}
		m.clearMissingPrice(symbol)
		side := strings.ToLower(strings.TrimSpace(p.Order.Side))
		lock := getPositionLock(p.Order.FreqtradeID)
		lock.Lock()
		m.evaluateOne(ctx, side, quote, p)
		lock.Unlock()
	}
}

func (m *Manager) reportMissingPrice(symbol string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.missingPrice == nil {
		m.missingPrice = make(map[string]bool)
	}
	if m.missingPrice[symbol] {
		m.mu.Unlock()
		return
	}
	m.missingPrice[symbol] = true
	m.mu.Unlock()

	logger.Warnf("自动平仓监控暂停：缺少 WSS 最新价 %s", symbol)
	m.notify("自动平仓监控暂停 ⚠️",
		fmt.Sprintf("标的: %s", symbol),
		"原因: 缺少 WSS 最新价，已暂停 tier/止损/止盈监控",
	)
}

func (m *Manager) clearMissingPrice(symbol string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.missingPrice != nil {
		delete(m.missingPrice, symbol)
	}
	m.mu.Unlock()
}

func (m *Manager) evaluateOne(ctx context.Context, side string, quote TierPriceQuote, p database.LiveOrderWithTiers) {
	stop := p.Tiers.StopLoss
	tp := p.Tiers.TakeProfit

	if price, hit := priceForStopLoss(side, quote, stop); hit && stop > 0 {
		m.startPendingClose(ctx, p, "stop_loss", price, 1, database.OperationStopLoss)
		return
	}
	if price, hit := priceForTakeProfit(side, quote, tp); hit && tp > 0 {
		m.startPendingClose(ctx, p, "take_profit", price, 1, database.OperationTakeProfit)
		return
	}

	tierName, target, ratio := nextPendingTierFromLive(p.Tiers)
	if tierName == "" || ratio <= 0 || target <= 0 {
		return
	}
	if price, hit := priceForTierTrigger(side, quote, target); hit {
		m.startPendingClose(ctx, p, tierName, price, ratio, opForTier(tierName))
	}
}

func nextPendingTierFromLive(t database.LiveTierRecord) (string, float64, float64) {
	if !t.Tier1Done && t.Tier1 > 0 {
		return "tier1", t.Tier1, ratioOrDefault(t.Tier1Ratio, defaultTier1Ratio)
	}
	if !t.Tier2Done && t.Tier2 > 0 {
		return "tier2", t.Tier2, ratioOrDefault(t.Tier2Ratio, defaultTier2Ratio)
	}
	if !t.Tier3Done && t.Tier3 > 0 {
		return "tier3", t.Tier3, ratioOrDefault(t.Tier3Ratio, defaultTier3Ratio)
	}
	return "", 0, 0
}

func opForTier(name string) database.OperationType {
	switch strings.ToLower(name) {
	case "tier1":
		return database.OperationTier1
	case "tier2":
		return database.OperationTier2
	case "tier3":
		return database.OperationTier3
	default:
		return database.OperationFailed
	}
}

// startPendingClose 在命中 tier/tp/sl 时先更新本地仓位/tiers，再触发 freqtrade ForceExit。
func (m *Manager) startPendingClose(ctx context.Context, p database.LiveOrderWithTiers, kind string, price float64, ratio float64, op database.OperationType) {
	tradeID := p.Order.FreqtradeID
	symbol := strings.ToUpper(strings.TrimSpace(p.Order.Symbol))
	side := strings.ToLower(strings.TrimSpace(p.Order.Side))
	if tradeID == 0 || symbol == "" || side == "" {
		return
	}
	if m.hasPendingExit(tradeID) {
		return
	}

	currAmount := valOrZero(p.Order.Amount)
	if currAmount <= 0 {
		return
	}

	remainingRatio := p.Tiers.RemainingRatio
	if remainingRatio <= 0 {
		remainingRatio = 1
	}

	effectiveRatio := math.Min(math.Max(ratio, 0), remainingRatio)
	if strings.EqualFold(kind, "stop_loss") || strings.EqualFold(kind, "take_profit") {
		effectiveRatio = remainingRatio
	}
	if effectiveRatio <= 0 {
		return
	}

	closeRatioOnCurrent := math.Min(1, effectiveRatio/remainingRatio)
	closeQty := currAmount * closeRatioOnCurrent
	if closeQty <= 0 {
		return
	}
	entry := valOrZero(p.Order.Price)
	now := time.Now()

	if m.posRepo == nil {
		return
	}

	tier := p.Tiers
	originalSL := tier.StopLoss
	tier.IsPlaceholder = false
	tier.Timestamp = now
	tier.UpdatedAt = now

	slAdjusted := false
	switch strings.ToLower(kind) {
	case "stop_loss":
		tier.Tier1Done, tier.Tier2Done, tier.Tier3Done = true, true, true
		tier.RemainingRatio = 0
		tier.Status = 2
	case "take_profit":
		tier.Tier1Done, tier.Tier2Done, tier.Tier3Done = true, true, true
		tier.RemainingRatio = 0
		tier.Status = 1
	case "tier1":
		tier.Tier1Done = true
		tier.Status = 3
		if entry > 0 && !floatEqual(tier.StopLoss, entry) {
			tier.StopLoss = entry
			slAdjusted = true
		}
		tier.RemainingRatio = math.Max(0, remainingRatio-effectiveRatio)
	case "tier2":
		tier.Tier2Done = true
		tier.Status = 4
		tier.RemainingRatio = math.Max(0, remainingRatio-effectiveRatio)
	case "tier3":
		tier.Tier3Done = true
		tier.Status = 5
		tier.RemainingRatio = math.Max(0, remainingRatio-effectiveRatio)
	}
	if tier.Tier3 > 0 && !floatEqual(tier.TakeProfit, tier.Tier3) {
		tier.TakeProfit = tier.Tier3
	}
	fullClose := tier.RemainingRatio <= 0.00001 || closeRatioOnCurrent >= 0.999
	if strings.EqualFold(kind, "stop_loss") || strings.EqualFold(kind, "take_profit") {
		fullClose = true
	}
	if fullClose {
		closeRatioOnCurrent = 1
		closeQty = currAmount
		tier.Tier1Done, tier.Tier2Done, tier.Tier3Done = true, true, true
		if strings.EqualFold(kind, "stop_loss") {
			tier.Status = 2
		}
		if strings.EqualFold(kind, "take_profit") {
			tier.Status = 1
		}
		tier.RemainingRatio = 0
	}

	closingStatus := database.LiveOrderStatusClosingPartial
	if fullClose {
		closingStatus = database.LiveOrderStatusClosingFull
	}

	order := database.LiveOrderRecord{
		FreqtradeID:   tradeID,
		Symbol:        symbol,
		Side:          side,
		Amount:        p.Order.Amount,
		InitialAmount: p.Order.InitialAmount,
		StakeAmount:   p.Order.StakeAmount,
		Leverage:      p.Order.Leverage,
		PositionValue: p.Order.PositionValue,
		Price:         p.Order.Price,
		ClosedAmount:  p.Order.ClosedAmount,
		IsSimulated:   p.Order.IsSimulated,
		Status:        closingStatus,
		StartTime:     p.Order.StartTime,
		EndTime:       nil,
		CreatedAt:     p.Order.CreatedAt,
		UpdatedAt:     now,
		RawData:       p.Order.RawData,
	}
	if order.CreatedAt.IsZero() {
		order.CreatedAt = now
	}
	if order.StartTime == nil || order.StartTime.IsZero() {
		tmp := now
		order.StartTime = &tmp
	}
	if tier.FreqtradeID == 0 {
		tier.FreqtradeID = tradeID
	}
	if strings.TrimSpace(tier.Symbol) == "" {
		tier.Symbol = symbol
	}

	if err := m.posRepo.SavePosition(ctx, order, tier); err != nil {
		m.appendOperation(ctx, tradeID, symbol, database.OperationFailed, map[string]any{
			"event_type": strings.ToUpper(kind),
			"price":      price,
			"ratio":      effectiveRatio,
			"error":      err.Error(),
		})
		m.notify("自动平仓写入失败 ❌",
			fmt.Sprintf("交易ID: %d", tradeID),
			fmt.Sprintf("标的: %s", symbol),
			fmt.Sprintf("事件: %s", strings.ToUpper(kind)),
			fmt.Sprintf("错误: %v", err),
		)
		return
	}
	logger.Infof("freqtrade tier trigger trade=%d symbol=%s kind=%s price=%.4f ratio=%.4f close_qty=%.4f", tradeID, symbol, kind, price, effectiveRatio, closeQty)

	if slAdjusted {
		m.posRepo.InsertModification(ctx, database.TierModificationLog{
			FreqtradeID: tradeID,
			Field:       database.TierFieldStopLoss,
			OldValue:    formatPrice(originalSL),
			NewValue:    formatPrice(tier.StopLoss),
			Source:      3,
			Reason:      "价格到达 tier1，上移止损到入场价",
			Timestamp:   now,
		})
	}

	m.updateCacheOrderTiers(order, tier)
	expectedAmount := math.Max(0, currAmount-closeQty)
	m.addPendingExit(pendingExit{
		TradeID:        tradeID,
		Symbol:         symbol,
		Side:           side,
		Kind:           kind,
		TargetPrice:    price,
		Ratio:          effectiveRatio,
		PrevAmount:     currAmount,
		PrevClosed:     valOrZero(p.Order.ClosedAmount),
		ExpectedAmount: expectedAmount,
		RequestedAt:    now,
		Operation:      op,
		EntryPrice:     entry,
		Stake:          valOrZero(p.Order.StakeAmount),
		Leverage:       valOrZero(p.Order.Leverage),
	})
	m.appendOperation(ctx, tradeID, symbol, op, map[string]any{
		"event_type":       "CLOSING_" + strings.ToUpper(kind),
		"price":            price,
		"close_ratio":      effectiveRatio,
		"close_quantity":   closeQty,
		"expected_amount":  expectedAmount,
		"remaining_ratio":  tier.RemainingRatio,
		"status":           statusText(order.Status),
		"side":             side,
		"stake":            valOrZero(p.Order.StakeAmount),
		"leverage":         valOrZero(p.Order.Leverage),
		"entry_price":      entry,
		"take_profit":      tier.TakeProfit,
		"stop_loss":        tier.StopLoss,
		"tier1/2/3_done":   fmt.Sprintf("%v/%v/%v", tier.Tier1Done, tier.Tier2Done, tier.Tier3Done),
		"tier1/2/3_target": fmt.Sprintf("%.4f/%.4f/%.4f", tier.Tier1, tier.Tier2, tier.Tier3),
	})

	payload := ForceExitPayload{TradeID: fmt.Sprintf("%d", tradeID)}
	if closingStatus == database.LiveOrderStatusClosingPartial && closeQty > 0 {
		payload.Amount = closeQty
	}
	if m.client != nil {
		if err := m.client.ForceExit(ctx, payload); err != nil {
			m.appendOperation(ctx, tradeID, symbol, database.OperationFailed, map[string]any{
				"event_type": strings.ToUpper(kind),
				"error":      err.Error(),
				"amount":     closeQty,
			})
			m.notify("自动平仓指令失败 ❌",
				fmt.Sprintf("交易ID: %d", tradeID),
				fmt.Sprintf("标的: %s", symbol),
				fmt.Sprintf("事件: %s", strings.ToUpper(kind)),
				fmt.Sprintf("错误: %v", err),
			)
			return
		}
	}
}

// autoPartial 逻辑改为 pending，由 startPendingClose 触发。
