package decision

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	brcfg "brale/internal/config"
	"brale/internal/decision/render"
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

// 中文说明：
// LegacyEngineAdapter：沿用“旧版思路”的提示词结构（System 模板 + User 数据摘要），
// 但底层通过 ModelProvider 调用，并用 JSON 数组解析为决策结果；
// 后续可替换为严格对接旧版 decision/engine.go（需要完整账户/持仓/指标上下文）。

type LegacyEngineAdapter struct {
	Providers     []provider.ModelProvider
	Agg           Aggregator
	Observer      DecisionObserver
	AgentNotifier notifier.TextNotifier
	AgentHistory  AgentOutputHistory

	PromptMgr      *strategy.Manager // 提示词管理器
	SystemTemplate string            // 使用的系统模板名

	KStore      market.KlineStore // K线存储（用于构建 User 摘要）
	Intervals   []string          // 要在摘要中展示的周期
	HorizonName string
	MultiAgent  brcfg.MultiAgentConfig
	// ProviderPreference 控制同权重/同动作时的模型优先级，按配置顺序匹配。
	ProviderPreference []string
	FinalDisabled      map[string]bool

	ExitPlans *exitplan.Registry

	Name_ string

	Parallel bool // 并行调用多个模型

	// 是否为每个模型分别输出思维链与JSON（便于对比调参）
	LogEachModel bool
	// 是否在日志中输出 Structured Blocks 调试提示
	DebugStructuredBlocks bool

	// 可选：补充 OI 与资金费率
	Metrics *market.MetricsService

	// 调用超时（秒）：限制单模型调用最长时间，避免卡死
	TimeoutSeconds int
}

const priceWindowBars = 4

func (e *LegacyEngineAdapter) Name() string {
	if e.Name_ != "" {
		return e.Name_
	}
	return "legacy-adapter"
}

