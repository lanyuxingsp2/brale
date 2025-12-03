package freqtrade

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"brale/internal/decision"
	"brale/internal/gateway/database"
	"brale/internal/logger"
)

// handleEntry 处理 freqtrade webhook entry/entry_fill。
func (m *Manager) handleEntry(ctx context.Context, msg WebhookMessage, filled bool) {
	tradeID := int(msg.TradeID)
	lock := getPositionLock(tradeID)
	lock.Lock()
	defer lock.Unlock()

	logger.Debugf("freqtrade manager: handleEntry trade=%d filled=%v type=%s", tradeID, filled, strings.ToLower(strings.TrimSpace(msg.Type)))
	logger.Debugf("freqtrade manager: entry payload trade=%d pair=%s dir=%s amount=%.6f stake=%.4f open_rate=%.4f order_rate=%.4f lev=%.2f", tradeID, msg.Pair, msg.Direction, float64(msg.Amount), float64(msg.StakeAmount), float64(msg.OpenRate), float64(msg.OrderRate), float64(msg.Leverage))

	symbol := freqtradePairToSymbol(msg.Pair)
	side := strings.ToLower(strings.TrimSpace(msg.Direction))
	price := firstNonZero(float64(msg.OpenRate), float64(msg.OrderRate))
	amount := float64(msg.Amount)
	stake := float64(msg.StakeAmount)
	openTime := parseFreqtradeTime(msg.OpenDate)
	traceID := m.lookupTrace(tradeID)
	if traceID == "" {
		traceID = m.ensureTrace(fmt.Sprintf("%d", tradeID))
	}

	pos := Position{
		TradeID:    tradeID,
		Symbol:     symbol,
		Side:       side,
		Amount:     amount,
		Stake:      stake,
		Leverage:   float64(msg.Leverage),
		EntryPrice: price,
		OpenedAt:   openTime,
	}

	now := time.Now()

	// 仅缓存，不重复写 DB，entry_fill 后再落库。
	if _, ok := m.decisionForTrade(tradeID); !ok {
		m.bindDecisionBySymbol(tradeID, symbol, side)
	}
	m.mu.Lock()
	m.positions[tradeID] = pos
	m.mu.Unlock()
	m.storeTrade(symbol, side, tradeID)
	m.updateCacheOrderTiers(database.LiveOrderRecord{
		FreqtradeID: tradeID,
		Symbol:      strings.ToUpper(symbol),
		Side:        side,
		Amount:      ptrFloat(amount),
		StakeAmount: ptrFloat(stake),
		Leverage:    ptrFloat(float64(msg.Leverage)),
		Price:       ptrFloat(price),
		Status:      database.LiveOrderStatusOpening,
	}, database.LiveTierRecord{})

	if !filled {
		logger.Infof("freqtrade webhook entry trade=%d symbol=%s side=%s stake=%.2f amount=%.4f status=pending", tradeID, strings.ToUpper(symbol), side, stake, amount)
		m.logWebhook(ctx, traceID, tradeID, symbol, "entry", msg)
		m.notify("Freqtrade 建仓下单 ⌛",
			fmt.Sprintf("交易ID: %d", tradeID),
			fmt.Sprintf("标的: %s (%s)", strings.ToUpper(symbol), msg.Pair),
			fmt.Sprintf("方向: %s  杠杆: x%.2f", strings.ToUpper(side), float64(msg.Leverage)),
			fmt.Sprintf("委托: %.2f USDT 价格: %s", stake, formatPrice(price)),
			fmt.Sprintf("Trace: %s", traceID),
		)
		return
	}

	logger.Debugf("freqtrade manager: entry fill trade=%d 开始写入 tiers", tradeID)
	logger.Infof("freqtrade webhook entry fill trade=%d symbol=%s side=%s price=%.4f amount=%.4f", tradeID, strings.ToUpper(symbol), side, price, amount)
	m.logWebhook(ctx, traceID, tradeID, symbol, "entry_fill", msg)
	if tr, err := m.fetchTradeByID(ctx, tradeID); err == nil && tr != nil {
		sideFromAPI := strings.ToLower(strings.TrimSpace(tr.Side))
		if sideFromAPI == "" {
			if tr.IsShort {
				sideFromAPI = "short"
			} else {
				sideFromAPI = "long"
			}
		}
		if sideFromAPI != "" {
			side = sideFromAPI
		}
		if tr.OpenRate > 0 {
			price = tr.OpenRate
		}
		if tr.Amount > 0 {
			amount = tr.Amount
		}
		if tr.StakeAmount > 0 {
			stake = tr.StakeAmount
		}
		if tr.Leverage > 0 {
			msg.Leverage = numericFloat(tr.Leverage)
		}
		openTime = parseFreqtradeTime(tr.OpenDate)
	} else if err != nil {
		logger.Warnf("freqtrade manager: 获取 trade=%d 详情失败: %v", tradeID, err)
	}

	orderFilled := database.LiveOrderRecord{
		FreqtradeID:   tradeID,
		Symbol:        strings.ToUpper(symbol),
		Side:          side,
		Amount:        ptrFloat(amount),
		InitialAmount: ptrFloat(amount),
		StakeAmount:   ptrFloat(stake),
		Leverage:      ptrFloat(float64(msg.Leverage)),
		PositionValue: ptrFloat(stake * float64(msg.Leverage)),
		Price:         ptrFloat(price),
		ClosedAmount:  ptrFloat(0),
		IsSimulated:   ptrBool(false),
		Status:        database.LiveOrderStatusOpen,
		StartTime:     &openTime,
		CreatedAt:     now,
		UpdatedAt:     time.Now(),
		RawData:       marshalRaw(msg),
	}

	if m.posRepo != nil {
		if err := m.posRepo.UpsertOrder(ctx, orderFilled); err != nil {
			logger.Errorf("freqtrade manager: 写入 live_orders (entry_fill) 失败 trade=%d err=%v", tradeID, err)
		} else {
			m.updateCacheOrderTiers(orderFilled, database.LiveTierRecord{})
		}
	}

	m.mu.Lock()
	m.positions[tradeID] = Position{
		TradeID:    tradeID,
		Symbol:     symbol,
		Side:       side,
		Amount:     amount,
		Stake:      stake,
		Leverage:   float64(msg.Leverage),
		EntryPrice: price,
		OpenedAt:   openTime,
	}
	m.mu.Unlock()

	// 补齐 tiers：仅在缺少 tp/sl 时才写；有决策则用决策，否则保留占位。
	if m.posRepo == nil {
		return
	}
	_, tierRec, ok, err := m.posRepo.GetPosition(ctx, tradeID)
	if err != nil {
		logger.Warnf("freqtrade manager: 读取 position 失败 trade=%d err=%v", tradeID, err)
		return
	}
	hasTpSl := ok && tierRec.TakeProfit > 0 && tierRec.StopLoss > 0
	if hasTpSl {
		return
	}
	if dec, ok := m.decisionForTrade(tradeID); ok {
		if dec.TakeProfit <= 0 || dec.StopLoss <= 0 {
			logger.Warnf("freqtrade manager: 决策缺少 tp/sl，跳过 tier 写入 trade=%d", tradeID)
			m.forgetDecision(tradeID)
			return
		}
		_, _ = m.persistLiveTiersFromDecision(ctx, tradeID, symbol, side, price, dec.TakeProfit, dec.StopLoss, dec.Tiers, dec.Reasoning)
		m.forgetDecision(tradeID)
		return
	}
	// 无决策且缺 tp/sl：保持占位，待人工补齐。
	logger.Warnf("freqtrade manager: 缺少 tp/sl 且无决策，保留占位 tiers trade=%d", tradeID)
}

