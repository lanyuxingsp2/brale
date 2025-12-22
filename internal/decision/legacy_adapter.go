package decision

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	brcfg "brale/internal/config"
	"brale/internal/exitplan"
	"brale/internal/gateway/notifier"
	"brale/internal/gateway/provider"
	"brale/internal/logger"
	"brale/internal/market"
	jsonutil "brale/internal/pkg/jsonutil"
	textutil "brale/internal/pkg/text"
	"brale/internal/strategy"
	"brale/internal/types"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"
)

type DecisionEngine struct {
	Providers       []provider.ModelProvider
	Agg             Aggregator
	Observer        DecisionObserver
	AgentNotifier   notifier.TextNotifier
	AgentHistory    AgentOutputHistory
	ProviderHistory ProviderOutputHistory

	PromptBuilder PromptBuilder
	PromptMgr     *strategy.Manager

	KStore      market.KlineStore
	Intervals   []string
	HorizonName string
	MultiAgent  brcfg.MultiAgentConfig

	ProviderPreference []string
	ProviderRoles      map[string]string
	StageProviders     map[string]string
	FinalDisabled      map[string]bool

	ExitPlans *exitplan.Registry

	Name_ string

	Parallel bool

	LogEachModel bool

	TimeoutSeconds int
}

const priceWindowBars = 4

func (e *DecisionEngine) Name() string {
	if e.Name_ != "" {
		return e.Name_
	}
	return "decision-engine"
}

// Decide is the main entry point for the decision engine.
// Supports both single-symbol and multi-symbol (batch) analysis.
//
// Logic:
// 1. Groups analysis contexts by symbol.
// 2. If single symbol: directly calls decideSingle.
// 3. If multiple symbols:
//   - Iterates through symbols sequentially (to manage rate limits/context).
//   - Aggregates results into a single DecisionResult.
//   - Merges meta-voting breakdowns if available.
func (e *DecisionEngine) Decide(ctx context.Context, input Context) (DecisionResult, error) {
	order, grouped := groupAnalysisBySymbol(input.Analysis, input.Candidates)

	if len(order) <= 1 {

		input.Prompt.User = ""

		if len(order) == 1 {
			sym := order[0]
			input.Candidates = []string{sym}
			input.Analysis = CloneSlice(grouped[sym])

			input.ProfilePrompts = filterProfilePrompts(input.ProfilePrompts, sym)

			input.Positions = filterPositions(input.Positions, input.Candidates)
		}
		return e.decideSingle(ctx, input, true)
	}

	result := DecisionResult{
		TraceID: fmt.Sprintf("batch-%s", uuid.NewString()),
	}
	blocks := make([]SymbolDecisionOutput, 0, len(order))
	var mergedBreakdown *MetaVoteBreakdown
	delay := true
	for _, sym := range order {
		symInput := input
		symInput.Candidates = []string{sym}
		symInput.Analysis = CloneSlice(grouped[sym])

		symInput.ProfilePrompts = filterProfilePrompts(input.ProfilePrompts, sym)

		symInput.Prompt.User = ""

		symInput.Positions = filterPositions(input.Positions, []string{sym})
		partial, err := e.decideSingle(ctx, symInput, delay)
		if err != nil {
			return DecisionResult{}, err
		}
		delay = false
		result.Decisions = append(result.Decisions, partial.Decisions...)
		mergedBreakdown = mergeMetaVoteBreakdowns(mergedBreakdown, partial.MetaBreakdown)
		blocks = append(blocks, SymbolDecisionOutput{
			Symbol:      sym,
			RawOutput:   partial.RawOutput,
			RawJSON:     partial.RawJSON,
			MetaSummary: partial.MetaSummary,
			TraceID:     partial.TraceID,
		})
	}
	result.SymbolResults = blocks
	result.MetaBreakdown = mergedBreakdown
	if len(blocks) == 1 {

		result.RawOutput = blocks[0].RawOutput
		result.RawJSON = blocks[0].RawJSON
		result.MetaSummary = blocks[0].MetaSummary
		result.TraceID = blocks[0].TraceID
	} else {
		result.MetaSummary = mergeSymbolSummaries(blocks)
	}
	return result, nil
}

