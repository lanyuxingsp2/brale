package prompt

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"brale/internal/decision"
	"brale/internal/exitplan"
	"brale/internal/logger"
	"brale/internal/profile"
	promptkit "brale/internal/prompt"
)

// StandardStrategy implements the default prompting logic.
type StandardStrategy struct {
	exitPlans       *exitplan.Registry
	exitPlanPrompts map[string]promptkit.ExitPlanPrompt
}

// NewStandardStrategy creates a new StandardStrategy.
func NewStandardStrategy(plans *exitplan.Registry, prompts map[string]promptkit.ExitPlanPrompt) *StandardStrategy {
	return &StandardStrategy{
		exitPlans:       plans,
		exitPlanPrompts: prompts,
	}
}

func (s *StandardStrategy) Build(
	ctx *decision.Context,
	activeProfiles map[string]*profile.Runtime,
	featureLines map[string][]string,
	allProfiles []*profile.Runtime,
) error {
	if ctx == nil {
		return nil
	}
	// 1. Build Prompt Bundle
	ctx.Prompt = s.buildProfilePromptBundle(activeProfiles, featureLines)

	// 2. Build Global Exit Plan Directive
	ctx.ExitPlanDirective = s.renderExitPlanDirective(allProfiles)

	// 3. Build Per-Profile Directives
	ctx.ProfilePrompts = s.buildProfilePrompts(ctx.Candidates, activeProfiles)
	return nil
}

type profilePromptData struct {
	Profile            string
	ContextTag         string
	Targets            []string
	MiddlewareFeatures string
	Features           string
	ExitPlanSchema     string
}

func (s *StandardStrategy) buildProfilePromptBundle(active map[string]*profile.Runtime, featureLines map[string][]string) decision.PromptBundle {
	var bundle decision.PromptBundle
	if len(active) == 0 {
		return bundle
	}
	userBlocks := make([]string, 0, len(active))

	keys := make([]string, 0, len(active))
	for k := range active {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, name := range keys {
		rt := active[name]
		if rt.UserTemplate != nil {
			data := profilePromptData{
				Profile:            rt.Definition.Name,
				ContextTag:         rt.Definition.ContextTag,
				Targets:            append([]string(nil), rt.Definition.Targets...),
				MiddlewareFeatures: strings.Join(featureLines[name], "\n"),
			}
			data.Features = data.MiddlewareFeatures
			dir := s.resolveProfileExitDirective(rt)
			data.ExitPlanSchema = dir
			var buf bytes.Buffer
			if err := rt.UserTemplate.Execute(&buf, data); err != nil {
				logger.Warnf("PromptStrategy: profile prompt rendering failed profile=%s err=%v", rt.Definition.Name, err)
				continue
			}
			block := strings.TrimSpace(buf.String())
			if block != "" {
				userBlocks = append(userBlocks, block)
			}
		}
	}
	if len(userBlocks) > 0 {
		bundle.User = strings.Join(userBlocks, "\n\n")
	}
	return bundle
}

func (s *StandardStrategy) resolveProfileExitDirective(rt *profile.Runtime) string {
	text, example := s.buildProfileExitDirective(rt, "")
	text = strings.TrimSpace(text)
	example = strings.TrimSpace(example)
	if text == "" && example == "" {
		return ""
	}
	var builder strings.Builder
	if text != "" {
		builder.WriteString(text)
	}
	// example intentionally ignored here for prompt body, usually appended or handled separately
	return strings.TrimSpace(builder.String())
}

func (s *StandardStrategy) buildProfilePrompts(candidates []string, activeProfiles map[string]*profile.Runtime) map[string]decision.ProfilePromptSpec {
	if len(candidates) == 0 {
		return nil
	}
	prompts := make(map[string]decision.ProfilePromptSpec, len(candidates))
	seen := make(map[string]struct{}, len(candidates))
	for _, sym := range candidates {
		key := strings.ToUpper(strings.TrimSpace(sym))
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}

		rt, ok := activeProfiles[rtNameForSymbol(sym, activeProfiles)]
		if !ok {
			found := false
			for _, r := range activeProfiles {
				if containsString(r.Definition.ExitPlans.Allowed, sym) {
					if containsString(r.Definition.Targets, sym) || containsString(r.Definition.TargetsUpper(), strings.ToUpper(sym)) {
						rt = r
						found = true
						break
					}
				}
			}
			if !found {
				continue
			}
		}

		promptText := rt.UserPrompt
		promptRef := strings.TrimSpace(rt.Definition.Prompts.User)
		sysPrompts := decision.CloneStringMap(rt.SystemPromptsByModel)
		exitText, example := s.buildProfileExitDirective(rt, sym)
		// 只有当所有字段均为空时才跳过
		if len(sysPrompts) == 0 && strings.TrimSpace(promptText) == "" && strings.TrimSpace(exitText) == "" && strings.TrimSpace(example) == "" {
			continue
		}
		prompts[sym] = decision.ProfilePromptSpec{
			Profile:              rt.Definition.Name,
			ContextTag:           rt.Definition.ContextTag,
			PromptRef:            promptRef,
			SystemPromptsByModel: sysPrompts,
			UserPrompt:           promptText,
			ExitConstraints:      exitText,
			Example:              example,
		}
	}
	return prompts
}

