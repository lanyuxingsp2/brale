package agent

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"brale/internal/decision"
	"brale/internal/gateway/notifier"
	"brale/internal/logger"
	"brale/internal/pkg/utils"
)

const (
	entryFillTimeout = 11 * time.Minute
)

func (s *LiveService) notifyMetaSummary(res decision.DecisionResult) {
	if s.tg == nil || !strings.EqualFold(s.cfg.AI.Aggregation, "meta") {
		return
	}
	if err := s.sendMetaSummaryTelegram(res); err != nil {
		logger.Warnf("Telegram push failed (meta): %v", err)
	}
}

func (s *LiveService) sendMetaSummaryTelegram(res decision.DecisionResult) error {
	if s.tg == nil {
		return nil
	}

	if bd := res.MetaBreakdown; bd != nil && len(bd.Symbols) > 0 {
		sections := buildMetaBreakdownSections(res, bd)
		msg := notifier.StructuredMessage{
			Icon:      "🗳️",
			Title:     "Meta 聚合投票",
			Sections:  sections,
			Timestamp: time.Now().UTC(),
		}
		return s.tg.SendStructured(msg)
	}

	summary := strings.TrimSpace(res.MetaSummary)
	if summary == "" && len(res.SymbolResults) > 0 {
		chunks := make([]string, 0, len(res.SymbolResults))
		for _, blk := range res.SymbolResults {
			if txt := strings.TrimSpace(blk.MetaSummary); txt != "" {
				label := strings.TrimSpace(blk.Symbol)
				if label == "" {
					label = "-"
				}
				chunks = append(chunks, fmt.Sprintf("[%s]\n%s", label, txt))
			}
		}
		summary = strings.Join(chunks, "\n\n")
	}
	if summary == "" {
		return nil
	}

	lines := strings.Split(summary, "\n")
	var conclusion string
	var weights []string
	var reasons []string
	for _, raw := range lines {
		line := strings.TrimSpace(strings.ReplaceAll(raw, "```", "'''"))
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "Meta聚合："):
			conclusion = strings.TrimSpace(strings.TrimPrefix(line, "Meta聚合："))
		case strings.Contains(line, "=>"):
			weights = append(weights, line)
		default:
			line = strings.TrimPrefix(line, "- ")
			line = strings.TrimPrefix(line, "• ")
			if line != "" {
				reasons = append(reasons, line)
			}
		}
	}
	sections := make([]notifier.MessageSection, 0, 3)
	if conclusion != "" {
		sections = append(sections, notifier.MessageSection{Title: "结论", Lines: []string{conclusion}})
	}
	if len(weights) > 0 {
		sections = append(sections, notifier.MessageSection{Title: "投票权重", Lines: weights})
	}
	if len(reasons) > 0 {
		sections = append(sections, notifier.MessageSection{Title: "Agent 参考", Lines: reasons})
	}
	msg := notifier.StructuredMessage{
		Icon:      "🗳️",
		Title:     "Meta 聚合投票",
		Sections:  sections,
		Timestamp: time.Now().UTC(),
	}
	return s.tg.SendStructured(msg)
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
		lines := make([]string, 0, len(sym.Providers))
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
				lines = append(lines, fmt.Sprintf("%s[%s]: %s", id, formatMetaWeight(p.Weight), act))
				continue
			}
			lines = append(lines, fmt.Sprintf("%s: %s", id, act))
		}
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
	// Only show when non-default to keep messages compact.
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

func (s *LiveService) notifyOpenAfterFill(ctx context.Context, d decision.Decision, fallbackPrice float64, validateIv string) {
	if s.execManager == nil {
		s.notifyOpen(ctx, d, fallbackPrice, validateIv)
		return
	}
	symbol := strings.ToUpper(strings.TrimSpace(d.Symbol))
	if symbol == "" {
		s.notifyOpen(ctx, d, fallbackPrice, validateIv)
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
				s.notifyEntryTimeout(ctx, dec)
				return
			case <-ticker.C:
				if entry := s.lookupEntryPrice(sym); entry > 0 {
					s.notifyOpen(ctx, dec, entry, validateIv)
					return
				}
			}
		}
	}(d, symbol)
}