// decideSingle executes the decision pipeline for a single symbol context.
//
// Pipeline:
// 1. Run Multi-Agents (if enabled) to generate intermediate insights (Pattern, Trend, etc).
// 2. Compose Prompts: Merge system templates + user context + agent insights.
// 3. Collect Vision Payloads: If chart images are available in analysis.
// 4. Call LLMs (Parallel): Invoke all configured providers.
//   - Resolves model-specific system prompts dynamically.
//
// 5. Aggregate: Combine outputs using the configured strategy (FirstWins or MetaVoting).
// 6. Trace: Log full decision trace for debugging/audit.
func (e *DecisionEngine) decideSingle(ctx context.Context, input Context, applyDelay bool) (DecisionResult, error) {
	insights := e.runMultiAgents(ctx, input)
	if e.PromptBuilder == nil {
		return DecisionResult{}, fmt.Errorf("prompt builder not configured")
	}
	if prevProviders := e.lookupPreviousProviderOutputs(ctx, input); len(prevProviders) > 0 {
		input.PreviousProviderOutputs = prevProviders
	}
	promptsByProvider, fallbackPrompt, err := e.prepareProviderPrompts(ctx, input, insights)
	if err != nil {
		return DecisionResult{}, err
	}
	baseSys := fallbackPrompt.system
	baseUsr := fallbackPrompt.user

	if applyDelay {
		time.Sleep(5 * time.Second)
	}

	outs := e.invokeProvidersWithPrompts(ctx, promptsByProvider, fallbackPrompt, input)

	if len(e.ProviderPreference) > 0 {
		outs = orderOutputsByPreference(outs, e.ProviderPreference)
	}

	agg := e.Agg
	if agg == nil {
		agg = FirstWinsAggregator{}
	}
	if e.LogEachModel {
		e.logModelTables(outs)
	}
	best, err := agg.Aggregate(ctx, outs)
	if err != nil {
		return DecisionResult{}, err
	}
	result := best.Parsed
	result.Decisions = NormalizeAndAlignDecisions(result.Decisions, input.Positions)
	AttachDecisionProfiles(result.Decisions, input.FeatureReports)
	best.Parsed.Decisions = result.Decisions

	traceID := uuid.NewString()
	if e.Observer != nil {
		bestSys := baseSys
		if resolved, err := resolveSystemPromptForFinalModel(input.ProfilePrompts, input.Candidates, best.ProviderID); err == nil && strings.TrimSpace(resolved) != "" {
			bestSys = resolved
		}
		e.Observer.AfterDecide(ctx, DecisionTrace{
			TraceID:       traceID,
			SystemPrompt:  bestSys,
			UserPrompt:    baseUsr,
			Outputs:       cloneOutputs(outs),
			Best:          best,
			Candidates:    CloneSlice(input.Candidates),
			Timeframes:    CloneSlice(e.Intervals),
			HorizonName:   e.HorizonName,
			Positions:     CloneSlice(input.Positions),
			AgentInsights: CloneSlice(insights),
		})
	}
	result.TraceID = traceID
	best.Parsed.TraceID = traceID
	return result, nil
}

type providerPrompt struct {
	system string
	user   string
	images []provider.ImagePayload
}

