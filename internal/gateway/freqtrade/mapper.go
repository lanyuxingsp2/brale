package freqtrade

import (
	"encoding/json"
	"time"

	"brale/internal/gateway/database"
	"brale/internal/store/model"
)

func deref[T any](ptr *T) T {
	if ptr == nil {
		var zero T
		return zero
	}
	return *ptr
}

func normalizeUnixMillis(ts int64) int64 {
	if ts <= 0 {
		return 0
	}

	if ts < 1_000_000_000_000 {
		return ts * 1000
	}
	return ts
}

func derefUnixMillis(t *time.Time) int64 {
	if t == nil {
		return 0
	}
	return t.UnixMilli()
}

func timeFromUnixMillis(ts int64) time.Time {
	ts = normalizeUnixMillis(ts)
	if ts <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ts)
}

func toOrderModel(rec database.LiveOrderRecord) *model.LiveOrderModel {
	isSimulated := 0
	if rec.IsSimulated != nil && *rec.IsSimulated {
		isSimulated = 1
	}

	return &model.LiveOrderModel{
		FreqtradeID:       rec.FreqtradeID,
		Symbol:            rec.Symbol,
		Side:              rec.Side,
		Status:            model.LiveOrderStatus(rec.Status),
		RawData:           rec.RawData,
		CreatedAtUnix:     rec.CreatedAt.UnixMilli(),
		UpdatedAtUnix:     rec.UpdatedAt.UnixMilli(),
		IsSimulated:       isSimulated,
		Amount:            deref(rec.Amount),
		InitialAmount:     deref(rec.InitialAmount),
		StakeAmount:       deref(rec.StakeAmount),
		Leverage:          deref(rec.Leverage),
		PositionValue:     deref(rec.PositionValue),
		Price:             deref(rec.Price),
		ClosedAmount:      deref(rec.ClosedAmount),
		StartTimestamp:    derefUnixMillis(rec.StartTime),
		EndTimestamp:      derefUnixMillis(rec.EndTime),
		PnLRatio:          deref(rec.PnLRatio),
		PnLUSD:            deref(rec.PnLUSD),
		CurrentPrice:      deref(rec.CurrentPrice),
		CurrentProfitRate: deref(rec.CurrentProfitRatio),
		CurrentProfitAbs:  deref(rec.CurrentProfitAbs),
		UnrealizedRatio:   deref(rec.UnrealizedPnLRatio),
		UnrealizedUSD:     deref(rec.UnrealizedPnLUSD),
		RealizedRatio:     deref(rec.RealizedPnLRatio),
		RealizedUSD:       deref(rec.RealizedPnLUSD),
		LastStatusSync:    derefUnixMillis(rec.LastStatusSync),
	}
}

func fromOrderModel(m *model.LiveOrderModel) database.LiveOrderRecord {
	if m == nil {
		return database.LiveOrderRecord{}
	}

	isSimulated := m.IsSimulated == 1
	startTime := timeFromUnixMillis(m.StartTimestamp)
	var startPtr *time.Time
	if !startTime.IsZero() {
		startPtr = &startTime
	}
	var endTime *time.Time
	if m.EndTimestamp > 0 {
		t := timeFromUnixMillis(m.EndTimestamp)
		endTime = &t
	}
	var lastSync *time.Time
	if m.LastStatusSync > 0 {
		t := timeFromUnixMillis(m.LastStatusSync)
		lastSync = &t
	}

	return database.LiveOrderRecord{
		FreqtradeID:        m.FreqtradeID,
		Symbol:             m.Symbol,
		Side:               m.Side,
		Amount:             &m.Amount,
		InitialAmount:      &m.InitialAmount,
		StakeAmount:        &m.StakeAmount,
		Leverage:           &m.Leverage,
		PositionValue:      &m.PositionValue,
		Price:              &m.Price,
		ClosedAmount:       &m.ClosedAmount,
		IsSimulated:        &isSimulated,
		Status:             database.LiveOrderStatus(m.Status),
		StartTime:          startPtr,
		EndTime:            endTime,
		RawData:            m.RawData,
		CreatedAt:          timeFromUnixMillis(m.CreatedAtUnix),
		UpdatedAt:          timeFromUnixMillis(m.UpdatedAtUnix),
		PnLRatio:           &m.PnLRatio,
		PnLUSD:             &m.PnLUSD,
		CurrentPrice:       &m.CurrentPrice,
		CurrentProfitRatio: &m.CurrentProfitRate,
		CurrentProfitAbs:   &m.CurrentProfitAbs,
		UnrealizedPnLRatio: &m.UnrealizedRatio,
		UnrealizedPnLUSD:   &m.UnrealizedUSD,
		RealizedPnLRatio:   &m.RealizedRatio,
		RealizedPnLUSD:     &m.RealizedUSD,
		LastStatusSync:     lastSync,
	}
}

func fromStrategyModel(m *model.StrategyInstanceModel) database.StrategyInstanceRecord {
	if m == nil {
		return database.StrategyInstanceRecord{}
	}
	var lastEval *time.Time
	if m.LastEvalUnix != nil {
		t := time.Unix(*m.LastEvalUnix, 0)
		lastEval = &t
	}
	var nextEval *time.Time
	if m.NextEvalUnix != nil {
		t := time.Unix(*m.NextEvalUnix, 0)
		nextEval = &t
	}

	return database.StrategyInstanceRecord{
		ID:              m.ID,
		TradeID:         m.TradeID,
		PlanID:          m.PlanID,
		PlanComponent:   m.PlanComponent,
		PlanVersion:     m.PlanVersion,
		ParamsJSON:      string(m.ParamsJSON),
		StateJSON:       string(m.StateJSON),
		Status:          database.StrategyStatus(m.Status),
		DecisionTraceID: m.DecisionTraceID,
		LastEvalAt:      lastEval,
		NextEvalAfter:   nextEval,
		CreatedAt:       time.Unix(m.CreatedAtUnix, 0),
		UpdatedAt:       time.Unix(m.UpdatedAtUnix, 0),
	}
}

func fromOperationModel(m *model.TradeOperationModel) database.TradeOperationRecord {
	if m == nil {
		return database.TradeOperationRecord{}
	}
	var details map[string]any
	if err := json.Unmarshal(m.Details, &details); err != nil || details == nil {
		details = make(map[string]any)
	}
	return database.TradeOperationRecord{
		FreqtradeID: m.FreqtradeID,
		Symbol:      m.Symbol,
		Operation:   database.OperationType(m.Operation),
		Details:     details,
		Timestamp:   time.UnixMilli(m.Timestamp),
	}
}
