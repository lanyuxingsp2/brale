package decision

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"brale/internal/logger"
	"brale/internal/market"
	formatutil "brale/internal/pkg/format"
	jsonutil "brale/internal/pkg/jsonutil"
	textutil "brale/internal/pkg/text"
)

func (b *DefaultPromptBuilder) renderAgentBlocks(insights []AgentInsight) string {
	if len(insights) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n## Multi-Agent 协作\n")
	stageOrder := []string{agentStageIndicator, agentStagePattern, agentStageTrend}
	stageMap := make(map[string]AgentInsight, len(insights))
	for _, ins := range insights {
		if ins.Stage == "" {
			continue
		}
		stageMap[ins.Stage] = ins
	}
	write := func(ins AgentInsight) {
		if ins.Stage == "" {
			return
		}
		title := formatAgentStageTitle(ins.Stage)
		provider := strings.TrimSpace(ins.ProviderID)
		if provider == "" {
			provider = "-"
		}
		header := fmt.Sprintf("- [%s | 模型:%s] ", title, provider)
		output := strings.TrimSpace(ins.Output)
		if output != "" {
			sb.WriteString(header)
			sb.WriteString(textutil.Truncate(output, 3600))
			sb.WriteString("\n")
			return
		}
		status := "无输出"
		if errTxt := strings.TrimSpace(ins.Error); errTxt != "" {
			status = errTxt
		}
		if ins.Warned {
			status += "（已通知）"
		}
		sb.WriteString(header + status + "\n")
	}
	used := make(map[string]bool, len(stageOrder))
	for _, stage := range stageOrder {
		if ins, ok := stageMap[stage]; ok {
			write(ins)
			used[stage] = true
		}
	}
	for _, ins := range insights {
		if used[ins.Stage] {
			continue
		}
		write(ins)
	}
	sb.WriteString("↑ 若 Agent 提示某组件已触发，保持现有退出计划，仅对未触发段位进行增改。\n")
	return sb.String()
}

func (b *DefaultPromptBuilder) renderPreviousReasoning(reasonMap map[string]string) string {
	if len(reasonMap) == 0 {
		return ""
	}
	type entry struct {
		symbol string
		reason string
	}
	entries := make([]entry, 0, len(reasonMap))
	for sym, reason := range reasonMap {
		symbol := strings.ToUpper(strings.TrimSpace(sym))
		reason = strings.TrimSpace(reason)
		if symbol == "" || reason == "" {
			continue
		}
		entries = append(entries, entry{symbol: symbol, reason: reason})
	}
	if len(entries) == 0 {
		return ""
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].symbol < entries[j].symbol
	})
	var sb strings.Builder
	sb.WriteString("\n## 上一轮决策回顾\n")
	for _, ent := range entries {
		sb.WriteString(fmt.Sprintf("- %s：%s\n", ent.symbol, textutil.Truncate(ent.reason, 1000)))
	}
	return sb.String()
}

func (b *DefaultPromptBuilder) renderOutputConstraints(input Context) string {
	return renderOutputConstraints(input.ProfilePrompts, "只可以返回json数据，并且只可以有单个action。示例:")
}

func (b *DefaultPromptBuilder) renderDerivativesMetrics(ctxs []string, directives map[string]ProfileDirective) string {
	if b.Metrics == nil || len(ctxs) == 0 || len(directives) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n## 市场衍生品数据 (Market Derivatives Data)\n")
	for _, sym := range ctxs {
		dir, ok := lookupDirective(sym, directives)
		if !ok || !dir.allowDerivatives() {
			continue
		}
		metricsData, ok := b.Metrics.Get(sym)
		if !ok || metricsData.Error != "" {
			sb.WriteString(fmt.Sprintf("- %s: 获取衍生品数据失败 (%s)\n", strings.ToUpper(sym), metricsData.Error))
			continue
		}

		sb.WriteString(fmt.Sprintf("- %s:\n", strings.ToUpper(sym)))
		if dir.IncludeOI {
			sb.WriteString(fmt.Sprintf("  - 最新未平仓量 (OI): %.2f\n", metricsData.OI))
			for _, tf := range b.Metrics.GetTargetTimeframes() {
				if oldOI, ok := metricsData.OIHistory[tf]; ok && oldOI > 0 {
					changePct := (metricsData.OI - oldOI) / oldOI * 100
					sb.WriteString(fmt.Sprintf("    - OI %s前: %.2f (%.2f%%)\n", tf, oldOI, changePct))
				} else {
					sb.WriteString(fmt.Sprintf("    - OI %s前: 无数据\n", tf))
				}
			}
		}
		if dir.IncludeFunding {
			sb.WriteString(fmt.Sprintf("  - 资金费率 (Funding Rate): %.4f%%\n", metricsData.FundingRate*100))
		}
	}
	sb.WriteString("请结合这些衍生品数据评估市场情绪和资金动向。\n")
	return sb.String()
}

