package ai

// 配置驱动的 Provider 工厂（不再使用环境变量）。

// 中文说明：
// 根据环境变量与配置，构造 MCP 模型提供方列表。
// 为了避免在此层面写死配置结构，外部只需告知启用哪些 provider ID，
// 这里通过常见环境变量查找对应凭据：
// - DeepSeek: DEEPSEEK_API_KEY；可选 AI_CUSTOM_URL/AI_CUSTOM_MODEL 覆盖
// - Qwen:     QWEN_API_KEY；   可选 AI_CUSTOM_URL/AI_CUSTOM_MODEL 覆盖

// BuildMCPProviders 简化工厂：按 flags 启用 deepseek/qwen 两类 provider
type ModelCfg struct {
    ID, Provider, APIURL, APIKey, Model string
    Enabled bool
    Headers map[string]string // 额外请求头（如 X-API-Key / Organization）
}

// BuildProvidersFromConfig 根据配置文件的模型条目构造 Provider 列表
func BuildProvidersFromConfig(models []ModelCfg) []ModelProvider {
    out := make([]ModelProvider, 0, len(models))
    for _, m := range models {
        if !m.Enabled { continue }
        c := &OpenAIChatClient{BaseURL: m.APIURL, APIKey: m.APIKey, Model: m.Model, ExtraHeaders: m.Headers}
        out = append(out, NewOpenAIModelProvider(m.ID, true, c))
    }
    return out
}
