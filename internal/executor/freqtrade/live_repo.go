package freqtrade

import (
	"context"
	"fmt"
	"strings"
	"time"

	"brale/internal/gateway/database"
	"brale/internal/logger"
)

// PositionRepo 封装 live_* 表的常用操作，方便后续逐步替换旧存储。
type PositionRepo struct {
	store database.LivePositionStore
}

func NewPositionRepo(store database.LivePositionStore) *PositionRepo {
	if store == nil {
		return nil
	}
	return &PositionRepo{store: store}
}

// UpsertOrder 写入/更新 live_orders。
func (r *PositionRepo) UpsertOrder(ctx context.Context, rec database.LiveOrderRecord) error {
	if r == nil || r.store == nil {
		return fmt.Errorf("live position store 未初始化")
	}
	logger.Infof("PositionRepo: 写入 live_orders trade=%d 标的=%s 方向=%s 状态=%s 数量=%.6f 成交价=%.4f 仓位=%.4f 杠杆=%.2f 已平仓=%.6f 未实现盈亏比例=%.4f%% 未实现盈亏金额=%.2f 已实现盈亏比例=%.4f%% 已实现盈亏金额=%.2f",
		rec.FreqtradeID, strings.ToUpper(rec.Symbol), strings.ToLower(rec.Side), statusText(rec.Status),
		valOrZero(rec.Amount), valOrZero(rec.Price), valOrZero(rec.StakeAmount), valOrZero(rec.Leverage), valOrZero(rec.ClosedAmount),
		valOrZero(rec.UnrealizedPnLRatio)*100, valOrZero(rec.UnrealizedPnLUSD),
		valOrZero(rec.RealizedPnLRatio)*100, valOrZero(rec.RealizedPnLUSD))
	return r.store.UpsertLiveOrder(ctx, rec)
}

// UpsertTiers 写入/更新 live_tiers。
func (r *PositionRepo) UpsertTiers(ctx context.Context, rec database.LiveTierRecord) error {
	if r == nil || r.store == nil {
		return fmt.Errorf("live position store 未初始化")
	}
	logger.Infof("PositionRepo: 写入 live_tiers trade=%d 标的=%s 止盈=%.4f 止损=%.4f tier1=%.4f/%.2f tier2=%.4f/%.2f tier3=%.4f/%.2f 来源=%s",
		rec.FreqtradeID, strings.ToUpper(rec.Symbol), rec.TakeProfit, rec.StopLoss,
		rec.Tier1, rec.Tier1Ratio, rec.Tier2, rec.Tier2Ratio, rec.Tier3, rec.Tier3Ratio, strings.TrimSpace(rec.Source))
	return r.store.UpsertLiveTiers(ctx, rec)
}

// SavePosition 原子写入订单与 tiers。
func (r *PositionRepo) SavePosition(ctx context.Context, order database.LiveOrderRecord, tier database.LiveTierRecord) error {
	if r == nil || r.store == nil {
		return fmt.Errorf("live position store 未初始化")
	}
	logger.Infof("PositionRepo: 原子写入订单+tiers trade=%d 标的=%s 状态=%s 止盈=%.4f 止损=%.4f 开仓价=%.4f 数量=%.6f 未实现盈亏比例=%.4f%% 未实现盈亏金额=%.2f 已实现盈亏比例=%.4f%% 已实现盈亏金额=%.2f",
		order.FreqtradeID, strings.ToUpper(order.Symbol), statusText(order.Status), tier.TakeProfit, tier.StopLoss, valOrZero(order.Price), valOrZero(order.Amount),
		valOrZero(order.UnrealizedPnLRatio)*100, valOrZero(order.UnrealizedPnLUSD),
		valOrZero(order.RealizedPnLRatio)*100, valOrZero(order.RealizedPnLUSD))
	return r.store.SavePosition(ctx, order, tier)
}

// AppendOperation 写入 trade_operation_log。
func (r *PositionRepo) AppendOperation(ctx context.Context, op database.TradeOperationRecord) {
	if r == nil || r.store == nil {
		return
	}
	logger.Infof("PositionRepo: 写入操作流水 trade=%d 标的=%s op=%d", op.FreqtradeID, strings.ToUpper(op.Symbol), op.Operation)
	_ = r.store.AppendTradeOperation(ctx, op)
}

