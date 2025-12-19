package trader

import (
	"encoding/json"
	"time"

	"brale/internal/gateway/database"
	"brale/internal/gateway/exchange"
	"brale/internal/strategy"
	"brale/internal/strategy/exit"
)

type SignalEntryPayload struct {
	Order exchange.OpenRequest
}

type PositionOpeningPayload struct {
	TradeID   string    `json:"trade_id"`
	Symbol    string    `json:"symbol"`
	Side      string    `json:"side"`
	Stake     float64   `json:"stake"`
	Leverage  float64   `json:"leverage"`
	Amount    float64   `json:"amount"`
	Price     float64   `json:"price"`
	CreatedAt time.Time `json:"created_at"`
}

type PositionClosingPayload struct {
	TradeID    string    `json:"trade_id"`
	Symbol     string    `json:"symbol"`
	Side       string    `json:"side"`
	CreatedAt  time.Time `json:"created_at"`
	ClosePrice float64   `json:"close_price"`
}

type PositionOpenedPayload struct {
	TradeID  string
	Symbol   string
	Side     string
	Price    float64
	Amount   float64
	Stake    float64
	Leverage float64
	OpenedAt time.Time
}

type PositionClosedPayload struct {
	TradeID         string    `json:"trade_id"`
	Symbol          string    `json:"symbol"`
	Side            string    `json:"side"`
	Amount          float64   `json:"amount"`
	RemainingAmount float64   `json:"remaining_amount"`
	ClosePrice      float64   `json:"close_price"`
	Reason          string    `json:"reason"`
	PnL             float64   `json:"pnl"`
	PnLPct          float64   `json:"pnl_pct"`
	ClosedAt        time.Time `json:"closed_at"`
}

type SignalExitPayload struct {
	TradeID        int
	Symbol         string
	Side           string
	CloseRatio     float64 `json:"close_ratio,omitempty"`
	IsInitialRatio bool    `json:"is_initial_ratio,omitempty"`
}

type PriceUpdatePayload struct {
	Symbol string
	Quote  strategy.MarketQuote
}

type EventType string

const (
	EvtPriceUpdate EventType = "PRICE_UPDATE"

	EvtPositionOpening EventType = "POSITION_OPENING"

	EvtPositionClosing EventType = "POSITION_CLOSING"

	EvtSignalEntry EventType = "SIGNAL_ENTRY"

	EvtSignalExit EventType = "SIGNAL_EXIT"

	EvtManualOrder EventType = "MANUAL_ORDER"

	EvtPositionOpened EventType = "POSITION_OPENED"
	EvtPositionClosed EventType = "POSITION_CLOSED"

	EvtPlanEvent EventType = "PLAN_EVENT"

	EvtSyncPlans EventType = "SYNC_PLANS"

	EvtPlanStateUpdate EventType = "PLAN_STATE_UPDATE"

	EvtOrderResult EventType = "ORDER_RESULT"
)

type SyncPlansPayload struct {
	TradeID int                         `json:"trade_id"`
	Version int64                       `json:"version,omitempty"`
	Plans   []exit.StrategyPlanSnapshot `json:"plans"`
}

type PlanEventPayload struct {
	TradeID   int                    `json:"trade_id"`
	EventType string                 `json:"event_type"`
	Context   map[string]interface{} `json:"context"`
}

type OrderResultPayload struct {
	RequestID string
	Action    string
	Reason    string
	TradeID   string
	Symbol    string
	Side      string
	FillPrice float64
	FillSize  float64
	PnL       float64
	PnLPct    float64
	Original  interface{}
	OrderID   string
	Error     string
	Timestamp time.Time `json:"timestamp"`
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

const (
	OrderActionOpen  = "open"
	OrderActionClose = "close"
)

type EventEnvelope struct {
	ID        string
	Type      EventType
	Payload   json.RawMessage
	CreatedAt time.Time
	TradeID   int
	Symbol    string

	ReplyCh chan error `json:"-"`
}

type State struct {
	Positions map[string]*exchange.Position

	ByTradeID map[string]string

	Plans map[string][]exit.StrategyPlanSnapshot

	PlanVersions map[string]int64

	SymbolIndex map[string]string

	TradeEvents map[string][]database.TradeOperationRecord
}

func NewState() *State {
	return &State{
		Positions:    make(map[string]*exchange.Position),
		ByTradeID:    make(map[string]string),
		Plans:        make(map[string][]exit.StrategyPlanSnapshot),
		PlanVersions: make(map[string]int64),
		SymbolIndex:  make(map[string]string),
		TradeEvents:  make(map[string][]database.TradeOperationRecord),
	}
}

func (s *State) TradeIDBySymbol(symbol string) string {
	if s == nil || s.SymbolIndex == nil {
		return ""
	}
	return s.SymbolIndex[normalizeSymbol(symbol)]
}
