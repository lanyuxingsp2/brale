package decision

// 中文说明：
// 引擎输入数据类型与提示词载体。
// 当前仅保留给 LegacyEngineAdapter 使用，不再暴露/依赖通用引擎接口。

import (
	"context"
	"time"
)

// Context 引擎输入上下文（简化版）：可扩展为账户/持仓/候选币/指标等
type Context struct {
	Candidates    []string              // 候选币种
	Market        map[string]MarketData // 各币种聚合指标（可后续填充）
	Positions     []PositionSnapshot    // 当前持仓信息
	Account       AccountSnapshot       // 账户资金概要
	Prompt        PromptBundle          // System/User 提示词
	Analysis      []AnalysisContext     // 新版结构化分析上下文
	LastDecisions []DecisionMemory      // 上一次决策（可选）
	LastRawJSON   string                // 上一次决策的原始 JSON（可选）
}

// MarketData 占位结构：后续可接 K 线指标/OI/Funding 等
type MarketData struct{}

// PositionSnapshot 提供给模型的仓位摘要。
type PositionSnapshot struct {
	Symbol          string  `json:"symbol"`
	Side            string  `json:"side"`
	EntryPrice      float64 `json:"entry_price"`
	Quantity        float64 `json:"quantity"`
	Stake           float64 `json:"stake,omitempty"`
	Leverage        float64 `json:"leverage,omitempty"`
	TakeProfit      float64 `json:"take_profit"`
	StopLoss        float64 `json:"stop_loss"`
	CurrentPrice    float64 `json:"current_price,omitempty"`
	UnrealizedPn    float64 `json:"unrealized_pn"`
	UnrealizedPnPct float64 `json:"unrealized_pn_pct"`
	PositionValue   float64 `json:"position_value,omitempty"`
	AccountRatio    float64 `json:"account_ratio,omitempty"`
	RR              float64 `json:"rr"`
	HoldingMs       int64   `json:"holding_ms"`
	RemainingRatio  float64 `json:"remaining_ratio,omitempty"`
	Tier1Target     float64 `json:"tier1_target,omitempty"`
	Tier1Ratio      float64 `json:"tier1_ratio,omitempty"`
	Tier1Done       bool    `json:"tier1_done,omitempty"`
	Tier2Target     float64 `json:"tier2_target,omitempty"`
	Tier2Ratio      float64 `json:"tier2_ratio,omitempty"`
	Tier2Done       bool    `json:"tier2_done,omitempty"`
	Tier3Target     float64 `json:"tier3_target,omitempty"`
	Tier3Ratio      float64 `json:"tier3_ratio,omitempty"`
	Tier3Done       bool    `json:"tier3_done,omitempty"`
	TierNotes       string  `json:"tier_notes,omitempty"`
}

// AccountSnapshot 汇报账户权益/可用资金等，供提示词展示。
type AccountSnapshot struct {
	Total     float64   `json:"total"`
	Available float64   `json:"available"`
	Used      float64   `json:"used"`
	Currency  string    `json:"currency"`
	UpdatedAt time.Time `json:"updated_at"`
}

// PromptBundle 引擎使用的提示词材料
type PromptBundle struct {
	System string
	User   string
}

// Decider 决策器接口：将上下文转为决策结果
type Decider interface {
	Decide(ctx context.Context, input Context) (DecisionResult, error)
}
