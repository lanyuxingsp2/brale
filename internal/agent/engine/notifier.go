package engine

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"brale/internal/decision"
	"brale/internal/gateway/notifier"
	"brale/internal/logger"
)

const (
	entryFillTimeout = 11 * time.Minute
)

type Notifier interface {
	SendStructured(msg notifier.StructuredMessage) error
}

func (e *LiveEngine) notifyMetaSummary(res decision.DecisionResult) {
	if e.Notifier == nil || e.Config == nil || !strings.EqualFold(e.Config.AI.Aggregation, "meta") {
		return
	}
	if err := e.sendMetaSummaryTelegram(res); err != nil {
		logger.Warnf("Telegram push failed (meta): %v", err)
	}
}

func (e *LiveEngine) sendMetaSummaryTelegram(res decision.DecisionResult) error {
	if e.Notifier == nil {
		return nil
	}

	if bd := res.MetaBreakdown; bd != nil && len(bd.Symbols) > 0 {
		logger.Infof("Meta breakdown: %d symbols, sending structured format", len(bd.Symbols))
		sections := buildMetaBreakdownSections(res, bd)
		msg := notifier.StructuredMessage{
			Icon:      "🗳️",
			Title:     "Meta 聚合投票",
			Sections:  sections,
			Timestamp: time.Now().UTC(),
		}
		return e.Notifier.SendStructured(msg)
	}

	logger.Warnf("Meta breakdown missing or empty, skipping notification.")
	return nil
}

func buildMetaBreakdownSections(res decision.DecisionResult, bd *decision.MetaVoteBreakdown) []notifier.MessageSection {
	sections := make([]notifier.MessageSection, 0, 2+len(bd.Symbols))

	conclusionLines := make([]string, 0, 2)
	if line := metaSummaryFirstLine(res.MetaSummary); line != "" {
		conclusionLines = append(conclusionLines, line)
	}
	if final := renderMetaFinalActions(res.Decisions); final != "" {
		conclusionLines = append(conclusionLines, final)
	}
	if len(conclusionLines) > 0 {
		sections = append(sections, notifier.MessageSection{Title: "结论", Lines: conclusionLines})
	}

	for _, sym := range bd.Symbols {
		title := strings.TrimSpace(sym.Symbol)
		if title == "" {
			title = "-"
		}

		if votePart := renderMetaVotesInline(sym.Votes); votePart != "" {
			title = fmt.Sprintf("%s（%s）", title, votePart)
		}
		lines := make([]string, 0, len(sym.Providers)+1)

		for _, p := range sym.Providers {
			act := strings.Join(p.Actions, ", ")
			if strings.TrimSpace(act) == "" {
				act = "-"
			}
			id := strings.TrimSpace(p.ProviderID)
			if id == "" {
				id = "-"
			}

			if shouldShowMetaProviderWeight(p.Weight) {
				lines = append(lines, fmt.Sprintf("- %s[%s]: %s", id, formatMetaWeight(p.Weight), act))
			} else {
				lines = append(lines, fmt.Sprintf("- %s: %s", id, act))
			}
		}

		finalAction := strings.TrimSpace(sym.FinalAction)
		if finalAction == "" {
			finalAction = "HOLD"
		}
		lines = append(lines, fmt.Sprintf("→ 结论：%s", strings.ToUpper(finalAction)))
		sections = append(sections, notifier.MessageSection{Title: title, Lines: lines})
	}

	return sections
}

func metaSummaryFirstLine(summary string) string {
	summary = strings.TrimSpace(strings.ReplaceAll(summary, "```", "'''"))
	if summary == "" {
		return ""
	}
	if idx := strings.Index(summary, "\n"); idx >= 0 {
		summary = summary[:idx]
	}
	summary = strings.TrimSpace(summary)
	summary = strings.TrimPrefix(summary, "Meta聚合：")
	return strings.TrimSpace(summary)
}

func renderMetaFinalActions(decisions []decision.Decision) string {
	if len(decisions) == 0 {
		return ""
	}
	if len(decisions) == 1 && decision.NormalizeAction(decisions[0].Action) == "hold" {
		return "最终执行：HOLD"
	}
	parts := make([]string, 0, len(decisions))
	for _, d := range decisions {
		act := decision.NormalizeAction(d.Action)
		if act == "" {
			continue
		}
		sym := strings.ToUpper(strings.TrimSpace(d.Symbol))
		if sym == "" {
			sym = "-"
		}
		parts = append(parts, fmt.Sprintf("%s %s", sym, act))
	}
	if len(parts) == 0 {
		return ""
	}
	return "最终执行：" + strings.Join(parts, " / ")
}