func (e *DecisionEngine) prepareProviderPrompts(ctx context.Context, input Context, insights []AgentInsight) (map[string]providerPrompt, providerPrompt, error) {
	promptsByProvider := make(map[string]providerPrompt, len(e.Providers))
	var fallbackPrompt providerPrompt

	for idx, p := range e.Providers {
		allowedStages := allowedAgentStagesForProvider(p.ID(), input.ProfilePrompts, input.Candidates, e.ProviderRoles)
		filteredInsights := filterAgentInsightsByStage(insights, allowedStages)
		sys, usr, imgs, err := e.PromptBuilder.Build(ctx, input, filteredInsights)
		if err != nil {
			return nil, providerPrompt{}, err
		}
		entry := providerPrompt{system: sys, user: usr, images: imgs}
		promptsByProvider[p.ID()] = entry
		if idx == 0 || fallbackPrompt.user == "" {
			fallbackPrompt = entry
		}
	}
	if fallbackPrompt.user == "" {
		sys, usr, imgs, err := e.PromptBuilder.Build(ctx, input, insights)
		if err != nil {
			return nil, providerPrompt{}, err
		}
		fallbackPrompt = providerPrompt{system: sys, user: usr, images: imgs}
	}
	return promptsByProvider, fallbackPrompt, nil
}

func (e *DecisionEngine) invokeProvidersWithPrompts(ctx context.Context, promptsByProvider map[string]providerPrompt, fallbackPrompt providerPrompt, input Context) []ModelOutput {
	return e.collectModelOutputs(ctx, func(c context.Context, p provider.ModelProvider) ModelOutput {
		sys, err := resolveSystemPromptForFinalModel(input.ProfilePrompts, input.Candidates, p.ID())
		if err != nil {
			return ModelOutput{ProviderID: p.ID(), Err: err}
		}
		merged := mergePrompt(fallbackPrompt, promptsByProvider[p.ID()])
		return e.callProvider(c, p, sys, merged.user, merged.images)
	})
}

func mergePrompt(base, override providerPrompt) providerPrompt {
	out := base
	if strings.TrimSpace(override.user) != "" {
		out.user = override.user
	}
	if len(override.images) > 0 {
		out.images = override.images
	}
	if strings.TrimSpace(override.system) != "" {
		out.system = override.system
	}
	return out
}

func (e *DecisionEngine) logModelTables(outs []ModelOutput) {
	results := make([]ResultRow, 0, 8)

	for _, o := range outs {
		if o.Err != nil || o.Raw == "" {
			reason := ""
			if o.Err != nil {
				reason = o.Err.Error()
			} else {
				reason = "无输出"
			}
			results = append(results, ResultRow{Provider: o.ProviderID, Action: "失败", Reason: reason, Failed: true})
			continue
		}
		_, _, ok := jsonutil.ExtractJSONWithOffset(o.Raw)
		if ok {
			if len(o.Parsed.Decisions) > 0 {
				for _, d := range o.Parsed.Decisions {
					r := ResultRow{Provider: o.ProviderID, Action: d.Action, Symbol: d.Symbol, Reason: d.Reasoning}
					r.Reason = textutil.Truncate(r.Reason, 3600)
					results = append(results, r)
				}
			} else {
				results = append(results, ResultRow{Provider: o.ProviderID, Action: "失败", Reason: "未解析到决策", Failed: true})
			}
		} else {
			results = append(results, ResultRow{Provider: o.ProviderID, Action: "失败", Reason: "未找到 JSON 决策数组", Failed: true})
		}
	}

	tResults := RenderResultsTable(results, 0)
	logger.InfoBlock(tResults)
}

func (e *DecisionEngine) lookupPreviousProviderOutputs(ctx context.Context, input Context) []ProviderOutputSnapshot {
	if e == nil || e.ProviderHistory == nil {
		return nil
	}
	symbol := ""
	if len(input.Candidates) == 1 {
		symbol = normalizeSymbol(input.Candidates[0])
	}
	if symbol == "" {
		symbol = agentSymbolFromContexts(input.Analysis)
	}
	if symbol == "" {
		return nil
	}
	out, err := e.ProviderHistory.LatestProviderOutputs(ctx, symbol)
	if err != nil {
		logger.Debugf("lookupPreviousProviderOutputs failed symbol=%s err=%v", symbol, err)
		return nil
	}
	return out
}