// InsertModification 写入 live_modification_log。
func (r *PositionRepo) InsertModification(ctx context.Context, log database.TierModificationLog) {
	if r == nil || r.store == nil {
		return
	}
	logger.Infof("PositionRepo: 写入修改流水 trade=%d 字段=%d 旧值=%s 新值=%s 来源=%d 原因=%s",
		log.FreqtradeID, log.Field, strings.TrimSpace(log.OldValue), strings.TrimSpace(log.NewValue), log.Source, strings.TrimSpace(log.Reason))
	_ = r.store.InsertTierModification(ctx, log)
}

// ActivePositions 返回活跃仓位及 tier 状态。
func (r *PositionRepo) ActivePositions(ctx context.Context, limit int) ([]database.LiveOrderWithTiers, error) {
	if r == nil || r.store == nil {
		return nil, fmt.Errorf("live position store 未初始化")
	}
	return r.store.ListActivePositions(ctx, limit)
}

// RecentPositions 返回最近的仓位（含已平/在途）。
func (r *PositionRepo) RecentPositions(ctx context.Context, limit int) ([]database.LiveOrderWithTiers, error) {
	if r == nil || r.store == nil {
		return nil, fmt.Errorf("live position store 未初始化")
	}
	return r.store.ListRecentPositions(ctx, limit)
}

// RecentPositionsPaged 返回带分页的仓位列表及总数。
func (r *PositionRepo) RecentPositionsPaged(ctx context.Context, symbol string, limit int, offset int) ([]database.LiveOrderWithTiers, int, error) {
	if r == nil || r.store == nil {
		return nil, 0, fmt.Errorf("live position store 未初始化")
	}
	if limit <= 0 || limit > 500 {
		limit = 10
	}
	if offset < 0 {
		offset = 0
	}
	total, err := r.store.CountRecentPositions(ctx, symbol)
	if err != nil {
		return nil, 0, err
	}
	positions, err := r.store.ListRecentPositionsPaged(ctx, symbol, limit, offset)
	if err != nil {
		return nil, total, err
	}
	return positions, total, nil
}

// GetPosition 返回单个仓位与 tier。
func (r *PositionRepo) GetPosition(ctx context.Context, tradeID int) (database.LiveOrderRecord, database.LiveTierRecord, bool, error) {
	if r == nil || r.store == nil {
		return database.LiveOrderRecord{}, database.LiveTierRecord{}, false, fmt.Errorf("live position store 未初始化")
	}
	return r.store.GetLivePosition(ctx, tradeID)
}

// TierLogs 返回 tier 修改记录。
func (r *PositionRepo) TierLogs(ctx context.Context, tradeID int, limit int) ([]database.TierModificationLog, error) {
	if r == nil || r.store == nil {
		return nil, fmt.Errorf("live position store 未初始化")
	}
	return r.store.ListTierModifications(ctx, tradeID, limit)
}

// TradeEvents 返回操作流水。
func (r *PositionRepo) TradeEvents(ctx context.Context, tradeID int, limit int) ([]database.TradeOperationRecord, error) {
	if r == nil || r.store == nil {
		return nil, fmt.Errorf("live position store 未初始化")
	}
	return r.store.ListTradeOperations(ctx, tradeID, limit)
}

// defaultTierRatios 返回 0.33/0.33/0.34。
func defaultTierRatios() (float64, float64, float64) {
	return defaultTier1Ratio, defaultTier2Ratio, defaultTier3Ratio
}

// buildDefaultTiers 生成一个默认 tier 配置，供同步失联仓位时使用。
func buildDefaultTiers(tradeID int, symbol string) database.LiveTierRecord {
	t1, t2, t3 := defaultTierRatios()
	now := time.Now()
	return database.LiveTierRecord{
		FreqtradeID:    tradeID,
		Symbol:         symbol,
		Tier1Ratio:     t1,
		Tier2Ratio:     t2,
		Tier3Ratio:     t3,
		RemainingRatio: 1,
		Status:         0,
		Timestamp:      now,
		UpdatedAt:      now,
		CreatedAt:      now,
	}
}

func buildPlaceholderTiers(tradeID int, symbol string) database.LiveTierRecord {
	t := buildDefaultTiers(tradeID, symbol)
	t.IsPlaceholder = true
	return t
}
