package jsonutil

import (
	"encoding/json"
	"strings"
)

// Pretty formats JSON string with indentation; returns original on error.
func Pretty(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return raw
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return raw
	}
	buf, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return raw
	}
	return string(buf)
}
