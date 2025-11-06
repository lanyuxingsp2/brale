package ai

import (
    "bytes"
    "encoding/json"
    "strings"
)

// PrettyJSON 尝试对 JSON 文本进行缩进美化；失败则返回原文
func PrettyJSON(raw string) string {
    raw = strings.TrimSpace(raw)
    if raw == "" { return raw }
    var v any
    if err := json.Unmarshal([]byte(raw), &v); err != nil {
        return raw
    }
    b, err := json.MarshalIndent(v, "", "  ")
    if err != nil { return raw }
    return string(b)
}

// TrimTo 限制字符串长度，超长则追加省略号
func TrimTo(s string, max int) string {
    if max <= 0 { return s }
    if len(s) <= max { return s }
    var buf bytes.Buffer
    buf.WriteString(s[:max])
    buf.WriteString("...")
    return buf.String()
}

// 删除本地表格格式化，改用 go-pretty 渲染
