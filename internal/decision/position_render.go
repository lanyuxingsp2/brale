package decision

import (
	"fmt"
	"strings"

	formatutil "brale/internal/pkg/format"
	"brale/internal/types"
)

func (b *DefaultPromptBuilder) renderAccountOverview(account types.AccountSnapshot) string {
	var sb strings.Builder
	if account.Total <= 0 && account.Available <= 0 && account.Used <= 0 {
		return ""
	}
	currency := strings.ToUpper(strings.TrimSpace(account.Currency))
	if currency == "" {
		currency = "USDT"
	}
	sb.WriteString("\n## 账户资金\n")
	line := fmt.Sprintf("- 权益: %.2f %s", account.Total, currency)
	if account.Available > 0 {
		line += fmt.Sprintf(" · 可用: %.2f", account.Available)
	}
	if account.Used > 0 {
		line += fmt.Sprintf(" · 已使用: %.2f", account.Used)
	}
	sb.WriteString(line + "\n")
	return sb.String()
}

func (b *DefaultPromptBuilder) renderPositionDetails(positions []PositionSnapshot) string {
	if len(positions) == 0 {
		return "\n## 当前持仓\n当前无持仓，只可返回 hold open_long open_short指令。\n"
	}
	var sb strings.Builder
	sb.WriteString("\n## 当前持仓\n")
	for _, pos := range positions {
		line := fmt.Sprintf("- %s %s entry=%.4f",
			strings.ToUpper(pos.Symbol), strings.ToUpper(pos.Side), pos.EntryPrice)
		if pos.Stake > 0 {
			line += fmt.Sprintf(" stake=%.2f", pos.Stake)
		}
		if pos.Leverage > 0 {
			line += fmt.Sprintf(" lev=x%.2f", pos.Leverage)
		}
		if pos.CurrentPrice > 0 {
			line += fmt.Sprintf(" last=%.4f", pos.CurrentPrice)
		}
		if pos.HoldingMs > 0 {
			line += fmt.Sprintf(" holding=%s", formatutil.Duration(pos.HoldingMs))
		}
		sb.WriteString(line + "\n")
		if len(pos.PlanSummaries) > 0 {
			for _, note := range pos.PlanSummaries {
				msg := strings.TrimSpace(note)
				if msg == "" {
					continue
				}
				sb.WriteString("    → " + msg + "\n")
			}
		}
		if jsonText := strings.TrimSpace(pos.PlanStateJSON); jsonText != "" {
			if planText := renderPlanStateSummary(jsonText); planText != "" {
				sb.WriteString(planText)
				sb.WriteString("    提示：只可修改 未触达 阶段的字段，已触达 的阶段请原值返回。\n")
			} else {
				sb.WriteString("    ⚠️ 无法解析策略结构，以下为原始 JSON：\n")
				sb.WriteString("    exit_plan_state_json:\n")
				lines := strings.Split(jsonText, "\n")
				for _, line := range lines {
					sb.WriteString("        " + line + "\n")
				}
			}
		}
	}
	sb.WriteString("请结合上述仓位判断是否需要平仓、加仓或调整计划。\n")
	return sb.String()
}
