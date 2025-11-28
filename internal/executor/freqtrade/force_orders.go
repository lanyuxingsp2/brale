package freqtrade

import (
	"context"
	"fmt"
	"strings"
	"time"

	"brale/internal/decision"
	"brale/internal/gateway/database"
	"brale/internal/logger"
)

// forceEnter/forceExit 封装 freqtrade 下单，供 Execute 使用。

func (m *Manager) forceEnter(ctx context.Context, traceID string, d decision.Decision) error {
	pair := formatFreqtradePair(d.Symbol)
	if pair == "" {
		return fmt.Errorf("unable to convert symbol %s to freqtrade pair", d.Symbol)
	}
	side := deriveSide(d.Action)
	if side == "" {
		return fmt.Errorf("unsupported action: %s", d.Action)
	}
	stake := d.PositionSizeUSD
	if stake <= 0 {
		if m.cfg.DefaultStakeUSD > 0 {
			stake = m.cfg.DefaultStakeUSD
		}
	}
	if stake <= 0 {
		return fmt.Errorf("stake amount missing")
	}
	lev := float64(d.Leverage)
	if lev <= 0 && m.cfg.DefaultLeverage > 0 {
		lev = float64(m.cfg.DefaultLeverage)
	}
	logger.Infof("freqtrade manager: ForceEnter trace=%s symbol=%s pair=%s side=%s stake=%.2f lev=%.2f", m.ensureTrace(traceID), strings.ToUpper(d.Symbol), pair, side, stake, lev)

	payload := ForceEnterPayload{
		Pair:        pair,
		Side:        side,
		StakeAmount: stake,
		EntryTag:    m.entryTag(),
	}
	if lev > 0 {
		payload.Leverage = lev
	}

	recData := map[string]any{"payload": payload, "status": "forceenter"}
	resp, err := m.client.ForceEnter(ctx, payload)
	if err != nil {
		m.logExecutor(ctx, traceID, d, 0, "forceenter_error", recData, err)
		m.discardQueuedDecision(traceID)
		m.notify("Freqtrade 建仓请求失败 ❌",
			fmt.Sprintf("标的: %s (%s)", strings.ToUpper(d.Symbol), pair),
			fmt.Sprintf("方向: %s  杠杆: x%.2f", strings.ToUpper(side), lev),
			fmt.Sprintf("仓位: %.2f USDT", stake),
			fmt.Sprintf("Trace: %s", m.ensureTrace(traceID)),
			fmt.Sprintf("错误: %v", err),
		)
		return err
	}
	m.storeTrade(d.Symbol, side, resp.TradeID)
	m.storeTrace(resp.TradeID, traceID)
	m.logExecutor(ctx, traceID, d, resp.TradeID, "forceenter_success", recData, nil)
	m.persistPendingPosition(ctx, traceID, resp.TradeID, d, side, stake, lev)
	logger.Infof("freqtrade manager: ForceEnter success trace=%s symbol=%s trade_id=%d", m.ensureTrace(traceID), strings.ToUpper(d.Symbol), resp.TradeID)

	// 交由 webhook entry_fill 同步本地仓位/tiers。
	m.notify("Freqtrade 建仓请求已发送 ✅",
		fmt.Sprintf("交易ID: %d", resp.TradeID),
		fmt.Sprintf("标的: %s (%s)", strings.ToUpper(d.Symbol), pair),
		fmt.Sprintf("方向: %s  杠杆: x%.2f", strings.ToUpper(side), lev),
		fmt.Sprintf("仓位: %.2f USDT", stake),
		fmt.Sprintf("Trace: %s", m.ensureTrace(traceID)),
	)
	return nil
}

func (m *Manager) forceExit(ctx context.Context, traceID string, d decision.Decision) error {
	side := deriveSide(d.Action)
	if side == "" {
		return fmt.Errorf("unsupported action: %s", d.Action)
	}
	tradeID, ok := m.lookupTrade(d.Symbol, side)
	if !ok {
		// freqtrade 无仓位时视为本地幽灵仓，标记已平。
		m.reconcileGhostClose(ctx, d.Symbol, side)
		return nil
	}
	logger.Infof("freqtrade manager: ForceExit trace=%s symbol=%s side=%s trade_id=%d ratio=%.4f", m.ensureTrace(traceID), strings.ToUpper(d.Symbol), side, tradeID, clampCloseRatio(d.CloseRatio))
	payload := ForceExitPayload{TradeID: fmt.Sprintf("%d", tradeID)}
	ratio := clampCloseRatio(d.CloseRatio)
	if ratio > 0 && ratio < 1 {
		if amt := m.lookupAmount(ctx, tradeID); amt > 0 {
			payload.Amount = amt * ratio
		}
	}
	recData := map[string]any{"payload": payload, "status": "forceexit", "close_ratio": d.CloseRatio}
	if err := m.client.ForceExit(ctx, payload); err != nil {
		m.logExecutor(ctx, traceID, d, tradeID, "forceexit_error", recData, err)
		m.notify("Freqtrade 平仓指令失败 ❌",
			fmt.Sprintf("交易ID: %d", tradeID),
			fmt.Sprintf("标的: %s", strings.ToUpper(d.Symbol)),
			fmt.Sprintf("方向: %s", strings.ToUpper(side)),
			fmt.Sprintf("平仓比例: %.2f", clampCloseRatio(d.CloseRatio)),
			fmt.Sprintf("Trace: %s", m.ensureTrace(traceID)),
			fmt.Sprintf("错误: %v", err),
		)
		return err
	}
	m.logExecutor(ctx, traceID, d, tradeID, "forceexit_success", recData, nil)
	if !(ratio > 0 && ratio < 1 && payload.Amount > 0) {
		m.deleteTrade(d.Symbol, side)
	}
	logger.Infof("freqtrade manager: ForceExit success trace=%s trade_id=%d ratio=%.4f", m.ensureTrace(traceID), tradeID, clampCloseRatio(d.CloseRatio))
	return nil
}

