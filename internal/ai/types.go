package ai

// 中文说明：
// 本文件定义 AI 决策相关的通用数据结构，供引擎与聚合器使用。

// Decision 单笔 AI 决策动作（与旧版保持兼容的最小字段集）
type Decision struct {
	Symbol          string  `json:"symbol"`
	Action          string  `json:"action"`             // open_long/open_short/close_long/close_short/hold/wait
	Leverage        int     `json:"leverage,omitempty"` // 杠杆（仅开仓）
	PositionSizeUSD float64 `json:"position_size_usd,omitempty"`
	StopLoss        float64 `json:"stop_loss,omitempty"`
	TakeProfit      float64 `json:"take_profit,omitempty"`
	Confidence      int     `json:"confidence,omitempty"`
	Reasoning       string  `json:"reasoning,omitempty"`
}

// DecisionResult AI 决策输出（可包含多币种）
type DecisionResult struct {
	Decisions []Decision
	RawOutput string // 原始模型输出（便于调试）
}
