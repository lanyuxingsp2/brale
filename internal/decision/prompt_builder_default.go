package decision

import (
	"context"
	"fmt"
	"strings"

	"brale/internal/decision/render"
	"brale/internal/gateway/provider"
	"brale/internal/market"
	"brale/internal/strategy"
	"brale/internal/types"
)

// DefaultPromptBuilder is the production prompt builder used by DecisionEngine.
// It assembles the user summary (account/positions/klines/agents/constraints) and returns optional images.
type DefaultPromptBuilder struct {
	PromptMgr             *strategy.Manager
	Store                 market.KlineStore
	Metrics               *market.MetricsService
	Sentiment             *market.SentimentService
	FearGreed             *market.FearGreedService
	Intervals             []string
	DebugStructuredBlocks bool
}

func NewDefaultPromptBuilder(promptMgr *strategy.Manager, store market.KlineStore, metrics *market.MetricsService, sentiment *market.SentimentService, fearGreed *market.FearGreedService, intervals []string, debug bool) *DefaultPromptBuilder {
	out := &DefaultPromptBuilder{
		PromptMgr:             promptMgr,
		Store:                 store,
		Metrics:               metrics,
		Sentiment:             sentiment,
		FearGreed:             fearGreed,
		DebugStructuredBlocks: debug,
	}
	if len(intervals) > 0 {
		out.Intervals = append([]string(nil), intervals...)
	}
	return out
}

func (b *DefaultPromptBuilder) Build(ctx context.Context, input Context, insights []AgentInsight) (string, string, []provider.ImagePayload, error) {
	system := strings.TrimSpace(input.Prompt.System)
	userSummary := strings.TrimSpace(b.buildUserSummary(ctx, input, insights))
	userExtra := strings.TrimSpace(input.Prompt.User)

	var user string
	switch {
	case userSummary != "" && userExtra != "":
		user = userSummary + "\n\n" + userExtra
	case userSummary != "":
		user = userSummary
	default:
		user = userExtra
	}

	images := b.collectVisionPayloads(input.Analysis)
	return system, user, images, nil
}

func (b *DefaultPromptBuilder) buildUserSummary(ctx context.Context, input Context, insights []AgentInsight) string {
	b.refreshFearGreedOnDemand(ctx, input.Candidates, input.Directives)

	ds := b.buildDerivativesSection(ctx, input.Analysis, input.Directives)
	input.DataAgeSec = mergeDataAgeSec(input.DataAgeSec, ds.DataAgeSec)
	liqClose := computeLiquidationClose(input.Positions)
	if ds.LeverageCrowded && liqClose && (ds.FundingStressed || ds.PriceTrigger) {
		input.HardFlags.LiqRiskFlag = true
	}

	sections := render.Sections{
		Header:            b.renderHeader(input),
		Account:           b.renderAccountOverview(input.Account, augmentMarketData(input.Market, input.Analysis)),
		Previous:          b.renderPreviousReasoning(input.PreviousReasoning),
		PreviousProviders: b.renderPreviousProviderOutputs(input.PreviousProviderOutputs),
		Derivatives:       "", // provider 阶段无需在主 prompt 展示衍生品数据
		Positions:         b.renderPositionDetails(filterPositions(input.Positions, input.Candidates)),
		Klines:            b.renderKlineWindows(input.Analysis, input.Directives),
		Agents:            b.renderAgentBlocks(insights),
		Guidelines:        b.renderOutputConstraints(input),
	}

	var loader render.TemplateLoader
	if b.PromptMgr != nil {
		loader = b.PromptMgr
	}
	summary := render.RenderSummary(loader, sections)
	logStructuredBlocksDebug(b.DebugStructuredBlocks, input.Analysis)
	return summary
}

func (b *DefaultPromptBuilder) refreshFearGreedOnDemand(ctx context.Context, symbols []string, directives map[string]ProfileDirective) {
	if b == nil || b.FearGreed == nil || len(symbols) == 0 || len(directives) == 0 {
		return
	}
	for _, sym := range symbols {
		dir, ok := lookupDirective(sym, directives)
		if !ok || !dir.DerivativesEnabled || !dir.IncludeFearGreed {
			continue
		}
		b.FearGreed.RefreshIfStale(ctx)
		return
	}
}

func (b *DefaultPromptBuilder) collectVisionPayloads(ctxs []AnalysisContext) []provider.ImagePayload {
	if len(ctxs) == 0 {
		return nil
	}
	out := make([]provider.ImagePayload, 0, 4)
	for _, ac := range ctxs {
		if ac.ImageB64 == "" {
			continue
		}
		desc := fmt.Sprintf("%s %s %s", ac.Symbol, ac.Interval, ac.ImageNote)
		out = append(out, provider.ImagePayload{DataURI: ac.ImageB64, Description: strings.TrimSpace(desc)})
		if len(out) >= cap(out) {
			break
		}
	}
	return out
}

func mergeDataAgeSec(base map[string]int64, extra map[string]int64) map[string]int64 {
	if len(extra) == 0 && len(base) == 0 {
		return base
	}
	out := make(map[string]int64, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		if v < 0 {
			v = 0
		}
		out[k] = v
	}
	return out
}

// computeLiquidationClose is a heuristic: high leverage + deep drawdown implies接近强平。
func computeLiquidationClose(positions []types.PositionSnapshot) bool {
	for _, p := range positions {
		if p.Leverage >= 10 && p.UnrealizedPnPct <= -40 {
			return true
		}
	}
	return false
}
