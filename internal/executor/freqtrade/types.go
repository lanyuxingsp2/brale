package freqtrade

import (
	"fmt"
	"strings"
	"time"

	"brale/internal/gateway/database"
)

// APIPosition 用于 /api/live/freqtrade/positions 返回的数据结构。
type APIPosition struct {
	TradeID            int                             `json:"trade_id"`
	Symbol             string                          `json:"symbol"`
	Side               string                          `json:"side"`
	EntryPrice         float64                         `json:"entry_price"`
	Amount             float64                         `json:"amount"`
	InitialAmount      float64                         `json:"initial_amount,omitempty"`
	Stake              float64                         `json:"stake"`
	Leverage           float64                         `json:"leverage"`
	PositionValue      float64                         `json:"position_value,omitempty"`
	OpenedAt           int64                           `json:"opened_at"`
	HoldingMs          int64                           `json:"holding_ms"`
	StopLoss           float64                         `json:"stop_loss,omitempty"`
	TakeProfit         float64                         `json:"take_profit,omitempty"`
	CurrentPrice       float64                         `json:"current_price,omitempty"`
	PnLRatio           float64                         `json:"pnl_ratio,omitempty"`
	PnLUSD             float64                         `json:"pnl_usd,omitempty"`
	RealizedPnLRatio   float64                         `json:"realized_pnl_ratio,omitempty"`
	RealizedPnLUSD     float64                         `json:"realized_pnl_usd,omitempty"`
	UnrealizedPnLRatio float64                         `json:"unrealized_pnl_ratio,omitempty"`
	UnrealizedPnLUSD   float64                         `json:"unrealized_pnl_usd,omitempty"`
	RemainingRatio     float64                         `json:"remaining_ratio,omitempty"`
	Tier1              TierInfo                        `json:"tier1"`
	Tier2              TierInfo                        `json:"tier2"`
	Tier3              TierInfo                        `json:"tier3"`
	TierNotes          string                          `json:"tier_notes,omitempty"`
	Placeholder        bool                            `json:"placeholder,omitempty"`
	TierLogs           []database.TierModificationLog  `json:"tier_logs,omitempty"`
	Events             []database.TradeOperationRecord `json:"events,omitempty"`
	Status             string                          `json:"status"`
	ClosedAt           int64                           `json:"closed_at,omitempty"`
	ExitPrice          float64                         `json:"exit_price,omitempty"`
	ExitReason         string                          `json:"exit_reason,omitempty"`
}

type TierInfo struct {
	Target float64 `json:"target"`
	Ratio  float64 `json:"ratio"`
	Done   bool    `json:"done"`
}

// PositionPnLValue returns the notional value that should be used when converting
// a PnL ratio into USD. Prefer the explicit position_value from DB; otherwise
// fall back to stake * max(leverage, 1).
func PositionPnLValue(stake, leverage, positionValue float64) float64 {
	if positionValue > 0 {
		return positionValue
	}
	if stake <= 0 {
		return 0
	}
	if leverage <= 0 {
		leverage = 1
	}
	return stake * leverage
}

// RemainingPositionValue scales the initial notional value by the ratio of
// current amount to the initial amount, representing the open portion worth.
func RemainingPositionValue(stake, leverage, positionValue, amount, initialAmount float64) float64 {
	base := PositionPnLValue(stake, leverage, positionValue)
	if base <= 0 {
		return 0
	}
	if initialAmount <= 0 {
		return base
	}
	if amount <= 0 {
		return 0
	}
	frac := amount / initialAmount
	if frac <= 0 {
		return 0
	}
	if frac > 1 {
		frac = 1
	}
	return base * frac
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

// ManualOpenRequest 描述手动开仓需要的最小字段。
type ManualOpenRequest struct {
	Symbol          string  `json:"symbol" form:"symbol"`
	Side            string  `json:"side" form:"side"`
	PositionSizeUSD float64 `json:"position_size_usd" form:"position_size_usd"`
	Leverage        int     `json:"leverage" form:"leverage"`
	StopLoss        float64 `json:"stop_loss" form:"stop_loss"`
	TakeProfit      float64 `json:"take_profit" form:"take_profit"`
	Tier1Target     float64 `json:"tier1_target" form:"tier1_target"`
	Tier1Ratio      float64 `json:"tier1_ratio" form:"tier1_ratio"`
	Tier2Target     float64 `json:"tier2_target" form:"tier2_target"`
	Tier2Ratio      float64 `json:"tier2_ratio" form:"tier2_ratio"`
	Tier3Target     float64 `json:"tier3_target" form:"tier3_target"`
	Tier3Ratio      float64 `json:"tier3_ratio" form:"tier3_ratio"`
	Reason          string  `json:"reason" form:"reason"`
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
