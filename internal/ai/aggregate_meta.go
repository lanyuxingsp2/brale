package ai

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"
)

// MetaAggregator：逐币种多数决（留权重，默认相等）。
// 规则：
// - 每个模型对同一 symbol 仅投一票（按 close > open > hold 优先级取其“主”动作）
// - 汇总各模型对该 symbol 的票数（支持权重），多数者为最终动作；票数相等则输出 hold
// - 若任意 symbol 存在分歧，则在结果中填充 MetaSummary 便于外层做 Telegram 推送
type MetaAggregator struct{ Weights map[string]float64 }

type metaChoice struct {
	ID       string
	Decision Decision
	Weight   float64
}

func (a MetaAggregator) Name() string { return "meta" }

func pri(action string) int {
	switch action {
	case "close_long", "close_short":
		return 1
	case "open_long", "open_short":
		return 2
	case "hold":
		return 3
	default:
		return 9
	}
}

func (a MetaAggregator) Aggregate(ctx context.Context, outputs []ModelOutput) (ModelOutput, error) {
	// symbol -> action -> weight
	votes := map[string]map[string]float64{}
	// symbol -> list of provider choices (带上原始决策，后续用于复用 reasoning/TP/SL 等字段)
	details := map[string][]metaChoice{}

	// 收集投票
	for _, o := range outputs {
		if o.Err != nil || len(o.Parsed.Decisions) == 0 {
			continue
		}
		// 每个 provider 每个 symbol 只取一个主动作
		chosen := map[string]Decision{} // symbol -> decision（已归一化动作）
		for _, d := range o.Parsed.Decisions {
			s := strings.TrimSpace(d.Symbol)
			if s == "" {
				continue
			}
			d.Action = NormalizeAction(d.Action)
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
			if _, ok := votes[sym]; !ok {
				votes[sym] = map[string]float64{}
			}
			votes[sym][d.Action] += w
			details[sym] = append(details[sym], metaChoice{ID: o.ProviderID, Decision: d, Weight: w})
		}
	}

	if len(votes) == 0 {
		return ModelOutput{}, errors.New("无可用的模型输出")
	}

	// 逐 symbol 产出最终决策
	decisions := make([]Decision, 0, len(votes))
	anyDisagree := false
	winners := make(map[string]float64) // provider -> 累积权重（与最终动作一致）
	for sym, am := range votes {
		if len(am) == 1 {
			// 一致
			for act := range am {
				decisions = append(decisions, pickDecision(details[sym], act, sym))
				for _, c := range details[sym] {
					if c.Decision.Action == act {
						winners[c.ID] += c.Weight
					}
				}
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
		finalA := bestA
		if tie {
			finalA = "hold"
		}
		decisions = append(decisions, pickDecision(details[sym], finalA, sym))
		if finalA != "" {
			for _, c := range details[sym] {
				if c.Decision.Action == finalA {
					winners[c.ID] += c.Weight
				}
			}
		}
	}

	// 构造 MetaSummary（仅在出现分歧时）
	summary := ""
	if anyDisagree {
		var b strings.Builder
		b.WriteString("Meta聚合：多模型存在分歧，采用加权多数决。\n")
		// 固定顺序输出
		syms := make([]string, 0, len(details))
		for s := range details {
			syms = append(syms, s)
		}
		sort.Strings(syms)
		for _, s := range syms {
			// 统计该 symbol 的加权票
			weightByAction := map[string]float64{}
			for _, c := range details[s] {
				weightByAction[c.Decision.Action] += c.Weight
			}
			// 选出最终动作（与上面一致逻辑）
			finalA := ""
			bestW := -1.0
			tie := false
			for act, w := range weightByAction {
				if w > bestW {
					bestW = w
					finalA = act
					tie = false
				} else if w == bestW {
					tie = true
				}
			}
			if tie {
				finalA = "hold"
			}
			b.WriteString(fmt.Sprintf("%s → %s (权重%.2f)\n", s, finalA, bestW))
			// 各模型选择与理由（不再截断理由；换行缩进展示）
			for _, c := range details[s] {
				act := c.Decision.Action
				b.WriteString(fmt.Sprintf("- %s[%.2f]: %s\n", c.ID, c.Weight, act))
				reason := strings.TrimSpace(c.Decision.Reasoning)
				if reason != "" {
					// 规范换行并缩进
					reason = strings.ReplaceAll(reason, "\r\n", "\n")
					reason = strings.ReplaceAll(reason, "\r", "\n")
					reason = strings.ReplaceAll(reason, "```", "'''")
					// 每行添加前缀缩进
					lines := strings.Split(reason, "\n")
					for i, ln := range lines {
						if i == 0 {
							b.WriteString("  reason: " + ln + "\n")
						} else {
							b.WriteString("           " + ln + "\n")
						}
					}
				}
			}
		}
		summary = b.String()
	}

	res := DecisionResult{Decisions: decisions, MetaSummary: summary}
	best := ModelOutput{ProviderID: "meta", Parsed: res}
	// 如果只有一个 provider，沿用它的原始输出
	if len(outputs) == 1 && outputs[0].Err == nil {
		best.Raw = outputs[0].Raw
		best.Parsed.RawJSON = outputs[0].Parsed.RawJSON
		return best, nil
	}
	if id := pickWeightedProvider(winners); id != "" {
		if out := findProviderOutput(outputs, id); out != nil && out.Err == nil && out.Raw != "" {
			best.Raw = out.Raw
			best.Parsed.RawJSON = out.Parsed.RawJSON
		}
	}
	return best, nil
}

func pickDecision(choices []metaChoice, action, symbol string) Decision {
	act := NormalizeAction(action)
	bestIdx := -1
	bestWeight := -1.0
	for i, c := range choices {
		if NormalizeAction(c.Decision.Action) != act {
			continue
		}
		if c.Weight > bestWeight {
			bestWeight = c.Weight
			bestIdx = i
		}
	}
	if bestIdx == -1 {
		return Decision{Symbol: symbol, Action: act}
	}
	dup := choices[bestIdx].Decision
	dup.Symbol = symbol
	dup.Action = act
	return dup
}

func pickWeightedProvider(weights map[string]float64) string {
	total := 0.0
	ids := make([]string, 0, len(weights))
	for id, w := range weights {
		if w <= 0 {
			continue
		}
		total += w
		ids = append(ids, id)
	}
	if total == 0 || len(ids) == 0 {
		return ""
	}
	sort.Strings(ids)
	rnd := rand.New(rand.NewSource(time.Now().UnixNano()))
	target := rnd.Float64() * total
	acc := 0.0
	for _, id := range ids {
		w := weights[id]
		if w <= 0 {
			continue
		}
		acc += w
		if target < acc {
			return id
		}
	}
	return ids[len(ids)-1]
}

func findProviderOutput(outputs []ModelOutput, id string) *ModelOutput {
	for i := range outputs {
		if outputs[i].ProviderID == id {
			return &outputs[i]
		}
	}
	return nil
}
