package web

import "embed"

// Templates 包含后台页面所需的 HTML 模板。
//
//go:embed templates/*.html
var Templates embed.FS

// Static 包含静态资源（CSS/JS）。
//
//go:embed static
var Static embed.FS
