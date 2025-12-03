package decision

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"brale/internal/ai/parser"
	brcfg "brale/internal/config"
	"brale/internal/decision/render"
	"brale/internal/gateway/provider"
	"brale/internal/logger"
	"brale/internal/market"
	formatutil "brale/internal/pkg/format"
	jsonutil "brale/internal/pkg/jsonutil"
	textutil "brale/internal/pkg/text"
	"brale/internal/strategy"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"
)

// TextNotifier 定义最小化的通知接口，便于注入 Telegram 等实现。
type TextNotifier interface {
	SendText(text string) error
}

// 中文说明：
// LegacyEngineAdapter：沿用“旧版思路”的提示词结构（System 模板 + User 数据摘要），
// 但底层通过 ModelProvider 调用，并用 JSON 数组解析为决策结果；
// 后续可替换为严格对接旧版 decision/engine.go（需要完整账户/持仓/指标上下文）。

type LegacyEngineAdapter struct {
	Providers     []provider.ModelProvider
	Agg           Aggregator
	Observer      DecisionObserver
	AgentNotifier TextNotifier

	PromptMgr      *strategy.Manager // 提示词管理器
	SystemTemplate string            // 使用的系统模板名

	KStore      market.KlineStore // K线存储（用于构建 User 摘要）
	Intervals   []string          // 要在摘要中展示的周期
	Horizon     brcfg.HorizonProfile
	HorizonName string
	MultiAgent  brcfg.MultiAgentConfig
	// ProviderPreference 控制同权重/同动作时的模型优先级，按配置顺序匹配。
	ProviderPreference []string

	Name_ string

	Parallel bool // 并行调用多个模型

	// 是否为每个模型分别输出思维链与JSON（便于对比调参）
	LogEachModel bool
	// 是否在日志中输出 Structured Blocks 调试提示
	DebugStructuredBlocks bool

	// 可选：补充 OI 与资金费率
	Metrics        *market.MetricsService
	IncludeOI      bool
	IncludeFunding bool

	// 调用超时（秒）：限制单模型调用最长时间，避免卡死
	TimeoutSeconds int
}

const priceWindowBars = 6

func (e *LegacyEngineAdapter) Name() string {
	if e.Name_ != "" {
		return e.Name_
	}
	return "legacy-adapter"
}

