package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// LivePositionStore 描述新的仓位/tiers 存储能力。
type LivePositionStore interface {
	UpsertLiveOrder(ctx context.Context, rec LiveOrderRecord) error
	UpsertLiveTiers(ctx context.Context, rec LiveTierRecord) error
	SavePosition(ctx context.Context, order LiveOrderRecord, tier LiveTierRecord) error
	InsertTierModification(ctx context.Context, log TierModificationLog) error
	AppendTradeOperation(ctx context.Context, op TradeOperationRecord) error
	ListTierModifications(ctx context.Context, freqtradeID int, limit int) ([]TierModificationLog, error)
	ListTradeOperations(ctx context.Context, freqtradeID int, limit int) ([]TradeOperationRecord, error)
	GetLivePosition(ctx context.Context, freqtradeID int) (LiveOrderRecord, LiveTierRecord, bool, error)
	ListActivePositions(ctx context.Context, limit int) ([]LiveOrderWithTiers, error)
	ListRecentPositions(ctx context.Context, limit int) ([]LiveOrderWithTiers, error)
	ListRecentPositionsPaged(ctx context.Context, symbol string, limit int, offset int) ([]LiveOrderWithTiers, error)
	CountRecentPositions(ctx context.Context, symbol string) (int, error)
	AddOrderPnLColumns() error
}

var _ LivePositionStore = (*DecisionLogStore)(nil)

// LiveOrderStatus 对应 live_orders.status。
type LiveOrderStatus int

const (
	LiveOrderStatusOpen           LiveOrderStatus = 1
	LiveOrderStatusClosed         LiveOrderStatus = 2
	LiveOrderStatusPartial        LiveOrderStatus = 3
	LiveOrderStatusRetrying       LiveOrderStatus = 4
	LiveOrderStatusOpening        LiveOrderStatus = 5
	LiveOrderStatusClosingPartial LiveOrderStatus = 6
	LiveOrderStatusClosingFull    LiveOrderStatus = 7
)

type execContext interface {
	ExecContext(context.Context, string, ...interface{}) (sql.Result, error)
}

// LiveOrderRecord 表示需要写入/更新的 live_orders 数据。
type LiveOrderRecord struct {
	FreqtradeID        int
	Symbol             string
	Side               string
	Amount             *float64
	InitialAmount      *float64
	StakeAmount        *float64
	Leverage           *float64
	PositionValue      *float64
	Price              *float64
	ClosedAmount       *float64
	IsSimulated        *bool
	Status             LiveOrderStatus
	StartTime          *time.Time
	EndTime            *time.Time
	RawData            string
	CreatedAt          time.Time
	UpdatedAt          time.Time
	PnLRatio           *float64
	PnLUSD             *float64
	CurrentPrice       *float64
	CurrentProfitRatio *float64
	CurrentProfitAbs   *float64
	UnrealizedPnLRatio *float64
	UnrealizedPnLUSD   *float64
	RealizedPnLRatio   *float64
	RealizedPnLUSD     *float64
	LastStatusSync     *time.Time
}

// LiveTierRecord 表示 live_tiers 表的记录。
type LiveTierRecord struct {
	FreqtradeID    int
	Symbol         string
	TakeProfit     float64
	StopLoss       float64
	Tier1          float64
	Tier1Ratio     float64
	Tier1Done      bool
	Tier2          float64
	Tier2Ratio     float64
	Tier2Done      bool
	Tier3          float64
	Tier3Ratio     float64
	Tier3Done      bool
	RemainingRatio float64
	Status         int
	IsPlaceholder  bool
	Source         string
	Reason         string
	TierNotes      string
	Timestamp      time.Time
	UpdatedAt      time.Time
	CreatedAt      time.Time
}

// TierField 表示 live_modification_log.field_modified 的取值。
type TierField int

const (
	TierFieldTakeProfit TierField = 1
	TierFieldStopLoss   TierField = 2
	TierFieldTier1      TierField = 3
	TierFieldTier2      TierField = 4
	TierFieldTier3      TierField = 5
	TierFieldTier1Ratio TierField = 6
	TierFieldTier2Ratio TierField = 7
	TierFieldTier3Ratio TierField = 8
)

// TierModificationLog 封装 tier 的修改流水。
type TierModificationLog struct {
	FreqtradeID int
	Field       TierField
	OldValue    string
	NewValue    string
	Source      int
	Reason      string
	Timestamp   time.Time
}

// OperationType 对应 trade_operation_log.operation。
type OperationType int

const (
	OperationOpen        OperationType = 1
	OperationTier1       OperationType = 2
	OperationTier2       OperationType = 3
	OperationTier3       OperationType = 4
	OperationTakeProfit  OperationType = 5
	OperationStopLoss    OperationType = 6
	OperationAdjust      OperationType = 7
	OperationUpdateTiers OperationType = 8
	OperationFailed      OperationType = 10
)

// TradeOperationRecord 表示一次仓位操作流水。
type TradeOperationRecord struct {
	FreqtradeID int
	Symbol      string
	Operation   OperationType
	Details     map[string]any
	Timestamp   time.Time
}

// LiveOrderWithTiers 聚合仓位与策略。
type LiveOrderWithTiers struct {
	Order LiveOrderRecord
	Tiers LiveTierRecord
}

