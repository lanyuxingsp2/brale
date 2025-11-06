package ai

// 中文说明：
// 决策排序与去重：
// - 优先级：close_* > open_* > hold/wait
// - 防重复：同一币种同方向的 open_* 仅保留第一条

func OrderAndDedup(ds []Decision) []Decision {
	if len(ds) <= 1 {
		return ds
	}
	// 简单稳定选择排序实现优先级排序
	pri := func(a string) int {
		switch a {
		case "close_long", "close_short":
			return 1
		case "open_long", "open_short":
			return 2
		case "hold", "wait":
			return 3
		default:
			return 9
		}
	}
	out := make([]Decision, len(ds))
	copy(out, ds)
	for i := 0; i < len(out)-1; i++ {
		min := i
		for j := i + 1; j < len(out); j++ {
			if pri(out[j].Action) < pri(out[min].Action) {
				min = j
			}
		}
		if min != i {
			out[i], out[min] = out[min], out[i]
		}
	}
	// 去重 open_*
	seen := map[string]bool{}
	filtered := make([]Decision, 0, len(out))
	for _, d := range out {
		if d.Action == "open_long" || d.Action == "open_short" {
			key := d.Symbol + "#" + d.Action
			if seen[key] {
				continue
			}
			seen[key] = true
		}
		filtered = append(filtered, d)
	}
	return filtered
}