// handleExit 处理 freqtrade webhook exit/exit_fill。
func (m *Manager) handleExit(ctx context.Context, msg WebhookMessage, event string) {
	tradeID := int(msg.TradeID)
	lock := getPositionLock(tradeID)
	lock.Lock()
	defer lock.Unlock()

	logger.Debugf("freqtrade manager: handleExit trade=%d type=%s", tradeID, event)
	logger.Debugf("freqtrade manager: exit payload trade=%d pair=%s dir=%s amount=%.6f stake=%.4f close_rate=%.4f order_rate=%.4f reason=%s profit_ratio=%.6f", tradeID, msg.Pair, msg.Direction, float64(msg.Amount), float64(msg.StakeAmount), float64(msg.CloseRate), float64(msg.OrderRate), strings.TrimSpace(msg.ExitReason), parseProfitRatio(msg.ProfitRatio))

	m.mu.Lock()
	pos := m.positions[tradeID]
	m.mu.Unlock()
	symbol := strings.ToUpper(strings.TrimSpace(pos.Symbol))
	if symbol == "" {
		symbol = strings.ToUpper(freqtradePairToSymbol(msg.Pair))
	}
	side := strings.ToLower(strings.TrimSpace(pos.Side))
	if side == "" {
		side = strings.ToLower(strings.TrimSpace(msg.Direction))
	}
	closePrice := firstNonZero(float64(msg.CloseRate), float64(msg.OrderRate))
	fill := float64(msg.Amount)
	if fill < 0 {
		fill = 0
	}
	closedAt := parseFreqtradeTime(msg.CloseDate)
	traceID := m.lookupTrace(tradeID)
	m.deleteTrace(tradeID)

	pendingCopy, hasPending := m.peekPendingExit(tradeID)
	var (
		pe  pendingExit
		err error
	)
	if hasPending {
		pe = pendingCopy
		if pe.TargetPrice == 0 && closePrice > 0 {
			pe.TargetPrice = closePrice
		}
	} else {
		pe, err = m.pendingFromWebhook(ctx, tradeID, symbol, side, closePrice, fill, msg)
		if err != nil {
			logger.Warnf("freqtrade manager: handleExit build pending trade=%d err=%v", tradeID, err)
			return
		}
	}

	var snapshot *Trade
	if trade, snapErr := m.fetchTradeDetailWithRetry(ctx, tradeID); snapErr == nil && trade != nil {
		snapshot = trade
	} else if snapErr != nil && !errors.Is(snapErr, errTradeNotFound) {
		logger.Warnf("freqtrade manager: 获取 trade=%d 详情失败 err=%v", tradeID, snapErr)
	}

	orderRec, _, actualClosed, err := m.finalizePendingExit(ctx, pe, snapshot)
	if err != nil {
		logger.Errorf("freqtrade manager: 同步 exit 失败 trade=%d err=%v", tradeID, err)
		return
	}
	if hasPending {
		m.popPendingExit(tradeID)
	}

	newAmount := valOrZero(orderRec.Amount)
	m.recordExitConfirm(tradeID, newAmount)
	reason := strings.TrimSpace(msg.ExitReason)
	op := operationFromReason(reason, pe.Operation)
	m.appendOperation(ctx, tradeID, orderRec.Symbol, op, map[string]any{
		"event_type": strings.ToUpper(event),
		"price":      closePrice,
		"reason":     reason,
		"closed":     actualClosed,
		"remaining":  newAmount,
		"status":     statusText(orderRec.Status),
		"pnl_ratio":  valOrZero(orderRec.RealizedPnLRatio),
		"pnl_usd":    valOrZero(orderRec.RealizedPnLUSD),
	})

	m.logWebhook(ctx, traceID, tradeID, orderRec.Symbol, event, msg)
	m.recordOrder(ctx, msg, freqtradeAction(orderRec.Side, true), closePrice, closedAt)
	title := "Freqtrade 平仓完成 ✅"
	if orderRec.Status != database.LiveOrderStatusClosed {
		title = "Freqtrade 部分平仓确认 ✅"
	}
	m.notify(title,
		fmt.Sprintf("交易ID: %d", tradeID),
		fmt.Sprintf("标的: %s", strings.ToUpper(orderRec.Symbol)),
		fmt.Sprintf("方向: %s", strings.ToUpper(orderRec.Side)),
		fmt.Sprintf("平仓价: %s", formatPrice(closePrice)),
		fmt.Sprintf("成交数量: %s", formatQty(actualClosed)),
		fmt.Sprintf("剩余数量: %s", formatQty(newAmount)),
		fmt.Sprintf("原因: %s", reason),
		fmt.Sprintf("Trace: %s", traceID),
	)
	logger.Infof("freqtrade webhook exit trade=%d event=%s reason=%s closed=%.4f remaining=%.4f status=%s", tradeID, event, reason, actualClosed, newAmount, statusText(orderRec.Status))
}

