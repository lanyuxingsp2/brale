package execution

import (
	"context"
	"encoding/json"
	"time"
)

// Order 描述一条执行记录（实盘或回测共用）。
type Order struct {
	ID         int64           `json:"id"`
	RunID      string          `json:"run_id,omitempty"`
	Symbol     string          `json:"symbol"`
	Action     string          `json:"action"`
	Side       string          `json:"side"`
	Type       string          `json:"type"`
	Price      float64         `json:"price"`
	Quantity   float64         `json:"quantity"`
	Notional   float64         `json:"notional"`
	Fee        float64         `json:"fee"`
	Timeframe  string          `json:"timeframe"`
	DecidedAt  time.Time       `json:"decided_at"`
	ExecutedAt time.Time       `json:"executed_at"`
	TakeProfit float64         `json:"take_profit,omitempty"`
	StopLoss   float64         `json:"stop_loss,omitempty"`
	ExpectedRR float64         `json:"expected_rr,omitempty"`
	Decision   json.RawMessage `json:"decision,omitempty"`
	Meta       json.RawMessage `json:"meta,omitempty"`
}

// Recorder 用于落地执行记录（可由回测或实盘实现）。
type Recorder interface {
	RecordOrder(ctx context.Context, order *Order) (int64, error)
}
