package ai

import (
    "strings"

    "github.com/jedib0t/go-pretty/v6/table"
    "github.com/jedib0t/go-pretty/v6/text"
)

// ThoughtRow 用于汇总多个模型的思维链
type ThoughtRow struct {
    Provider string
    Thought  string
    Failed   bool
}

// ResultRow 用于汇总多个模型的决策结果
type ResultRow struct {
    Provider string
    Action   string
    Symbol   string
    Reason   string
    Failed   bool
}

func colorAI(name string) string {
    return text.Colors{text.FgHiCyan, text.Bold}.Sprint(name)
}

func colorAction(a string) string {
    n := NormalizeAction(a)
    // 兼容 buy/sell 仅用于显示着色
    if strings.EqualFold(a, "buy") { n = "open_long" }
    if strings.EqualFold(a, "sell") { n = "open_short" }
    switch n {
    case "open_long":
        return text.Colors{text.FgHiGreen, text.Bold}.Sprint(a)
    case "open_short":
        return text.Colors{text.FgHiRed, text.Bold}.Sprint(a)
    case "close_long", "close_short":
        return text.Colors{text.FgHiYellow}.Sprint(a)
    case "hold":
        return text.Colors{text.FgHiBlue}.Sprint(a)
    default:
        return text.Colors{text.FgHiWhite}.Sprint(a)
    }
}

// RenderThoughtsTable 将多个模型的思维链汇总为一张表
func RenderThoughtsTable(rows []ThoughtRow, width int) string {
    tw := table.NewWriter()
    tw.SetStyle(table.StyleRounded)
    tw.Style().Options.DrawBorder = true
    tw.Style().Options.SeparateHeader = true
    tw.Style().Options.SeparateRows = true
    tw.Style().Color.Header = text.Colors{text.FgHiCyan, text.Bold}
    // 限制整行最大宽度，避免在某些终端被强制换行为两行边框
    if width > 0 {
        tw.SetAllowedRowLength(width)
    }

    tw.AppendHeader(table.Row{"AI", "思维链"})
    for _, r := range rows {
        aiCell := colorAI(r.Provider)
        content := r.Thought
        if r.Failed {
            content = text.Colors{text.FgHiRed}.Sprint("失败: " + strings.TrimSpace(content))
        }
        // 直接按字符数截断，然后再按列宽强制换行
        wrapW := width - 26
        if wrapW < 60 { wrapW = 60 }
        // 经验：限制为约 8 行的字符量，先截断再换行
        maxChars := wrapW * 8
        content = TrimTo(content, maxChars)
        content = forceWrap(content, wrapW)
        tw.AppendRow(table.Row{aiCell, content})
    }
    // 列宽配置：AI 列较窄，思维链列尽量展示更多并换行
    if width <= 0 { width = 180 }
    // 为了不超过整行宽度，将第二列比总宽度略小（扣除首列+边框间隙）
    col2 := width - 26
    if col2 < 60 { col2 = 60 }
    tw.SetColumnConfigs([]table.ColumnConfig{
        {Number: 1, WidthMax: 22, Align: text.AlignLeft, VAlign: text.VAlignTop},
        {Number: 2, WidthMax: col2, Align: text.AlignLeft, VAlign: text.VAlignTop},
    })
    return tw.Render()
}

