package freqtrade

import (
	"context"
	"fmt"
	"strings"
	"time"

	"brale/internal/gateway/database"
	"brale/internal/logger"
)

const ghostCloseGrace = 15 * time.Second

func ordersEqual(a, b database.LiveOrderRecord) bool {
	return a.FreqtradeID == b.FreqtradeID &&
		strings.EqualFold(strings.TrimSpace(a.Symbol), strings.TrimSpace(b.Symbol)) &&
		strings.EqualFold(strings.TrimSpace(a.Side), strings.TrimSpace(b.Side)) &&
		a.Status == b.Status &&
		floatEqual(valOrZero(a.Amount), valOrZero(b.Amount)) &&
		floatEqual(valOrZero(a.InitialAmount), valOrZero(b.InitialAmount)) &&
		floatEqual(valOrZero(a.StakeAmount), valOrZero(b.StakeAmount)) &&
		floatEqual(valOrZero(a.Leverage), valOrZero(b.Leverage)) &&
		floatEqual(valOrZero(a.PositionValue), valOrZero(b.PositionValue)) &&
		floatEqual(valOrZero(a.Price), valOrZero(b.Price)) &&
		floatEqual(valOrZero(a.ClosedAmount), valOrZero(b.ClosedAmount))
}

func tiersEqual(a, b database.LiveTierRecord) bool {
	return a.FreqtradeID == b.FreqtradeID &&
		strings.EqualFold(strings.TrimSpace(a.Symbol), strings.TrimSpace(b.Symbol)) &&
		floatEqual(a.TakeProfit, b.TakeProfit) &&
		floatEqual(a.StopLoss, b.StopLoss) &&
		floatEqual(a.Tier1, b.Tier1) &&
		floatEqual(a.Tier1Ratio, b.Tier1Ratio) &&
		a.Tier1Done == b.Tier1Done &&
		floatEqual(a.Tier2, b.Tier2) &&
		floatEqual(a.Tier2Ratio, b.Tier2Ratio) &&
		a.Tier2Done == b.Tier2Done &&
		floatEqual(a.Tier3, b.Tier3) &&
		floatEqual(a.Tier3Ratio, b.Tier3Ratio) &&
		a.Tier3Done == b.Tier3Done &&
		floatEqual(a.RemainingRatio, b.RemainingRatio) &&
		a.Status == b.Status &&
		a.IsPlaceholder == b.IsPlaceholder
}

