package backtest

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"brale/internal/execution"
)

// ResultRecorder 将执行记录写入 backtest_orders 表，供 Recorder 接口复用。
type ResultRecorder struct {
	store *ResultStore
}

func NewResultRecorder(store *ResultStore) *ResultRecorder {
	return &ResultRecorder{store: store}
}

func (r *ResultRecorder) RecordOrder(ctx context.Context, order *execution.Order) (int64, error) {
	if r == nil || r.store == nil {
		return 0, fmt.Errorf("result recorder 未初始化")
	}
	if order == nil {
		return 0, fmt.Errorf("order 不能为空")
	}
	var decidedAt, executedAt time.Time
	if !order.DecidedAt.IsZero() {
		decidedAt = order.DecidedAt
	} else {
		decidedAt = time.Now()
	}
	if !order.ExecutedAt.IsZero() {
		executedAt = order.ExecutedAt
	} else {
		executedAt = decidedAt
	}
	backOrder := &Order{
		RunID:      order.RunID,
		Action:     order.Action,
		Side:       order.Side,
		Type:       order.Type,
		Price:      order.Price,
		Quantity:   order.Quantity,
		Notional:   order.Notional,
		Fee:        order.Fee,
		Timeframe:  order.Timeframe,
		DecidedAt:  decidedAt,
		ExecutedAt: executedAt,
		TakeProfit: order.TakeProfit,
		StopLoss:   order.StopLoss,
		ExpectedRR: order.ExpectedRR,
	}
	if len(order.Decision) > 0 {
		backOrder.Decision = append(json.RawMessage{}, order.Decision...)
	}
	if len(order.Meta) > 0 {
		backOrder.Meta = append(json.RawMessage{}, order.Meta...)
	}
	return r.store.InsertOrder(ctx, backOrder)
}