func (s *LiveService) notifyOpen(ctx context.Context, d decision.Decision, entryPrice float64, validateIv string) {
	if s.tg == nil {
		return
	}
	rrVal := 0.0
	if entryPrice > 0 {
		var risk, reward float64
		switch d.Action {
		case "open_long":
			risk = entryPrice - d.StopLoss
			reward = d.TakeProfit - entryPrice
		case "open_short":
			risk = d.StopLoss - entryPrice
			reward = entryPrice - d.TakeProfit
		}
		if risk > 0 && reward > 0 {
			rrVal = reward / risk
		}
	}

	if entryPrice > 0 {
		if rrVal > 0 {
			logger.Infof("开仓详情: %s %s entry=%.4f RR=%.2f sl=%.4f tp=%.4f",
				d.Symbol, d.Action, entryPrice, rrVal, d.StopLoss, d.TakeProfit)
		} else {
			logger.Infof("开仓详情: %s %s entry=%.4f sl=%.4f tp=%.4f",
				d.Symbol, d.Action, entryPrice, d.StopLoss, d.TakeProfit)
		}
	}

	actionCN := renderActionCN(d.Action)
	side := deriveSide(d.Action)
	if actionCN == "" {
		actionCN = d.Action
	}
	sections := make([]notifier.MessageSection, 0, 4)
	priceLines := make([]string, 0, 3)
	if entryPrice > 0 {
		iv := ""
		if validateIv != "" {
			iv = " · 周期 " + strings.ToUpper(validateIv)
		}
		priceLines = append(priceLines, fmt.Sprintf("当前价格 %.4f%s", entryPrice, iv))
	}
	if rrVal > 0 {
		priceLines = append(priceLines, fmt.Sprintf("即时风险回报：%.2f", rrVal))
	}
	if len(priceLines) > 0 {
		sections = append(sections, notifier.MessageSection{Title: "行情", Lines: priceLines})
	}
	tradeLines := make([]string, 0, 4)
	if d.Leverage > 0 {
		tradeLines = append(tradeLines, fmt.Sprintf("杠杆 %dx", d.Leverage))
	}
	if d.PositionSizeUSD > 0 {
		tradeLines = append(tradeLines, fmt.Sprintf("仓位 %.0f USDT", d.PositionSizeUSD))
	}
	if d.Confidence > 0 {
		tradeLines = append(tradeLines, fmt.Sprintf("模型信心 %d%%", d.Confidence))
	}
	if len(tradeLines) > 0 {
		sections = append(sections, notifier.MessageSection{Title: "仓位", Lines: tradeLines})
	}
	if plan := s.renderExitPlanSummary(d.ExitPlan, d.ExitPlanVersion, entryPrice, side); plan != "" {
		planLines := strings.Split(plan, "\n")
		sections = append(sections, notifier.MessageSection{Title: "策略", Lines: planLines})
		logger.Infof("策略详情：\n%s", plan)
	}
	if reason := strings.TrimSpace(d.Reasoning); reason != "" {
		reasonLines := strings.Split(reason, "\n")
		sections = append(sections, notifier.MessageSection{Title: "触发理由", Lines: reasonLines})
	}
	msg := notifier.StructuredMessage{
		Icon:      "🚀",
		Title:     fmt.Sprintf("信号触发：%s %s", strings.ToUpper(strings.TrimSpace(d.Symbol)), actionCN),
		Sections:  sections,
		Timestamp: time.Now().UTC(),
	}
	if err := s.tg.SendStructured(msg); err != nil {
		logger.Warnf("Telegram 推送失败: %v", err)
	}
}

func (s *LiveService) lookupEntryPrice(symbol string) float64 {
	if s.execManager == nil {
		return 0
	}
	return s.execManager.EntryPriceBySymbol(strings.ToUpper(symbol))
}