// handleEntryCancel 处理下单被取消。
func (m *Manager) handleEntryCancel(ctx context.Context, msg WebhookMessage) {
	tradeID := int(msg.TradeID)
	lock := getPositionLock(tradeID)
	lock.Lock()
	defer lock.Unlock()

	logger.Debugf("freqtrade manager: handleEntryCancel trade=%d", tradeID)

	symbol := freqtradePairToSymbol(msg.Pair)
	side := strings.ToLower(strings.TrimSpace(msg.Direction))
	traceID := m.lookupTrace(tradeID)
	m.deleteTrace(tradeID)
	m.deleteTrade(symbol, side)

	now := time.Now()
	if m.posRepo != nil {
		rec := database.LiveOrderRecord{
			FreqtradeID: tradeID,
			Symbol:      strings.ToUpper(symbol),
			Side:        side,
			Status:      database.LiveOrderStatusClosed,
			EndTime:     &now,
			UpdatedAt:   now,
			RawData:     marshalRaw(msg),
		}
		if err := m.posRepo.UpsertOrder(ctx, rec); err != nil {
			logger.Errorf("freqtrade manager: 更新 live_orders (cancel) 失败 trade=%d err=%v", tradeID, err)
		} else {
			logger.Debugf("freqtrade manager: live_orders cancel 更新 trade=%d", tradeID)
		}
		m.appendOperation(ctx, tradeID, symbol, database.OperationFailed, map[string]any{
			"event_type": "ENTRY_CANCEL",
			"reason":     strings.TrimSpace(msg.Reason),
		})
	}

	m.mu.Lock()
	delete(m.positions, tradeID)
	m.mu.Unlock()
	logger.Infof("freqtrade manager: entry cancel trade_id=%d %s %s reason=%s", tradeID, symbol, side, msg.Reason)
	m.logWebhook(ctx, traceID, tradeID, symbol, "entry_cancel", msg)
	m.notify("Freqtrade 建仓被取消 ⚠️",
		fmt.Sprintf("交易ID: %d", tradeID),
		fmt.Sprintf("标的: %s (%s)", strings.ToUpper(symbol), msg.Pair),
		fmt.Sprintf("方向: %s", strings.ToUpper(side)),
		fmt.Sprintf("原因: %s", strings.TrimSpace(msg.Reason)),
		fmt.Sprintf("Trace: %s", traceID),
	)
}

