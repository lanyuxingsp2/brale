package exit

import (
	"context"

	"brale/internal/decision"
	"brale/internal/gateway/database"
)

// PlanHandler implements an exit strategy type (e.g., atr_trailing, combo_group).
// Lifecycle: Validate -> Instantiate (on entry fill) -> OnPrice (each tick) -> OnAdjust (manual tweak).
type PlanHandler interface {
	ID() string

	// Validates plan params before trade entry. Called during decision validation.
	Validate(params map[string]any) error

	// Creates strategy instances after entry fill. Returns one instance per component.
	Instantiate(ctx context.Context, args InstantiateArgs) ([]PlanInstance, error)

	// Called on each price tick. Returns event if trigger condition met.
	OnPrice(ctx context.Context, inst PlanInstance, price float64) (*PlanEvent, error)

	// Handles runtime param changes (e.g., tighten stop via admin panel).
	OnAdjust(ctx context.Context, inst PlanInstance, params map[string]any) (*PlanEvent, error)
}

// StrategyStore persists exit strategy instances.
// Instances are created on entry fill, updated on trigger, finalized on exit fill.
type StrategyStore interface {
	ListStrategyInstances(ctx context.Context, tradeID int) ([]database.StrategyInstanceRecord, error)
	UpdateStrategyInstanceState(ctx context.Context, tradeID int, planID, component, stateJSON string, status database.StrategyStatus) error
	InsertStrategyInstances(ctx context.Context, recs []database.StrategyInstanceRecord) error
	ActiveTradeIDs(ctx context.Context) ([]int, error)
	InsertStrategyChangeLog(ctx context.Context, rec database.StrategyChangeLogRecord) error
}

// InstantiateArgs contains everything needed to create strategy instances.
type InstantiateArgs struct {
	TradeID       int
	PlanID        string
	PlanVersion   int
	PlanSpec      map[string]any // Params from LLM decision
	Decision      decision.Decision
	EntryPrice    float64 // From entry fill webhook
	Side          string  // "long" or "short"
	Symbol        string
	DecisionTrace string // Links to decision log for debugging
}

// PlanInstance is the runtime state of a single strategy component.
type PlanInstance struct {
	Record database.StrategyInstanceRecord
	Plan   map[string]any // Immutable params from instantiation
	State  map[string]any // Mutable state (e.g., current trailing stop level)
}

// PlanEvent is emitted when a strategy condition triggers.
type PlanEvent struct {
	TradeID       int
	PlanID        string
	PlanComponent string
	Type          string // See constants below
	Details       map[string]any
}

// FinalTakeProfit means a TP-class component triggered an exit.
// Does NOT imply the trade was profitable. Use entry/exit price for P&L.

// FinalStopLoss means a SL-class component triggered an exit.
// Does NOT imply the trade was a loss. Use entry/exit price for P&L.

const (
	PlanEventTypeTierHit    = "tier_hit"    // Multi-tier: one tier filled
	PlanEventTypeStopLoss   = "stop_loss"   // Intermediate stop adjustment
	PlanEventTypeTakeProfit = "take_profit" // Intermediate TP adjustment

	PlanEventTypeFinalStopLoss   = "final_stop_loss"   // Close position at stop
	PlanEventTypeFinalTakeProfit = "final_take_profit" // Close position at TP
	PlanEventTypeAdjust          = "plan_adjust"       // Manual param change
)
