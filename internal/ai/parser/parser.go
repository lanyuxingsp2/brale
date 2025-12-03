package parser

import "strings"

const codeFence = "```"

func ExtractJSON(raw string) (string, bool) {
	out, _, ok := extract(raw)
	return out, ok
}

func ExtractJSONWithOffset(raw string) (string, int, bool) {
	return extract(raw)
}

func extract(raw string) (string, int, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", -1, false
	}
	if arr, offset, ok := extractFromFence(raw); ok {
		return arr, offset, true
	}
	return extractJSONArray(raw)
}

func extractFromFence(raw string) (string, int, bool) {
	start := strings.Index(raw, codeFence)
	if start == -1 {
		return "", -1, false
	}
	rest := raw[start+len(codeFence):]
	end := strings.Index(rest, codeFence)
	if end == -1 {
		return "", -1, false
	}
	block := rest[:end]
	offset := start + len(codeFence)
	block = strings.TrimLeft(block, "\r\n")
	if idx := strings.Index(block, "\n"); idx != -1 {
		first := strings.TrimSpace(block[:idx])
		if first != "" && !strings.ContainsAny(first, "[{") {
			block = block[idx+1:]
			offset += idx + 1
		}
	}
	block = strings.TrimSpace(block)
	if block == "" {
		return "", -1, false
	}
	arr, rel, ok := extractJSONArray(block)
	if ok {
		return arr, offset + rel, true
	}
	return block, offset, true
}

func extractJSONArray(raw string) (string, int, bool) {
	start := strings.Index(raw, "[")
	if start == -1 {
		return "", -1, false
	}
	depth := 0
	for i := start; i < len(raw); i++ {
		switch raw[i] {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return strings.TrimSpace(raw[start : i+1]), start, true
			}
		}
	}
	return "", -1, false
}
