package decision

import "strings"

// buildPreferenceIndex 把优先级列表映射为 provider -> 顺序索引，过滤空值与重复。
func buildPreferenceIndex(pref []string) map[string]int {
	idx := make(map[string]int, len(pref))
	for i, id := range pref {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, exists := idx[id]; exists {
			continue
		}
		idx[id] = i
	}
	return idx
}