// Decide 构建 System/User 提示词 → 调用多个模型 → 聚合 → 解析 JSON 决策
func (e *LegacyEngineAdapter) Decide(ctx context.Context, input Context) (DecisionResult, error) {
	order, grouped := groupAnalysisBySymbol(input.Analysis, input.Candidates)
	// 没有可拆分或仅单币种，沿用旧逻辑
	if len(order) <= 1 {
		// 无论何种情况，进入 decideSingle 前都应清空 Prompt.User，
		// 因为 buildUserSummary -> renderOutputConstraints 会重新根据 ProfilePrompts 渲染 UserPrompt。
		// 如果不清空，composePrompts 会将 input.Prompt.User (bundle) 追加在末尾，导致 UserPrompt 内容重复 (一次在 Guidelines, 一次在 userExtra)。
		input.Prompt.User = ""

		if len(order) == 1 {
			sym := order[0]
			input.Candidates = []string{sym}
			input.Analysis = CloneSlice(grouped[sym])
			// 过滤 ProfilePrompts，只保留当前 symbol 的配置
			input.ProfilePrompts = filterProfilePrompts(input.ProfilePrompts, sym)
			// 过滤 Positions，只保留当前 symbol 的持仓
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
		// 过滤 ProfilePrompts，只保留当前 symbol 的配置，避免输出其他 symbol 的约束
		symInput.ProfilePrompts = filterProfilePrompts(input.ProfilePrompts, sym)
		// 清空 Prompt.User，避免追加所有 profiles 的 user template（已通过 ProfilePrompts 处理）
		symInput.Prompt.User = ""

		// 过滤 Positions，只保留当前 symbol 的持仓
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
		// 回退到旧行为，避免前端解析差异
		result.RawOutput = blocks[0].RawOutput
		result.RawJSON = blocks[0].RawJSON
		result.MetaSummary = blocks[0].MetaSummary
		result.TraceID = blocks[0].TraceID
	} else {
		result.MetaSummary = mergeSymbolSummaries(blocks)
	}
	return result, nil
}

func (e *LegacyEngineAdapter) decideSingle(ctx context.Context, input Context, applyDelay bool) (DecisionResult, error) {
	insights := e.runMultiAgents(ctx, input)
	baseSys, usr := e.ComposePrompts(ctx, input, insights)
	visionPayloads := e.collectVisionPayloads(input.Analysis)

	if applyDelay {
		time.Sleep(5 * time.Second)
	}

	outs := e.collectModelOutputs(ctx, func(c context.Context, p provider.ModelProvider) ModelOutput {
		sys, err := resolveSystemPromptForFinalModel(input.ProfilePrompts, input.Candidates, p.ID())
		if err != nil {
			return ModelOutput{ProviderID: p.ID(), Err: err}
		}
		return e.callProvider(c, p, sys, usr, visionPayloads)
	})

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
			UserPrompt:    usr,
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

func (e *LegacyEngineAdapter) logModelTables(outs []ModelOutput) {
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

// ComposePrompts 返回当前配置下的 System/User 提示词。
func (e *LegacyEngineAdapter) ComposePrompts(ctx context.Context, input Context, insights []AgentInsight) (string, string) {
	system := strings.TrimSpace(input.Prompt.System)
	userSummary := strings.TrimSpace(e.buildUserSummary(ctx, input, insights))
	userExtra := strings.TrimSpace(input.Prompt.User)
	switch {
	case userSummary != "" && userExtra != "":
		return system, userSummary + "\n\n" + userExtra
	case userSummary != "":
		return system, userSummary
	default:
		return system, userExtra
	}
}

func (e *LegacyEngineAdapter) loadTemplate(name string) string {
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

// buildUserSummary 将候选币种与当前仓位的摘要组装为 User 提示词
func (e *LegacyEngineAdapter) buildUserSummary(ctx context.Context, input Context, insights []AgentInsight) string {
	e.refreshDerivativesOnDemand(ctx, input.Candidates, input.Directives)
	sections := render.Sections{
		Account:     e.renderAccountOverview(input.Account),
		Previous:    e.renderPreviousReasoning(input.PreviousReasoning),
		Derivatives: e.renderDerivativesMetrics(input.Candidates, input.Directives),
		Positions:   e.renderPositionDetails(input.Positions),
		Klines:      e.renderKlineWindows(input.Analysis),
		Agents:      e.renderAgentBlocks(insights),
		Guidelines:  e.renderOutputConstraints(input),
	}
	var loader render.TemplateLoader
	if e.PromptMgr != nil {
		loader = e.PromptMgr
	}
	summary := render.RenderSummary(loader, sections)
	logStructuredBlocksDebug(e.DebugStructuredBlocks, input.Analysis)
	return summary
}

func (e *LegacyEngineAdapter) refreshDerivativesOnDemand(ctx context.Context, symbols []string, directives map[string]ProfileDirective) {
	if e == nil || e.Metrics == nil || len(symbols) == 0 || len(directives) == 0 {
		return
	}

	need := make([]string, 0, len(symbols))
	for _, sym := range symbols {
		dir, ok := lookupDirective(sym, directives)
		if !ok || !dir.allowDerivatives() {
			continue
		}
		s := strings.ToUpper(strings.TrimSpace(sym))
		if s == "" {
			continue
		}
		need = append(need, s)
	}
	if len(need) == 0 {
		return
	}

	var eg errgroup.Group
	eg.SetLimit(4)
	for _, sym := range need {
		sym := sym
		eg.Go(func() error {
			refreshCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			e.Metrics.RefreshSymbol(refreshCtx, sym)
			return nil
		})
	}
	_ = eg.Wait()
}

func (e *LegacyEngineAdapter) collectVisionPayloads(ctxs []AnalysisContext) []provider.ImagePayload {
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

func (e *LegacyEngineAdapter) callProvider(parent context.Context, p provider.ModelProvider, system, user string, baseImages []provider.ImagePayload) ModelOutput {
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
		Raw:           raw,
		Parsed:        parsed,
		Err:           err,
		Images:        CloneSlice(payload.Images),
		VisionEnabled: visionEnabled,
		ImageCount:    len(payload.Images),
	}
}

func (e *LegacyEngineAdapter) collectModelOutputs(ctx context.Context, call func(context.Context, provider.ModelProvider) ModelOutput) []ModelOutput {
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

func (e *LegacyEngineAdapter) validateExitPlans(decisions []Decision) error {
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

func (e *LegacyEngineAdapter) isFinalStageDisabled(id string) bool {
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

// CloneSlice performs a shallow copy of a slice using Go generics.
func CloneSlice[T any](src []T) []T {
	if len(src) == 0 {
		return nil
	}
	dst := make([]T, len(src))
	copy(dst, src)
	return dst
}

// cloneOutputs performs a deep copy of ModelOutput slices, ensuring ImagePayloads are also copied.
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

// filterProfilePrompts 过滤 ProfilePrompts，只保留指定 symbol 的配置
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

// filterPositions 过滤持仓列表，只保留 candidates 中包含的 symbol
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
