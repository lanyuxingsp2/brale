package freqtrade

import (
	"strings"
	"time"

	"brale/internal/gateway/database"
)

type pendingState int

const (
	pendingStateQueued pendingState = iota + 1
	pendingStateWaitingFill
	pendingStateConfirmed
	pendingStateFailed
)

// pendingExit 记录自动平仓在 freqtrade 中的执行情况。
type pendingExit struct {
	TradeID        int
	Symbol         string
	Side           string
	Kind           string
	CoveredTiers   []string
	TargetPrice    float64
	Ratio          float64
	PrevAmount     float64
	PrevClosed     float64
	InitialAmount  float64
	TargetAmount   float64
	ExpectedAmount float64
	RequestedAt    time.Time
	LastCheck      time.Time
	ConfirmedAt    time.Time
	State          pendingState
	ForceFull      bool
	Operation      database.OperationType
	EntryPrice     float64
	Stake          float64
	Leverage       float64
	ConfirmedTrade *Trade
	FailReason     string
}

func (pe pendingExit) clone() pendingExit {
	clone := pe
	if len(pe.CoveredTiers) > 0 {
		clone.CoveredTiers = append([]string(nil), pe.CoveredTiers...)
	}
	if pe.ConfirmedTrade != nil {
		tmp := *pe.ConfirmedTrade
		clone.ConfirmedTrade = &tmp
	}
	return clone
}

func (pe pendingExit) coveredTierRange() string {
	if len(pe.CoveredTiers) == 0 {
		return ""
	}
	if len(pe.CoveredTiers) == 1 {
		return strings.ToUpper(pe.CoveredTiers[0])
	}
	return strings.ToUpper(pe.CoveredTiers[0]) + "~" + strings.ToUpper(pe.CoveredTiers[len(pe.CoveredTiers)-1])
}

func (pe pendingExit) describeKind() string {
	if len(pe.CoveredTiers) == 0 || !strings.HasPrefix(strings.ToLower(pe.Kind), "tier") {
		return strings.ToUpper(pe.Kind)
	}
	return pe.coveredTierRange()
}

func (pe pendingExit) stateDesc() string {
	switch pe.State {
	case pendingStateQueued:
		return "QUEUED"
	case pendingStateWaitingFill:
		return "WAITING_FILL"
	case pendingStateConfirmed:
		return "CONFIRMED"
	case pendingStateFailed:
		return "FAILED"
	default:
		return "UNKNOWN"
	}
}

func (pe *pendingExit) markWaiting() {
	if pe == nil {
		return
	}
	pe.State = pendingStateWaitingFill
	pe.LastCheck = time.Now()
}

func (pe *pendingExit) markConfirmed(snapshot *Trade) {
	if pe == nil {
		return
	}
	pe.State = pendingStateConfirmed
	pe.ConfirmedAt = time.Now()
	if snapshot != nil {
		copy := *snapshot
		pe.ConfirmedTrade = &copy
	}
}

func (pe *pendingExit) markFailed(reason string) {
	if pe == nil {
		return
	}
	pe.State = pendingStateFailed
	pe.FailReason = reason
	pe.LastCheck = time.Now()
}