// Decide 构建 System/User 提示词 → 调用多个模型 → 聚合 → 解析 JSON 决策
func (e *LegacyEngineAdapter) Decide(ctx context.Context, input Context) (DecisionResult, error) {
	insights := e.runMultiAgents(ctx, input)
	sys, usr := e.ComposePrompts(ctx, input, insights)
	visionPayloads := e.collectVisionPayloads(input.Analysis)

	outs := e.collectModelOutputs(ctx, func(c context.Context, p provider.ModelProvider) ModelOutput {
		return e.callProvider(c, p, sys, usr, visionPayloads)
	})

	if len(e.ProviderPreference) > 0 {
		outs = orderOutputsByPreference(outs, e.ProviderPreference)
	}

	// 聚合选择一个有效输出
	agg := e.Agg
	if agg == nil {
		agg = FirstWinsAggregator{}
	}
	// 可选：在聚合前输出每个模型的原始结果（思维链 + JSON）
	if e.LogEachModel {
		// 汇总表：所有模型的思维链
		thoughts := make([]ThoughtRow, 0, len(outs))
		// 汇总表：所有模型的结果（逐条决策）
		results := make([]ResultRow, 0, 8)

		for _, o := range outs {
			if o.Err != nil || o.Raw == "" {
				reason := ""
				if o.Err != nil {
					reason = o.Err.Error()
				} else {
					reason = "无输出"
				}
				thoughts = append(thoughts, ThoughtRow{Provider: o.ProviderID, Thought: reason, Failed: true})
				results = append(results, ResultRow{Provider: o.ProviderID, Action: "失败", Reason: reason, Failed: true})
				continue
			}
			_, start, ok := parser.ExtractJSONWithOffset(o.Raw)
			if ok {
				thought := strings.TrimSpace(o.Raw[:start])
				thought = textutil.Truncate(thought, 2400)
				thoughts = append(thoughts, ThoughtRow{Provider: o.ProviderID, Thought: thought})
				// 逐条填充结果
				if len(o.Parsed.Decisions) > 0 {
					for _, d := range o.Parsed.Decisions {
						r := ResultRow{Provider: o.ProviderID, Action: d.Action, Symbol: d.Symbol, Reason: d.Reasoning}
						// 尽量不裁剪，但仍保底上限
						r.Reason = textutil.Truncate(r.Reason, 3600)
						results = append(results, r)
					}
				} else {
					// 没有解析出的决策，一样标记失败
					results = append(results, ResultRow{Provider: o.ProviderID, Action: "失败", Reason: "未解析到决策", Failed: true})
				}
			} else {
				thoughts = append(thoughts, ThoughtRow{Provider: o.ProviderID, Thought: "未找到 JSON 决策数组", Failed: true})
				results = append(results, ResultRow{Provider: o.ProviderID, Action: "失败", Reason: "未找到 JSON 决策数组", Failed: true})
			}
		}

		// 渲染两张合并表
		tThoughts := RenderThoughtsTable(thoughts, 0)
		tResults := RenderResultsTable(results, 0)
		logger.Infof("")
		logger.InfoBlock(tThoughts)
		logger.InfoBlock(tResults)
	}
	best, err := agg.Aggregate(ctx, outs)
	if err != nil {
		return DecisionResult{}, err
	}
	result := best.Parsed
	result.Decisions = NormalizeAndAlignDecisions(result.Decisions, input.Positions)
	best.Parsed.Decisions = result.Decisions

	traceID := uuid.NewString()
	if e.Observer != nil {
		e.Observer.AfterDecide(ctx, DecisionTrace{
			TraceID:       traceID,
			SystemPrompt:  sys,
			UserPrompt:    usr,
			Outputs:       cloneOutputs(outs),
			Best:          best,
			Candidates:    cloneStrings(input.Candidates),
			Timeframes:    cloneStrings(e.Intervals),
			HorizonName:   e.HorizonName,
			Positions:     cloneSnapshots(input.Positions),
			AgentInsights: cloneAgentInsights(insights),
		})
	}
	result.TraceID = traceID
	best.Parsed.TraceID = traceID
	return result, nil
}

func orderOutputsByPreference(outs []ModelOutput, pref []string) []ModelOutput {
	if len(outs) <= 1 || len(pref) == 0 {
		return outs
	}
	idx := buildPreferenceIndex(pref)
	fallback := len(idx) + 1
	sort.SliceStable(outs, func(i, j int) bool {
		ri := fallback
		if v, ok := idx[outs[i].ProviderID]; ok {
			ri = v
		}
		rj := fallback
		if v, ok := idx[outs[j].ProviderID]; ok {
			rj = v
		}
		if ri != rj {
			return ri < rj
		}
		return outs[i].ProviderID < outs[j].ProviderID
	})
	return outs
}

// ComposePrompts 返回当前配置下的 System/User 提示词。
func (e *LegacyEngineAdapter) ComposePrompts(ctx context.Context, input Context, insights []AgentInsight) (string, string) {
	return e.loadSystem(), e.buildUserSummary(ctx, input, insights)
}

// loadSystem 从 PromptManager 读取系统模板
func (e *LegacyEngineAdapter) loadSystem() string {
	if tpl := e.loadTemplate(e.SystemTemplate); tpl != "" {
		return tpl
	}
	return "你是专业的加密货币交易AI。请根据市场数据与风险控制做出决策。\n"
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
	sections := render.Sections{
		LastDecisions: e.renderLastDecisions(input.LastDecisions),
		Positions:     e.renderCurrentPositions(input.Account, input.Positions),
		Derivatives:   e.renderDerivativesMetrics(ctx, input.Candidates),
		Klines:        e.renderKlineWindows(input.Analysis),
		Insights:      e.renderInsights(insights),
		Guidelines:    e.decisionGuidelines(),
	}
	summary := render.RenderSummary(e.PromptMgr, sections)
	e.logStructuredBlocksDebug(input.Analysis)
	return summary
}

type agentStageConfig struct {
	name       string
	tplName    string
	providerID string
	builder    func([]AnalysisContext, brcfg.MultiAgentConfig) string
}