// SyncOpenPositions 从 freqtrade 拉取持仓，写入 live_orders/live_tiers。
func (m *Manager) SyncOpenPositions(ctx context.Context) (int, error) {
	if m == nil || m.client == nil || m.posRepo == nil {
		return 0, fmt.Errorf("freqtrade manager 未初始化")
	}
	logger.Infof("freqtrade sync: fetching trades from freqtrade...")
	trades, err := m.client.ListTrades(ctx)
	if err != nil {
		return 0, err
	}
	logger.Infof("freqtrade sync: got %d trades from freqtrade", len(trades))
	for i, tr := range trades {
		logger.Infof("freqtrade sync: trade[%d] raw=%+v", i, tr)
	}
	if len(trades) > 0 {
		m.hadTradeSnap = true
	}

	active, _ := m.posRepo.ActivePositions(ctx, 500)
	activeMap := make(map[int]database.LiveOrderWithTiers, len(active))
	for _, p := range active {
		activeMap[p.Order.FreqtradeID] = p
	}
	// 追加近期记录，允许“复活”误关的仓位。
	if recent, err := m.posRepo.RecentPositions(ctx, 500); err == nil {
		for _, p := range recent {
			if p.Order.FreqtradeID == 0 {
				continue
			}
			if _, exists := activeMap[p.Order.FreqtradeID]; !exists {
				activeMap[p.Order.FreqtradeID] = p
			}
		}
	}

	count := 0
	openSeen := false
	for _, tr := range trades {
		isOpen := tr.IsOpen || tr.Amount > 0
		if !isOpen {
			continue
		}
		openSeen = true
		m.hadOpenTrade = true
		tradeID := tr.ID
		lock := getPositionLock(tradeID)
		lock.Lock()
		func() {
			defer lock.Unlock()
			if m.hasPendingExit(tradeID) {
				logger.Debugf("freqtrade sync: skip trade=%d pending exit", tradeID)
				return
			}
			symbol := freqtradePairToSymbol(tr.Pair)
			side := strings.ToLower(strings.TrimSpace(tr.Side))
			if side == "" {
				if tr.IsShort {
					side = "short"
				} else {
					side = "long"
				}
			}
			m.storeTrade(symbol, side, tradeID)
			entry := tr.OpenRate
			now := time.Now()
			start := parseFreqtradeTime(tr.OpenDate)
		existing, existed := activeMap[tradeID]
		order := existing.Order
			if !existed {
				order = database.LiveOrderRecord{
					FreqtradeID: tradeID,
					Symbol:      strings.ToUpper(symbol),
					Side:        side,
					Status:      database.LiveOrderStatusOpen,
					CreatedAt:   now,
				}
			}
			// 始终以 freqtrade 回传方向为准，避免旧记录的 side 混淆。
			order.Side = side
			// 仅同步 freqtrade 数值字段，避免覆盖本地 tp/sl、tiers 与状态（若已存在且有持仓）。
			order.Amount = ptrFloat(tr.Amount)
			order.InitialAmount = ptrFloat(tr.Amount)
			order.StakeAmount = ptrFloat(tr.StakeAmount)
			order.Leverage = ptrFloat(tr.Leverage)
			order.PositionValue = ptrFloat(tr.StakeAmount * tr.Leverage)
			order.Price = ptrFloat(entry)
		// 若 freqtrade 仍然持仓，则重置已平仓数量。
		if tr.IsOpen {
			order.ClosedAmount = ptrFloat(0)
		} else if order.ClosedAmount == nil {
			order.ClosedAmount = ptrFloat(valOrZero(existing.Order.ClosedAmount))
		}
			if order.StartTime == nil || order.StartTime.IsZero() {
				order.StartTime = &start
			}
			if order.CreatedAt.IsZero() {
				order.CreatedAt = now
			}
		order.UpdatedAt = now
		order.RawData = marshalRaw(tr)

		if order.Status == database.LiveOrderStatusOpening && tr.IsOpen && tr.Amount > 0 {
			order.Status = database.LiveOrderStatusOpen
		}
		if order.Status == database.LiveOrderStatusClosed && tr.IsOpen && tr.Amount > 0 {
			order.Status = database.LiveOrderStatusOpen
			order.ClosedAmount = ptrFloat(0)
			if order.EndTime != nil {
				order.EndTime = nil
			}
		}
		if order.Status == 0 {
			if tr.Amount > 0 {
				order.Status = database.LiveOrderStatusOpen
			} else {
				order.Status = database.LiveOrderStatusOpening
			}
		}
		// 以 freqtrade 回传为准，确保 is_open/amount>0 一律标记为 open。
		if isOpen {
			order.Status = database.LiveOrderStatusOpen
		}

		// tiers：已有持仓时保持本地记录，不改动 tp/sl/完成状态。
		tier := buildPlaceholderTiers(tradeID, strings.ToUpper(symbol))
		if existed {
			tier = existing.Tiers
			} else if m.posRepo != nil {
				if storedOrder, storedTier, ok, err := m.posRepo.GetPosition(ctx, tradeID); err == nil && ok {
					existing = database.LiveOrderWithTiers{Order: storedOrder, Tiers: storedTier}
					existed = true
					tier = storedTier
					if storedOrder.Status != 0 && storedOrder.Status != database.LiveOrderStatusClosed {
						order.Status = storedOrder.Status
					}
					logger.Debugf("freqtrade sync: reuse stored tiers for trade=%d symbol=%s", tradeID, symbol)
				}
			}
			// 仅在本地无记录时，用 freqtrade 回传补全 tp/sl；已有仓位则不覆盖。
			if !existed && tr.TakeProfit > 0 {
				tier.TakeProfit = tr.TakeProfit
				if tier.Tier3 <= 0 {
					tier.Tier3 = tr.TakeProfit
				}
			}
			if !existed && tr.StopLoss > 0 {
				tier.StopLoss = tr.StopLoss
			}
			if !existed {
				if hasCompleteTier(tier) {
					tier.IsPlaceholder = false
				} else {
					tier.IsPlaceholder = true
				}
				tier.Timestamp = now
				tier.UpdatedAt = now
				if tier.CreatedAt.IsZero() {
					tier.CreatedAt = now
				}
			}

		logger.Debugf("freqtrade sync: upsert freqtrade trade=%d symbol=%s side=%s entry=%.4f amount=%.6f existed_local=%v tiers_complete=%v status=%s", tradeID, symbol, side, entry, tr.Amount, existed, hasCompleteTier(tier), statusText(order.Status))
		if err := m.posRepo.SavePosition(ctx, order, tier); err == nil {
			m.updateCacheOrderTiers(order, tier)
		} else {
			// 回退到非事务写入，避免因兼容问题丢失数据。
			_ = m.posRepo.UpsertOrder(ctx, order)
				_ = m.posRepo.UpsertTiers(ctx, tier)
				m.updateCacheOrderTiers(order, tier)
			}
			if !existed {
				logger.Debugf("freqtrade sync: reconcile add trade=%d symbol=%s side=%s reason=freqtrade_open_local_missing", tradeID, symbol, side)
				m.notifyReconcileAdded(order, tier)
			} else {
				logger.Debugf("freqtrade sync: no change trade=%d symbol=%s side=%s skip_op_log", tradeID, symbol, side)
			}
			if !existed {
				op := database.TradeOperationRecord{
					FreqtradeID: tradeID,
					Symbol:      strings.ToUpper(symbol),
					Operation:   database.OperationOpen,
					Details: map[string]any{
						"event_type": "SYNC_OPEN",
						"pair":       tr.Pair,
						"side":       side,
						"entry":      entry,
						"amount":     tr.Amount,
						"leverage":   tr.Leverage,
						"status":     statusText(order.Status),
					},
					Timestamp: now,
				}
				m.posRepo.AppendOperation(ctx, op)
			}

		m.mu.Lock()
		m.positions[tradeID] = Position{
			TradeID:    tradeID,
			Symbol:     symbol,
				Side:       side,
				Amount:     tr.Amount,
				Stake:      tr.StakeAmount,
				Leverage:   tr.Leverage,
				EntryPrice: entry,
			OpenedAt:   start,
		}
		m.mu.Unlock()
		delete(activeMap, tradeID)
		count++
		}()
	}

	// 启动前几秒 freqtrade 可能尚未返回完整持仓，跳过幽灵仓位自动关闭，避免误关。
	if len(trades) == 0 && time.Since(m.startedAt) < ghostCloseGrace {
		logger.Debugf("freqtrade sync: skip ghost close during startup grace trades=0")
		return count, nil
	}

	// 没有 trade 返回时，再次确认是否 API 抖动，避免误关；尚未见过任何 trade 时也跳过关闭。
	if len(trades) == 0 {
		if !m.hadTradeSnap {
			logger.Debugf("freqtrade sync: skip ghost close (no snapshot yet)")
			return count, nil
		}
		logger.Warnf("freqtrade sync: trades empty, retrying after 2s")
		time.Sleep(2 * time.Second)
		if retry, err := m.client.ListTrades(ctx); err == nil && len(retry) > 0 {
			logger.Warnf("freqtrade sync: trades empty once but returned after retry, skip ghost close this round")
			return count, nil
		}
		// 仍然为空，直接跳过幽灵关闭，等待下轮。
		logger.Warnf("freqtrade sync: trades empty after retry, skip ghost close this round")
		return count, nil
	}

	// 若本次没有任何 open 仓位，且从未见过 open 仓位，则跳过幽灵关闭。
	if !openSeen && !m.hadOpenTrade {
		logger.Debugf("freqtrade sync: skip ghost close (no open trades seen yet)")
		return count, nil
	}

	// 处理“本地有、freqtrade 无”的幽灵仓位。
	for _, ghost := range activeMap {
		tradeID := ghost.Order.FreqtradeID
		if tradeID == 0 {
			continue
		}
		if m.hasPendingExit(tradeID) {
			continue
		}
		lock := getPositionLock(tradeID)
		lock.Lock()
		func() {
			defer lock.Unlock()
			now := time.Now()
			order := ghost.Order
			order.Amount = ptrFloat(0)
			order.ClosedAmount = ptrFloat(valOrZero(ghost.Order.Amount))
			order.Status = database.LiveOrderStatusClosed
			order.EndTime = &now
			order.UpdatedAt = now

			tier := ghost.Tiers
			if tier.FreqtradeID == 0 {
				tier.FreqtradeID = tradeID
				tier.Symbol = order.Symbol
			}
			// 不动 tiers 完成状态，避免错误标记为已触发。

			// 同步删除映射，避免残留。
			m.deleteTrade(strings.ToUpper(order.Symbol), order.Side)

			_ = m.posRepo.SavePosition(ctx, order, tier)
			m.updateCacheOrderTiers(order, tier)
			logger.Debugf("freqtrade sync: reconcile close ghost trade=%d symbol=%s closed_amount=%.6f reason=freqtrade_missing", tradeID, order.Symbol, valOrZero(order.ClosedAmount))
			m.notifyReconcileClosed(order, tier)
		}()
	}

	return count, nil
}
