package exchange

import (
	"context"
	"time"
)

type Position struct {
	ID            string
	Symbol        string
	Side          string
	Amount        float64
	InitialAmount float64
	EntryPrice    float64
	Leverage      float64
	StakeAmount   float64
	OpenedAt      time.Time
	UpdatedAt     time.Time
	IsOpen        bool

	UnrealizedPnL      float64
	UnrealizedPnLRatio float64
	CurrentPrice       float64

	StopLoss   float64
	TakeProfit float64

	Raw map[string]any
}

type Balance struct {
	StakeCurrency string
	Total         float64
	Available     float64
	Used          float64
	Wallets       map[string]float64
	UpdatedAt     time.Time
	Raw           map[string]any
}

type PriceQuote struct {
	Symbol    string
	Last      float64
	Bid       float64
	Ask       float64
	High      float64
	Low       float64
	UpdatedAt time.Time
}

type OpenRequest struct {
	Symbol      string
	Side        string
	Amount      float64
	Stake       float64
	Leverage    float64
	Price       float64
	OrderType   string
	Tag         string
	ReduceOnly  bool
	TimeInForce string
}

type CloseRequest struct {
	PositionID string
	Symbol     string
	Side       string
	Amount     float64
	Price      float64
	OrderType  string
	Reason     string
}

type OpenResult struct {
	PositionID string
	OrderID    string
}

type ManualOpenRequest struct {
	Symbol          string  `json:"symbol" form:"symbol"`
	Side            string  `json:"side" form:"side"`
	PositionSizeUSD float64 `json:"position_size_usd" form:"position_size_usd"`
	Leverage        int     `json:"leverage" form:"leverage"`
	EntryPrice      float64 `json:"entry_price" form:"entry_price"`
	StopLoss        float64 `json:"stop_loss" form:"stop_loss"`
	TakeProfit      float64 `json:"take_profit" form:"take_profit"`
	ExitCombo       string  `json:"exit_combo" form:"exit_combo"`
	Tier1Target     float64 `json:"tier1_target" form:"tier1_target"`
	Tier1Ratio      float64 `json:"tier1_ratio" form:"tier1_ratio"`
	Tier2Target     float64 `json:"tier2_target" form:"tier2_target"`
	Tier2Ratio      float64 `json:"tier2_ratio" form:"tier2_ratio"`
	Tier3Target     float64 `json:"tier3_target" form:"tier3_target"`
	Tier3Ratio      float64 `json:"tier3_ratio" form:"tier3_ratio"`
	SLTier1Target   float64 `json:"sl_tier1_target" form:"sl_tier1_target"`
	SLTier1Ratio    float64 `json:"sl_tier1_ratio" form:"sl_tier1_ratio"`
	SLTier2Target   float64 `json:"sl_tier2_target" form:"sl_tier2_target"`
	SLTier2Ratio    float64 `json:"sl_tier2_ratio" form:"sl_tier2_ratio"`
	SLTier3Target   float64 `json:"sl_tier3_target" form:"sl_tier3_target"`
	SLTier3Ratio    float64 `json:"sl_tier3_ratio" form:"sl_tier3_ratio"`
	Reason          string  `json:"reason" form:"reason"`
}

type PositionListOptions struct {
	Symbol      string
	Page        int
	PageSize    int
	Status      string
	IncludeLogs bool
	LogsLimit   int
}

type APIOrder struct {
	OrderID   string  `json:"order_id,omitempty"`
	Side      string  `json:"side,omitempty"`
	OrderType string  `json:"order_type,omitempty"`
	Tag       string  `json:"tag,omitempty"`
	Amount    float64 `json:"amount,omitempty"`
	Filled    float64 `json:"filled,omitempty"`
	Price     float64 `json:"price,omitempty"`
	Cost      float64 `json:"cost,omitempty"`
	Status    string  `json:"status,omitempty"`
	IsOpen    bool    `json:"is_open,omitempty"`
	OpenedAt  int64   `json:"opened_at,omitempty"`
	FilledAt  int64   `json:"filled_at,omitempty"`
	PnLRatio  float64 `json:"pnl_ratio,omitempty"`
	PnLUSD    float64 `json:"pnl_usd,omitempty"`
}

