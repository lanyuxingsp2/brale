package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	brcfg "brale/internal/config"
	"brale/internal/indicators"
	"brale/internal/logger"
	"brale/internal/prompt"
	"brale/internal/store"
)

// 中文说明：
// LegacyEngineAdapter：沿用“旧版思路”的提示词结构（System 模板 + User 数据摘要），
// 但底层通过 ModelProvider 调用，并用 JSON 数组解析为决策结果；
// 后续可替换为严格对接旧版 decision/engine.go（需要完整账户/持仓/指标上下文）。

type LegacyEngineAdapter struct {
	Providers []ModelProvider // 可配置多个模型
	Agg       Aggregator      // 聚合策略（默认 first-wins）
	Observer  DecisionObserver

	PromptMgr      *prompt.Manager // 提示词管理器
	SystemTemplate string          // 使用的系统模板名

	KStore      store.KlineStore // K线存储（用于构建 User 摘要）
	Intervals   []string         // 要在摘要中展示的周期
	Horizon     brcfg.HorizonProfile
	HorizonName string

	Name_ string

	Parallel bool // 并行调用多个模型

	// 是否为每个模型分别输出思维链与JSON（便于对比调参）
	LogEachModel bool

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

// DecisionObserver 在每次模型聚合后回调，便于外部记录输入/输出。
type DecisionObserver interface {
	AfterDecide(ctx context.Context, trace DecisionTrace)
}

// DecisionTrace 描述一次完整调用的材料与结果。
type DecisionTrace struct {
	SystemPrompt string
	UserPrompt   string
	Outputs      []ModelOutput
	Best         ModelOutput
	Candidates   []string
	Timeframes   []string
	HorizonName  string
	Positions    []PositionSnapshot
}

func (e *LegacyEngineAdapter) Name() string {
	if e.Name_ != "" {
		return e.Name_
	}
	return "legacy-adapter"
}

// Decide 构建 System/User 提示词 → 调用多个模型 → 聚合 → 解析 JSON 决策
func (e *LegacyEngineAdapter) Decide(ctx context.Context, input Context) (DecisionResult, error) {
	sys, usr := e.ComposePrompts(ctx, input)

	// 调用所有已启用模型（可并行），带超时控制
	outs := make([]ModelOutput, 0, len(e.Providers))
	timeout := e.TimeoutSeconds
	callOne := func(parent context.Context, p ModelProvider) ModelOutput {
		cctx := parent
		var cancel context.CancelFunc
		if timeout > 0 {
			cctx, cancel = context.WithTimeout(parent, time.Duration(timeout)*time.Second)
			defer cancel()
		}
		logger.Debugf("调用模型: %s", p.ID())
		raw, err := p.Call(cctx, sys, usr)
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
		return ModelOutput{ProviderID: p.ID(), Raw: raw, Parsed: parsed, Err: err}
	}
	if e.Parallel {
		enabled := 0
		ch := make(chan ModelOutput, len(e.Providers))
		for _, p := range e.Providers {
			if p != nil && p.Enabled() {
				enabled++
				go func(p ModelProvider) { ch <- callOne(ctx, p) }(p)
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
	result.Decisions = normalizeAndAlignDecisions(result.Decisions, input.Positions)
	best.Parsed.Decisions = result.Decisions

	if e.Observer != nil {
		e.Observer.AfterDecide(ctx, DecisionTrace{
			SystemPrompt: sys,
			UserPrompt:   usr,
			Outputs:      cloneOutputs(outs),
			Best:         best,
			Candidates:   cloneStrings(input.Candidates),
			Timeframes:   cloneStrings(e.Intervals),
			HorizonName:  e.HorizonName,
			Positions:    cloneSnapshots(input.Positions),
		})
	}
	return result, nil
}

// ComposePrompts 返回当前配置下的 System/User 提示词。
func (e *LegacyEngineAdapter) ComposePrompts(ctx context.Context, input Context) (string, string) {
	return e.loadSystem(), e.buildUserSummary(ctx, input)
}

// loadSystem 从 PromptManager 读取系统模板
func (e *LegacyEngineAdapter) loadSystem() string {
	if e.PromptMgr == nil {
		return ""
	}
	if s, ok := e.PromptMgr.Get(e.SystemTemplate); ok {
		return s
	}
	return "你是专业的加密货币交易AI。请根据市场数据与风险控制做出决策。\n"
}

// buildUserSummary 将候选币种与当前仓位的摘要组装为 User 提示词
func (e *LegacyEngineAdapter) buildUserSummary(ctx context.Context, input Context) string {
	var b strings.Builder
	horizonName := e.HorizonName
	if horizonName == "" {
		horizonName = "default"
	}
	b.WriteString(fmt.Sprintf("# 候选币种数据摘要（持仓周期：%s）\n\n", horizonName))
	profile := e.Horizon
	emaCfg := profile.Indicators.EMA
	if emaCfg.Fast <= 0 {
		emaCfg.Fast = 21
	}
	if emaCfg.Mid <= 0 {
		emaCfg.Mid = 50
	}
	if emaCfg.Slow <= 0 {
		emaCfg.Slow = 200
	}
	rsiCfg := profile.Indicators.RSI
	if rsiCfg.Period <= 0 {
		rsiCfg.Period = 14
	}
	if rsiCfg.Oversold == 0 {
		rsiCfg.Oversold = 30
	}
	if rsiCfg.Overbought == 0 {
		rsiCfg.Overbought = 70
	}
	if rsiCfg.Overbought <= rsiCfg.Oversold {
		rsiCfg.Oversold = 30
		rsiCfg.Overbought = 70
	}
	if len(input.LastDecisions) > 0 {
		b.WriteString("\n## 上次 AI 决策概览\n")
		mem := append([]DecisionMemory(nil), input.LastDecisions...)
		sort.Slice(mem, func(i, j int) bool {
			if strings.EqualFold(mem[i].Symbol, mem[j].Symbol) {
				return mem[i].DecidedAt.After(mem[j].DecidedAt)
			}
			return mem[i].Symbol < mem[j].Symbol
		})
		var fullPrev []Decision
		for _, m := range mem {
			age := time.Since(m.DecidedAt).Round(time.Minute)
			if age < 0 {
				age = 0
			}
			b.WriteString(fmt.Sprintf("- %s (%s 前)\n", strings.ToUpper(m.Symbol), age))
			for _, d := range m.Decisions {
				reason := strings.TrimSpace(d.Reasoning)
				if len(reason) > 120 {
					reason = reason[:120] + "…"
				}
				b.WriteString(fmt.Sprintf("    • %s size=%.0f tp=%.4f sl=%.4f %s\n",
					strings.ToLower(d.Action), d.PositionSizeUSD, d.TakeProfit, d.StopLoss, reason))
			}
			fullPrev = append(fullPrev, m.Decisions...)
		}
		if raw := strings.TrimSpace(input.LastRawJSON); raw != "" {
			b.WriteString("\n```json\n")
			b.WriteString(PrettyJSON(raw))
			b.WriteString("\n```\n")
		} else if len(fullPrev) > 0 {
			if rawJSON, err := json.MarshalIndent(fullPrev, "", "  "); err == nil {
				b.WriteString("\n```json\n")
				b.Write(rawJSON)
				b.WriteString("\n```\n")
			}
		}
		b.WriteString("请结合上一轮思路评估是否需要延续/调整。\n")
	}

	group := map[string]string{}
	for _, tf := range profile.EntryTimeframes {
		group[tf] = "entry"
	}
	for _, tf := range profile.ConfirmTimeframes {
		group[tf] = "confirm"
	}
	for _, tf := range profile.BackgroundTimeframes {
		group[tf] = "background"
	}
	labelText := func(tf string) string {
		switch group[tf] {
		case "entry":
			return "入场"
		case "confirm":
			return "确认"
		case "background":
			return "背景"
		default:
			return ""
		}
	}
	intervals := e.Intervals
	if len(intervals) == 0 {
		intervals = profile.AllTimeframes()
	}
	for _, sym := range input.Candidates {
		b.WriteString(sym)
		b.WriteString(": ")
		first := true
		for _, iv := range intervals {
			if e.KStore == nil {
				continue
			}
			ks, _ := e.KStore.Get(ctx, sym, iv)
			if len(ks) == 0 {
				continue
			}
			closes := make([]float64, 0, len(ks))
			for _, k := range ks {
				closes = append(closes, k.Close)
			}
			last := ks[len(ks)-1]
			emaFast := indicators.EMA(closes, emaCfg.Fast)
			emaMid := indicators.EMA(closes, emaCfg.Mid)
			emaSlow := indicators.EMA(closes, emaCfg.Slow)
			macd := indicators.MACD(closes)
			rsi := indicators.RSI(closes, rsiCfg.Period)
			rsiState := "中性"
			if rsi >= rsiCfg.Overbought {
				rsiState = "超买"
			} else if rsi <= rsiCfg.Oversold {
				rsiState = "超卖"
			}
			trendRef := emaMid
			refName := fmt.Sprintf("EMA%d", emaCfg.Mid)
			switch group[iv] {
			case "entry":
				if emaFast != 0 {
					trendRef = emaFast
					refName = fmt.Sprintf("EMA%d", emaCfg.Fast)
				}
			case "background":
				if emaSlow != 0 {
					trendRef = emaSlow
					refName = fmt.Sprintf("EMA%d", emaCfg.Slow)
				}
			}
			trend := ""
			if trendRef != 0 {
				if last.Close >= trendRef {
					trend = fmt.Sprintf(" | 趋势: 收盘在%s上方", refName)
				} else {
					trend = fmt.Sprintf(" | 趋势: 收盘在%s下方", refName)
				}
			}
			tag := labelText(iv)
			if tag != "" {
				tag = "[" + tag + "]"
			}
			if !first {
				b.WriteString(" || ")
			}
			b.WriteString(fmt.Sprintf("%s%s 收盘=%.4f | EMA(%d/%d/%d)=%.3f/%.3f/%.3f%s | RSI%d=%.2f(%s, 阈=%.0f/%.0f) | MACD=%.3f",
				iv, tag, last.Close, emaCfg.Fast, emaCfg.Mid, emaCfg.Slow, emaFast, emaMid, emaSlow, trend, rsiCfg.Period, rsi, rsiState, rsiCfg.Oversold, rsiCfg.Overbought, macd))
			first = false
		}
		if first {
			b.WriteString("(暂无数据)")
		}
		// 附加 OI 与资金费率（若启用）
		if e.Metrics != nil {
			if e.IncludeOI {
				if v, err := e.Metrics.OI(ctx, sym); err == nil && v > 0 {
					b.WriteString(fmt.Sprintf(" | OI=%.2f", v))
				}
			}
			if e.IncludeFunding {
				if v, err := e.Metrics.Funding(ctx, sym); err == nil {
					b.WriteString(fmt.Sprintf(" | Funding=%.5f", v))
				}
			}
		}
		b.WriteString("\n")
	}
	if len(input.Positions) > 0 {
		b.WriteString("\n## 当前持仓\n")
		for _, pos := range input.Positions {
			upnl := fmt.Sprintf("未实现PnL=%.2f", pos.UnrealizedPn)
			if pos.UnrealizedPnPct != 0 {
				upnl = fmt.Sprintf("未实现PnL=%.2f (%.2f%%)", pos.UnrealizedPn, pos.UnrealizedPnPct*100)
			}
			line := fmt.Sprintf("- %s %s 入场=%.4f 数量=%.4f RR=%.2f %s",
				pos.Symbol, strings.ToUpper(pos.Side), pos.EntryPrice, pos.Quantity, pos.RR, upnl)
			if pos.TakeProfit > 0 {
				line += fmt.Sprintf(" TP=%.4f", pos.TakeProfit)
			}
			if pos.StopLoss > 0 {
				line += fmt.Sprintf(" SL=%.4f", pos.StopLoss)
			}
			b.WriteString(line + "\n")
		}
		b.WriteString("请结合上述仓位判断是否需要平仓、加仓或调整计划。\n")
	}

	b.WriteString("\n请先输出一段简短的【思维链】（最多3句，说明判断依据与步骤），然后换行仅输出 JSON 数组作为最终结果；数组中每项必须包含 symbol、action，并附带简短的 reasoning 字段。仅当已有对应方向仓位且需要部分减仓/止盈时，才在 JSON 中提供 close_ratio（0-1，表示释放仓位比例）或 position_size_usd；无仓位时不要返回 close_ratio。当 action 为 open_long/open_short 时，务必返回 take_profit 与 stop_loss 字段（使用绝对价格，浮点数）。当 action 为 adjust_stop_loss 时，必须返回新的 stop_loss 价格，否则视为无效。\n")
	b.WriteString("示例:\n思维链: 4h 供需区不明确，15m 未出现有效形态，MACD 未确认，bull_score 与 bear_score分数均不满足。\n[ {\"symbol\":\"BTCUSDT\",\"action\":\"hold\",\"reasoning\":\"bull_score 与 bear_score分数均不满足\"} ]\n")
	return b.String()
}

func cloneOutputs(src []ModelOutput) []ModelOutput {
	if len(src) == 0 {
		return nil
	}
	dst := make([]ModelOutput, len(src))
	copy(dst, src)
	return dst
}