func (s *StandardStrategy) renderExitPlanDirective(runtimes []*profile.Runtime) string {
	if len(runtimes) == 0 {
		return ""
	}
	comboKeys := uniqueComboKeys(runtimes)
	if prompts := s.lookupComboPrompts(comboKeys); len(prompts) > 0 {
		return formatExitPlanConstraints(prompts)
	}
	if s.exitPlans == nil {
		return ""
	}
	planSets := make([][]string, 0, len(runtimes))
	for _, rt := range runtimes {
		if len(rt.Definition.ExitPlans.Allowed) == 0 {
			continue
		}
		copyAllowed := append([]string(nil), rt.Definition.ExitPlans.Allowed...)
		planSets = append(planSets, copyAllowed)
	}
	allowed := exitplan.MergeAllowedSchemas(planSets)
	if len(allowed) == 0 {
		return ""
	}
	return s.exitPlans.AllowedSchema(allowed)
}

func (s *StandardStrategy) buildProfileExitDirective(rt *profile.Runtime, symbol string) (string, string) {
	if rt == nil {
		return "", ""
	}
	prompts := s.lookupComboPrompts(rt.Definition.ExitPlans.ComboKeys())
	if len(prompts) > 0 {
		text := formatExitPlanConstraints(prompts)
		sampleSymbol := strings.ToUpper(strings.TrimSpace(symbol))
		if sampleSymbol == "" && len(rt.Definition.Targets) > 0 {
			sampleSymbol = strings.ToUpper(strings.TrimSpace(rt.Definition.Targets[0]))
		}
		example := buildDecisionExampleJSON(sampleSymbol, prompts[0].JSONExample)
		return text, example
	}
	if s.exitPlans == nil {
		return "", ""
	}
	return s.exitPlans.AllowedSchema(rt.Definition.ExitPlans.Allowed), ""
}

func (s *StandardStrategy) lookupComboPrompts(keys []string) []promptkit.ExitPlanPrompt {
	if len(keys) == 0 || len(s.exitPlanPrompts) == 0 {
		return nil
	}
	seen := make(map[string]struct{})
	result := make([]promptkit.ExitPlanPrompt, 0, len(keys))
	for _, raw := range keys {
		norm := promptkit.NormalizeComboKey(raw)
		if norm == "" {
			continue
		}
		if _, ok := seen[norm]; ok {
			continue
		}
		seen[norm] = struct{}{}
		if prompt, ok := s.exitPlanPrompts[norm]; ok {
			result = append(result, prompt)
		}
	}
	return result
}

// Helpers

func rtNameForSymbol(sym string, active map[string]*profile.Runtime) string {
	for name, rt := range active {
		for _, t := range rt.Definition.Targets {
			if strings.EqualFold(t, sym) {
				return name
			}
		}
	}
	return ""
}

func containsString(list []string, val string) bool {
	for _, v := range list {
		if strings.EqualFold(v, val) {
			return true
		}
	}
	return false
}

func uniqueComboKeys(runtimes []*profile.Runtime) []string {
	if len(runtimes) == 0 {
		return nil
	}
	seen := make(map[string]struct{})
	var keys []string
	for _, rt := range runtimes {
		if rt == nil {
			continue
		}
		for _, raw := range rt.Definition.ExitPlans.ComboKeys() {
			key := promptkit.NormalizeComboKey(raw)
			if key == "" {
				continue
			}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

func formatExitPlanConstraints(prompts []promptkit.ExitPlanPrompt) string {
	if len(prompts) == 0 {
		return ""
	}
	var builder strings.Builder
	builder.WriteString("### Exit Plan Constraints\n")
	for idx, prompt := range prompts {
		builder.WriteString(fmt.Sprintf("%d. %s (%s)\n", idx+1, prompt.Title, prompt.Key))
		if desc := strings.TrimSpace(prompt.Description); desc != "" {
			builder.WriteString("   - " + desc + "\n")
		}
		for _, line := range prompt.Constraints {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			builder.WriteString("   - " + line + "\n")
		}
	}
	return strings.TrimSpace(builder.String())
}

func buildDecisionExampleJSON(symbol, planJSON string) string {
	planJSON = strings.TrimSpace(planJSON)
	if planJSON == "" {
		return ""
	}
	if symbol == "" {
		symbol = "SYMBOL/USDT"
	}
	var plan map[string]any
	if err := json.Unmarshal([]byte(planJSON), &plan); err != nil {
		return ""
	}

	type exampleStruct struct {
		Symbol          string  `json:"symbol"`
		Action          string  `json:"action"`
		Reasoning       string  `json:"reasoning"`
		PositionSizeUSD float64 `json:"position_size_usd"`
		Leverage        int     `json:"leverage"`
		ExitPlan        any     `json:"exit_plan"`
	}

	example := exampleStruct{
		Symbol:          strings.ToUpper(symbol),
		Action:          "open_long",
		Reasoning:       "此处填入对当前 action 的判断，100 字以内。",
		PositionSizeUSD: 1000,
		Leverage:        3,
		ExitPlan:        plan,
	}

	payload := []exampleStruct{example}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return ""
	}
	return string(data)
}
