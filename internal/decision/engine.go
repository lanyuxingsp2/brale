package decision

import (
	"context"

	"brale/internal/types"
)

// Context is the full input to the LLM decision engine.
// Built by scheduler each cycle from market data, positions, and profile configs.
type Context struct {
	Candidates        []string                     // Symbols to analyze this cycle
	Market            map[string]MarketData        // Real-time market snapshot per symbol
	Positions         []types.PositionSnapshot     // Currently open positions
	Account           types.AccountSnapshot        // Balance, margin, equity
	ProfilePrompts    map[string]ProfilePromptSpec // Per-symbol prompt configuration
	Prompt            PromptBundle                 // Final rendered system+user prompts
	Analysis          []AnalysisContext            // Klines, indicators, technical data
	FeatureReports    []types.FeatureReport        // Middleware feature outputs
	ExitPlanDirective string                       // Exit strategy constraints for prompt
	PreviousReasoning map[string]string            // Last cycle's reasoning per symbol
	Insights          []AgentInsight               // Multi-agent intermediate outputs
	Directives        map[string]ProfileDirective  // Symbol-specific trading rules
}

// MarketData is the point-in-time snapshot of a symbol's market state.
type MarketData struct {
	Symbol      string  `json:"symbol"`
	Price       float64 `json:"price"`
	Volume24h   float64 `json:"volume_24h"`
	OI          float64 `json:"oi"`
	FundingRate float64 `json:"funding_rate"`
	MarkPrice   float64 `json:"mark_price"`
}

// PromptBundle holds the final prompts sent to LLM.
type PromptBundle struct {
	System string
	User   string
}

// ProfilePromptSpec configures how prompts are built for each symbol/profile.
// SystemPromptsByModel allows different system prompts per LLM provider.
type ProfilePromptSpec struct {
	Profile              string
	ContextTag           string
	PromptRef            string
	SystemPromptsByModel map[string]string
	UserPrompt           string
	ExitConstraints      string
	Example              string
}

// Decider is the core decision interface.
// Takes market context, returns trading decisions. Implemented by DecisionEngine.
type Decider interface {
	Decide(ctx context.Context, input Context) (DecisionResult, error)
}
