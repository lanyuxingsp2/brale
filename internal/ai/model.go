package ai

import "context"

// 中文说明：
// 模型提供方（ModelProvider）抽象。可封装 DeepSeek/Qwen/自定义 OpenAI 兼容接口。

// ModelProvider 模型调用接口
type ModelProvider interface {
	ID() string
	Enabled() bool
	// Call 执行模型推理，返回完整文本（包含思维链与 JSON 决策数组）
	Call(ctx context.Context, systemPrompt, userPrompt string) (string, error)
}
