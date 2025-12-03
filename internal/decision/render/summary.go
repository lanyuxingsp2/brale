package render

import (
	"strings"
	"text/template"

	"brale/internal/logger"
)

type TemplateLoader interface {
	Get(name string) (string, bool)
}

type Sections struct {
	LastDecisions string
	Positions     string
	Derivatives   string
	Klines        string
	Insights      string
	Guidelines    string
}

const defaultTemplate = `# 决策输入（Multi-Agent 汇总）
{{if .LastDecisions}}{{.LastDecisions}}{{end}}
{{if .Positions}}{{.Positions}}{{end}}
{{if .Derivatives}}{{.Derivatives}}{{end}}
{{if .Klines}}{{.Klines}}{{end}}
{{if .Insights}}{{.Insights}}{{end}}
{{.Guidelines}}`

var defaultSummaryTemplate = template.Must(template.New("user_summary_default").Parse(defaultTemplate))

func RenderSummary(loader TemplateLoader, sections Sections) string {
	tmpl := defaultSummaryTemplate
	if loader != nil {
		if text, ok := loader.Get("user_summary"); ok {
			parsed, err := template.New("user_summary").Parse(text)
			if err != nil {
				logger.Warnf("user summary 模板解析失败: %v", err)
			} else {
				tmpl = parsed
			}
		}
	}
	var b strings.Builder
	if err := tmpl.Execute(&b, sections); err != nil {
		logger.Warnf("user summary 模板渲染失败: %v", err)
		return fallbackSummary(sections)
	}
	return b.String()
}

func fallbackSummary(sections Sections) string {
	var b strings.Builder
	b.WriteString("# 决策输入（Multi-Agent 汇总）\n")
	if s := strings.TrimSpace(sections.LastDecisions); s != "" {
		b.WriteString(s)
	}
	if s := strings.TrimSpace(sections.Positions); s != "" {
		b.WriteString(s)
	}
	if s := strings.TrimSpace(sections.Derivatives); s != "" {
		b.WriteString(s)
	}
	if s := strings.TrimSpace(sections.Klines); s != "" {
		b.WriteString(s)
	}
	if s := strings.TrimSpace(sections.Insights); s != "" {
		b.WriteString(s)
	}
	b.WriteString(sections.Guidelines)
	return b.String()
}
