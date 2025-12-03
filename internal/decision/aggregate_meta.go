package decision

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"time"

	textutil "brale/internal/pkg/text"
)

// MetaAggregator：逐币种多数决（留权重，默认相等）。
// 规则：
// - 每个模型可以返回多个动作，针对同一 symbol+action 仅记一票
// - 对 symbol+action 进行加权投票，权重达到 2/3（默认）阈值才执行；不足则忽略
// - 若 "hold" 票数达到阈值，则整轮观望
// - MetaSummary 会列出各 symbol 各动作的票数，方便查看分歧
type MetaAggregator struct {
	Weights    map[string]float64
	Preference []string // 权重/动作相同时用于决策与原文选择的优先级
}

type metaChoice struct {
	ID       string
	Decision Decision
	Weight   float64
}

func (a MetaAggregator) Name() string { return "meta" }

const holdSymbolKey = "__META_HOLD__"

func (a MetaAggregator) Aggregate(ctx context.Context, outputs []ModelOutput) (ModelOutput, error) {
	votes := map[string]map[string]float64{}        // symbol -> action -> weight
	details := map[string]map[string][]metaChoice{} // symbol -> action -> choices
	seen := map[string]map[string]bool{}            // provider -> sym#act -> seen?
	totalWeight := 0.0                              // 累积参与投票的权重
	prefIndex := buildPreferenceIndex(a.Preference)
	for _, o := range outputs {
		if o.Err != nil || len(o.Parsed.Decisions) == 0 {
			continue
		}
		w := 1.0
		if a.Weights != nil {
			if v, ok := a.Weights[o.ProviderID]; ok && v > 0 {
				w = v
			}
		}
		if seen[o.ProviderID] == nil {
			seen[o.ProviderID] = map[string]bool{}
		}
		hadAction := false
		for _, d := range o.Parsed.Decisions {
			act := NormalizeAction(d.Action)
			if act == "" {
				continue
			}
			sym := strings.ToUpper(strings.TrimSpace(d.Symbol))
			if act == "hold" {
				sym = holdSymbolKey
			} else if sym == "" {
				continue
			}
			key := sym + "#" + act
			if seen[o.ProviderID][key] {
				continue
			}
			seen[o.ProviderID][key] = true
			hadAction = true
			if _, ok := votes[sym]; !ok {
				votes[sym] = map[string]float64{}
			}
			if _, ok := details[sym]; !ok {
				details[sym] = map[string][]metaChoice{}
			}
			votes[sym][act] += w
			details[sym][act] = append(details[sym][act], metaChoice{ID: o.ProviderID, Decision: d, Weight: w})
		}
		if hadAction {
			totalWeight += w
		}
	}
	if len(votes) == 0 || totalWeight == 0 {
		return ModelOutput{}, errors.New("无可用的模型输出")
	}
	threshold := computeThreshold(totalWeight)

	// 若 hold 票达到阈值，则整轮观望
	if hv, ok := votes[holdSymbolKey]; ok {
		if hv["hold"] >= threshold {
			res := DecisionResult{
				Decisions:   []Decision{{Action: "hold"}},
				MetaSummary: buildHoldSummary(votes, details, threshold),
			}
			return buildMetaOutput(outputs, res, map[string]float64{}, prefIndex), nil
		}
	}

	decisions := make([]Decision, 0)
	winners := make(map[string]float64)
	syms := make([]string, 0, len(votes))
	for sym := range votes {
		if sym == holdSymbolKey {
			continue
		}
		syms = append(syms, sym)
	}
	sort.Strings(syms)
	for _, sym := range syms {
		actionNames := make([]string, 0, len(votes[sym]))
		for act := range votes[sym] {
			actionNames = append(actionNames, act)
		}
		sort.Strings(actionNames)
		for _, act := range actionNames {
			weight := votes[sym][act]
			if weight < threshold || act == "hold" {
				continue
			}
			choice := pickDecision(details[sym][act], act, sym, prefIndex)
			decisions = append(decisions, choice)
			for _, c := range details[sym][act] {
				if NormalizeAction(c.Decision.Action) == act {
					winners[c.ID] += c.Weight
				}
			}
		}
	}
	if len(decisions) == 0 {
		// 没有任何动作满足阈值，退回 hold
		res := DecisionResult{
			Decisions:   []Decision{{Action: "hold"}},
			MetaSummary: buildHoldSummary(votes, details, threshold),
		}
		return buildMetaOutput(outputs, res, map[string]float64{}, prefIndex), nil
	}
	summary := buildActionSummary(votes, details, threshold)
	res := DecisionResult{Decisions: decisions, MetaSummary: summary}
	return buildMetaOutput(outputs, res, winners, prefIndex), nil
}

