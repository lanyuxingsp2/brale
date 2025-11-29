package database

import "context"

// AddOrderPnLColumns 为 live_orders 添加 pnl_ratio/pnl_usd 列（幂等）。
func (s *DecisionLogStore) AddOrderPnLColumns() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	db := s.db
	s.mu.Unlock()
	if db == nil {
		return nil
	}
	queries := []string{
		"ALTER TABLE live_orders ADD COLUMN pnl_ratio REAL DEFAULT 0",
		"ALTER TABLE live_orders ADD COLUMN pnl_usd REAL DEFAULT 0",
		"ALTER TABLE live_orders ADD COLUMN current_price REAL DEFAULT 0",
		"ALTER TABLE live_orders ADD COLUMN current_profit_ratio REAL DEFAULT 0",
		"ALTER TABLE live_orders ADD COLUMN current_profit_abs REAL DEFAULT 0",
		"ALTER TABLE live_orders ADD COLUMN unrealized_pnl_ratio REAL DEFAULT 0",
		"ALTER TABLE live_orders ADD COLUMN unrealized_pnl_usd REAL DEFAULT 0",
		"ALTER TABLE live_orders ADD COLUMN realized_pnl_ratio REAL DEFAULT 0",
		"ALTER TABLE live_orders ADD COLUMN realized_pnl_usd REAL DEFAULT 0",
		"ALTER TABLE live_orders ADD COLUMN last_status_sync INTEGER",
	}
	for _, q := range queries {
		if _, err := db.ExecContext(context.Background(), q); err != nil {
			// 忽略已存在错误
			continue
		}
	}
	return nil
}