// persistLiveTiersFromDecision 用决策/入场价初始化 live_tiers。
func (m *Manager) persistLiveTiersFromDecision(ctx context.Context, tradeID int, symbol, side string, entry, tp, sl float64, tiers *decision.DecisionTiers, reason string) (database.LiveTierRecord, error) {
	if m.posRepo == nil {
		return database.LiveTierRecord{}, fmt.Errorf("live position store 未初始化")
	}
	now := time.Now()
	tier := buildDefaultTiers(tradeID, strings.ToUpper(symbol))
	tier.StopLoss = sl

	tier.TakeProfit = tp
	if tiers != nil {
		if tiers.Tier1Target > 0 {
			tier.Tier1 = tiers.Tier1Target
		}
		if tiers.Tier2Target > 0 {
			tier.Tier2 = tiers.Tier2Target
		}
		if tiers.Tier3Target > 0 {
			tier.Tier3 = tiers.Tier3Target
			if tier.TakeProfit == 0 {
				tier.TakeProfit = tiers.Tier3Target
			}
		}
		if tiers.Tier1Ratio > 0 {
			tier.Tier1Ratio = tiers.Tier1Ratio
		}
		if tiers.Tier2Ratio > 0 {
			tier.Tier2Ratio = tiers.Tier2Ratio
		}
		if tiers.Tier3Ratio > 0 {
			tier.Tier3Ratio = tiers.Tier3Ratio
		}
	}
	if tier.Tier3 > 0 {
		tier.TakeProfit = tier.Tier3
	}
	tier.IsPlaceholder = false
	tier.UpdatedAt = now
	tier.Timestamp = now
	tier.CreatedAt = now
	if err := m.validateTierRecord(entry, side, tier, true); err != nil {
		return tier, err
	}
	if err := m.posRepo.UpsertTiers(ctx, tier); err != nil {
		logger.Errorf("freqtrade manager: 写入 live_tiers 失败 trade=%d err=%v", tradeID, err)
		return tier, err
	}
	logger.Debugf("freqtrade manager: live_tiers upsert trade=%d symbol=%s", tradeID, symbol)
	m.updateCacheOrderTiers(database.LiveOrderRecord{FreqtradeID: tradeID}, tier)
	m.recordTierInit(ctx, tradeID, tier, reason)
	return tier, nil
}
