package decision

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	brcfg "brale/internal/config"
	"brale/internal/gateway/provider"
	"brale/internal/logger"
	"brale/internal/market"
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

	Name_ string

	Parallel bool // 并行调用多个模型

	// 是否为每个模型分别输出思维链与JSON（便于对比调参）
	LogEachModel bool
	// 是否在日志中输出 Structured Blocks 调试提示
	DebugStructuredBlocks bool

	// 可选：补充 OI 与资金费率
	Metrics interface {
		OI(ctx context.Context, symbol string) (float64, error)
		Funding(ctx context.Context, symbol string) (float64, error)
	}
	IncludeOI      bool
	IncludeFunding bool

	// 调用超时（秒）：限制单模型调用最长时间，避免卡死
	TimeoutSeconds int
}

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

	// 调用所有已启用模型（可并行），带超时控制
	outs := make([]ModelOutput, 0, len(e.Providers))
	timeout := e.TimeoutSeconds
	callOne := func(parent context.Context, p provider.ModelProvider) ModelOutput {
		cctx := parent
		var cancel context.CancelFunc
		if timeout > 0 {
			cctx, cancel = context.WithTimeout(parent, time.Duration(timeout)*time.Second)
			defer cancel()
		}
		logger.Debugf("调用模型: %s", p.ID())
		payload := provider.ChatPayload{
			System:     sys,
			User:       usr,
			ExpectJSON: p.ExpectsJSON(),
		}
		if p.SupportsVision() {
			payload.Images = visionPayloads
		}
		logAIInput("main", p.ID(), fmt.Sprintf("final decision (images=%d)", len(payload.Images)), payload.System, payload.User)
		raw, err := p.Call(cctx, payload)
		parsed := DecisionResult{}
		if err == nil {
			if arr, ok := ExtractJSONArrayCompat(raw); ok {
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
				// 捕获未包含 JSON 数组的情况，记录部分原始文本帮助排查
				snippet := raw
				if len(snippet) > 160 {
					snippet = snippet[:160] + "..."
				}
				logger.Warnf("模型 %s 响应未包含 JSON 决策数组，片段: %q", p.ID(), snippet)
				err = fmt.Errorf("未找到 JSON 决策数组")
			}
		} else {
			logger.Warnf("模型 %s 调用失败: %v", p.ID(), err)
		}
		return ModelOutput{
			ProviderID: p.ID(),
			Raw:        raw,
			Parsed:     parsed,
			Err:        err,
			Images:     cloneImagePayloads(payload.Images),
		}
	}
	if e.Parallel {
		enabled := 0
		ch := make(chan ModelOutput, len(e.Providers))
		for _, p := range e.Providers {
			if p != nil && p.Enabled() {
				enabled++
				go func(p provider.ModelProvider) { ch <- callOne(ctx, p) }(p)
			}
		}
		for i := 0; i < enabled; i++ {
			outs = append(outs, <-ch)
		}
	} else {
		for _, p := range e.Providers {
			if p != nil && p.Enabled() {
				outs = append(outs, callOne(ctx, p))
			}
		}
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
			_, start, ok := ExtractJSONArrayWithIndex(o.Raw)
			if ok {
				thought := strings.TrimSpace(o.Raw[:start])
				thought = TrimTo(thought, 2400)
				thoughts = append(thoughts, ThoughtRow{Provider: o.ProviderID, Thought: thought})
				// 逐条填充结果
				if len(o.Parsed.Decisions) > 0 {
					for _, d := range o.Parsed.Decisions {
						r := ResultRow{Provider: o.ProviderID, Action: d.Action, Symbol: d.Symbol, Reason: d.Reasoning}
						// 尽量不裁剪，但仍保底上限
						r.Reason = TrimTo(r.Reason, 3600)
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
		// 更大的列宽，尽量不裁剪
		tThoughts := RenderThoughtsTable(thoughts, 180)
		tResults := RenderResultsTable(results, 180)
		logger.Infof("\n%s\n%s", tThoughts, tResults)
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
	var b strings.Builder
	b.WriteString("# 决策输入（Multi-Agent 汇总）\n")
	e.appendLastDecisions(&b, input.LastDecisions)
	e.appendCurrentPositions(&b, input.Positions)
	e.logStructuredBlocksDebug(input.Analysis)
	if len(insights) > 0 {
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
				b.WriteString(TrimTo(out, 3600))
				b.WriteString("\n")
				return
			}
			status := "无可用结论"
			if ins.Warned {
				status += "（已通知）"
			}
			if errTxt := strings.TrimSpace(ins.Error); errTxt != "" {
				errTxt = TrimTo(errTxt, 240)
				status += ": " + errTxt
			}
			b.WriteString(prefix + status + "\n")
		}
		b.WriteString("\n## Multi-Agent Insights\n")
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
		b.WriteString("请基于这些多 Agent 结论与风险约束输出最终 JSON 决策。\n")
	} else {
		b.WriteString("\n## Multi-Agent Insights\n- 暂无可用结论，请谨慎观望或输出空决策。\n")
	}
	req := `请先输出简短的【思维链】（1句，说明判断依据与结论），换行后仅输出 JSON 数组。
	数组中每项需含：symbol、action、reasoning。
	reasoning中当多空信号差距在20以上时，需要包含bull_score，bear_score，resistance_upper，resistance_lower
	如已有仓位且需要部分止盈/减仓，添加 close_ratio（0-1），无仓位时勿返回。
	若 action 为 open_long 或 open_short，必须返回 take_profit、stop_loss（绝对价，浮点）及 leverage（2–50，依信号强度）。
若 action 为 adjust_stop_loss，必须返回新 stop_loss。
示例:
思维链: 4h 供需不明、15m 无形态。
[{"symbol":"BTCUSDT","action":"hold","reasoning":"多空信号不足"}]
`
	b.WriteString(req)

	return b.String()
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
	logAIInput(fmt.Sprintf("multi-agent:%s", stage.name), provider.ID(), describeAgentPurpose(stage.name), tpl, user)
	out, err := e.invokeAgentProvider(ctx, provider, tpl, user)
	if err != nil {
		ins.Error = err.Error()
		ins.Warned = e.emitAgentWarning(stage.name, provider.ID(), ins.Error)
		logger.Warnf("%s agent 调用失败: %v", stage.name, err)
		return ins
	}
	trimmed := strings.TrimSpace(TrimTo(out, 4000))
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

func cloneOutputs(src []ModelOutput) []ModelOutput {
	if len(src) == 0 {
		return nil
	}
	dst := make([]ModelOutput, len(src))
	for i := range src {
		dst[i] = src[i]
		dst[i].Images = cloneImagePayloads(src[i].Images)
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

func logAIInput(kind, providerID, purpose, systemPrompt, userPrompt string) {
	if strings.TrimSpace(kind) == "" {
		kind = "unknown"
	}
	sysPreview := TrimTo(systemPrompt, 2000)
	userPreview := TrimTo(userPrompt, 4000)
	logger.Debugf("[AI][request] kind=%s provider=%s purpose=%s system_prompt=%q user_prompt=%q",
		kind, strings.TrimSpace(providerID), purpose, sysPreview, userPreview)
}

// formatVolumeSlice 将成交量切片格式化为紧凑的字符串，例如 "[123, 456, 789]"
func formatVolumeSlice(volumes []float64) string {
	if len(volumes) == 0 {
		return "[]"
	}
	parts := make([]string, len(volumes))
	for i, v := range volumes {
		parts[i] = fmt.Sprintf("%.0f", v)
	}
	return "[" + strings.Join(parts, ", ") + "]"
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
	logger.Warnf(msg)
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
			b.WriteString("  形态: " + TrimTo(ac.PatternReport, 240) + "\n")
		}
		if ac.TrendReport != "" {
			b.WriteString("  趋势: " + TrimTo(ac.TrendReport, 240) + "\n")
		}
		if ac.ImageNote != "" {
			b.WriteString("  图示: " + TrimTo(ac.ImageNote, 160) + "\n")
		}
		if ac.IndicatorJSON != "" {
			b.WriteString("  指标JSON:\n")
			b.WriteString(TrimTo(PrettyJSON(ac.IndicatorJSON), 600))
			b.WriteString("\n")
		}
		if ac.KlineJSON != "" {
			b.WriteString("  RawKline:\n")
			b.WriteString(TrimTo(ac.KlineJSON, 600))
			b.WriteString("\n")
		}
	}
	b.WriteString("--- End Structured Debug ---")
	logger.Debugf("%s", b.String())
}

func (e *LegacyEngineAdapter) appendLastDecisions(b *strings.Builder, records []DecisionMemory) {
	if len(records) == 0 || b == nil {
		return
	}
	mem := append([]DecisionMemory(nil), records...)
	sort.Slice(mem, func(i, j int) bool {
		if strings.EqualFold(mem[i].Symbol, mem[j].Symbol) {
			return mem[i].DecidedAt.After(mem[j].DecidedAt)
		}
		return mem[i].Symbol < mem[j].Symbol
	})
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

func (e *LegacyEngineAdapter) appendCurrentPositions(b *strings.Builder, positions []PositionSnapshot) {
	if len(positions) == 0 || b == nil {
		return
	}
	b.WriteString("\n## 当前持仓\n")
	for _, pos := range positions {
		line := fmt.Sprintf("- %s %s entry=%.4f",
			strings.ToUpper(pos.Symbol), strings.ToUpper(pos.Side), pos.EntryPrice)
		if pos.Quantity > 0 {
			line += fmt.Sprintf(" size=%.0f", pos.Quantity)
		}
		if pos.TakeProfit > 0 {
			line += fmt.Sprintf(" tp=%.4f", pos.TakeProfit)
		}
		if pos.StopLoss > 0 {
			line += fmt.Sprintf(" sl=%.4f", pos.StopLoss)
		}
		if pos.HoldingMs > 0 {
			line += fmt.Sprintf(" holding=%s", formatHoldingDuration(pos.HoldingMs))
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("请结合上述仓位判断是否需要平仓、加仓或调整计划。\n")
}

func formatHoldingDuration(ms int64) string {
	if ms <= 0 {
		return "-"
	}
	d := time.Duration(ms) * time.Millisecond
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	if h > 0 {
		return fmt.Sprintf("%dh%dm", h, m)
	}
	if m > 0 {
		return fmt.Sprintf("%dm%ds", m, d/time.Second)
	}
	return fmt.Sprintf("%ds", d/time.Second)
}