func (b *DefaultPromptBuilder) renderKlineWindows(ctxs []AnalysisContext) string {
	windows, latestLine := collectKlineWindows(ctxs)
	if len(windows) == 0 {
		return ""
	}
	sortKlineWindows(windows, buildIntervalRank(b.Intervals))
	return renderKlineWindowsOutput(windows, latestLine)
}

func collectKlineWindows(ctxs []AnalysisContext) ([]klineWindow, string) {
	if len(ctxs) == 0 {
		return nil, ""
	}
	windows := make([]klineWindow, 0, len(ctxs))
	var latestLine string
	for _, ac := range ctxs {
		win, ok, line := buildKlineWindow(ac)
		if !ok {
			continue
		}
		if latestLine == "" && line != "" {
			latestLine = line
		}
		windows = append(windows, win)
	}
	return windows, latestLine
}

func buildKlineWindow(ac AnalysisContext) (klineWindow, bool, string) {
	csvData := strings.TrimSpace(ac.KlineCSV)
	bars, err := parseRecentCandles(ac.KlineJSON, priceWindowBars)
	if err != nil {
		logger.Debugf("kline snapshot 解析失败 %s %s: %v", ac.Symbol, ac.Interval, err)
		return klineWindow{}, false, ""
	}
	if len(bars) == 0 && csvData == "" {
		return klineWindow{}, false, ""
	}
	symbol := strings.ToUpper(strings.TrimSpace(ac.Symbol))
	interval := strings.TrimSpace(ac.Interval)
	if symbol == "" || interval == "" {
		return klineWindow{}, false, ""
	}
	win := klineWindow{
		Symbol:   symbol,
		Interval: interval,
		Horizon:  strings.TrimSpace(ac.ForecastHorizon),
		Trend:    ac.TrendReport,
		CSV:      csvData,
		Bars:     bars,
	}
	return win, true, buildLatestPriceLine(symbol, bars)
}

func buildLatestPriceLine(symbol string, bars []market.Candle) string {
	if len(bars) == 0 || strings.TrimSpace(symbol) == "" {
		return ""
	}
	latest := bars[len(bars)-1]
	return fmt.Sprintf("最新价格：%s 收=%.4f 时间=%s",
		strings.ToUpper(strings.TrimSpace(symbol)),
		latest.Close,
		time.UnixMilli(latest.CloseTime).UTC().Format(time.RFC3339))
}

func sortKlineWindows(windows []klineWindow, rank map[string]int) {
	if len(windows) < 2 {
		return
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
}

func renderKlineWindowsOutput(windows []klineWindow, latestLine string) string {
	var sb strings.Builder
	sb.WriteString("\n## Price Windows（最近 4 根，最新在前）\n")
	if latestLine != "" {
		sb.WriteString(latestLine + "\n")
	}
	for _, win := range windows {
		writeKlineWindow(&sb, win)
	}
	sb.WriteString("请结合这些时间窗口评估当前价格位置与动量。\n")
	return sb.String()
}

func writeKlineWindow(sb *strings.Builder, win klineWindow) {
	if sb == nil || win.Symbol == "" || win.Interval == "" {
		return
	}
	header := fmt.Sprintf("- %s %s", win.Symbol, win.Interval)
	if win.Horizon != "" {
		header += fmt.Sprintf(" (%s)", win.Horizon)
	}
	sb.WriteString(header + "\n")
	for idx := len(win.Bars) - 1; idx >= 0; idx-- {
		bar := win.Bars[idx]
		sb.WriteString(fmt.Sprintf("    [%s, o=%s, h=%s, l=%s, c=%s, v=%.2f]\n",
			bar.TimeString(),
			formatutil.Float(bar.Open, 4),
			formatutil.Float(bar.High, 4),
			formatutil.Float(bar.Low, 4),
			formatutil.Float(bar.Close, 4),
			bar.Volume,
		))
	}
	if len(win.Bars) > 0 {
		if summary := market.Candles(win.Bars).Snapshot(win.Interval, win.Trend); summary != "" {
			sb.WriteString("  Snapshot: " + summary + "\n")
		}
		sb.WriteString("\n")
	}
}

func logStructuredBlocksDebug(debug bool, ctxs []AnalysisContext) {
	if !debug || len(ctxs) == 0 {
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