func renderMetaVotesInline(votes []decision.MetaActionVote) string {
	if len(votes) == 0 {
		return ""
	}
	parts := make([]string, 0, len(votes))
	for _, v := range votes {
		act := strings.TrimSpace(v.Action)
		if act == "" || v.Weight <= 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s:%s", act, formatMetaWeight(v.Weight)))
	}
	return strings.Join(parts, " / ")
}

func shouldShowMetaProviderWeight(weight float64) bool {
	if weight <= 0 {
		return false
	}

	return math.Abs(weight-1.0) > 1e-9
}

func formatMetaWeight(weight float64) string {
	if weight == 0 {
		return "0"
	}
	if math.Abs(weight-math.Round(weight)) <= 1e-9 {
		return fmt.Sprintf("%.0f", weight)
	}
	return fmt.Sprintf("%.2f", weight)
}

func (e *LiveEngine) notifyOpenAfterFill(ctx context.Context, d decision.Decision, fallbackPrice float64, validateIv string) {

	symbol := strings.ToUpper(strings.TrimSpace(d.Symbol))
	if symbol == "" {
		e.notifyOpen(ctx, d, fallbackPrice, validateIv)
		return
	}
	go func(dec decision.Decision, sym string) {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		timeout := time.After(entryFillTimeout)
		for {
			select {
			case <-ctx.Done():
				return
			case <-timeout:
				logger.Warnf("Waiting for %s entry_fill timeout, sending timeout alert", sym)
				e.notifyEntryTimeout(ctx, dec)
				return
			case <-ticker.C:
				if entry := e.lookupEntryPrice(ctx, sym); entry > 0 {
					e.notifyOpen(ctx, dec, entry, validateIv)
					return
				}
			}
		}
	}(d, symbol)
}

func (e *LiveEngine) lookupEntryPrice(ctx context.Context, symbol string) float64 {
	positions, err := e.PosService.ListPositions(ctx)
	if err != nil {
		return 0
	}
	for _, p := range positions {
		if strings.EqualFold(p.Symbol, symbol) {
			return p.EntryPrice
		}
	}
	return 0
}

func (e *LiveEngine) notifyOpen(ctx context.Context, d decision.Decision, entryPrice float64, validateIv string) {
	if e.Notifier == nil {
		return
	}
	rrVal := computeRiskReward(d.Action, entryPrice, d.StopLoss, d.TakeProfit)
	logOpenDetails(d, entryPrice, rrVal)
	actionCN := renderActionCN(d.Action)
	side := deriveSide(d.Action)
	if actionCN == "" {
		actionCN = d.Action
	}

	sections := e.buildOpenSections(d, entryPrice, rrVal, validateIv, side)
	msg := notifier.StructuredMessage{
		Icon:      "🚀",
		Title:     fmt.Sprintf("信号触发：%s %s", strings.ToUpper(strings.TrimSpace(d.Symbol)), actionCN),
		Sections:  sections,
		Timestamp: time.Now().UTC(),
	}
	if err := e.Notifier.SendStructured(msg); err != nil {
		logger.Warnf("Telegram 推送失败: %v", err)
	}
}

func computeRiskReward(action string, entryPrice, stopLoss, takeProfit float64) float64 {
	if entryPrice <= 0 {
		return 0
	}
	var risk, reward float64
	switch action {
	case "open_long":
		risk = entryPrice - stopLoss
		reward = takeProfit - entryPrice
	case "open_short":
		risk = stopLoss - entryPrice
		reward = entryPrice - takeProfit
	}
	if risk > 0 && reward > 0 {
		return reward / risk
	}
	return 0
}

func logOpenDetails(d decision.Decision, entryPrice, rrVal float64) {
	if entryPrice <= 0 {
		return
	}
	if rrVal > 0 {
		logger.Infof("开仓详情: %s %s entry=%.4f RR=%.2f sl=%.4f tp=%.4f",
			d.Symbol, d.Action, entryPrice, rrVal, d.StopLoss, d.TakeProfit)
		return
	}
	logger.Infof("开仓详情: %s %s entry=%.4f sl=%.4f tp=%.4f",
		d.Symbol, d.Action, entryPrice, d.StopLoss, d.TakeProfit)
}

