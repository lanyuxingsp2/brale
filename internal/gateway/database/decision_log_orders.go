package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"brale/internal/decision"
	"brale/internal/market"
)

// RecordOrder 实现 market.，写入 live_orders。
func (s *DecisionLogStore) RecordOrder(ctx context.Context, order *market.Order) (int64, error) {
	s.mu.Lock()
	db := s.db
	s.mu.Unlock()
	if db == nil {
		return 0, fmt.Errorf("decision log store 未初始化")
	}
	if order == nil {
		return 0, fmt.Errorf("order 不能为空")
	}
	symbol := strings.ToUpper(strings.TrimSpace(order.Symbol))
	if symbol == "" {
		return 0, fmt.Errorf("order.symbol 不能为空")
	}
	ts := order.DecidedAt
	if ts.IsZero() {
		ts = time.Now()
	}
	execAt := order.ExecutedAt
	if execAt.IsZero() {
		execAt = ts
	}
	now := time.Now().UnixMilli()
	decisionStr := nullIfEmptyJSON(order.Decision)
	metaStr := nullIfEmptyJSON(order.Meta)
	res, err := db.ExecContext(ctx, `
		INSERT INTO live_order_logs
			(ts, symbol, action, side, type, price, quantity, notional, fee, timeframe,
			 decided_at, executed_at, decision_json, meta_json, take_profit, stop_loss, expected_rr, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ts.UnixMilli(), symbol, order.Action, order.Side, order.Type, order.Price,
		order.Quantity, order.Notional, order.Fee, order.Timeframe, ts.UnixMilli(), execAt.UnixMilli(),
		decisionStr, metaStr, nullIfZero(order.TakeProfit), nullIfZero(order.StopLoss), nullIfZero(order.ExpectedRR), now)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return id, nil
}

// ListOrders 返回最新的 live orders。
func (s *DecisionLogStore) ListOrders(ctx context.Context, symbol string, limit int) ([]market.Order, error) {
	s.mu.Lock()
	db := s.db
	s.mu.Unlock()
	if db == nil {
		return nil, fmt.Errorf("decision log store 未初始化")
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	args := []interface{}{limit}
	query := `SELECT id, symbol, action, side, type, price, quantity, notional, fee, timeframe,
		decided_at, executed_at, decision_json, meta_json, take_profit, stop_loss, expected_rr
		FROM live_order_logs`
	if sym := strings.TrimSpace(symbol); sym != "" {
		query += " WHERE symbol = ?"
		args = append([]interface{}{strings.ToUpper(sym)}, args...)
	} else {
		query += " WHERE 1=1"
	}
	query += " ORDER BY decided_at DESC, id DESC LIMIT ?"
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []market.Order
	for rows.Next() {
		var (
			ord                  market.Order
			decided, executed    sql.NullInt64
			decisionStr, metaStr sql.NullString
			tp, sl, rr           sql.NullFloat64
		)
		if err := rows.Scan(&ord.ID, &ord.Symbol, &ord.Action, &ord.Side, &ord.Type, &ord.Price,
			&ord.Quantity, &ord.Notional, &ord.Fee, &ord.Timeframe, &decided, &executed,
			&decisionStr, &metaStr, &tp, &sl, &rr); err != nil {
			return nil, err
		}
		if decided.Valid {
			ord.DecidedAt = timeFromMillis(decided.Int64)
		}
		if executed.Valid {
			ord.ExecutedAt = timeFromMillis(executed.Int64)
		}
		if decisionStr.Valid {
			ord.Decision = json.RawMessage(decisionStr.String)
		}
		if metaStr.Valid {
			ord.Meta = json.RawMessage(metaStr.String)
		}
		if tp.Valid {
			ord.TakeProfit = tp.Float64
		}
		if sl.Valid {
			ord.StopLoss = sl.Float64
		}
		if rr.Valid {
			ord.ExpectedRR = rr.Float64
		}
		out = append(out, ord)
	}
	return out, rows.Err()
}

func nullIfEmptyJSON(data []byte) interface{} {
	if len(data) == 0 {
		return nil
	}
	return string(data)
}

func nullIfZero(v float64) interface{} {
	if v == 0 {
		return nil
	}
	return v
}

// SaveLastDecision upsert.
func (s *DecisionLogStore) SaveLastDecision(ctx context.Context, rec decision.LastDecisionRecord) error {
	s.mu.Lock()
	db := s.db
	s.mu.Unlock()
	if db == nil {
		return fmt.Errorf("decision log store 未初始化")
	}
	sym := strings.ToUpper(strings.TrimSpace(rec.Symbol))
	if sym == "" {
		return fmt.Errorf("symbol 不能为空")
	}
	decisionsJSON, err := json.Marshal(rec.Decisions)
	if err != nil {
		return err
	}
	ts := rec.DecidedAt
	if ts.IsZero() {
		ts = time.Now()
	}
	now := time.Now().UnixMilli()
	_, err = db.ExecContext(ctx, `
		INSERT INTO last_decisions(symbol, horizon, decided_at, decisions_json, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(symbol) DO UPDATE SET
			decided_at=excluded.decided_at,
			horizon=excluded.horizon,
			decisions_json=excluded.decisions_json,
			updated_at=excluded.updated_at`,
		sym, rec.Horizon, ts.UnixMilli(), string(decisionsJSON), now)
	return err
}

func (s *DecisionLogStore) LoadLastDecisions(ctx context.Context) ([]decision.LastDecisionRecord, error) {
	s.mu.Lock()
	db := s.db
	s.mu.Unlock()
	if db == nil {
		return nil, fmt.Errorf("decision log store 未初始化")
	}
	rows, err := db.QueryContext(ctx, `SELECT symbol, horizon, decided_at, decisions_json FROM last_decisions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []decision.LastDecisionRecord
	for rows.Next() {
		var sym, horizon, decisionsStr string
		var decided int64
		if err := rows.Scan(&sym, &horizon, &decided, &decisionsStr); err != nil {
			return nil, err
		}
		var decisions []decision.Decision
		if decisionsStr != "" {
			if err := json.Unmarshal([]byte(decisionsStr), &decisions); err != nil {
				continue
			}
		}
		out = append(out, decision.LastDecisionRecord{
			Symbol:    sym,
			Horizon:   horizon,
			DecidedAt: timeFromMillis(decided),
			Decisions: decisions,
		})
	}
	return out, rows.Err()
}

func timeFromMillis(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}
