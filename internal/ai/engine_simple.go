package ai

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
)

// 中文说明：
// SimpleJSONEngine：最小可用的引擎实现。
// - 调用单个 ModelProvider，获取完整文本输出
// - 从文本中提取首个 JSON 数组并解析为 []Decision
// - 不做复杂校验（交由上层或旧版引擎适配器处理）

type SimpleJSONEngine struct {
	Model ModelProvider
	Name_ string
}

func (e *SimpleJSONEngine) Name() string {
	if e.Name_ != "" {
		return e.Name_
	}
	return "simple-json-engine"
}

func (e *SimpleJSONEngine) Decide(ctx context.Context, input Context) (DecisionResult, error) {
	if e.Model == nil || !e.Model.Enabled() {
		return DecisionResult{}, errors.New("模型未配置或未启用")
	}
	raw, err := e.Model.Call(ctx, input.Prompt.System, input.Prompt.User)
	if err != nil {
		return DecisionResult{RawOutput: raw}, err
	}
	arr, ok := extractJSONArray(raw)
	if !ok {
		return DecisionResult{RawOutput: raw}, errors.New("未找到 JSON 决策数组")
	}
	var ds []Decision
	if err := json.Unmarshal([]byte(arr), &ds); err != nil {
		return DecisionResult{RawOutput: raw}, err
	}
	return DecisionResult{Decisions: ds, RawOutput: raw}, nil
}

// extractJSONArray 在字符串中查找首个 JSON 数组（不做严格语法检查）
func extractJSONArray(s string) (string, bool) {
	start := strings.Index(s, "[")
	if start == -1 {
		return "", false
	}
	depth := 0
	for i := start; i < len(s); i++ {
		switch s[i] {
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return strings.TrimSpace(s[start : i+1]), true
			}
		}
	}
	return "", false
}