type APIPosition struct {
	TradeID            int        `json:"trade_id"`
	Symbol             string     `json:"symbol"`
	Side               string     `json:"side"`
	EntryPrice         float64    `json:"entry_price"`
	Amount             float64    `json:"amount"`
	InitialAmount      float64    `json:"initial_amount,omitempty"`
	Stake              float64    `json:"stake"`
	Leverage           float64    `json:"leverage"`
	PositionValue      float64    `json:"position_value,omitempty"`
	OpenedAt           int64      `json:"opened_at"`
	HoldingMs          int64      `json:"holding_ms"`
	StopLoss           float64    `json:"stop_loss,omitempty"`
	TakeProfit         float64    `json:"take_profit,omitempty"`
	CurrentPrice       float64    `json:"current_price"`
	PnLRatio           float64    `json:"pnl_ratio"`
	PnLUSD             float64    `json:"pnl_usd"`
	UnrealizedPnLRatio float64    `json:"unrealized_pnl_ratio"`
	UnrealizedPnLUSD   float64    `json:"unrealized_pnl_usd"`
	RealizedPnLRatio   float64    `json:"realized_pnl_ratio,omitempty"`
	RealizedPnLUSD     float64    `json:"realized_pnl_usd,omitempty"`
	RemainingRatio     float64    `json:"remaining_ratio"`
	Placeholder        bool       `json:"placeholder,omitempty"`
	CloseHistory       []APIOrder `json:"close_history,omitempty"`

	Status     string  `json:"status"`
	ClosedAt   int64   `json:"closed_at,omitempty"`
	ExitPrice  float64 `json:"exit_price,omitempty"`
	ExitReason string  `json:"exit_reason,omitempty"`
}

type PositionListResult struct {
	TotalCount int           `json:"total_count"`
	Page       int           `json:"page"`
	PageSize   int           `json:"page_size"`
	Positions  []APIPosition `json:"positions"`
}

type TradeEvent struct {
	FreqtradeID int            `json:"freqtrade_id"`
	Symbol      string         `json:"symbol"`
	Operation   int            `json:"operation"`
	Details     map[string]any `json:"details,omitempty"`
	Timestamp   time.Time      `json:"timestamp"`
}

type PlanUpdateHook interface {
	NotifyPlanUpdated(context.Context, int)
}

type WebhookMessage struct {
	Type        string  `json:"type"`
	TradeID     int64   `json:"trade_id"`
	Pair        string  `json:"pair"`
	Direction   string  `json:"direction"`
	Limit       float64 `json:"limit"`
	Amount      float64 `json:"amount"`
	StakeAmount float64 `json:"stake_amount"`
	OpenDate    string  `json:"open_date"`
	CloseDate   string  `json:"close_date"`
	Value       float64 `json:"value"`
	CloseRate   float64 `json:"close_rate"`
	OpenRate    float64 `json:"open_rate"`
	CurrentRate float64 `json:"current_rate"`
	ProfitRatio float64 `json:"profit_ratio"`
	ProfitAbs   float64 `json:"profit_abs"`
	ExitReason  string  `json:"exit_reason"`
	Reason      string  `json:"reason"`
	Leverage    int     `json:"leverage"`
}

type PlanStateUpdatePayload struct {
	TradeID         int    `json:"trade_id"`
	PlanID          string `json:"plan_id"`
	PlanComponent   string `json:"plan_component"`
	PlanVersion     int    `json:"plan_version"`
	StatusCode      int    `json:"status_code"`
	ParamsJSON      string `json:"params_json"`
	StateJSON       string `json:"state_json"`
	DecisionTraceID string `json:"decision_trace_id,omitempty"`
	Version         int64  `json:"version,omitempty"`
	UpdatedAt       int64  `json:"updated_at,omitempty"`
}