func (s *LiveService) notifyEntryTimeout(ctx context.Context, d decision.Decision) {
	if s.tg == nil {
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
	if err := s.tg.SendStructured(msg); err != nil {
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

func (s *LiveService) renderExitPlanSummary(spec *decision.ExitPlanSpec, version int, entryPrice float64, side string) string {
	if spec == nil || strings.TrimSpace(spec.ID) == "" {
		return ""
	}
	label := strings.TrimSpace(spec.ID)
	if s.exitPlans != nil {
		if tpl, ok := s.exitPlans.Template(label); ok {
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

func summarizePlanParams(params map[string]any, entryPrice float64, side string) []string {
	if len(params) == 0 {
		return nil
	}
	lines := make([]string, 0, 4)
	if v, ok := utils.AsFloat(params["stop_loss_pct"]); ok && v != 0 {
		lines = append(lines, "· 初始止损 "+utils.FormatPercent(v)+approxPrice(entryPrice, v, side))
	}
	if v, ok := utils.AsFloat(params["final_stop_loss_pct"]); ok && v != 0 {
		lines = append(lines, "· 最终止损 "+utils.FormatPercent(v)+approxPrice(entryPrice, v, side))
	}
	if v, ok := utils.AsFloat(params["final_take_profit_pct"]); ok && v != 0 {
		lines = append(lines, "· 最终止盈 "+utils.FormatPercent(v)+approxPrice(entryPrice, v, side))
	}
	if v, ok := utils.AsFloat(params["take_profit_pct"]); ok && v != 0 {
		lines = append(lines, "· 止盈 "+utils.FormatPercent(v)+approxPrice(entryPrice, v, side))
	}
	if tiers := summarizeTiers(params["tiers"], entryPrice, side); tiers != "" {
		lines = append(lines, "· 分段止盈："+tiers)
	}
	if children := summarizePlanChildren(params["children"], entryPrice, side); len(children) > 0 {
		lines = append(lines, children...)
	}
	return lines
}

func summarizeTiers(raw any, entryPrice float64, side string) string {
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return ""
	}
	details := make([]string, 0, len(list))
	for idx, entry := range list {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		target, _ := utils.AsFloat(m["target"])
		ratio, _ := utils.AsFloat(m["ratio"])
		priceSuffix := approxPrice(entryPrice, target, side)
		details = append(details, fmt.Sprintf("T%d 目标%s%s · 比例%s", idx+1, utils.FormatPercent(target), priceSuffix, utils.FormatPercent(ratio)))
	}
	return strings.Join(details, "；")
}

func approxPrice(entry float64, pct float64, side string) string {
	price := targetPrice(entry, pct, side)
	if price <= 0 {
		return ""
	}
	return fmt.Sprintf(" ≈ %.2f", price)
}

func targetPrice(entry float64, pct float64, side string) float64 {
	if entry <= 0 {
		return 0
	}
	side = strings.ToLower(strings.TrimSpace(side))
	adj := pct
	if side == "short" {
		adj = -pct
	}
	return entry * (1 + adj)
}

func summarizePlanChildren(raw any, entryPrice float64, side string) []string {
	children, ok := raw.([]any)
	if !ok || len(children) == 0 {
		return nil
	}
	lines := make([]string, 0, len(children))
	for _, item := range children {
		child, ok := item.(map[string]any)
		if !ok {
			continue
		}
		component := strings.TrimSpace(fmt.Sprint(child["component"]))
		params, _ := child["params"].(map[string]any)
		lines = append(lines, summarizePlanComponent(component, params, entryPrice, side)...)
	}
	return lines
}

func summarizePlanComponent(component string, params map[string]any, entryPrice float64, side string) []string {
	component = strings.TrimSpace(component)
	if component == "" {
		if nested := summarizePlanChildren(params["children"], entryPrice, side); len(nested) > 0 {
			return nested
		}
		return nil
	}
	switch component {
	case "tp_tiers", "tp_single", "sl_tiers", "sl_single":
		return summarizeTierComponent(component, params, entryPrice, side)
	case "tp_atr", "sl_atr":
		return summarizeATRComponent(component, params)
	default:
		if nested := summarizePlanChildren(params["children"], entryPrice, side); len(nested) > 0 {
			return nested
		}
		if tiers := summarizeTierComponent(component, params, entryPrice, side); len(tiers) > 0 {
			return tiers
		}
		return nil
	}
}

type tierEntry struct {
	Price float64
	Ratio float64
}

func summarizeTierComponent(component string, params map[string]any, entryPrice float64, side string) []string {
	entries := parseTierEntries(params["tiers"], entryPrice, side)
	if len(entries) == 0 {
		return nil
	}
	label := tierComponentPrefix(component)
	lines := make([]string, 0, len(entries))
	for idx, entry := range entries {
		stage := label
		if len(entries) > 1 {
			stage = fmt.Sprintf("%s#%d", label, idx+1)
		}
		line := fmt.Sprintf("· %s @%.4f", stage, entry.Price)
		if entry.Ratio > 0 {
			line += fmt.Sprintf(" · 比例%s", utils.FormatRatio(entry.Ratio))
		}
		lines = append(lines, line)
	}
	return lines
}

func parseTierEntries(raw any, entryPrice float64, side string) []tierEntry {
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return nil
	}
	out := make([]tierEntry, 0, len(list))
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		price, _ := utils.NumberFromKeys(m, "target_price", "targetPrice", "price")
		if price <= 0 && entryPrice > 0 {
			if pct, ok := utils.NumberFromKeys(m, "target", "target_pct"); ok {
				price = targetPrice(entryPrice, pct, side)
			}
		}
		if price <= 0 {
			continue
		}
		ratio, _ := utils.AsFloat(m["ratio"])
		out = append(out, tierEntry{Price: price, Ratio: ratio})
	}
	return out
}

func summarizeATRComponent(component string, params map[string]any) []string {
	if len(params) == 0 {
		return nil
	}
	label := tierComponentPrefix(component)
	atrValue, _ := utils.AsFloat(params["atr_value"])
	trigger, _ := utils.AsFloat(params["trigger_multiplier"])
	trail, _ := utils.AsFloat(params["trail_multiplier"])
	line := fmt.Sprintf("· %s ATR=%.4f", label, atrValue)
	if trigger > 0 {
		line += fmt.Sprintf(" · 触发%.2fx", trigger)
	}
	if trail > 0 {
		line += fmt.Sprintf(" · 追踪%.2fx", trail)
	}
	return []string{line}
}

func tierComponentPrefix(component string) string {
	switch component {
	case "tp_tiers", "tp_single":
		return "止盈"
	case "sl_tiers", "sl_single":
		return "止损"
	case "tp_atr":
		return "ATR 止盈"
	case "sl_atr":
		return "ATR 止损"
	default:
		return strings.ToUpper(component)
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
