package decision

// 中文说明：
// 引擎输入数据类型与提示词载体。
// 当前仅保留给 LegacyEngineAdapter 使用，不再暴露/依赖通用引擎接口。

import (
	"context"

	"brale/internal/types"
)

// Context 引擎输入上下文（简化版）：可扩展为账户/持仓/候选币/指标等
type Context struct {
	Candidates        []string                 // 候选币种
	Market            map[string]MarketData    // 各币种聚合指标（可后续填充）
	Positions         []types.PositionSnapshot // 当前持仓信息
	Account           types.AccountSnapshot    // 账户资金概要
	ProfilePrompts    map[string]ProfilePromptSpec
	Prompt            PromptBundle          // System/User 提示词
	Analysis          []AnalysisContext     // 新版结构化分析上下文
	FeatureReports    []types.FeatureReport // Pipeline 输出的语义化特征
	ExitPlanDirective string                // Exit Plan JSON Schema 提示段落
	PreviousReasoning map[string]string     // 每个币种上一轮的推理摘要
	Insights          []AgentInsight        // 多 Agent 协作产出
	Directives        map[string]ProfileDirective
}

// MarketData 占位结构：后续可接 K 线指标/OI/Funding 等
// MarketData 聚合市场指标：K 线/衍生品数据
type MarketData struct {
	Symbol      string  `json:"symbol"`
	Price       float64 `json:"price"`
	Volume24h   float64 `json:"volume_24h"`
	OI          float64 `json:"oi"`           // Open Interest
	FundingRate float64 `json:"funding_rate"` // Funding Rate
	MarkPrice   float64 `json:"mark_price"`
}

// PromptBundle 引擎使用的提示词材料
type PromptBundle struct {
	System string
	User   string
}

// ProfilePromptSpec 记录单个 symbol 对应 profile 的提示词模板。
type ProfilePromptSpec struct {
	Profile              string
	ContextTag           string
	PromptRef            string
	SystemPromptsByModel map[string]string // model.id -> system prompt（最终决策阶段按模型选择）
	UserPrompt           string
	ExitConstraints      string
	Example              string
}

// Decider 决策器接口：将上下文转为决策结果
type Decider interface {
	Decide(ctx context.Context, input Context) (DecisionResult, error)
}
