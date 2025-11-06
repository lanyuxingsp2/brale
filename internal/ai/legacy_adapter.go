package ai

import (
    "context"
    "encoding/json"
    "fmt"
    "strings"
    "time"

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

	PromptMgr      *prompt.Manager // 提示词管理器
	SystemTemplate string          // 使用的系统模板名

	KStore    store.KlineStore // K线存储（用于构建 User 摘要）
	Intervals []string         // 要在摘要中展示的周期

	Name_ string

	Parallel bool // 并行调用多个模型

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
	sys := e.loadSystem()
	usr := e.buildUserSummary(ctx, input.Candidates)

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
            if arr, ok := extractJSONArrayCompat(raw); ok {
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
	best, err := agg.Aggregate(ctx, outs)
	if err != nil {
		return DecisionResult{}, err
	}
	return best.Parsed, nil
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

// buildUserSummary 将候选币种的最后一根 K 线收盘价（按配置周期）汇总到文案中
func (e *LegacyEngineAdapter) buildUserSummary(ctx context.Context, candidates []string) string {
	var b strings.Builder
	b.WriteString("# 候选币种数据摘要\n\n")
	for _, sym := range candidates {
		b.WriteString(sym)
		b.WriteString(": ")
		empty := true
		for i, iv := range e.Intervals {
			if e.KStore == nil {
				continue
			}
			ks, _ := e.KStore.Get(ctx, sym, iv)
			if len(ks) == 0 {
				continue
			}
			last := ks[len(ks)-1]
			if !empty {
				b.WriteString(" | ")
			}
			b.WriteString(iv)
			b.WriteString(" 收盘=")
			b.WriteString(fmt.Sprintf("%.4f@%d", last.Close, last.CloseTime))
			// 附加简要指标（EMA20/MACD/RSI14）
			closes := make([]float64, 0, len(ks))
			for _, k := range ks {
				closes = append(closes, k.Close)
			}
			ema20 := indicators.EMA(closes, 20)
			macd := indicators.MACD(closes)
			rsi14 := indicators.RSI(closes, 14)
            b.WriteString(fmt.Sprintf(" EMA20=%.3f MACD=%.3f RSI14=%.2f", ema20, macd, rsi14))
            // 通俗描述
            trend := "下方"
            if last.Close >= ema20 { trend = "上方" }
            momentum := "中性"
            if macd > 0.0 { momentum = "多头动能" } else if macd < 0.0 { momentum = "空头动能" }
            rsiSig := "中性"
            if rsi14 >= 70 { rsiSig = "超买" } else if rsi14 <= 30 { rsiSig = "超卖" }
            b.WriteString(fmt.Sprintf(" | 趋势: 收盘在EMA20%s | 动能: %s | RSI: %s", trend, momentum, rsiSig))
			empty = false
			// 限制每个币的展示长度，避免过长
			if i >= 2 {
				break
			}
		}
		if empty {
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
    b.WriteString("\n请先输出一段简短的【思维链】（最多3句，说明判断依据与步骤），然后换行仅输出 JSON 数组作为最终结果；数组中每项必须包含 symbol、action，并附带简短的 reasoning 字段。\n")
    b.WriteString("示例:\n思维链: 4h 供需区不明确，15m 未出现有效形态，MACD 未确认。\n[ {\"symbol\":\"BTCUSDT\",\"action\":\"hold\",\"reasoning\":\"未满足三步确认，暂不入场\"} ]\n")
    return b.String()
}

// extractJSONArrayCompat：提取首个 JSON 数组（与 SimpleJSONEngine 的逻辑一致，重复实现避免导出内部符号）
func extractJSONArrayCompat(s string) (string, bool) {
	start := strings.Index(s, "[")
	if start == -1 {
		return "", false
	}
	depth := 0
	for i := start; i < len(s); i++ {
		switch s[i] {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return strings.TrimSpace(s[start : i+1]), true
			}
		}
	}
	return "", false
}