func (e *LiveEngine) buildOpenSections(d decision.Decision, entryPrice, rrVal float64, validateIv, side string) []notifier.MessageSection {
	sections := make([]notifier.MessageSection, 0, 4)

	if lines := buildPriceLines(entryPrice, rrVal, validateIv); len(lines) > 0 {
		sections = append(sections, notifier.MessageSection{Title: "行情", Lines: lines})
	}
	if lines := buildTradeLines(d); len(lines) > 0 {
		sections = append(sections, notifier.MessageSection{Title: "仓位", Lines: lines})
	}
	if plan := e.renderExitPlanSummary(d.ExitPlan, d.ExitPlanVersion, entryPrice, side); plan != "" {
		planLines := strings.Split(plan, "\n")
		sections = append(sections, notifier.MessageSection{Title: "策略", Lines: planLines})
		logger.Infof("策略详情：\n%s", plan)
	}
	if lines := buildReasonLines(d.Reasoning); len(lines) > 0 {
		sections = append(sections, notifier.MessageSection{Title: "触发理由", Lines: lines})
	}
	return sections
}

func buildPriceLines(entryPrice, rrVal float64, validateIv string) []string {
	lines := make([]string, 0, 3)
	if entryPrice > 0 {
		iv := ""
		if validateIv != "" {
			iv = " · 周期 " + strings.ToUpper(validateIv)
		}
		lines = append(lines, fmt.Sprintf("当前价格 %.4f%s", entryPrice, iv))
	}
	if rrVal > 0 {
		lines = append(lines, fmt.Sprintf("即时风险回报：%.2f", rrVal))
	}
	return lines
}

func buildTradeLines(d decision.Decision) []string {
	lines := make([]string, 0, 4)
	if d.Leverage > 0 {
		lines = append(lines, fmt.Sprintf("杠杆 %dx", d.Leverage))
	}
	if d.PositionSizeUSD > 0 {
		lines = append(lines, fmt.Sprintf("仓位 %.0f USDT", d.PositionSizeUSD))
	}
	if d.Confidence > 0 {
		lines = append(lines, fmt.Sprintf("模型信心 %d%%", d.Confidence))
	}
	return lines
}

func buildReasonLines(reason string) []string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil
	}
	return strings.Split(reason, "\n")
}

func (e *LiveEngine) notifyEntryTimeout(ctx context.Context, d decision.Decision) {
	if e.Notifier == nil {
		return
	}
	actionCN := renderActionCN(d.Action)
	if actionCN == "" {
		actionCN = d.Action
	}
	lines := []string{
		"已等待超过 11 分钟仍未收到交易所 entry_fill 回执，可能尚未成交或被拒单。",
		"请检查交易所委托状态，必要时手动撤单/重试。",
	}
	msg := notifier.StructuredMessage{
		Icon:      "⏱️",
		Title:     fmt.Sprintf("下单超时：%s %s", strings.ToUpper(strings.TrimSpace(d.Symbol)), actionCN),
		Sections:  []notifier.MessageSection{{Title: "提醒", Lines: lines}},
		Timestamp: time.Now().UTC(),
	}
	if err := e.Notifier.SendStructured(msg); err != nil {
		logger.Warnf("Telegram 推送失败(timeout): %v", err)
	}
}

func renderActionCN(action string) string {
	switch action {
	case "open_long":
		return "开多"
	case "open_short":
		return "开空"
	case "close_long":
		return "平多"
	case "close_short":
		return "平空"
	case "hold", "wait":
		return "观望"
	case "update_exit_plan":
		return "更新策略"
	default:
		return ""
	}
}

func deriveSide(action string) string {
	switch action {
	case "open_long", "close_long":
		return "long"
	case "open_short", "close_short":
		return "short"
	default:
		return ""
	}
}

func (e *LiveEngine) renderExitPlanSummary(spec *decision.ExitPlanSpec, version int, entryPrice float64, side string) string {
	if spec == nil || strings.TrimSpace(spec.ID) == "" {
		return ""
	}
	label := strings.TrimSpace(spec.ID)
	if e.ExitPlans != nil {
		if tpl, ok := e.ExitPlans.Template(label); ok {
			label = tpl.ID
			if version <= 0 {
				version = tpl.Version
			}
		}
	}
	var builder strings.Builder
	builder.WriteString("策略：")
	if version > 0 {
		builder.WriteString(fmt.Sprintf("%s (v%d)", label, version))
	} else {
		builder.WriteString(label)
	}
	paramLines := summarizePlanParams(spec.Params, entryPrice, side)
	if len(paramLines) > 0 {
		builder.WriteString("\n" + strings.Join(paramLines, "\n"))
	}
	return builder.String()
}