// RenderResultsTable 将多个模型的结果汇总为一张表
func RenderResultsTable(rows []ResultRow, width int) string {
    tw := table.NewWriter()
    tw.SetStyle(table.StyleRounded)
    tw.Style().Options.DrawBorder = true
    tw.Style().Options.SeparateHeader = true
    tw.Style().Options.SeparateRows = true
    tw.Style().Color.Header = text.Colors{text.FgHiCyan, text.Bold}

    tw.AppendHeader(table.Row{"AI", "action", "symbol", "reasoning"})
    for _, r := range rows {
        aiCell := colorAI(r.Provider)
        actionCell := r.Action
        reasonCell := r.Reason
        if r.Failed {
            actionCell = text.Colors{text.FgHiRed}.Sprint("失败")
            reasonCell = text.Colors{text.FgHiRed}.Sprint(strings.TrimSpace(reasonCell))
        } else {
            actionCell = colorAction(actionCell)
        }
        // 强制换行：对 reasoning 列
        reasonCell = forceWrap(reasonCell, width)
        tw.AppendRow(table.Row{aiCell, actionCell, r.Symbol, reasonCell})
    }
    if width <= 0 { width = 180 }
    tw.SetColumnConfigs([]table.ColumnConfig{
        {Number: 1, WidthMax: 22, Align: text.AlignLeft, VAlign: text.VAlignTop},
        {Number: 2, WidthMax: 14, Align: text.AlignLeft, VAlign: text.VAlignTop},
        {Number: 3, WidthMax: 16, Align: text.AlignLeft, VAlign: text.VAlignTop},
        {Number: 4, WidthMax: width, Align: text.AlignLeft, VAlign: text.VAlignTop},
    })
    return tw.Render()
}

// RenderBlockTable 仍保留：在需要单列面板时使用
func RenderBlockTable(title, content string) string {
    tw := table.NewWriter()
    tw.SetStyle(table.StyleRounded)
    tw.Style().Options.DrawBorder = true
    tw.Style().Options.SeparateHeader = true
    tw.AppendHeader(table.Row{title})
    // 强制换行：与单列面板宽度一致
    content = forceWrap(content, 180)
    tw.AppendRow(table.Row{content})
    tw.SetColumnConfigs([]table.ColumnConfig{{Number: 1, Align: text.AlignLeft, VAlign: text.VAlignTop, WidthMax: 180}})
    return tw.Render()
}

// RenderFinalDecisionsTable 渲染最终聚合后的决策表，并将行标红
func RenderFinalDecisionsTable(ds []Decision, width int) string {
    tw := table.NewWriter()
    tw.SetStyle(table.StyleRounded)
    tw.Style().Options.DrawBorder = true
    tw.Style().Options.SeparateHeader = true
    tw.Style().Options.SeparateRows = true
    tw.Style().Color.Header = text.Colors{text.FgHiCyan, text.Bold}

    tw.AppendHeader(table.Row{"action", "symbol", "reasoning"})
    for _, d := range ds {
        act := text.Colors{text.FgHiRed, text.Bold}.Sprint(d.Action)
        reason := d.Reasoning
        // 尽量不裁剪，但仍设置较大上限
        reason = TrimTo(reason, 7200)
        // 强制换行
        reason = forceWrap(reason, width)
        tw.AppendRow(table.Row{act, d.Symbol, reason})
    }
    if width <= 0 { width = 180 }
    tw.SetColumnConfigs([]table.ColumnConfig{
        {Number: 1, WidthMax: 16, Align: text.AlignLeft, VAlign: text.VAlignTop},
        {Number: 2, WidthMax: 18, Align: text.AlignLeft, VAlign: text.VAlignTop},
        {Number: 3, WidthMax: width, Align: text.AlignLeft, VAlign: text.VAlignTop},
    })
    // 行整体标红（强调最终决策）
    tw.SetRowPainter(func(row table.Row) text.Colors { return text.Colors{text.FgHiRed} })
    return tw.Render()
}

// forceWrap 对字符串按给定宽度强制换行（简单基于 rune 计数）
func forceWrap(s string, width int) string {
    if width <= 0 || s == "" {
        return s
    }
    var b strings.Builder
    col := 0
    for _, r := range s {
        if r == '\n' {
            b.WriteRune('\n')
            col = 0
            continue
        }
        if col >= width {
            b.WriteRune('\n')
            col = 0
        }
        b.WriteRune(r)
        col++
    }
    return b.String()
}

// 备注：如需改为固定行数截断，可改回按行处理。