func (e *LegacyEngineAdapter) runMultiAgents(ctx context.Context, input Context) []AgentInsight {
	cfg := e.MultiAgent
	if !cfg.Enabled || len(input.Analysis) == 0 || e.PromptMgr == nil {
		return nil
	}
	stages := []agentStageConfig{
		{name: agentStageIndicator, tplName: cfg.IndicatorTemplate, providerID: cfg.IndicatorProvider, builder: buildIndicatorAgentPrompt},
		{name: agentStagePattern, tplName: cfg.PatternTemplate, providerID: cfg.PatternProvider, builder: buildPatternAgentPrompt},
		{name: agentStageTrend, tplName: cfg.TrendTemplate, providerID: cfg.TrendProvider, builder: buildTrendAgentPrompt},
	}
	results := make([]AgentInsight, len(stages))
	groupCtx := ctx
	if groupCtx == nil {
		groupCtx = context.Background()
	}
	eg, egCtx := errgroup.WithContext(groupCtx)
	for i, stage := range stages {
		i, stage := i, stage
		eg.Go(func() error {
			results[i] = e.executeAgentStage(egCtx, stage, input.Analysis, cfg)
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		logger.Debugf("multi-agent errgroup: %v", err)
	}
	out := make([]AgentInsight, 0, len(results))
	for _, ins := range results {
		if ins.Stage == "" {
			continue
		}
		out = append(out, ins)
	}
	return out
}

func (e *LegacyEngineAdapter) renderInsights(insights []AgentInsight) string {
	var b strings.Builder
	b.WriteString("\n## Multi-Agent Insights\n")
	if len(insights) == 0 {
		b.WriteString("- 暂无可用结论，请谨慎观望或输出空决策。\n")
		return b.String()
	}
	stageOrder := []string{agentStageIndicator, agentStagePattern, agentStageTrend}
	stageMap := make(map[string]AgentInsight, len(insights))
	for _, ins := range insights {
		if ins.Stage == "" {
			continue
		}
		stageMap[ins.Stage] = ins
	}
	writeInsight := func(ins AgentInsight) {
		if ins.Stage == "" {
			return
		}
		title := formatAgentStageTitle(ins.Stage)
		provider := strings.TrimSpace(ins.ProviderID)
		if provider == "" {
			provider = "-"
		}
		prefix := fmt.Sprintf("- [%s | 模型:%s] ", title, provider)
		if out := strings.TrimSpace(ins.Output); out != "" {
			b.WriteString(prefix)
			b.WriteString(textutil.Truncate(out, 3600))
			b.WriteString("\n")
			return
		}
		status := "无可用结论"
		if ins.Warned {
			status += "（已通知）"
		}
		if errTxt := strings.TrimSpace(ins.Error); errTxt != "" {
			errTxt = textutil.Truncate(errTxt, 240)
			status += ": " + errTxt
		}
		b.WriteString(prefix + status + "\n")
	}
	used := map[string]bool{}
	for _, stage := range stageOrder {
		if ins, ok := stageMap[stage]; ok {
			writeInsight(ins)
			used[stage] = true
		}
	}
	for _, ins := range insights {
		if used[ins.Stage] {
			continue
		}
		writeInsight(ins)
	}
	b.WriteString("请基于这些多 Agent 结论，市场衍生品数据，以及时间窗口与风险约束输出最终 JSON 决策。\n")
	return b.String()
}

func (e *LegacyEngineAdapter) executeAgentStage(ctx context.Context, stage agentStageConfig, ctxs []AnalysisContext, cfg brcfg.MultiAgentConfig) AgentInsight {
	tpl := strings.TrimSpace(e.loadTemplate(stage.tplName))
	if tpl == "" {
		return AgentInsight{}
	}
	user := strings.TrimSpace(stage.builder(ctxs, cfg))
	ins := AgentInsight{Stage: stage.name, System: tpl, User: user}
	if user == "" {
		ins.Error = "输入为空"
		ins.Warned = e.emitAgentWarning(stage.name, stage.providerID, ins.Error)
		return ins
	}
	preferred := strings.TrimSpace(stage.providerID)
	provider := e.findAgentProvider(preferred)
	if provider == nil {
		if preferred != "" {
			ins.Error = fmt.Sprintf("未找到模型 %s", preferred)
		} else {
			ins.Error = "无可用模型"
		}
		ins.Warned = e.emitAgentWarning(stage.name, preferred, ins.Error)
		return ins
	}
	ins.ProviderID = provider.ID()
	purpose := describeAgentPurpose(stage.name)
	logAIInput(fmt.Sprintf("multi-agent:%s", stage.name), provider.ID(), purpose, tpl, user, nil)
	out, err := e.invokeAgentProvider(ctx, provider, tpl, user)
	logger.LogLLMResponse(fmt.Sprintf("multi-agent:%s", stage.name), provider.ID(), purpose, out)
	if err != nil {
		ins.Error = err.Error()
		ins.Warned = e.emitAgentWarning(stage.name, provider.ID(), ins.Error)
		logger.Warnf("%s agent 调用失败: %v", stage.name, err)
		return ins
	}
	trimmed := strings.TrimSpace(textutil.Truncate(out, 4000))
	if trimmed == "" {
		ins.Error = "模型返回空文本"
		ins.Warned = e.emitAgentWarning(stage.name, provider.ID(), ins.Error)
		return ins
	}
	ins.Output = trimmed
	return ins
}

func (e *LegacyEngineAdapter) findAgentProvider(preferred string) provider.ModelProvider {
	if len(e.Providers) == 0 {
		return nil
	}
	preferred = strings.TrimSpace(preferred)
	if preferred != "" {
		for _, p := range e.Providers {
			if p != nil && p.Enabled() && strings.EqualFold(p.ID(), preferred) {
				return p
			}
		}
		return nil
	}
	for _, p := range e.Providers {
		if p != nil && p.Enabled() {
			return p
		}
	}
	return nil
}

func (e *LegacyEngineAdapter) invokeAgentProvider(ctx context.Context, p provider.ModelProvider, system, user string) (string, error) {
	if p == nil {
		return "", fmt.Errorf("agent provider 未配置")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	timeout := e.TimeoutSeconds
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
		defer cancel()
	}
	payload := provider.ChatPayload{System: system, User: user}
	return p.Call(ctx, payload)
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

func summarizeImagePayloads(imgs []provider.ImagePayload) []string {
	if len(imgs) == 0 {
		return nil
	}
	out := make([]string, 0, len(imgs))
	for _, img := range imgs {
		var b strings.Builder
		desc := strings.TrimSpace(img.Description)
		if desc == "" {
			desc = "(no description)"
		}
		b.WriteString(desc)
		if data := strings.TrimSpace(img.DataURI); data != "" {
			preview := textutil.Truncate(data, 512)
			b.WriteString("\nDATA: ")
			b.WriteString(preview)
		}
		out = append(out, b.String())
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
		payload.Images = cloneImagePayloads(baseImages)
	}
	purpose := fmt.Sprintf("final decision (images=%d)", len(payload.Images))
	logAIInput("main", p.ID(), purpose, payload.System, payload.User, summarizeImagePayloads(payload.Images))
	raw, err := p.Call(cctx, payload)
	logger.LogLLMResponse("main", p.ID(), purpose, raw)

	parsed := DecisionResult{}
	if err == nil {
		if arr, ok := parser.ExtractJSON(raw); ok {
			var ds []Decision
			if je := json.Unmarshal([]byte(arr), &ds); je == nil {
				parsed.Decisions = ds
				parsed.RawOutput = raw
				parsed.RawJSON = arr
				logger.Infof("模型 %s 解析到 %d 条决策", p.ID(), len(ds))
			} else {
				err = je
			}
		} else {
			snippet := raw
			if len(snippet) > 160 {
				snippet = snippet[:160] + "..."
			}
			logger.Warnf("模型 %s 响应未包含 JSON 决策数组，片段: %q", p.ID(), snippet)
			err = fmt.Errorf("未找到 JSON 决策数组")
		}
	}
	if err != nil {
		logger.Warnf("模型 %s 调用失败: %v", p.ID(), err)
	}
	return ModelOutput{
		ProviderID:    p.ID(),
		Raw:           raw,
		Parsed:        parsed,
		Err:           err,
		Images:        cloneImagePayloads(payload.Images),
		VisionEnabled: visionEnabled,
		ImageCount:    len(payload.Images),
	}
}

func (e *LegacyEngineAdapter) collectModelOutputs(ctx context.Context, call func(context.Context, provider.ModelProvider) ModelOutput) []ModelOutput {
	if !e.Parallel {
		outs := make([]ModelOutput, 0, len(e.Providers))
		for _, p := range e.Providers {
			if p != nil && p.Enabled() {
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

func cloneOutputs(src []ModelOutput) []ModelOutput {
	if len(src) == 0 {
		return nil
	}
	dst := make([]ModelOutput, len(src))
	for i := range src {
		dst[i] = src[i]
		dst[i].Images = cloneImagePayloads(src[i].Images)
		dst[i].VisionEnabled = src[i].VisionEnabled
		dst[i].ImageCount = src[i].ImageCount
	}
	return dst
}

func cloneAgentInsights(src []AgentInsight) []AgentInsight {
	if len(src) == 0 {
		return nil
	}
	dst := make([]AgentInsight, len(src))
	copy(dst, src)
	return dst
}

func cloneImagePayloads(src []provider.ImagePayload) []provider.ImagePayload {
	if len(src) == 0 {
		return nil
	}
	dst := make([]provider.ImagePayload, len(src))
	copy(dst, src)
	return dst
}

func cloneSnapshots(src []PositionSnapshot) []PositionSnapshot {
	if len(src) == 0 {
		return nil
	}
	dst := make([]PositionSnapshot, len(src))
	copy(dst, src)
	return dst
}

func cloneStrings(src []string) []string {
	if len(src) == 0 {
		return nil
	}
	dst := make([]string, len(src))
	copy(dst, src)
	return dst
}

func describeAgentPurpose(stage string) string {
	switch stage {
	case agentStageIndicator:
		return "指标洞察"
	case agentStagePattern:
		return "形态叙事"
	case agentStageTrend:
		return "趋势/支撑阻力分析"
	default:
		return "通用分析"
	}
}

func logAIInput(kind, providerID, purpose, systemPrompt, userPrompt string, imageNotes []string) {
	if strings.TrimSpace(kind) == "" {
		kind = "unknown"
	}
	sysPreview := textutil.Truncate(systemPrompt, 2000)
	userPreview := textutil.Truncate(userPrompt, 4000)
	logger.Debugf("[AI][request] kind=%s provider=%s purpose=%s system_prompt=%q user_prompt=%q",
		kind, strings.TrimSpace(providerID), purpose, sysPreview, userPreview)
	logger.LogLLMRequest(kind, strings.TrimSpace(providerID), purpose, systemPrompt, userPrompt, imageNotes, "")
}

func (e *LegacyEngineAdapter) emitAgentWarning(stage, providerID, reason string) bool {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return false
	}
	title := formatAgentStageTitle(stage)
	providerID = strings.TrimSpace(providerID)
	msg := fmt.Sprintf("[Multi-Agent 警告] %s 无输出: %s", title, reason)
	if providerID != "" {
		msg = fmt.Sprintf("[Multi-Agent 警告] %s(%s) 无输出: %s", title, providerID, reason)
	}
	logger.Warnf("%s", msg)
	n := e.AgentNotifier
	if n == nil {
		return false
	}
	if err := n.SendText(msg); err != nil {
		logger.Warnf("multi-agent 警告推送失败: %v", err)
		return false
	}
	return true
}

func (e *LegacyEngineAdapter) logStructuredBlocksDebug(ctxs []AnalysisContext) {
	if !e.DebugStructuredBlocks || len(ctxs) == 0 {
		return
	}
	limit := 4
	if len(ctxs) < limit {
		limit = len(ctxs)
	}
	var b strings.Builder
	b.WriteString("--- Structured Debug ---\n")
	for i := 0; i < limit; i++ {
		ac := ctxs[i]
		b.WriteString(fmt.Sprintf("* %s %s (%s)\n", strings.ToUpper(ac.Symbol), ac.Interval, ac.ForecastHorizon))
		if ac.PatternReport != "" {
			b.WriteString("  形态: " + textutil.Truncate(ac.PatternReport, 240) + "\n")
		}
		if ac.TrendReport != "" {
			b.WriteString("  趋势: " + textutil.Truncate(ac.TrendReport, 240) + "\n")
		}
		if ac.ImageNote != "" {
			b.WriteString("  图示: " + textutil.Truncate(ac.ImageNote, 160) + "\n")
		}
		if ac.IndicatorJSON != "" {
			b.WriteString("  指标JSON:\n")
			b.WriteString(textutil.Truncate(jsonutil.Pretty(ac.IndicatorJSON), 600))
			b.WriteString("\n")
		}
		if ac.KlineJSON != "" {
			b.WriteString("  RawKline:\n")
			b.WriteString(textutil.Truncate(ac.KlineJSON, 600))
			b.WriteString("\n")
		}
	}
	b.WriteString("--- End Structured Debug ---")
	logger.Debugf("%s", b.String())
}

func (e *LegacyEngineAdapter) renderLastDecisions(records []DecisionMemory) string {
	if len(records) == 0 {
		return ""
	}
	mem := append([]DecisionMemory(nil), records...)
	sort.Slice(mem, func(i, j int) bool {
		if strings.EqualFold(mem[i].Symbol, mem[j].Symbol) {
			return mem[i].DecidedAt.After(mem[j].DecidedAt)
		}
		return mem[i].Symbol < mem[j].Symbol
	})
	var b strings.Builder
	b.WriteString("\n## 上次 AI 决策概览\n")
	for _, m := range mem {
		age := time.Since(m.DecidedAt).Round(time.Minute)
		if age < 0 {
			age = 0
		}
		b.WriteString(fmt.Sprintf("- %s (%s 前)\n", strings.ToUpper(m.Symbol), age))
		for _, d := range m.Decisions {
			reason := trimReasoning(d.Reasoning, 360)
			if reason == "" {
				reason = "无可用理由"
			}
			line := fmt.Sprintf("    • %s ", strings.ToLower(d.Action))
			if isOpenAction(d.Action) {
				parts := make([]string, 0, 6)
				if d.PositionSizeUSD > 0 {
					parts = append(parts, fmt.Sprintf("size=%.0f", d.PositionSizeUSD))
				}
				if d.Leverage > 0 {
					parts = append(parts, fmt.Sprintf("lev=%dx", d.Leverage))
				}
				if d.TakeProfit > 0 {
					parts = append(parts, fmt.Sprintf("tp=%.4f", d.TakeProfit))
				}
				if d.StopLoss > 0 {
					parts = append(parts, fmt.Sprintf("sl=%.4f", d.StopLoss))
				}
				if len(parts) > 0 {
					line += strings.Join(parts, " ") + " "
				}
			} else if d.CloseRatio > 0 {
				line += fmt.Sprintf("close_ratio=%.2f ", d.CloseRatio)
			}
			line += reason
			b.WriteString(line + "\n")
		}
	}
	b.WriteString("请结合上轮思路判断是否需要延续。\n")
	return b.String()
}

func trimReasoning(text string, max int) string {
	text = strings.TrimSpace(text)
	if max <= 0 || len(text) == 0 {
		return text
	}
	runes := []rune(text)
	if len(runes) <= max {
		return text
	}
	return string(runes[:max]) + "…"
}

func isOpenAction(action string) bool {
	action = strings.ToLower(strings.TrimSpace(action))
	return strings.HasPrefix(action, "open_")
}

func (e *LegacyEngineAdapter) renderCurrentPositions(account AccountSnapshot, positions []PositionSnapshot) string {
	var b strings.Builder
	if account.Total > 0 || account.Available > 0 {
		currency := strings.ToUpper(strings.TrimSpace(account.Currency))
		if currency == "" {
			currency = "USDT"
		}
		b.WriteString("\n## 账户概览\n")
		line := fmt.Sprintf("- 权益: %.2f %s", account.Total, currency)
		if account.Available > 0 {
			line += fmt.Sprintf(" · 可用: %.2f", account.Available)
		}
		if account.Used > 0 {
			line += fmt.Sprintf(" · 已占用: %.2f", account.Used)
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("\n## 当前持仓\n")
	if len(positions) == 0 {
		b.WriteString("- 当前无持仓 (0)，请勿返回 close_* / update_tiers / adjust_* 指令。\n")
		return b.String()
	}
	for _, pos := range positions {
		line := fmt.Sprintf("- %s %s entry=%.4f",
			strings.ToUpper(pos.Symbol), strings.ToUpper(pos.Side), pos.EntryPrice)
		if pos.Quantity > 0 {
			line += fmt.Sprintf(" qty=%.4f", pos.Quantity)
		}
		if pos.Stake > 0 {
			line += fmt.Sprintf(" stake=%.2f", pos.Stake)
		}
		if pos.Leverage > 0 {
			line += fmt.Sprintf(" lev=x%.2f", pos.Leverage)
		}
		if pos.CurrentPrice > 0 {
			line += fmt.Sprintf(" last=%.4f", pos.CurrentPrice)
		}
		if pos.TakeProfit > 0 {
			line += fmt.Sprintf(" tp=%.4f", pos.TakeProfit)
		}
		if pos.StopLoss > 0 {
			line += fmt.Sprintf(" sl=%.4f", pos.StopLoss)
		}
		if pos.UnrealizedPnPct != 0 || pos.UnrealizedPn != 0 {
			line += fmt.Sprintf(" pnl=%s(%+.2f)", formatutil.Percent(pos.UnrealizedPnPct), pos.UnrealizedPn)
		}
		if pos.HoldingMs > 0 {
			line += fmt.Sprintf(" holding=%s", formatutil.Duration(pos.HoldingMs))
		}
		if pos.RemainingRatio > 0 {
			line += fmt.Sprintf(" remaining=%s", formatutil.Percent(pos.RemainingRatio))
		}
		if pos.AccountRatio > 0 {
			line += fmt.Sprintf(" 占比=%s", formatutil.Percent(pos.AccountRatio))
		}
		tierLines := buildTierLines(pos)
		if tierLines != "" {
			line += "\n  " + tierLines
		}
		if strings.TrimSpace(pos.TierNotes) != "" {
			line += "\n  备注: " + pos.TierNotes
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("请结合上述仓位判断是否需要平仓、加仓或调整计划。\n")
	return b.String()
}

func (e *LegacyEngineAdapter) decisionGuidelines() string {
	if e.PromptMgr != nil {
		if tpl, ok := e.PromptMgr.Get("decision_guideline"); ok && strings.TrimSpace(tpl) != "" {
			return tpl + "\n"
		}
	}
	return defaultDecisionGuideline
}

// renderDerivativesMetrics 将 OI 与资金费率附加到 User 提示词中
func (e *LegacyEngineAdapter) renderDerivativesMetrics(ctx context.Context, candidates []string) string {
	if e.Metrics == nil || (!e.IncludeOI && !e.IncludeFunding) || len(candidates) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n## 市场衍生品数据 (Market Derivatives Data)\n")
	for _, sym := range candidates {
		metricsData, ok := e.Metrics.Get(sym)
		if !ok || metricsData.Error != "" {
			b.WriteString(fmt.Sprintf("- %s: 获取衍生品数据失败 (%s)\n", strings.ToUpper(sym), metricsData.Error))
			continue
		}

		b.WriteString(fmt.Sprintf("- %s:\n", strings.ToUpper(sym)))
		if e.IncludeOI {
			b.WriteString(fmt.Sprintf("  - 最新未平仓量 (OI): %.2f\n", metricsData.OI))
			for _, tf := range e.Metrics.GetTargetTimeframes() {
				if oldOI, ok := metricsData.OIHistory[tf]; ok && oldOI > 0 {
					changePct := (metricsData.OI - oldOI) / oldOI * 100
					b.WriteString(fmt.Sprintf("    - OI %s前: %.2f (%.2f%%)\n", tf, oldOI, changePct))
				} else {
					b.WriteString(fmt.Sprintf("    - OI %s前: 无数据\n", tf))
				}
			}
		}
		if e.IncludeFunding {
			b.WriteString(fmt.Sprintf("  - 资金费率 (Funding Rate): %.4f%%\n", metricsData.FundingRate*100))
		}
	}
	b.WriteString("请结合这些衍生品数据评估市场情绪和资金动向。\n")
	return b.String()
}

func buildTierLines(pos PositionSnapshot) string {
	parts := make([]string, 0, 3)
	if pos.Tier1Target > 0 {
		parts = append(parts, fmt.Sprintf("Tier1 %.4f (%s)%s",
			pos.Tier1Target, formatutil.Percent(pos.Tier1Ratio), doneFlag(pos.Tier1Done)))
	}
	if pos.Tier2Target > 0 {
		parts = append(parts, fmt.Sprintf("Tier2 %.4f (%s)%s",
			pos.Tier2Target, formatutil.Percent(pos.Tier2Ratio), doneFlag(pos.Tier2Done)))
	}
	if pos.Tier3Target > 0 {
		parts = append(parts, fmt.Sprintf("Tier3 %.4f (%s)%s",
			pos.Tier3Target, formatutil.Percent(pos.Tier3Ratio), doneFlag(pos.Tier3Done)))
	}
	if len(parts) == 0 {
		return ""
	}
	b := strings.Builder{}
	b.WriteString(strings.Join(parts, " | "))
	if strings.TrimSpace(pos.TierNotes) != "" {
		b.WriteString(" | ")
		b.WriteString(pos.TierNotes)
	}
	return b.String()
}

func doneFlag(done bool) string {
	if done {
		return " ✅"
	}
	return ""
}

type klineWindow struct {
	Symbol   string
	Interval string
	Horizon  string
	Trend    string
	Bars     []market.Candle
}

func (e *LegacyEngineAdapter) renderKlineWindows(ctxs []AnalysisContext) string {
	if len(ctxs) == 0 {
		return ""
	}
	rank := buildIntervalRank(e.Intervals)
	windows := make([]klineWindow, 0, len(ctxs))
	for _, ac := range ctxs {
		bars, err := parseRecentCandles(ac.KlineJSON, priceWindowBars)
		if err != nil {
			logger.Debugf("kline snapshot 解析失败 %s %s: %v", ac.Symbol, ac.Interval, err)
			continue
		}
		if len(bars) == 0 {
			continue
		}
		win := klineWindow{
			Symbol:   strings.ToUpper(strings.TrimSpace(ac.Symbol)),
			Interval: strings.TrimSpace(ac.Interval),
			Horizon:  strings.TrimSpace(ac.ForecastHorizon),
			Trend:    ac.TrendReport,
			Bars:     bars,
		}
		if win.Symbol == "" || win.Interval == "" {
			continue
		}
		windows = append(windows, win)
	}
	if len(windows) == 0 {
		return ""
	}
	sort.Slice(windows, func(i, j int) bool {
		if windows[i].Symbol == windows[j].Symbol {
			ri := intervalRankValue(windows[i].Interval, rank)
			rj := intervalRankValue(windows[j].Interval, rank)
			if ri != rj {
				return ri < rj
			}
			return windows[i].Interval < windows[j].Interval
		}
		return windows[i].Symbol < windows[j].Symbol
	})
	var b strings.Builder
	b.WriteString("\n## Price Windows（最近 6 根，最新在前）\n")
	for _, win := range windows {
		header := fmt.Sprintf("- %s %s", win.Symbol, win.Interval)
		if win.Horizon != "" {
			header += fmt.Sprintf(" (%s)", win.Horizon)
		}
		b.WriteString(header + "\n")
		b.WriteString("  Bars:\n")
		for idx := len(win.Bars) - 1; idx >= 0; idx-- {
			bar := win.Bars[idx]
			b.WriteString(fmt.Sprintf("    [%s, o=%s, h=%s, l=%s, c=%s, v=%.2f]\n",
				bar.TimeString(),
				formatutil.Float(bar.Open, 4),
				formatutil.Float(bar.High, 4),
				formatutil.Float(bar.Low, 4),
				formatutil.Float(bar.Close, 4),
				bar.Volume,
			))
		}
		if summary := market.Candles(win.Bars).Snapshot(win.Interval, win.Trend); summary != "" {
			b.WriteString("  Snapshot: " + summary + "\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("请结合这些时间窗口评估当前价格位置与动量。\n")
	return b.String()
}

func buildIntervalRank(intervals []string) map[string]int {
	if len(intervals) == 0 {
		return nil
	}
	rank := make(map[string]int, len(intervals))
	for idx, iv := range intervals {
		key := strings.ToLower(strings.TrimSpace(iv))
		if key == "" {
			continue
		}
		if _, exists := rank[key]; !exists {
			rank[key] = idx
		}
	}
	return rank
}

func intervalRankValue(iv string, rank map[string]int) int {
	if len(rank) == 0 {
		return 0
	}
	key := strings.ToLower(strings.TrimSpace(iv))
	if val, ok := rank[key]; ok {
		return val
	}
	return len(rank) + 1
}

func parseRecentCandles(raw string, keep int) ([]market.Candle, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || keep <= 0 {
		return nil, nil
	}
	var candles []market.Candle
	if err := json.Unmarshal([]byte(raw), &candles); err != nil {
		return nil, err
	}
	if len(candles) == 0 {
		return nil, nil
	}
	if len(candles) > keep {
		candles = candles[len(candles)-keep:]
	}
	return candles, nil
}