// UpsertLiveOrder 写入/更新 live_orders。
func (s *DecisionLogStore) upsertLiveOrderWithExec(ctx context.Context, exec execContext, rec LiveOrderRecord) error {
	s.mu.Lock()
	db := s.db
	s.mu.Unlock()
	if db == nil {
		return fmt.Errorf("decision log store 未初始化")
	}
	if rec.FreqtradeID <= 0 {
		return fmt.Errorf("freqtrade_id 必填")
	}
	symbol := strings.ToUpper(strings.TrimSpace(rec.Symbol))
	if symbol == "" {
		return fmt.Errorf("symbol 必填")
	}
	side := strings.ToLower(strings.TrimSpace(rec.Side))
	if side == "" {
		return fmt.Errorf("side 必填")
	}
	status := rec.Status
	if status == 0 {
		status = LiveOrderStatusOpen
	}
	now := time.Now()
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = now
	}
	if rec.UpdatedAt.IsZero() {
		rec.UpdatedAt = now
	}
	if rec.StartTime == nil || rec.StartTime.IsZero() {
		tmp := now
		rec.StartTime = &tmp
	}
	// Fix: Default to 0 (false) to satisfy NOT NULL constraint.
	isSim := 0
	if rec.IsSimulated != nil {
		isSim = boolToInt(*rec.IsSimulated)
	}
	amount := nullableFloat(rec.Amount)
	initialAmount := nullableFloat(rec.InitialAmount)
	stake := nullableFloat(rec.StakeAmount)
	leverage := nullableFloat(rec.Leverage)
	posVal := float64(0)
	if rec.PositionValue != nil {
		posVal = *rec.PositionValue
	}
	price := nullableFloat(rec.Price)
	closed := nullableFloat(rec.ClosedAmount)
	pnlRatio := nullableFloat(rec.PnLRatio)
	pnlUSD := nullableFloat(rec.PnLUSD)
	currentPrice := nullableFloat(rec.CurrentPrice)
	currentProfitRatio := nullableFloat(rec.CurrentProfitRatio)
	currentProfitAbs := nullableFloat(rec.CurrentProfitAbs)
	unrealizedPnLRatio := nullableFloat(rec.UnrealizedPnLRatio)
	unrealizedPnLUSD := nullableFloat(rec.UnrealizedPnLUSD)
	realizedPnLRatio := nullableFloat(rec.RealizedPnLRatio)
	realizedPnLUSD := nullableFloat(rec.RealizedPnLUSD)
	startTs := timeToMillisPtr(rec.StartTime)
	endTs := timeToMillisPtr(rec.EndTime)
	lastSync := timeToMillisPtr(rec.LastStatusSync)
	rawData := strings.TrimSpace(rec.RawData)
	if rawData == "" {
		rawData = ""
	}
	_, err := exec.ExecContext(ctx, `
		INSERT INTO live_orders
			(freqtrade_id, symbol, side, amount, initial_amount, stake_amount, leverage, position_value,
			 price, closed_amount, pnl_ratio, pnl_usd, current_price, current_profit_ratio, current_profit_abs,
			 unrealized_pnl_ratio, unrealized_pnl_usd, realized_pnl_ratio, realized_pnl_usd,
			 is_simulated, status, start_timestamp, end_timestamp, last_status_sync,
			 raw_data, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(freqtrade_id) DO UPDATE SET
			symbol=COALESCE(excluded.symbol, live_orders.symbol),
			side=COALESCE(excluded.side, live_orders.side),
			amount=COALESCE(excluded.amount, live_orders.amount),
			initial_amount=COALESCE(excluded.initial_amount, live_orders.initial_amount),
			stake_amount=COALESCE(excluded.stake_amount, live_orders.stake_amount),
			leverage=COALESCE(excluded.leverage, live_orders.leverage),
			position_value=COALESCE(excluded.position_value, live_orders.position_value),
			price=COALESCE(excluded.price, live_orders.price),
			closed_amount=COALESCE(excluded.closed_amount, live_orders.closed_amount),
			pnl_ratio=COALESCE(excluded.pnl_ratio, live_orders.pnl_ratio),
			pnl_usd=COALESCE(excluded.pnl_usd, live_orders.pnl_usd),
			current_price=COALESCE(excluded.current_price, live_orders.current_price),
			current_profit_ratio=COALESCE(excluded.current_profit_ratio, live_orders.current_profit_ratio),
			current_profit_abs=COALESCE(excluded.current_profit_abs, live_orders.current_profit_abs),
			unrealized_pnl_ratio=COALESCE(excluded.unrealized_pnl_ratio, live_orders.unrealized_pnl_ratio),
			unrealized_pnl_usd=COALESCE(excluded.unrealized_pnl_usd, live_orders.unrealized_pnl_usd),
			realized_pnl_ratio=COALESCE(excluded.realized_pnl_ratio, live_orders.realized_pnl_ratio),
			realized_pnl_usd=COALESCE(excluded.realized_pnl_usd, live_orders.realized_pnl_usd),
			is_simulated=COALESCE(excluded.is_simulated, live_orders.is_simulated),
			status=COALESCE(excluded.status, live_orders.status),
			start_timestamp=COALESCE(excluded.start_timestamp, live_orders.start_timestamp),
			end_timestamp=COALESCE(excluded.end_timestamp, live_orders.end_timestamp),
			last_status_sync=COALESCE(excluded.last_status_sync, live_orders.last_status_sync),
			raw_data=COALESCE(NULLIF(excluded.raw_data, ''), live_orders.raw_data),
			updated_at=excluded.updated_at;
	`, rec.FreqtradeID, symbol, side, amount, initialAmount, stake, leverage, posVal, price,
		closed, pnlRatio, pnlUSD, currentPrice, currentProfitRatio, currentProfitAbs,
		unrealizedPnLRatio, unrealizedPnLUSD, realizedPnLRatio, realizedPnLUSD,
		isSim, int(status), startTs, endTs, lastSync, nullIfEmptyString(rawData),
		rec.CreatedAt.UnixMilli(), rec.UpdatedAt.UnixMilli())
	return err
}

