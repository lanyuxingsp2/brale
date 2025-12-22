package decision

import (
	"context"
	"fmt"
	"strings"
	"time"

	brcfg "brale/internal/config"
	"brale/internal/gateway/provider"
	"brale/internal/logger"
	textutil "brale/internal/pkg/text"

	"golang.org/x/sync/errgroup"
)

// MarketMechanicsAgent execution rules:
// 1) Consume only the derivatives block (OI/Funding/Fear&Greed).
// 2) Do not inject derivatives into the main decision prompt (Scheme B).
// 3) Use cached derivatives data; do not force-refresh APIs per decision cycle.
// 4) Compute a fingerprint from derivatives snapshots and call LLM only when it changes.
// 5) Evaluate once per symbol per cycle; reuse previous output when unchanged.
type agentStageConfig struct {
	name       string
	tplName    string
	providerID string
	builder    func([]AnalysisContext, brcfg.MultiAgentConfig) string
}

func (e *DecisionEngine) runMultiAgents(ctx context.Context, input Context) []AgentInsight {
	cfg := e.MultiAgent
	if !cfg.Enabled || len(input.Analysis) == 0 || e.PromptMgr == nil {
		return nil
	}
	ctxs := filterAgentContexts(input.Analysis, input.Directives)
	if len(ctxs) == 0 {
		return nil
	}
	hasMechanics := false
	for _, ac := range ctxs {
		if dir, ok := lookupDirective(ac.Symbol, input.Directives); ok && dir.allowDerivatives() {
			hasMechanics = true
			break
		}
	}
	stages := []agentStageConfig{
		{name: agentStageIndicator, tplName: cfg.IndicatorTemplate, providerID: e.StageProviders[agentStageIndicator], builder: buildIndicatorAgentPrompt},
		{name: agentStagePattern, tplName: cfg.PatternTemplate, providerID: e.StageProviders[agentStagePattern], builder: buildPatternAgentPrompt},
		{name: agentStageTrend, tplName: cfg.TrendTemplate, providerID: e.StageProviders[agentStageTrend], builder: buildTrendAgentPrompt},
	}
	if hasMechanics {
		stages = append(stages, agentStageConfig{name: agentStageMechanics, tplName: cfg.MechanicsTemplate, providerID: e.StageProviders[agentStageMechanics]})
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
			if stage.name == agentStageMechanics {
				results[i] = e.executeAgentStage(egCtx, stage, ctxs, cfg, input)
				return nil
			}
			results[i] = e.executeAgentStage(egCtx, stage, ctxs, cfg, Context{})
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

func (e *DecisionEngine) executeAgentStage(ctx context.Context, stage agentStageConfig, ctxs []AnalysisContext, cfg brcfg.MultiAgentConfig, fullCtx Context) AgentInsight {
	tpl := strings.TrimSpace(e.loadTemplate(stage.tplName))
	if tpl == "" {
		return AgentInsight{}
	}
	user, latestUpdate, fingerprint := e.prepareAgentUser(ctx, stage, ctxs, cfg, fullCtx)
	ins := AgentInsight{Stage: stage.name, System: tpl, User: user, Fingerprint: fingerprint}

	if stop := e.validateAgentInput(&ins, stage, latestUpdate); stop {
		return ins
	}

	provider := e.selectAgentProvider(stage, &ins)
	if provider == nil {
		return ins
	}
	ins.ProviderID = provider.ID()

	prev := e.lookupPreviousAgentOutput(ctx, ctxs, stage.name, ins.ProviderID)
	if reused := e.reuseMechanicsOutput(stage.name, prev, fingerprint, latestUpdate, &ins); reused {
		return ins
	}
	if prev.Output != "" {
		ins.User = appendPreviousAgentOutput(ins.User, stage.name, ins.ProviderID, prev)
	}

	purpose := describeAgentPurpose(stage.name)
	logAIInput(fmt.Sprintf("multi-agent:%s", stage.name), provider.ID(), purpose, tpl, ins.User, nil)
	start := time.Now()
	out, err := e.invokeAgentProvider(ctx, provider, tpl, ins.User)
	logger.LogLLMResponse(fmt.Sprintf("multi-agent:%s", stage.name), provider.ID(), purpose, out)
	if err != nil {
		ins.Error = err.Error()
		ins.Warned = e.emitAgentWarning(stage.name, provider.ID(), ins.Error)
		logger.Warnf("%s agent 调用失败 provider=%s elapsed=%s err=%v", stage.name, provider.ID(), time.Since(start).Truncate(time.Millisecond), err)
		return ins
	}
	trimmed := strings.TrimSpace(textutil.Truncate(out, 4000))
	if trimmed == "" {
		ins.Error = "模型返回空文本"
		ins.Warned = e.emitAgentWarning(stage.name, provider.ID(), ins.Error)
		logger.Warnf("%s agent 返回空文本 provider=%s elapsed=%s", stage.name, provider.ID(), time.Since(start).Truncate(time.Millisecond))
		return ins
	}
	if containsBannedWords(trimmed) {
		ins.Error = "命中禁词，输出作废"
		ins.Warned = e.emitAgentWarning(stage.name, provider.ID(), ins.Error)
		ins.InvalidVote = true
		logger.Warnf("%s agent 输出命中禁词，已作废 provider=%s", stage.name, provider.ID())
		return ins
	}
	ins.Output = trimmed
	return ins
}

func (e *DecisionEngine) prepareAgentUser(ctx context.Context, stage agentStageConfig, ctxs []AnalysisContext, cfg brcfg.MultiAgentConfig, fullCtx Context) (string, time.Time, string) {
	if stage.name == agentStageMechanics {
		user, latestUpdate, fingerprint := e.buildMechanicsAgentPrompt(ctx, ctxs, fullCtx)
		return strings.TrimSpace(user), latestUpdate, fingerprint
	}
	return strings.TrimSpace(stage.builder(ctxs, cfg)), time.Time{}, ""
}

func (e *DecisionEngine) validateAgentInput(ins *AgentInsight, stage agentStageConfig, latestUpdate time.Time) bool {
	if stage.name != agentStageMechanics && ins.User == "" {
		ins.Error = "输入为空"
		ins.Warned = e.emitAgentWarning(stage.name, stage.providerID, ins.Error)
		return true
	}
	if stage.name != agentStageMechanics {
		return false
	}
	if ins.Fingerprint == "" {
		ins.Output = "衍生品数据尚未更新或获取失败，跳过本次分析。"
		return true
	}
	if ins.User == "" && ins.Fingerprint != "" {
		ins.Error = "输入为空"
		ins.Warned = e.emitAgentWarning(stage.name, stage.providerID, ins.Error)
		return true
	}
	_ = latestUpdate
	return false
}

func (e *DecisionEngine) selectAgentProvider(stage agentStageConfig, ins *AgentInsight) provider.ModelProvider {
	preferred := strings.TrimSpace(stage.providerID)
	if preferred == "" {
		ins.Error = "未配置模型"
		ins.Warned = true
		return nil
	}
	p := e.findAgentProvider(preferred)
	if p != nil {
		return p
	}
	if preferred != "" {
		ins.Error = fmt.Sprintf("未找到模型 %s", preferred)
	} else {
		ins.Error = "无可用模型"
	}
	ins.Warned = e.emitAgentWarning(stage.name, preferred, ins.Error)
	return nil
}

func (e *DecisionEngine) reuseMechanicsOutput(stageName string, prev AgentOutputSnapshot, fingerprint string, latestUpdate time.Time, ins *AgentInsight) bool {
	if stageName != agentStageMechanics || prev.Output == "" {
		return false
	}
	if prev.Fingerprint != "" && fingerprint == prev.Fingerprint {
		ins.Output = prev.Output
		return true
	}
	if prev.Fingerprint == "" && !latestUpdate.IsZero() && prev.Timestamp > 0 {
		prevTS := time.UnixMilli(prev.Timestamp)
		if !latestUpdate.After(prevTS) {
			ins.Output = prev.Output
			return true
		}
	}
	return false
}

func (e *DecisionEngine) lookupPreviousAgentOutput(ctx context.Context, ctxs []AnalysisContext, stage, providerID string) AgentOutputSnapshot {
	if e == nil || e.AgentHistory == nil {
		return AgentOutputSnapshot{}
	}
	symbol := agentSymbolFromContexts(ctxs)
	if symbol == "" {
		return AgentOutputSnapshot{}
	}
	stage = strings.TrimSpace(stage)
	providerID = strings.TrimSpace(providerID)
	if stage == "" || providerID == "" {
		return AgentOutputSnapshot{}
	}
	out, err := e.AgentHistory.LatestAgentOutput(ctx, symbol, stage, providerID)
	if err != nil {
		logger.Debugf("lookupPreviousAgentOutput failed symbol=%s stage=%s provider=%s err=%v", symbol, stage, providerID, err)
		return AgentOutputSnapshot{}
	}
	out.Output = strings.TrimSpace(out.Output)
	if out.Output == "" {
		return AgentOutputSnapshot{}
	}
	return out
}

func agentSymbolFromContexts(ctxs []AnalysisContext) string {
	symbol := ""
	for _, ac := range ctxs {
		s := strings.TrimSpace(ac.Symbol)
		if s == "" {
			continue
		}
		if symbol == "" {
			symbol = s
			continue
		}
		if !strings.EqualFold(symbol, s) {
			return ""
		}
	}
	return strings.ToUpper(strings.TrimSpace(symbol))
}

func appendPreviousAgentOutput(user, stage, providerID string, previous AgentOutputSnapshot) string {
	user = strings.TrimSpace(user)
	previous.Output = strings.TrimSpace(previous.Output)
	if user == "" || previous.Output == "" {
		return user
	}
	stage = strings.TrimSpace(stage)
	providerID = strings.TrimSpace(providerID)
	var b strings.Builder
	b.WriteString(user)
	b.WriteString("\n\n")
	b.WriteString("# 上一轮输出的决策 (Previous Agent Output)\n")
	if stage != "" || providerID != "" {
		b.WriteString("stage=")
		b.WriteString(stage)
		b.WriteString(" provider=")
		b.WriteString(providerID)
		b.WriteString("\n")
	}
	if previous.Timestamp > 0 {
		b.WriteString("执行时间=")
		b.WriteString(formatAgentOutputTimestamp(previous.Timestamp))
		b.WriteString("\n")
	}
	b.WriteString(previous.Output)
	return strings.TrimSpace(b.String())
}

func formatAgentOutputTimestamp(ts int64) string {
	if ts <= 0 {
		return ""
	}
	return time.UnixMilli(ts).UTC().Format(time.RFC3339)
}

func (e *DecisionEngine) findAgentProvider(preferred string) provider.ModelProvider {
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

func (e *DecisionEngine) invokeAgentProvider(ctx context.Context, p provider.ModelProvider, system, user string) (string, error) {
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

func containsBannedWords(text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	lower := strings.ToLower(text)
	banned := []string{
		"bullish", "bearish",
		"止损", "止盈",
		"一定", "必然", "确定",
	}
	for _, w := range banned {
		if strings.Contains(lower, w) {
			return true
		}
	}
	return false
}

func (e *DecisionEngine) emitAgentWarning(stage, providerID, reason string) bool {
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

func (e *DecisionEngine) buildMechanicsAgentPrompt(ctx context.Context, ctxs []AnalysisContext, fullCtx Context) (string, time.Time, string) {
	if e == nil || len(ctxs) == 0 {
		return "", time.Time{}, ""
	}
	pb, ok := e.PromptBuilder.(*DefaultPromptBuilder)
	if !ok || pb == nil {
		return "", time.Time{}, ""
	}
	sec := pb.buildDerivativesSection(ctx, ctxs, fullCtx.Directives)
	return sec.Text, sec.LatestUpdate, sec.Fingerprint
}

func describeAgentPurpose(stage string) string {
	switch stage {
	case agentStageIndicator:
		return "指标洞察"
	case agentStagePattern:
		return "形态叙事"
	case agentStageTrend:
		return "趋势/支撑阻力分析"
	case agentStageMechanics:
		return "市场力学/衍生品环境"
	default:
		return "通用分析"
	}
}

func lookupDirective(symbol string, directives map[string]ProfileDirective) (ProfileDirective, bool) {
	if len(directives) == 0 {
		return ProfileDirective{}, false
	}
	key := strings.ToUpper(strings.TrimSpace(symbol))
	if key == "" {
		return ProfileDirective{}, false
	}
	dir, ok := directives[key]
	return dir, ok
}

func filterAgentContexts(ctxs []AnalysisContext, directives map[string]ProfileDirective) []AnalysisContext {
	if len(ctxs) == 0 || len(directives) == 0 {
		return nil
	}
	out := make([]AnalysisContext, 0, len(ctxs))
	for _, ac := range ctxs {
		dir, ok := lookupDirective(ac.Symbol, directives)
		if !ok || !dir.MultiAgentEnabled {
			continue
		}
		out = append(out, ac)
	}
	return out
}
