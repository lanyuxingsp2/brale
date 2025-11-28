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

// UpdateTiersManual 供后台/HTTP 直接修改 tier。
func (m *Manager) UpdateTiersManual(ctx context.Context, req TierUpdateRequest) error {
	if m == nil {
		return fmt.Errorf("freqtrade manager 未初始化")
	}
	symbol := strings.ToUpper(strings.TrimSpace(req.Symbol))
	if symbol == "" {
		return fmt.Errorf("symbol 不能为空")
	}
	logger.Infof("freqtrade manual tier update trade_id=%d symbol=%s reason=%s", req.TradeID, symbol, strings.TrimSpace(req.Reason))
	d := decision.Decision{
		Symbol:     symbol,
		Action:     "update_tiers",
		Reasoning:  strings.TrimSpace(req.Reason),
		StopLoss:   req.StopLoss,
		TakeProfit: req.TakeProfit,
		Tiers: &decision.DecisionTiers{
			Tier1Target: req.Tier1Target,
			Tier1Ratio:  req.Tier1Ratio,
			Tier2Target: req.Tier2Target,
			Tier2Ratio:  req.Tier2Ratio,
			Tier3Target: req.Tier3Target,
			Tier3Ratio:  req.Tier3Ratio,
		},
	}
	traceID := fmt.Sprintf("manual-%d", time.Now().UnixNano())
	return m.updateTiers(ctx, traceID, d, false)
}

// ListTierLogs 读取 live_modification_log。
func (m *Manager) ListTierLogs(ctx context.Context, tradeID int, limit int) ([]database.TierModificationLog, error) {
	if m == nil || m.posRepo == nil {
		return nil, fmt.Errorf("freqtrade manager 未初始化")
	}
	return m.posRepo.TierLogs(ctx, tradeID, limit)
}

// ListTradeEvents 读取 trade_operation_log。
func (m *Manager) ListTradeEvents(ctx context.Context, tradeID int, limit int) ([]database.TradeOperationRecord, error) {
	if m == nil || m.posRepo == nil {
		return nil, fmt.Errorf("freqtrade manager 未初始化")
	}
	return m.posRepo.TradeEvents(ctx, tradeID, limit)
}
