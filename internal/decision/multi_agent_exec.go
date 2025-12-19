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
			results[i] = e.executeAgentStage(egCtx, stage, ctxs, cfg)
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

func (e *DecisionEngine) executeAgentStage(ctx context.Context, stage agentStageConfig, ctxs []AnalysisContext, cfg brcfg.MultiAgentConfig) AgentInsight {
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
	if prev := e.lookupPreviousAgentOutput(ctx, ctxs, stage.name, ins.ProviderID); prev != "" {
		user = appendPreviousAgentOutput(user, stage.name, ins.ProviderID, prev)
		ins.User = user
	}
	purpose := describeAgentPurpose(stage.name)
	logAIInput(fmt.Sprintf("multi-agent:%s", stage.name), provider.ID(), purpose, tpl, user, nil)
	start := time.Now()
	out, err := e.invokeAgentProvider(ctx, provider, tpl, user)
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
	ins.Output = trimmed
	return ins
}

func (e *DecisionEngine) lookupPreviousAgentOutput(ctx context.Context, ctxs []AnalysisContext, stage, providerID string) string {
	if e == nil || e.AgentHistory == nil {
		return ""
	}
	symbol := agentSymbolFromContexts(ctxs)
	if symbol == "" {
		return ""
	}
	stage = strings.TrimSpace(stage)
	providerID = strings.TrimSpace(providerID)
	if stage == "" || providerID == "" {
		return ""
	}
	out, err := e.AgentHistory.LatestAgentOutput(ctx, symbol, stage, providerID)
	if err != nil {
		logger.Debugf("lookupPreviousAgentOutput failed symbol=%s stage=%s provider=%s err=%v", symbol, stage, providerID, err)
		return ""
	}
	return strings.TrimSpace(out)
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

func appendPreviousAgentOutput(user, stage, providerID, previous string) string {
	user = strings.TrimSpace(user)
	previous = strings.TrimSpace(previous)
	if user == "" || previous == "" {
		return user
	}
	stage = strings.TrimSpace(stage)
	providerID = strings.TrimSpace(providerID)
	var b strings.Builder
	b.WriteString(user)
	b.WriteString("\n\n")
	b.WriteString("# Previous Agent Output\n")
	if stage != "" || providerID != "" {
		b.WriteString("stage=")
		b.WriteString(stage)
		b.WriteString(" provider=")
		b.WriteString(providerID)
		b.WriteString("\n")
	}
	b.WriteString(previous)
	return strings.TrimSpace(b.String())
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
