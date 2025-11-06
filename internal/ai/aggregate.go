package ai

import (
    "context"
    "errors"
    "fmt"
    "sort"
    "strings"
)

// 中文说明：
// 多模型聚合器，用于整合多个模型输出。

// ModelOutput 模型执行后的统一表示
type ModelOutput struct {
    ProviderID string
    Raw        string
    Parsed     DecisionResult
    Err        error
}

// Aggregator 聚合接口
type Aggregator interface {
    Aggregate(ctx context.Context, outputs []ModelOutput) (ModelOutput, error)
    Name() string
}

// FirstWinsAggregator 取第一个成功的输出
type FirstWinsAggregator struct{}

func (a FirstWinsAggregator) Name() string { return "first-wins" }

func (a FirstWinsAggregator) Aggregate(ctx context.Context, outputs []ModelOutput) (ModelOutput, error) {
    for _, o := range outputs {
        if o.Err == nil && len(o.Parsed.Decisions) > 0 {
            return o, nil
        }
    }
    return ModelOutput{}, errors.New("无可用的模型输出")
}

// MetaAggregator：逐币种多数决（留权重，默认相等）。
// 规则：
// - 每个模型对同一 symbol 仅投一票（按 close > open > hold/wait 优先级取其“主”动作）
// - 汇总各模型对该 symbol 的票数（支持权重），多数者为最终动作；票数相等则输出 hold
// - 若任意 symbol 存在分歧，则在结果中填充 MetaSummary 便于外层做 Telegram 推送
type MetaAggregator struct{ Weights map[string]float64 }

func (a MetaAggregator) Name() string { return "meta" }

func pri(action string) int {
    switch action {
    case "close_long", "close_short":
        return 1
    case "open_long", "open_short":
        return 2
    case "hold", "wait":
        return 3
    default:
        return 9
    }
}

// canonAction 将动作归一化：把 "wait" 视为 "hold"（二者同义，避免在 meta 聚合中制造分歧）
func canonAction(a string) string {
    a = strings.ToLower(strings.TrimSpace(a))
    if a == "wait" {
        return "hold"
    }
    return a
}

func (a MetaAggregator) Aggregate(ctx context.Context, outputs []ModelOutput) (ModelOutput, error) {
    // symbol -> action -> weight
    votes := map[string]map[string]float64{}
    // symbol -> list of provider choices (for summary)
    type choice struct{ ID, Action, Reason string; Weight float64 }
    details := map[string][]choice{}

    // 收集投票
    for _, o := range outputs {
        if o.Err != nil || len(o.Parsed.Decisions) == 0 {
            continue
        }
        // 每个 provider 每个 symbol 只取一个主动作
        chosen := map[string]Decision{} // symbol -> decision（已归一化动作）
        for _, d := range o.Parsed.Decisions {
            s := strings.TrimSpace(d.Symbol)
            if s == "" { continue }
            d.Action = canonAction(d.Action)
            if prev, ok := chosen[s]; !ok || pri(d.Action) < pri(prev.Action) {
                chosen[s] = d
            }
        }
        if len(chosen) == 0 {
            continue
        }
        w := 1.0
        if a.Weights != nil {
            if v, ok := a.Weights[o.ProviderID]; ok && v > 0 {
                w = v
            }
        }
        for sym, d := range chosen {
            if _, ok := votes[sym]; !ok { votes[sym] = map[string]float64{} }
            votes[sym][d.Action] += w
            details[sym] = append(details[sym], choice{ID: o.ProviderID, Action: d.Action, Reason: d.Reasoning, Weight: w})
        }
    }

    if len(votes) == 0 {
        return ModelOutput{}, errors.New("无可用的模型输出")
    }

    // 逐 symbol 产出最终决策
    decisions := make([]Decision, 0, len(votes))
    anyDisagree := false
    for sym, am := range votes {
        if len(am) == 1 {
            // 一致
            for act := range am {
                decisions = append(decisions, Decision{Symbol: sym, Action: act})
            }
            continue
        }
        anyDisagree = true
        bestA := ""
        bestW := -1.0
        tie := false
        for act, w := range am {
            if w > bestW {
                bestW = w
                bestA = act
                tie = false
            } else if w == bestW {
                tie = true
            }
        }
        if tie {
            decisions = append(decisions, Decision{Symbol: sym, Action: "hold"})
        } else {
            decisions = append(decisions, Decision{Symbol: sym, Action: bestA})
        }
    }

    // 构造 MetaSummary（仅在出现分歧时）
    summary := ""
    if anyDisagree {
        var b strings.Builder
        b.WriteString("Meta聚合：多模型存在分歧，采用加权多数决。\n")
        // 固定顺序输出
        syms := make([]string, 0, len(details))
        for s := range details { syms = append(syms, s) }
        sort.Strings(syms)
        for _, s := range syms {
            // 统计该 symbol 的加权票
            weightByAction := map[string]float64{}
            for _, c := range details[s] { weightByAction[c.Action] += c.Weight }
            // 选出最终动作（与上面一致逻辑）
            finalA := ""
            bestW := -1.0
            tie := false
            for act, w := range weightByAction {
                if w > bestW { bestW = w; finalA = act; tie = false } else if w == bestW { tie = true }
            }
            if tie { finalA = "hold" }
            b.WriteString(fmt.Sprintf("%s → %s (权重%.2f)\n", s, finalA, bestW))
            // 各模型选择与理由
            for _, c := range details[s] {
                reason := strings.TrimSpace(c.Reason)
                if len(reason) > 120 { reason = reason[:120] + "..." }
                b.WriteString(fmt.Sprintf("- %s[%.2f]: %s | %s\n", c.ID, c.Weight, c.Action, reason))
            }
        }
        summary = b.String()
    }

    res := DecisionResult{Decisions: decisions, MetaSummary: summary}
    return ModelOutput{ProviderID: "meta", Parsed: res}, nil
}
