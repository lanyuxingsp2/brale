package freqtrade

import (
	"fmt"
	"strings"
	"time"

	"brale/internal/gateway/database"
)

// APIPosition 用于 /api/live/freqtrade/positions 返回的数据结构。
type APIPosition struct {
	TradeID        int                             `json:"trade_id"`
	Symbol         string                          `json:"symbol"`
	Side           string                          `json:"side"`
	EntryPrice     float64                         `json:"entry_price"`
	Amount         float64                         `json:"amount"`
	Stake          float64                         `json:"stake"`
	Leverage       float64                         `json:"leverage"`
	PositionValue  float64                         `json:"position_value,omitempty"`
	OpenedAt       int64                           `json:"opened_at"`
	HoldingMs      int64                           `json:"holding_ms"`
	StopLoss       float64                         `json:"stop_loss,omitempty"`
	TakeProfit     float64                         `json:"take_profit,omitempty"`
	CurrentPrice   float64                         `json:"current_price,omitempty"`
	PnLRatio       float64                         `json:"pnl_ratio,omitempty"`
	PnLUSD         float64                         `json:"pnl_usd,omitempty"`
	RemainingRatio float64                         `json:"remaining_ratio,omitempty"`
	Tier1          TierInfo                        `json:"tier1"`
	Tier2          TierInfo                        `json:"tier2"`
	Tier3          TierInfo                        `json:"tier3"`
	TierNotes      string                          `json:"tier_notes,omitempty"`
	Placeholder    bool                            `json:"placeholder,omitempty"`
	TierLogs       []database.TierModificationLog  `json:"tier_logs,omitempty"`
	Events         []database.TradeOperationRecord `json:"events,omitempty"`
	Status         string                          `json:"status"`
	ClosedAt       int64                           `json:"closed_at,omitempty"`
	ExitPrice      float64                         `json:"exit_price,omitempty"`
	ExitReason     string                          `json:"exit_reason,omitempty"`
}

type TierInfo struct {
	Target float64 `json:"target"`
	Ratio  float64 `json:"ratio"`
	Done   bool    `json:"done"`
}

// TierLog/TradeEvent 兼容旧 HTTP 响应类型，直接复用新表结构。
type TierLog = database.TierModificationLog
type TradeEvent = database.TradeOperationRecord

type TierUpdateRequest struct {
	TradeID     int     `json:"trade_id" form:"trade_id"`
	Symbol      string  `json:"symbol" form:"symbol"`
	Side        string  `json:"side" form:"side"`
	StopLoss    float64 `json:"stop_loss" form:"stop_loss"`
	TakeProfit  float64 `json:"take_profit" form:"take_profit"`
	Tier1Target float64 `json:"tier1_target" form:"tier1_target"`
	Tier1Ratio  float64 `json:"tier1_ratio" form:"tier1_ratio"`
	Tier2Target float64 `json:"tier2_target" form:"tier2_target"`
	Tier2Ratio  float64 `json:"tier2_ratio" form:"tier2_ratio"`
	Tier3Target float64 `json:"tier3_target" form:"tier3_target"`
	Tier3Ratio  float64 `json:"tier3_ratio" form:"tier3_ratio"`
	Reason      string  `json:"reason" form:"reason"`
}

type PositionListOptions struct {
	Symbol      string
	Page        int
	PageSize    int
	IncludeLogs bool
	LogsLimit   int
}

type PositionListResult struct {
	TotalCount int           `json:"total_count"`
	Page       int           `json:"page"`
	PageSize   int           `json:"page_size"`
	Positions  []APIPosition `json:"positions"`
}

type pendingHit struct {
	target float64
	seenAt int64
}

type pendingExit struct {
	TradeID        int
	Symbol         string
	Side           string
	Kind           string // stop_loss, take_profit, tier1, tier2, tier3
	TargetPrice    float64
	Ratio          float64
	PrevAmount     float64
	PrevClosed     float64
	ExpectedAmount float64
	RequestedAt    time.Time
	Operation      database.OperationType
	EntryPrice     float64
	Stake          float64
	Leverage       float64
}

func formatQty(val float64) string {
	if val == 0 {
		return "-"
	}
	return fmt.Sprintf("%.4f", val)
}

func formatPrice(val float64) string {
	if val == 0 {
		return "-"
	}
	return fmt.Sprintf("%.4f", val)
}

func formatPercent(val float64) string {
	return fmt.Sprintf("%.0f%%", val*100)
}

func shortReason(desc string) string {
	desc = strings.TrimSpace(desc)
	if desc == "" {
		return ""
	}
	const maxLen = 200
	runes := []rune(desc)
	if len(runes) <= maxLen {
		return desc
	}
	return string(runes[:maxLen]) + "..."
}

func ptrFloat(v float64) *float64 { return &v }

func ptrBool(v bool) *bool { return &v }

func valOrZero(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

func timeToMillis(t *time.Time) int64 {
	if t == nil || t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func millisSince(t *time.Time) int64 {
	if t == nil || t.IsZero() {
		return 0
	}
	return time.Since(*t).Milliseconds()
}
