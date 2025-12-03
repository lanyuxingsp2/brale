package decision

import (
	"fmt"
	"strings"
)

// ThoughtRow 用于汇总多个模型的思维链。
type ThoughtRow struct {
	Provider string
	Thought  string
	Failed   bool
}

// ResultRow 用于汇总多个模型的决策结果。
type ResultRow struct {
	Provider string
	Action   string
	Symbol   string
	Reason   string
	Failed   bool
}

// RenderThoughtsTable 以纯文本形式输出模型思维链（逐条换行）。
func RenderThoughtsTable(rows []ThoughtRow, _ int) string {
	if len(rows) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("=== AI 思维链 ===\n")
	for _, row := range rows {
		provider := fallback(row.Provider, "-")
		status := ""
		if row.Failed {
			status = " [FAILED]"
		}
		sb.WriteString(fmt.Sprintf("- [%s]%s\n", provider, status))
		content := strings.TrimSpace(row.Thought)
		if content == "" {
			content = "(empty)"
		}
		sb.WriteString(indentLines(content, "  "))
		sb.WriteRune('\n')
	}
	return strings.TrimRight(sb.String(), "\n")
}

// RenderResultsTable 以纯文本形式输出模型决策结果。
func RenderResultsTable(rows []ResultRow, _ int) string {
	if len(rows) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("=== AI 决策结果 ===\n")
	for _, row := range rows {
		provider := fallback(row.Provider, "-")
		status := ""
		if row.Failed {
			status = " [FAILED]"
		}
		sb.WriteString(fmt.Sprintf("- [%s]%s action=%s symbol=%s\n", provider, status, strings.TrimSpace(row.Action), strings.TrimSpace(row.Symbol)))
		reason := strings.TrimSpace(row.Reason)
		if reason == "" {
			reason = "(no reasoning)"
		}
		sb.WriteString(indentLines(reason, "  "))
		sb.WriteRune('\n')
	}
	return strings.TrimRight(sb.String(), "\n")
}

// RenderBlockTable 输出简单的标题+正文块。
func RenderBlockTable(title, content string) string {
	title = fallback(title, "Note")
	body := strings.TrimSpace(content)
	if body == "" {
		body = "(empty)"
	}
	var sb strings.Builder
	sb.WriteString(title)
	sb.WriteRune('\n')
	sb.WriteString(indentLines(body, "  "))
	return strings.TrimRight(sb.String(), "\n")
}

// RenderFinalDecisionsTable 输出最终聚合结果。
func RenderFinalDecisionsTable(ds []Decision, _ int) string {
	var sb strings.Builder
	sb.WriteString("=== Final Decisions ===\n")
	if len(ds) == 0 {
		sb.WriteString("(none)")
		return sb.String()
	}
	for _, d := range ds {
		sl := "-"
		tp := "-"
		if d.StopLoss > 0 {
			sl = fmt.Sprintf("%.4f", d.StopLoss)
		}
		if d.TakeProfit > 0 {
			tp = fmt.Sprintf("%.4f", d.TakeProfit)
		}
		sb.WriteString(fmt.Sprintf("- action=%s symbol=%s sl=%s tp=%s\n", strings.TrimSpace(d.Action), strings.TrimSpace(d.Symbol), sl, tp))
		if tiers := formatTiers(d.Tiers); tiers != "" {
			sb.WriteString(indentLines(tiers, "    "))
			sb.WriteRune('\n')
		}
		reason := strings.TrimSpace(d.Reasoning)
		if reason != "" {
			sb.WriteString(indentLines(reason, "    "))
			sb.WriteRune('\n')
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

func formatTiers(t *DecisionTiers) string {
	if t == nil {
		return ""
	}
	var parts []string
	if t.Tier1Target > 0 {
		parts = append(parts, fmt.Sprintf("tier1=%.4f (%.2f%%)", t.Tier1Target, t.Tier1Ratio*100))
	}
	if t.Tier2Target > 0 {
		parts = append(parts, fmt.Sprintf("tier2=%.4f (%.2f%%)", t.Tier2Target, t.Tier2Ratio*100))
	}
	if t.Tier3Target > 0 {
		parts = append(parts, fmt.Sprintf("tier3=%.4f (%.2f%%)", t.Tier3Target, t.Tier3Ratio*100))
	}
	return strings.Join(parts, "; ")
}

func indentLines(text, prefix string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = prefix + line
	}
	return strings.Join(lines, "\n")
}

func fallback(val, def string) string {
	val = strings.TrimSpace(val)
	if val == "" {
		return def
	}
	return val
}