// persistPendingPosition 在发送建仓请求后立即写入 live_orders/live_tiers，标记为待确认。
func (m *Manager) persistPendingPosition(ctx context.Context, traceID string, tradeID int, d decision.Decision, side string, stake, lev float64) {
	if m == nil || m.posRepo == nil {
		return
	}
	symbol := strings.ToUpper(strings.TrimSpace(d.Symbol))
	if symbol == "" || tradeID <= 0 {
		return
	}
	side = strings.ToLower(strings.TrimSpace(side))
	now := time.Now()
	order := database.LiveOrderRecord{
		FreqtradeID:   tradeID,
		Symbol:        symbol,
		Side:          side,
		Amount:        ptrFloat(0),
		InitialAmount: ptrFloat(0),
		StakeAmount:   ptrFloat(stake),
		Leverage:      ptrFloat(lev),
		PositionValue: ptrFloat(stake * lev),
		Price:         ptrFloat(0),
		ClosedAmount:  ptrFloat(0),
		IsSimulated:   ptrBool(false),
		Status:        database.LiveOrderStatusOpening,
		StartTime:     &now,
		CreatedAt:     now,
		UpdatedAt:     now,
		RawData: marshalRaw(map[string]any{
			"event_type": "forceenter_pending",
			"trace_id":   m.ensureTrace(traceID),
			"decision":   d,
		}),
	}
	tier := buildTierFromDecision(tradeID, symbol, d, now)
	if err := m.posRepo.SavePosition(ctx, order, tier); err != nil {
		logger.Warnf("freqtrade manager: 预写入 pending 仓位失败 trade=%d err=%v", tradeID, err)
	} else {
		m.updateCacheOrderTiers(order, tier)
	}
	m.mu.Lock()
	m.positions[tradeID] = Position{
		TradeID:    tradeID,
		Symbol:     symbol,
		Side:       side,
		Stake:      stake,
		Leverage:   lev,
		OpenedAt:   now,
		EntryPrice: 0,
	}
	m.mu.Unlock()
	m.appendOperation(ctx, tradeID, symbol, database.OperationOpen, map[string]any{
		"event_type":   "PENDING_ENTRY",
		"stake_amount": stake,
		"leverage":     lev,
		"status":       statusText(database.LiveOrderStatusOpening),
	})
	logger.Infof("freqtrade manager: pending position recorded trace=%s trade_id=%d symbol=%s", m.ensureTrace(traceID), tradeID, symbol)
}

func buildTierFromDecision(tradeID int, symbol string, d decision.Decision, now time.Time) database.LiveTierRecord {
	tier := buildDefaultTiers(tradeID, strings.ToUpper(symbol))
	tier.StopLoss = d.StopLoss
	tier.TakeProfit = d.TakeProfit
	if d.Tiers != nil {
		if d.Tiers.Tier1Target > 0 {
			tier.Tier1 = d.Tiers.Tier1Target
		}
		if d.Tiers.Tier2Target > 0 {
			tier.Tier2 = d.Tiers.Tier2Target
		}
		if d.Tiers.Tier3Target > 0 {
			tier.Tier3 = d.Tiers.Tier3Target
		}
		if d.Tiers.Tier1Ratio > 0 {
			tier.Tier1Ratio = d.Tiers.Tier1Ratio
		}
		if d.Tiers.Tier2Ratio > 0 {
			tier.Tier2Ratio = d.Tiers.Tier2Ratio
		}
		if d.Tiers.Tier3Ratio > 0 {
			tier.Tier3Ratio = d.Tiers.Tier3Ratio
		}
	}
	if tier.Tier3 > 0 {
		tier.TakeProfit = tier.Tier3
	} else if tier.TakeProfit <= 0 && d.TakeProfit > 0 {
		tier.TakeProfit = d.TakeProfit
	}
	tier.IsPlaceholder = !hasCompleteTier(tier)
	tier.Source = "llm"
	tier.Reason = strings.TrimSpace(shortReason(d.Reasoning))
	tier.Timestamp = now
	tier.UpdatedAt = now
	tier.CreatedAt = now
	return tier
}
