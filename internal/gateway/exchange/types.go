// Package exchange defines a common abstraction for trading exchanges.
// This allows the system to work with different exchange backends (Freqtrade, CCXT, etc.)
// without changing the core execution logic.
package exchange

import (
	"context"
	"time"
)

// Position represents a trading position on any exchange.
type Position struct {
	ID            string    // Exchange-specific trade/position ID
	Symbol        string    // e.g., "BTC/USDT"
	Side          string    // "long" or "short"
	Amount        float64   // Current position size
	InitialAmount float64   // Initial position size (for partial closes)
	EntryPrice    float64   // Average entry price
	Leverage      float64   // Position leverage
	StakeAmount   float64   // Amount of stake currency used
	OpenedAt      time.Time // When position was opened
	UpdatedAt     time.Time // Last update timestamp
	IsOpen        bool      // Whether position is still open

	// PnL fields
	UnrealizedPnL      float64 // Unrealized profit/loss in quote currency
	UnrealizedPnLRatio float64 // Unrealized P&L as ratio
	RealizedPnL        float64 // Realized profit/loss (for partial closes)
	RealizedPnLRatio   float64 // Realized P&L as ratio
	CurrentPrice       float64 // Current market price

	// Risk management
	StopLoss   float64 // Stop loss price (0 if not set)
	TakeProfit float64 // Take profit price (0 if not set)

	// Raw data from exchange (for debugging/logging)
	Raw map[string]any
}

// Balance represents account balance information.
type Balance struct {
	StakeCurrency string             // Primary stake currency (e.g., "USDT")
	Total         float64            // Total balance
	Available     float64            // Available for trading
	Used          float64            // Currently in use (margin, etc.)
	Wallets       map[string]float64 // Per-currency balances
	UpdatedAt     time.Time          // When balance was last updated
	Raw           map[string]any     // Raw data from exchange
}

// PriceQuote represents current price information.
type PriceQuote struct {
	Symbol    string
	Last      float64   // Last traded price
	Bid       float64   // Best bid price
	Ask       float64   // Best ask price
	High      float64   // 24h high
	Low       float64   // 24h low
	UpdatedAt time.Time // Quote timestamp
}

// OpenRequest contains parameters for opening a position.
type OpenRequest struct {
	Symbol      string  // Trading pair (e.g., "BTC/USDT")
	Side        string  // "long" or "short"
	Amount      float64 // Position size (optional, use Stake if 0)
	Stake       float64 // Stake amount in quote currency
	Leverage    float64 // Leverage to use
	Price       float64 // Limit price (0 for market order)
	OrderType   string  // "market" or "limit"
	Tag         string  // Optional entry tag/reason
	ReduceOnly  bool    // Reduce only order
	TimeInForce string  // "GTC", "IOC", "FOK"
}

// CloseRequest contains parameters for closing a position.
type CloseRequest struct {
	PositionID string  // Exchange-specific position/trade ID
	Symbol     string  // Trading pair
	Side       string  // Position side being closed
	Amount     float64 // Amount to close (0 = close all)
	Price      float64 // Limit price (0 for market order)
	OrderType  string  // "market" or "limit"
	Reason     string  // Close reason for logging
}

// OpenResult contains the result of an open position request.
type OpenResult struct {
	PositionID string // Exchange-assigned position/trade ID
	OrderID    string // Order ID if applicable
}

// ManualOpenRequest describes minimal fields for manual opening.
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
	Status      string // active|closed|all (optional)
	IncludeLogs bool
	LogsLimit   int
}

// APIPosition is a representation of a position suitable for API responses.
type APIPosition struct {
	TradeID            int     `json:"trade_id"`
	Symbol             string  `json:"symbol"`
	Side               string  `json:"side"`
	EntryPrice         float64 `json:"entry_price"`
	Amount             float64 `json:"amount"`
	InitialAmount      float64 `json:"initial_amount,omitempty"`
	Stake              float64 `json:"stake"`
	Leverage           float64 `json:"leverage"`
	PositionValue      float64 `json:"position_value,omitempty"`
	OpenedAt           int64   `json:"opened_at"`
	HoldingMs          int64   `json:"holding_ms"`
	StopLoss           float64 `json:"stop_loss,omitempty"`
	TakeProfit         float64 `json:"take_profit,omitempty"`
	CurrentPrice       float64 `json:"current_price"`
	PnLRatio           float64 `json:"pnl_ratio"`
	PnLUSD             float64 `json:"pnl_usd"`
	RealizedPnLRatio   float64 `json:"realized_pnl_ratio"`
	RealizedPnLUSD     float64 `json:"realized_pnl_usd"`
	UnrealizedPnLRatio float64 `json:"unrealized_pnl_ratio"`
	UnrealizedPnLUSD   float64 `json:"unrealized_pnl_usd"`
	RemainingRatio     float64 `json:"remaining_ratio"`
	Placeholder        bool    `json:"placeholder,omitempty"`
	// Events             []TradeEvent           `json:"events,omitempty"` // Define TradeEvent if needed
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

// PlanUpdateHook interface for receiving plan update notifications
type PlanUpdateHook interface {
	NotifyPlanUpdated(context.Context, int)
}

// WebhookMessage structure
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
	Reason      string  `json:"reason"` // fallback for exit_reason
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
