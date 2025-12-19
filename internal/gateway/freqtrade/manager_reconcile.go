package freqtrade

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"brale/internal/gateway/database"
	"brale/internal/gateway/exchange"
	"brale/internal/logger"
	"brale/internal/trader"
)

func (m *Manager) reconcileTradeAsync(tradeID int) {
	m.reconcileTradeAsyncWithDelay(tradeID, 0)
}

func (m *Manager) reconcileTradeAsyncWithDelay(tradeID int, delay time.Duration) {
	if m == nil || m.client == nil || m.posRepo == nil || tradeID <= 0 {
		return
	}
	go func(id int, wait time.Duration) {
		if wait > 0 {
			time.Sleep(wait)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := m.reconcileTrade(ctx, id); err != nil && !errors.Is(err, errTradeNotFound) {
			logger.Warnf("freqtrade: reconcile failed trade=%d err=%v", id, err)
		}
	}(tradeID, delay)
}

func (m *Manager) reconcileTrade(ctx context.Context, tradeID int) error {
	if m == nil || m.client == nil || m.posRepo == nil {
		return fmt.Errorf("reconcile dependencies unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	trade, err := m.client.GetOpenTrade(ctx, tradeID)
	if err != nil {
		if !errors.Is(err, errTradeNotFound) {
			return err
		}
		trade, err = m.client.GetTrade(ctx, tradeID)
		if err != nil {
			return err
		}
	}
	if trade == nil {
		return fmt.Errorf("trade %d not found", tradeID)
	}
	rec := tradeToLiveRecord(trade)
	if rec.FreqtradeID == 0 {
		rec.FreqtradeID = tradeID
	}
	if strings.TrimSpace(rec.Symbol) == "" {
		rec.Symbol = strings.ToUpper(strings.TrimSpace(trade.Pair))
	}
	return m.posRepo.SavePosition(ctx, rec)
}

func (m *Manager) reconcileExitFillWithFreqtrade(ctx context.Context, msg exchange.WebhookMessage, payload *trader.PositionClosedPayload) {
	if m == nil || m.client == nil || payload == nil {
		return
	}
	tradeID, err := strconv.Atoi(strings.TrimSpace(payload.TradeID))
	if err != nil || tradeID <= 0 {
		return
	}
	baseCtx := ctx
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	if m.syncWithOpenTrade(baseCtx, tradeID, payload) {
		return
	}
	m.fillExitFromHistory(baseCtx, tradeID, payload)
}

func (m *Manager) syncWithOpenTrade(ctx context.Context, tradeID int, payload *trader.PositionClosedPayload) bool {
	statusCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	openTrade, err := m.client.GetOpenTrade(statusCtx, tradeID)
	cancel()
	switch {
	case err == nil && openTrade != nil:
		remoteRemaining := clampAmount(openTrade.Amount)
		if math.Abs(remoteRemaining-payload.RemainingAmount) > 1e-6 {
			logger.Warnf("freqtrade: trade=%d remaining mismatch，本地=%.4f 远端=%.4f，已使用远端数据", tradeID, payload.RemainingAmount, remoteRemaining)
			payload.RemainingAmount = remoteRemaining
		}
		return true
	case err != nil && !errors.Is(err, errTradeNotFound):
		logger.Warnf("freqtrade: 查询 /status 失败 trade=%d err=%v", tradeID, err)
		return true
	default:
		return false
	}
}

func (m *Manager) fillExitFromHistory(ctx context.Context, tradeID int, payload *trader.PositionClosedPayload) {
	historyCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	trade, err := m.client.GetTrade(historyCtx, tradeID)
	cancel()
	if err != nil {
		if !errors.Is(err, errTradeNotFound) {
			logger.Warnf("freqtrade: 查询历史 trades 失败 trade=%d err=%v", tradeID, err)
		}
		return
	}
	if trade == nil {
		return
	}
	payload.RemainingAmount = 0
	payload.ClosePrice = pickClosePrice(trade)
	payload.PnL = pickPnL(trade)
	payload.PnLPct = pickPnLPct(trade)
}

func pickClosePrice(trade *Trade) float64 {
	if trade.CloseRate > 0 {
		return trade.CloseRate
	}
	return 0
}

func pickPnL(trade *Trade) float64 {
	switch {
	case trade.CloseProfitAbs != 0:
		return trade.CloseProfitAbs
	case trade.ProfitAbs != 0:
		return trade.ProfitAbs
	default:
		return 0
	}
}

func pickPnLPct(trade *Trade) float64 {
	switch {
	case trade.CloseProfit != 0:
		return trade.CloseProfit
	case trade.ProfitRatio != 0:
		return trade.ProfitRatio
	default:
		return 0
	}
}

func (m *Manager) startPending(tradeID int, stage string) {
	if tradeID <= 0 {
		return
	}
	m.pendingMu.Lock()
	defer m.pendingMu.Unlock()
	if m.pending == nil {
		m.pending = make(map[int]*pendingState)
	}
	if prev, ok := m.pending[tradeID]; ok {
		if prev.timer != nil {
			prev.timer.Stop()
		}
	}
	timer := time.AfterFunc(pendingTimeout, func() {
		m.handlePendingTimeout(tradeID, stage)
	})
	m.pending[tradeID] = &pendingState{stage: stage, timer: timer}
}

func (m *Manager) clearPending(tradeID int, stage string) {
	m.pendingMu.Lock()
	defer m.pendingMu.Unlock()
	if m.pending == nil {
		return
	}
	if ps, ok := m.pending[tradeID]; ok {
		if ps.stage == stage && ps.timer != nil {
			ps.timer.Stop()
		}
		delete(m.pending, tradeID)
	}
}

func (m *Manager) handlePendingTimeout(tradeID int, stage string) {
	switch stage {
	case pendingStageOpening:
		logger.Warnf("freqtrade: 开仓超时 trade=%d，回退状态为失败", tradeID)
		m.updateOrderStatus(tradeID, database.LiveOrderStatusRetrying)
	case pendingStageClosing:
		logger.Warnf("freqtrade: 平仓超时 trade=%d，回退状态为 open", tradeID)
		m.updateOrderStatus(tradeID, database.LiveOrderStatusOpen)
	default:
	}
	m.pendingMu.Lock()
	if m.pending != nil {
		delete(m.pending, tradeID)
	}
	m.pendingMu.Unlock()
}

func (m *Manager) updateOrderStatus(tradeID int, status database.LiveOrderStatus) {
	if m == nil || m.posStore == nil || tradeID <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.posStore.UpdateOrderStatus(ctx, tradeID, status); err != nil {
		logger.Warnf("update order status failed trade=%d status=%d err=%v", tradeID, status, err)
	}
}
