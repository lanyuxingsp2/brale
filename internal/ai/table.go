package ai

import (
    "github.com/jedib0t/go-pretty/v6/table"
)

// RenderBlockTable 渲染单列表格，标题作为表头，内容放入一行
func RenderBlockTable(title, content string) string {
    t := table.NewWriter()
    // 使用简洁风格，便于日志展示
    t.SetStyle(table.StyleLight)
    t.AppendHeader(table.Row{title})
    t.AppendRow(table.Row{content})
    return t.Render()
}