func (s *DecisionLogStore) UpsertLiveOrder(ctx context.Context, rec LiveOrderRecord) error {
	s.mu.Lock()
	db := s.db
	s.mu.Unlock()
	if db == nil {
		return fmt.Errorf("decision log store 未初始化")
	}
	return s.upsertLiveOrderWithExec(ctx, db, rec)
}

func (s *DecisionLogStore) upsertLiveTiersWithExec(ctx context.Context, exec interface {
	ExecContext(context.Context, string, ...interface{}) (sql.Result, error)
}, rec LiveTierRecord) error {
	s.mu.Lock()
	db := s.db
	s.mu.Unlock()
	if db == nil {
		return fmt.Errorf("decision log store 未初始化")
	}
	if rec.FreqtradeID <= 0 {
		return fmt.Errorf("freqtrade_id 必填")
	}
	symbol := strings.ToUpper(strings.TrimSpace(rec.Symbol))
	if symbol == "" {
		return fmt.Errorf("symbol 必填")
	}
	now := time.Now()
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = now
	}
	if rec.UpdatedAt.IsZero() {
		rec.UpdatedAt = now
	}
	if rec.Timestamp.IsZero() {
		rec.Timestamp = now
	}
	_, err := exec.ExecContext(ctx, `
		INSERT INTO live_tiers
			(freqtrade_id, symbol, take_profit, stop_loss,
			 tier1, tier1_ratio, tier1_done,
			 tier2, tier2_ratio, tier2_done,
			 tier3, tier3_ratio, tier3_done,
			 remaining_ratio, status, source, reason, tier_notes, is_placeholder, timestamp, updated_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(freqtrade_id) DO UPDATE SET
			symbol=excluded.symbol,
			take_profit=excluded.take_profit,
			stop_loss=excluded.stop_loss,
			tier1=excluded.tier1,
			tier1_ratio=excluded.tier1_ratio,
			tier1_done=excluded.tier1_done,
			tier2=excluded.tier2,
			tier2_ratio=excluded.tier2_ratio,
			tier2_done=excluded.tier2_done,
			tier3=excluded.tier3,
			tier3_ratio=excluded.tier3_ratio,
			tier3_done=excluded.tier3_done,
			remaining_ratio=excluded.remaining_ratio,
			status=excluded.status,
			source=excluded.source,
			reason=excluded.reason,
			tier_notes=excluded.tier_notes,
			is_placeholder=excluded.is_placeholder,
			timestamp=excluded.timestamp,
			updated_at=excluded.updated_at;
	`, rec.FreqtradeID, symbol,
		rec.TakeProfit, rec.StopLoss,
		rec.Tier1, rec.Tier1Ratio, boolToInt(rec.Tier1Done),
		rec.Tier2, rec.Tier2Ratio, boolToInt(rec.Tier2Done),
		rec.Tier3, rec.Tier3Ratio, boolToInt(rec.Tier3Done),
		rec.RemainingRatio, rec.Status,
		nullIfEmptyString(rec.Source), nullIfEmptyString(rec.Reason), nullIfEmptyString(rec.TierNotes), boolToInt(rec.IsPlaceholder),
		rec.Timestamp.UnixMilli(), rec.UpdatedAt.UnixMilli(), rec.CreatedAt.UnixMilli())
	return err
}

// UpsertLiveTiers 写入/更新 live_tiers。
func (s *DecisionLogStore) UpsertLiveTiers(ctx context.Context, rec LiveTierRecord) error {
	s.mu.Lock()
	db := s.db
	s.mu.Unlock()
	if db == nil {
		return fmt.Errorf("decision log store 未初始化")
	}
	return s.upsertLiveTiersWithExec(ctx, db, rec)
}

// SavePosition 在一个事务内同时写入 live_orders 和 live_tiers，保证原子性。
func (s *DecisionLogStore) SavePosition(ctx context.Context, order LiveOrderRecord, tier LiveTierRecord) error {
	s.mu.Lock()
	db := s.db
	s.mu.Unlock()
	if db == nil {
		return fmt.Errorf("decision log store 未初始化")
	}
	if order.FreqtradeID <= 0 || tier.FreqtradeID <= 0 || order.FreqtradeID != tier.FreqtradeID {
		return fmt.Errorf("freqtrade_id 不一致或缺失")
	}
	if strings.TrimSpace(tier.Symbol) == "" {
		tier.Symbol = order.Symbol
	}

	tx, err := db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if err = s.upsertLiveOrderWithExec(ctx, tx, order); err != nil {
		return err
	}
	if err = s.upsertLiveTiersWithExec(ctx, tx, tier); err != nil {
		return err
	}
	err = tx.Commit()
	return err
}

