package ai

import "context"

// 中文说明：
// 决策引擎接口。后续将提供“旧版引擎适配器（LegacyEngineAdapter）”对接现有逻辑。

// DecisionEngine 决策引擎接口
type DecisionEngine interface {
	// Decide 基于上下文进行决策，返回结果（包含多币种动作）
	Decide(ctx context.Context, input Context) (DecisionResult, error)
	Name() string
}

// Context 引擎输入上下文（简化版）：实际落地时可扩展为账户/持仓/候选币/指标等
type Context struct {
	Candidates []string              // 候选币种
	Market     map[string]MarketData // 各币种聚合指标（可后续填充）
	Prompt     PromptBundle          // System/User 提示词
}

// MarketData 占位结构：后续接 K 线指标/OI/Funding 等
type MarketData struct{}

// PromptBundle 引擎使用的提示词材料
type PromptBundle struct {
	System string
	User   string
}