func (e *DecisionEngine) loadTemplate(name string) string {
	if e.PromptMgr == nil {
		return ""
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if s, ok := e.PromptMgr.Get(name); ok {
		return s
	}
	return ""
}

// callProvider invokes a single LLM provider and parses its output.
//
// Logic:
// 1. Prepares context with timeout.
// 2. Checks if provider supports Vision (images) and attaches if so.
// 3. Invokes API (p.Call).
// 4. Attempts to extract and parse JSON from the raw text response.
//   - Uses aggressive JSON extraction (ExtractJSON) to handle Markdown code blocks.
//   - Validates schema (CoerceDecisionArrayJSON, ValidateDecisionArray).
//   - Validates business logic (validateExitPlans).
//
// Returns a ModelOutput containing both raw response and parsed structure.
func (e *DecisionEngine) callProvider(parent context.Context, p provider.ModelProvider, system, user string, baseImages []provider.ImagePayload) ModelOutput {
	cctx := parent
	var cancel context.CancelFunc
	if timeout := e.TimeoutSeconds; timeout > 0 {
		cctx, cancel = context.WithTimeout(parent, time.Duration(timeout)*time.Second)
		defer cancel()
	}
	logger.Debugf("调用模型: %s", p.ID())
	visionEnabled := p.SupportsVision()
	payload := provider.ChatPayload{
		System:     system,
		User:       user,
		ExpectJSON: p.ExpectsJSON(),
	}
	if visionEnabled && len(baseImages) > 0 {
		payload.Images = CloneSlice(baseImages)
	}
	purpose := fmt.Sprintf("final decision (images=%d)", len(payload.Images))
	logAIInput("main", p.ID(), purpose, payload.System, payload.User, summarizeImagePayloads(payload.Images))
	start := time.Now()
	raw, err := p.Call(cctx, payload)
	logger.LogLLMResponse("main", p.ID(), purpose, raw)

	parsed := DecisionResult{}
	if err == nil {
		if block, ok := jsonutil.ExtractJSON(raw); ok {
			arr, cerr := CoerceDecisionArrayJSON(block)
			if cerr != nil {
				parsed.RawOutput = raw
				parsed.RawJSON = strings.TrimSpace(block)
				err = cerr
			} else if qerr := ValidateDecisionArray(arr); qerr != nil {
				parsed.RawOutput = raw
				parsed.RawJSON = arr
				err = qerr
			} else {
				var ds []Decision
				dec := json.NewDecoder(strings.NewReader(arr))
				dec.DisallowUnknownFields()
				if je := dec.Decode(&ds); je == nil {
					parsed.RawOutput = raw
					parsed.RawJSON = arr
					if verr := e.validateExitPlans(ds); verr != nil {
						err = verr
					} else {
						parsed.Decisions = ds
						logger.Infof("模型 %s 解析到 %d 条决策", p.ID(), len(ds))
					}
				} else {
					err = je
				}
			}
		} else {
			snippet := raw
			if len(snippet) > 160 {
				snippet = snippet[:160] + "..."
			}
			logger.Warnf("模型 %s 响应未包含 JSON 决策数组 elapsed=%s 片段=%q", p.ID(), time.Since(start).Truncate(time.Millisecond), snippet)
			err = fmt.Errorf("未找到 JSON 决策数组")
		}
	}
	if err != nil {
		logger.Warnf("模型 %s 调用失败 elapsed=%s err=%v", p.ID(), time.Since(start).Truncate(time.Millisecond), err)
	}
	return ModelOutput{
		ProviderID:    p.ID(),
		SystemPrompt:  payload.System,
		UserPrompt:    payload.User,
		Raw:           raw,
		Parsed:        parsed,
		Err:           err,
		Images:        CloneSlice(payload.Images),
		VisionEnabled: visionEnabled,
		ImageCount:    len(payload.Images),
	}
}

func (e *DecisionEngine) collectModelOutputs(ctx context.Context, call func(context.Context, provider.ModelProvider) ModelOutput) []ModelOutput {
	if !e.Parallel {
		outs := make([]ModelOutput, 0, len(e.Providers))
		for _, p := range e.Providers {
			if p != nil && p.Enabled() {
				if e.isFinalStageDisabled(p.ID()) {
					continue
				}
				outs = append(outs, call(ctx, p))
			}
		}
		return outs
	}
	enabled := 0
	for _, p := range e.Providers {
		if p != nil && p.Enabled() {
			enabled++
		}
	}
	if enabled == 0 {
		return nil
	}
	outs := make([]ModelOutput, 0, enabled)
	var mu sync.Mutex
	eg, egCtx := errgroup.WithContext(ctx)
	for _, p := range e.Providers {
		if p == nil || !p.Enabled() {
			continue
		}
		if e.isFinalStageDisabled(p.ID()) {
			continue
		}
		provider := p
		eg.Go(func() error {
			out := invokeProviderSafe(egCtx, provider, call)
			mu.Lock()
			outs = append(outs, out)
			mu.Unlock()
			return nil
		})
	}
	_ = eg.Wait()
	return outs
}

func (e *DecisionEngine) validateExitPlans(decisions []Decision) error {
	if e == nil || e.ExitPlans == nil {
		return nil
	}
	for idx := range decisions {
		d := &decisions[idx]
		action := strings.ToLower(strings.TrimSpace(d.Action))
		if action != "open_long" && action != "open_short" {
			continue
		}
		if d.ExitPlan == nil || strings.TrimSpace(d.ExitPlan.ID) == "" {
			return fmt.Errorf("决策#%d 缺少 exit_plan", idx+1)
		}
		tpl, err := e.ExitPlans.Validate(d.ExitPlan.ID, d.ExitPlan.Params)
		if err != nil {
			return fmt.Errorf("决策#%d exit_plan=%s 校验失败: %w", idx+1, d.ExitPlan.ID, err)
		}
		d.ExitPlanVersion = tpl.Version
	}
	return nil
}

func invokeProviderSafe(ctx context.Context, p provider.ModelProvider, call func(context.Context, provider.ModelProvider) ModelOutput) (out ModelOutput) {
	defer func() {
		if r := recover(); r != nil {
			logger.Warnf("模型 %s 调用 panic: %v", p.ID(), r)
			out = ModelOutput{
				ProviderID: p.ID(),
				Err:        fmt.Errorf("panic: %v", r),
			}
		}
	}()
	return call(ctx, p)
}

func (e *DecisionEngine) isFinalStageDisabled(id string) bool {
	if len(e.FinalDisabled) == 0 {
		return false
	}
	if strings.TrimSpace(id) == "" {
		return false
	}
	_, ok := e.FinalDisabled[strings.TrimSpace(id)]
	return ok
}

func groupAnalysisBySymbol(ctxs []AnalysisContext, candidates []string) ([]string, map[string][]AnalysisContext) {
	groups := make(map[string][]AnalysisContext)
	order := make([]string, 0)
	addSymbol := func(sym string) {
		sym = normalizeSymbol(sym)
		if sym == "" {
			return
		}
		if _, ok := groups[sym]; ok {
			return
		}
		groups[sym] = nil
		order = append(order, sym)
	}
	for _, sym := range candidates {
		addSymbol(sym)
	}
	for _, ac := range ctxs {
		sym := normalizeSymbol(ac.Symbol)
		if sym == "" {
			continue
		}
		if _, ok := groups[sym]; !ok {
			order = append(order, sym)
		}
		groups[sym] = append(groups[sym], ac)
	}
	if len(order) == 0 {
		for sym := range groups {
			order = append(order, sym)
		}
	}
	return order, groups
}

func normalizeSymbol(sym string) string {
	sym = strings.ToUpper(strings.TrimSpace(sym))
	if sym == "" {
		return ""
	}
	return sym
}

func mergeSymbolSummaries(blocks []SymbolDecisionOutput) string {
	if len(blocks) == 0 {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for _, blk := range blocks {
		summary := strings.TrimSpace(blk.MetaSummary)
		if summary == "" {
			continue
		}
		label := normalizeSymbol(blk.Symbol)
		if label == "" {
			label = "-"
		}
		parts = append(parts, fmt.Sprintf("[%s]\n%s", label, summary))
	}
	return strings.Join(parts, "\n\n")
}

func CloneSlice[T any](src []T) []T {
	if len(src) == 0 {
		return nil
	}
	dst := make([]T, len(src))
	copy(dst, src)
	return dst
}

func cloneOutputs(src []ModelOutput) []ModelOutput {
	if len(src) == 0 {
		return nil
	}
	dst := make([]ModelOutput, len(src))
	for i := range src {
		dst[i] = src[i]
		dst[i].Images = CloneSlice(src[i].Images)
		dst[i].VisionEnabled = src[i].VisionEnabled
		dst[i].ImageCount = src[i].ImageCount
	}
	return dst
}

func filterProfilePrompts(prompts map[string]ProfilePromptSpec, targetSymbol string) map[string]ProfilePromptSpec {
	if len(prompts) == 0 {
		return nil
	}
	targetNorm := normalizeSymbol(targetSymbol)
	if targetNorm == "" {
		return nil
	}
	result := make(map[string]ProfilePromptSpec, 1)
	for sym, spec := range prompts {
		if normalizeSymbol(sym) == targetNorm {
			result[sym] = spec
			break
		}
	}
	return result
}

func resolveSystemPromptForFinalModel(prompts map[string]ProfilePromptSpec, candidates []string, modelID string) (string, error) {
	if len(prompts) == 0 {
		return "", fmt.Errorf("profile prompts 为空，无法解析 system prompt")
	}
	if len(candidates) != 1 {
		return "", fmt.Errorf("final decision 仅支持单币种 system prompt 解析（当前 candidates=%d）", len(candidates))
	}
	symbol := normalizeSymbol(candidates[0])
	if symbol == "" {
		return "", fmt.Errorf("symbol 为空，无法解析 system prompt")
	}
	modelID = strings.TrimSpace(modelID)
	if modelID == "" {
		return "", fmt.Errorf("model id 为空，无法解析 system prompt")
	}
	for sym, spec := range prompts {
		if normalizeSymbol(sym) != symbol {
			continue
		}
		if len(spec.SystemPromptsByModel) == 0 {
			return "", fmt.Errorf("symbol=%s 未配置 prompts.system_by_model", symbol)
		}
		sys := strings.TrimSpace(spec.SystemPromptsByModel[modelID])
		if sys == "" {
			return "", fmt.Errorf("symbol=%s 缺少 system prompt 配置 model=%s", symbol, modelID)
		}
		return sys, nil
	}
	return "", fmt.Errorf("未找到 symbol=%s 对应的 profile prompts", symbol)
}

func filterPositions(positions []types.PositionSnapshot, candidates []string) []types.PositionSnapshot {
	if len(positions) == 0 || len(candidates) == 0 {
		return nil
	}
	candidateMap := make(map[string]struct{}, len(candidates))
	for _, sym := range candidates {
		norm := normalizeSymbol(sym)
		if norm != "" {
			candidateMap[norm] = struct{}{}
		}
	}
	if len(candidateMap) == 0 {
		return nil
	}
	filtered := make([]types.PositionSnapshot, 0, len(positions))
	for _, pos := range positions {
		norm := normalizeSymbol(pos.Symbol)
		if _, ok := candidateMap[norm]; ok {
			filtered = append(filtered, pos)
		}
	}
	return filtered
}