// InsertTierModification 插入 live_modification_log。
func (s *DecisionLogStore) InsertTierModification(ctx context.Context, log TierModificationLog) error {
	s.mu.Lock()
	db := s.db
	s.mu.Unlock()
	if db == nil {
		return fmt.Errorf("decision log store 未初始化")
	}
	if log.FreqtradeID <= 0 {
		return fmt.Errorf("freqtrade_id 必填")
	}
	if log.Timestamp.IsZero() {
		log.Timestamp = time.Now()
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO live_modification_log
			(freqtrade_id, field_modified, old_value, new_value, source, reason, timestamp)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		log.FreqtradeID, int(log.Field), nullIfEmptyString(log.OldValue),
		nullIfEmptyString(log.NewValue), log.Source, nullIfEmptyString(log.Reason),
		log.Timestamp.UnixMilli())
	return err
}

// AppendTradeOperation 插入 trade_operation_log。
func (s *DecisionLogStore) AppendTradeOperation(ctx context.Context, op TradeOperationRecord) error {
	s.mu.Lock()
	db := s.db
	s.mu.Unlock()
	if db == nil {
		return fmt.Errorf("decision log store 未初始化")
	}
	if op.FreqtradeID <= 0 {
		return fmt.Errorf("freqtrade_id 必填")
	}
	symbol := strings.ToUpper(strings.TrimSpace(op.Symbol))
	if symbol == "" {
		return fmt.Errorf("symbol 必填")
	}
	if op.Timestamp.IsZero() {
		op.Timestamp = time.Now()
	}
	detail := ""
	if len(op.Details) > 0 {
		if buf, err := json.Marshal(op.Details); err == nil {
			detail = string(buf)
		}
	}
	if strings.TrimSpace(detail) == "" {
		detail = "{}"
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO trade_operation_log
			(freqtrade_id, symbol, operation, details, timestamp)
		VALUES (?, ?, ?, ?, ?)`,
		op.FreqtradeID, symbol, int(op.Operation), detail, op.Timestamp.UnixMilli())
	return err
}

// ListTierModifications 返回指定仓位的修改流水（按时间倒序）。
func (s *DecisionLogStore) ListTierModifications(ctx context.Context, freqtradeID int, limit int) ([]TierModificationLog, error) {
	s.mu.Lock()
	db := s.db
	s.mu.Unlock()
	if db == nil {
		return nil, fmt.Errorf("decision log store 未初始化")
	}
	if freqtradeID <= 0 {
		return nil, fmt.Errorf("freqtrade_id 必填")
	}
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := db.QueryContext(ctx, `
		SELECT freqtrade_id, field_modified, old_value, new_value, source, reason, timestamp
		FROM live_modification_log
		WHERE freqtrade_id=?
		ORDER BY timestamp DESC
		LIMIT ?`, freqtradeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []TierModificationLog
	for rows.Next() {
		var log TierModificationLog
		var ts int64
		if err := rows.Scan(&log.FreqtradeID, &log.Field, &log.OldValue, &log.NewValue, &log.Source, &log.Reason, &ts); err != nil {
			return nil, err
		}
		log.Timestamp = time.UnixMilli(ts)
		list = append(list, log)
	}
	return list, rows.Err()
}

// ListTradeOperations 返回仓位的操作流水（按时间倒序）。
func (s *DecisionLogStore) ListTradeOperations(ctx context.Context, freqtradeID int, limit int) ([]TradeOperationRecord, error) {
	s.mu.Lock()
	db := s.db
	s.mu.Unlock()
	if db == nil {
		return nil, fmt.Errorf("decision log store 未初始化")
	}
	if freqtradeID <= 0 {
		return nil, fmt.Errorf("freqtrade_id 必填")
	}
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := db.QueryContext(ctx, `
		SELECT freqtrade_id, symbol, operation, details, timestamp
		FROM trade_operation_log
		WHERE freqtrade_id=?
		ORDER BY timestamp DESC
		LIMIT ?`, freqtradeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []TradeOperationRecord
	for rows.Next() {
		var op TradeOperationRecord
		var ts int64
		var details sql.NullString
		if err := rows.Scan(&op.FreqtradeID, &op.Symbol, &op.Operation, &details, &ts); err != nil {
			return nil, err
		}
		if details.Valid {
			_ = json.Unmarshal([]byte(details.String), &op.Details)
		}
		op.Timestamp = time.UnixMilli(ts)
		list = append(list, op)
	}
	return list, rows.Err()
}

func nullableFloat(v *float64) interface{} {
	if v == nil {
		return nil
	}
	return *v
}

func nullIfEmptyString(v string) interface{} {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	return v
}

func nullIfZeroFloat(v float64) interface{} {
	if v == 0 {
		return nil
	}
	return v
}

func timeToMillisPtr(t *time.Time) interface{} {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}

// GetLivePosition 查询单个仓位与策略。
func (s *DecisionLogStore) GetLivePosition(ctx context.Context, freqtradeID int) (LiveOrderRecord, LiveTierRecord, bool, error) {
	s.mu.Lock()
	db := s.db
	s.mu.Unlock()
	if db == nil {
		return LiveOrderRecord{}, LiveTierRecord{}, false, fmt.Errorf("decision log store 未初始化")
	}
	if freqtradeID <= 0 {
		return LiveOrderRecord{}, LiveTierRecord{}, false, fmt.Errorf("freqtrade_id 必填")
	}
	row := db.QueryRowContext(ctx, `
		SELECT o.freqtrade_id, o.symbol, o.side, o.amount, o.initial_amount, o.stake_amount, o.leverage, o.position_value,
		       o.price, o.closed_amount, o.pnl_ratio, o.pnl_usd, o.current_price, o.current_profit_ratio, o.current_profit_abs,
		       o.unrealized_pnl_ratio, o.unrealized_pnl_usd, o.realized_pnl_ratio, o.realized_pnl_usd,
		       o.is_simulated, o.status, o.start_timestamp, o.end_timestamp, o.last_status_sync, o.raw_data,
		       o.created_at, o.updated_at,
		       t.take_profit, t.stop_loss, t.tier1, t.tier1_ratio, t.tier1_done,
		       t.tier2, t.tier2_ratio, t.tier2_done, t.tier3, t.tier3_ratio, t.tier3_done,
		       t.remaining_ratio, t.status, t.source, t.reason, t.tier_notes, t.is_placeholder, t.timestamp, t.updated_at, t.created_at
		FROM live_orders o
		LEFT JOIN live_tiers t ON o.freqtrade_id = t.freqtrade_id
		WHERE o.freqtrade_id = ?`, freqtradeID)
	var (
		o                      LiveOrderRecord
		tier                   LiveTierRecord
		startTs, endTs         sql.NullInt64
		rawData                sql.NullString
		isSim                  sql.NullInt64
		tUpdated, tCreated     sql.NullInt64
		tEvent                 sql.NullInt64
		tStatus                sql.NullInt64
		tSource, tReason       sql.NullString
		tTierNotes             sql.NullString
		tPlaceholder           sql.NullInt64
		tTier1Done, tTier2Done sql.NullInt64
		tTier3Done             sql.NullInt64
		tTakeProfit, tStopLoss sql.NullFloat64
		tTier1, tTier1Ratio    sql.NullFloat64
		tTier2, tTier2Ratio    sql.NullFloat64
		tTier3, tTier3Ratio    sql.NullFloat64
		tRemain                sql.NullFloat64
		oAmount                sql.NullFloat64
		oInitial               sql.NullFloat64
		oStake                 sql.NullFloat64
		oLeverage              sql.NullFloat64
		oPosVal                sql.NullFloat64
		oPrice                 sql.NullFloat64
		oClosed                sql.NullFloat64
		oPnLRatio              sql.NullFloat64
		oPnLUSD                sql.NullFloat64
		oCurrentPrice          sql.NullFloat64
		oCurrentRatio          sql.NullFloat64
		oCurrentAbs            sql.NullFloat64
		oUnrealizedRatio       sql.NullFloat64
		oUnrealizedUSD         sql.NullFloat64
		oRealizedRatio         sql.NullFloat64
		oRealizedUSD           sql.NullFloat64
		oLastStatusSync        sql.NullInt64
		oCreated               sql.NullInt64
		oUpdated               sql.NullInt64
	)
	if err := row.Scan(
		&o.FreqtradeID, &o.Symbol, &o.Side, &oAmount, &oInitial, &oStake, &oLeverage, &oPosVal,
		&oPrice, &oClosed, &oPnLRatio, &oPnLUSD, &oCurrentPrice, &oCurrentRatio, &oCurrentAbs,
		&oUnrealizedRatio, &oUnrealizedUSD, &oRealizedRatio, &oRealizedUSD,
		&isSim, &o.Status, &startTs, &endTs, &oLastStatusSync, &rawData, &oCreated, &oUpdated,
		&tTakeProfit, &tStopLoss, &tTier1, &tTier1Ratio, &tTier1Done,
		&tTier2, &tTier2Ratio, &tTier2Done, &tTier3, &tTier3Ratio, &tTier3Done,
		&tRemain, &tStatus, &tSource, &tReason, &tTierNotes, &tPlaceholder, &tEvent, &tUpdated, &tCreated,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return LiveOrderRecord{}, LiveTierRecord{}, false, nil
		}
		return LiveOrderRecord{}, LiveTierRecord{}, false, err
	}
	if startTs.Valid {
		ts := time.UnixMilli(startTs.Int64)
		o.StartTime = &ts
	}
	if endTs.Valid {
		ts := time.UnixMilli(endTs.Int64)
		o.EndTime = &ts
	}
	if rawData.Valid {
		o.RawData = rawData.String
	}
	if oCreated.Valid {
		o.CreatedAt = time.UnixMilli(oCreated.Int64)
	}
	if oUpdated.Valid {
		o.UpdatedAt = time.UnixMilli(oUpdated.Int64)
	}
	if isSim.Valid {
		v := isSim.Int64 != 0
		o.IsSimulated = &v
	}
	if oAmount.Valid {
		v := oAmount.Float64
		o.Amount = &v
	}
	if oInitial.Valid {
		v := oInitial.Float64
		o.InitialAmount = &v
	}
	if oStake.Valid {
		v := oStake.Float64
		o.StakeAmount = &v
	}
	if oLeverage.Valid {
		v := oLeverage.Float64
		o.Leverage = &v
	}
	if oPosVal.Valid {
		v := oPosVal.Float64
		o.PositionValue = &v
	}
	if oPrice.Valid {
		v := oPrice.Float64
		o.Price = &v
	}
	if oClosed.Valid {
		v := oClosed.Float64
		o.ClosedAmount = &v
	}
	if oPnLRatio.Valid {
		v := oPnLRatio.Float64
		o.PnLRatio = &v
	}
	if oPnLUSD.Valid {
		v := oPnLUSD.Float64
		o.PnLUSD = &v
	}
	if oCurrentPrice.Valid {
		v := oCurrentPrice.Float64
		o.CurrentPrice = &v
	}
	if oCurrentRatio.Valid {
		v := oCurrentRatio.Float64
		o.CurrentProfitRatio = &v
	}
	if oCurrentAbs.Valid {
		v := oCurrentAbs.Float64
		o.CurrentProfitAbs = &v
	}
	if oUnrealizedRatio.Valid {
		v := oUnrealizedRatio.Float64
		o.UnrealizedPnLRatio = &v
	}
	if oUnrealizedUSD.Valid {
		v := oUnrealizedUSD.Float64
		o.UnrealizedPnLUSD = &v
	}
	if oRealizedRatio.Valid {
		v := oRealizedRatio.Float64
		o.RealizedPnLRatio = &v
	}
	if oRealizedUSD.Valid {
		v := oRealizedUSD.Float64
		o.RealizedPnLUSD = &v
	}
	if oLastStatusSync.Valid {
		ts := time.UnixMilli(oLastStatusSync.Int64)
		o.LastStatusSync = &ts
	}
	tier.FreqtradeID = o.FreqtradeID
	tier.Symbol = strings.ToUpper(strings.TrimSpace(o.Symbol))
	tier.TakeProfit = tTakeProfit.Float64
	tier.StopLoss = tStopLoss.Float64
	tier.Tier1 = tTier1.Float64
	tier.Tier1Ratio = tTier1Ratio.Float64
	tier.Tier1Done = tTier1Done.Valid && tTier1Done.Int64 != 0
	tier.Tier2 = tTier2.Float64
	tier.Tier2Ratio = tTier2Ratio.Float64
	tier.Tier2Done = tTier2Done.Valid && tTier2Done.Int64 != 0
	tier.Tier3 = tTier3.Float64
	tier.Tier3Ratio = tTier3Ratio.Float64
	tier.Tier3Done = tTier3Done.Valid && tTier3Done.Int64 != 0
	tier.RemainingRatio = tRemain.Float64
	if tStatus.Valid {
		tier.Status = int(tStatus.Int64)
	}
	tier.IsPlaceholder = tPlaceholder.Valid && tPlaceholder.Int64 != 0
	tier.Source = tSource.String
	tier.Reason = tReason.String
	tier.TierNotes = tTierNotes.String
	if tEvent.Valid {
		tier.Timestamp = time.UnixMilli(tEvent.Int64)
	}
	if tUpdated.Valid {
		tier.UpdatedAt = time.UnixMilli(tUpdated.Int64)
	}
	if tCreated.Valid {
		tier.CreatedAt = time.UnixMilli(tCreated.Int64)
	}
	return o, tier, true, nil
}

// ListActivePositions 列出活跃仓位（status in 1/3/4/5/6/7）。
func (s *DecisionLogStore) ListActivePositions(ctx context.Context, limit int) ([]LiveOrderWithTiers, error) {
	s.mu.Lock()
	db := s.db
	s.mu.Unlock()
	if db == nil {
		return nil, fmt.Errorf("decision log store 未初始化")
	}
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := db.QueryContext(ctx, `
		SELECT o.freqtrade_id, o.symbol, o.side, o.amount, o.initial_amount, o.stake_amount, o.leverage, o.position_value,
		       o.price, o.closed_amount, o.pnl_ratio, o.pnl_usd, o.current_price, o.current_profit_ratio, o.current_profit_abs,
		       o.unrealized_pnl_ratio, o.unrealized_pnl_usd, o.realized_pnl_ratio, o.realized_pnl_usd,
		       o.is_simulated, o.status, o.start_timestamp, o.end_timestamp, o.last_status_sync, o.raw_data,
		       o.created_at, o.updated_at,
		       t.take_profit, t.stop_loss, t.tier1, t.tier1_ratio, t.tier1_done,
		       t.tier2, t.tier2_ratio, t.tier2_done, t.tier3, t.tier3_ratio, t.tier3_done,
		       t.remaining_ratio, t.status, t.source, t.reason, t.tier_notes, t.is_placeholder, t.timestamp, t.updated_at, t.created_at
		FROM live_orders o
		LEFT JOIN live_tiers t ON o.freqtrade_id = t.freqtrade_id
		WHERE o.status IN (1,3,4,5,6,7)
		ORDER BY o.start_timestamp DESC, o.id DESC
		LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LiveOrderWithTiers
	for rows.Next() {
		rec, err := scanLivePositionRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// ListRecentPositions 返回最近的仓位（含关闭），按开仓/更新时间倒序。
func (s *DecisionLogStore) ListRecentPositions(ctx context.Context, limit int) ([]LiveOrderWithTiers, error) {
	return s.ListRecentPositionsPaged(ctx, "", limit, 0)
}

// ListRecentPositionsPaged 返回最近的仓位（含关闭），支持分页。
func (s *DecisionLogStore) ListRecentPositionsPaged(ctx context.Context, symbol string, limit int, offset int) ([]LiveOrderWithTiers, error) {
	s.mu.Lock()
	db := s.db
	s.mu.Unlock()
	if db == nil {
		return nil, fmt.Errorf("decision log store 未初始化")
	}
	if limit <= 0 || limit > 500 {
		limit = 10
	}
	if offset < 0 {
		offset = 0
	}
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	var args []interface{}
	var where strings.Builder
	where.WriteString(" WHERE 1=1")
	if symbol != "" {
		where.WriteString(" AND UPPER(o.symbol)=?")
		args = append(args, symbol)
	}
	rows, err := db.QueryContext(ctx, `
		SELECT o.freqtrade_id, o.symbol, o.side, o.amount, o.initial_amount, o.stake_amount, o.leverage, o.position_value,
		       o.price, o.closed_amount, o.pnl_ratio, o.pnl_usd, o.current_price, o.current_profit_ratio, o.current_profit_abs,
		       o.unrealized_pnl_ratio, o.unrealized_pnl_usd, o.realized_pnl_ratio, o.realized_pnl_usd,
		       o.is_simulated, o.status, o.start_timestamp, o.end_timestamp, o.last_status_sync, o.raw_data,
		       o.created_at, o.updated_at,
		       t.take_profit, t.stop_loss, t.tier1, t.tier1_ratio, t.tier1_done,
		       t.tier2, t.tier2_ratio, t.tier2_done, t.tier3, t.tier3_ratio, t.tier3_done,
		       t.remaining_ratio, t.status, t.source, t.reason, t.tier_notes, t.is_placeholder, t.timestamp, t.updated_at, t.created_at
		FROM live_orders o
		LEFT JOIN live_tiers t ON o.freqtrade_id = t.freqtrade_id`+where.String()+`
		ORDER BY COALESCE(o.end_timestamp, o.start_timestamp, o.created_at) DESC, o.id DESC
		LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LiveOrderWithTiers
	for rows.Next() {
		rec, err := scanLivePositionRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// CountRecentPositions 返回满足条件的仓位数量。
func (s *DecisionLogStore) CountRecentPositions(ctx context.Context, symbol string) (int, error) {
	s.mu.Lock()
	db := s.db
	s.mu.Unlock()
	if db == nil {
		return 0, fmt.Errorf("decision log store 未初始化")
	}
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	var args []interface{}
	var where strings.Builder
	where.WriteString(" WHERE 1=1")
	if symbol != "" {
		where.WriteString(" AND UPPER(symbol)=?")
		args = append(args, symbol)
	}
	var total int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM live_orders`+where.String(), args...).Scan(&total); err != nil {
		return 0, err
	}
	return total, nil
}

// scanLivePositionRow 复用扫描逻辑。
func scanLivePositionRow(rows *sql.Rows) (LiveOrderWithTiers, error) {
	var (
		o                      LiveOrderRecord
		tier                   LiveTierRecord
		startTs, endTs         sql.NullInt64
		rawData                sql.NullString
		isSim                  sql.NullInt64
		oCreated, oUpdated     sql.NullInt64
		tUpdated, tCreated     sql.NullInt64
		tStatus                sql.NullInt64
		tSource, tReason       sql.NullString
		tTierNotes             sql.NullString
		tPlaceholder           sql.NullInt64
		tTier1Done, tTier2Done sql.NullInt64
		tTier3Done             sql.NullInt64
		tTakeProfit, tStopLoss sql.NullFloat64
		tTier1, tTier1Ratio    sql.NullFloat64
		tTier2, tTier2Ratio    sql.NullFloat64
		tTier3, tTier3Ratio    sql.NullFloat64
		tRemain                sql.NullFloat64
		tEvent                 sql.NullInt64
		oAmount                sql.NullFloat64
		oInitial               sql.NullFloat64
		oStake                 sql.NullFloat64
		oLeverage              sql.NullFloat64
		oPosVal                sql.NullFloat64
		oPrice                 sql.NullFloat64
		oClosed                sql.NullFloat64
		oPnLRatio              sql.NullFloat64
		oPnLUSD                sql.NullFloat64
		oCurrentPrice          sql.NullFloat64
		oCurrentRatio          sql.NullFloat64
		oCurrentAbs            sql.NullFloat64
		oUnrealizedRatio       sql.NullFloat64
		oUnrealizedUSD         sql.NullFloat64
		oRealizedRatio         sql.NullFloat64
		oRealizedUSD           sql.NullFloat64
		oLastStatusSync        sql.NullInt64
	)
	if err := rows.Scan(
		&o.FreqtradeID, &o.Symbol, &o.Side, &oAmount, &oInitial, &oStake, &oLeverage, &oPosVal,
		&oPrice, &oClosed, &oPnLRatio, &oPnLUSD, &oCurrentPrice, &oCurrentRatio, &oCurrentAbs,
		&oUnrealizedRatio, &oUnrealizedUSD, &oRealizedRatio, &oRealizedUSD,
		&isSim, &o.Status, &startTs, &endTs, &oLastStatusSync, &rawData, &oCreated, &oUpdated,
		&tTakeProfit, &tStopLoss, &tTier1, &tTier1Ratio, &tTier1Done,
		&tTier2, &tTier2Ratio, &tTier2Done, &tTier3, &tTier3Ratio, &tTier3Done,
		&tRemain, &tStatus, &tSource, &tReason, &tTierNotes, &tPlaceholder, &tEvent, &tUpdated, &tCreated,
	); err != nil {
		return LiveOrderWithTiers{}, err
	}
	if startTs.Valid {
		ts := time.UnixMilli(startTs.Int64)
		o.StartTime = &ts
	}
	if endTs.Valid {
		ts := time.UnixMilli(endTs.Int64)
		o.EndTime = &ts
	}
	if rawData.Valid {
		o.RawData = rawData.String
	}
	if oCreated.Valid {
		o.CreatedAt = time.UnixMilli(oCreated.Int64)
	}
	if oUpdated.Valid {
		o.UpdatedAt = time.UnixMilli(oUpdated.Int64)
	}
	if isSim.Valid {
		v := isSim.Int64 != 0
		o.IsSimulated = &v
	}
	if oAmount.Valid {
		v := oAmount.Float64
		o.Amount = &v
	}
	if oInitial.Valid {
		v := oInitial.Float64
		o.InitialAmount = &v
	}
	if oStake.Valid {
		v := oStake.Float64
		o.StakeAmount = &v
	}
	if oLeverage.Valid {
		v := oLeverage.Float64
		o.Leverage = &v
	}
	if oPosVal.Valid {
		v := oPosVal.Float64
		o.PositionValue = &v
	}
	if oPrice.Valid {
		v := oPrice.Float64
		o.Price = &v
	}
	if oClosed.Valid {
		v := oClosed.Float64
		o.ClosedAmount = &v
	}
	if oPnLRatio.Valid {
		v := oPnLRatio.Float64
		o.PnLRatio = &v
	}
	if oPnLUSD.Valid {
		v := oPnLUSD.Float64
		o.PnLUSD = &v
	}
	if oCurrentPrice.Valid {
		v := oCurrentPrice.Float64
		o.CurrentPrice = &v
	}
	if oCurrentRatio.Valid {
		v := oCurrentRatio.Float64
		o.CurrentProfitRatio = &v
	}
	if oCurrentAbs.Valid {
		v := oCurrentAbs.Float64
		o.CurrentProfitAbs = &v
	}
	if oUnrealizedRatio.Valid {
		v := oUnrealizedRatio.Float64
		o.UnrealizedPnLRatio = &v
	}
	if oUnrealizedUSD.Valid {
		v := oUnrealizedUSD.Float64
		o.UnrealizedPnLUSD = &v
	}
	if oRealizedRatio.Valid {
		v := oRealizedRatio.Float64
		o.RealizedPnLRatio = &v
	}
	if oRealizedUSD.Valid {
		v := oRealizedUSD.Float64
		o.RealizedPnLUSD = &v
	}
	if oLastStatusSync.Valid {
		ts := time.UnixMilli(oLastStatusSync.Int64)
		o.LastStatusSync = &ts
	}
	tier.FreqtradeID = o.FreqtradeID
	tier.Symbol = strings.ToUpper(strings.TrimSpace(o.Symbol))
	tier.TakeProfit = tTakeProfit.Float64
	tier.StopLoss = tStopLoss.Float64
	tier.Tier1 = tTier1.Float64
	tier.Tier1Ratio = tTier1Ratio.Float64
	tier.Tier1Done = tTier1Done.Valid && tTier1Done.Int64 != 0
	tier.Tier2 = tTier2.Float64
	tier.Tier2Ratio = tTier2Ratio.Float64
	tier.Tier2Done = tTier2Done.Valid && tTier2Done.Int64 != 0
	tier.Tier3 = tTier3.Float64
	tier.Tier3Ratio = tTier3Ratio.Float64
	tier.Tier3Done = tTier3Done.Valid && tTier3Done.Int64 != 0
	tier.RemainingRatio = tRemain.Float64
	if tStatus.Valid {
		tier.Status = int(tStatus.Int64)
	}
	tier.IsPlaceholder = tPlaceholder.Valid && tPlaceholder.Int64 != 0
	tier.Source = tSource.String
	tier.Reason = tReason.String
	tier.TierNotes = tTierNotes.String
	if tEvent.Valid {
		tier.Timestamp = time.UnixMilli(tEvent.Int64)
	}
	if tUpdated.Valid {
		tier.UpdatedAt = time.UnixMilli(tUpdated.Int64)
	}
	if tCreated.Valid {
		tier.CreatedAt = time.UnixMilli(tCreated.Int64)
	}
	return LiveOrderWithTiers{Order: o, Tiers: tier}, nil
}