func pickDecision(choices []metaChoice, action, symbol string, pref map[string]int) Decision {
	act := NormalizeAction(action)
	maxWeight := -1.0
	for _, c := range choices {
		if NormalizeAction(c.Decision.Action) != act {
			continue
		}
		if c.Weight > maxWeight {
			maxWeight = c.Weight
		}
	}
	if maxWeight < 0 {
		return Decision{Symbol: symbol, Action: act}
	}
	bestIdx := -1
	bestRank := len(pref) + 1
	bestProvider := ""
	for i, c := range choices {
		if NormalizeAction(c.Decision.Action) != act || c.Weight != maxWeight {
			continue
		}
		rank := len(pref) + 1
		if v, ok := pref[c.ID]; ok {
			rank = v
		}
		if bestIdx == -1 || rank < bestRank || (rank == bestRank && c.ID < bestProvider) {
			bestIdx = i
			bestRank = rank
			bestProvider = c.ID
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

func pickWeightedProvider(weights map[string]float64, pref map[string]int) string {
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
	if len(pref) > 0 {
		bestID := ""
		bestRank := len(pref) + 1
		for id, w := range weights {
			if w <= 0 {
				continue
			}
			if r, ok := pref[id]; ok && r < bestRank {
				bestID = id
				bestRank = r
			}
		}
		if bestID != "" {
			return bestID
		}
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

func computeThreshold(total float64) float64 {
	if total <= 0 {
		return 0
	}
	val := total * 2.0 / 3.0
	return math.Max(val, total*0.5)
}

func buildHoldSummary(votes map[string]map[string]float64, details map[string]map[string][]metaChoice, threshold float64) string {
	holdWeight := 0.0
	if hv, ok := votes[holdSymbolKey]; ok {
		holdWeight = hv["hold"]
	}
	return fmt.Sprintf("Meta聚合：多数模型选择 HOLD（阈值 %.2f，hold 票 %.2f），本轮不执行任何动作。", threshold, holdWeight)
}

func buildActionSummary(votes map[string]map[string]float64, details map[string]map[string][]metaChoice, threshold float64) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("Meta聚合：动作按阈值 %.2f 加权多数决。\n", threshold))
	syms := make([]string, 0, len(votes))
	for sym := range votes {
		if sym == holdSymbolKey {
			continue
		}
		syms = append(syms, sym)
	}
	sort.Strings(syms)
	for _, sym := range syms {
		actNames := make([]string, 0, len(votes[sym]))
		for act := range votes[sym] {
			actNames = append(actNames, act)
		}
		sort.Strings(actNames)
		for _, act := range actNames {
			weight := votes[sym][act]
			b.WriteString(fmt.Sprintf("%s · %s => %.2f\n", sym, act, weight))
			for _, c := range details[sym][act] {
				reason := strings.TrimSpace(c.Decision.Reasoning)
				if reason != "" {
					reason = strings.ReplaceAll(reason, "\r\n", " ")
					reason = strings.ReplaceAll(reason, "\n", " ")
				}
				if reason == "" {
					reason = "-"
				}
				b.WriteString(fmt.Sprintf("  - %s[%.2f]: %s\n", c.ID, c.Weight, textutil.Truncate(reason, 480)))
			}
		}
	}
	return b.String()
}

func buildMetaOutput(outputs []ModelOutput, res DecisionResult, winners map[string]float64, pref map[string]int) ModelOutput {
	best := ModelOutput{ProviderID: "meta", Parsed: res}
	if len(outputs) == 1 && outputs[0].Err == nil {
		best.Raw = outputs[0].Raw
		best.Parsed.RawJSON = outputs[0].Parsed.RawJSON
		return best
	}
	if id := pickWeightedProvider(winners, pref); id != "" {
		if out := findProviderOutput(outputs, id); out != nil && out.Err == nil && out.Raw != "" {
			best.Raw = out.Raw
			best.Parsed.RawJSON = out.Parsed.RawJSON
		}
	}
	return best
}
