package freqtrade

import (
	"context"
	"fmt"
	"strings"
	"time"

	"brale/internal/decision"
	"brale/internal/gateway/database"
	"brale/internal/logger"
)

// adjustStopLoss/adjustTakeProfit/updateTiers：基于 live_tiers/live_orders，保持 take_profit 与 tier3 一致，并记录修改/操作流水。

func (m *Manager) adjustStopLoss(ctx context.Context, traceID string, d decision.Decision) error {
	if d.StopLoss <= 0 {
		return fmt.Errorf("invalid stop_loss for adjust_stop_loss")
	}
	tradeID, side, _, ok := m.findActiveTrade(d.Symbol)
	if !ok {
		return fmt.Errorf("freqtrade 没有 %s 的持仓，无法调整止损", d.Symbol)
	}
	lock := getPositionLock(tradeID)
	lock.Lock()
	defer lock.Unlock()

	newStop := d.StopLoss
	newTP := d.TakeProfit
	now := time.Now()
	orderRec, tierRec, ok, err := m.posRepo.GetPosition(ctx, tradeID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("未找到 freqtrade_id=%d 的仓位", tradeID)
	}
	entry := valOrZero(orderRec.Price)
	currentStop := tierRec.StopLoss
	currentTP := tierRec.TakeProfit

	changedStop := newStop > 0 && !floatEqual(newStop, currentStop)
	changedTP := newTP > 0 && !floatEqual(newTP, currentTP)
	if !changedStop && !changedTP {
		return nil
	}
	if newTP <= 0 {
		newTP = currentTP
	}
	if !changedStop {
		newStop = currentStop
	}

	// 更新 tiers
	tierRec.StopLoss = newStop
	tierRec.TakeProfit = newTP
	if newTP > 0 {
		tierRec.Tier3 = newTP
	} else if tierRec.Tier3 == 0 && tierRec.TakeProfit > 0 {
		tierRec.Tier3 = tierRec.TakeProfit
	}
	tierRec.IsPlaceholder = false
	tierRec.UpdatedAt = now
	if err := m.validateTierRecord(entry, side, tierRec, true); err != nil {
		return err
	}
	_ = m.posRepo.UpsertTiers(ctx, tierRec)
	m.updateCacheOrderTiers(database.LiveOrderRecord{FreqtradeID: tradeID}, tierRec)
	logger.Infof("freqtrade adjust_stop_loss trade=%d symbol=%s changed_stop=%v changed_tp=%v new_sl=%.4f new_tp=%.4f", tradeID, strings.ToUpper(d.Symbol), changedStop, changedTP, newStop, newTP)

	// 修改流水
	if changedStop {
		m.posRepo.InsertModification(ctx, database.TierModificationLog{
			FreqtradeID: tradeID,
			Field:       database.TierFieldStopLoss,
			OldValue:    formatPrice(currentStop),
			NewValue:    formatPrice(newStop),
			Source:      1,
			Reason:      strings.TrimSpace(d.Reasoning),
			Timestamp:   now,
		})
	}
	if changedTP {
		m.posRepo.InsertModification(ctx, database.TierModificationLog{
			FreqtradeID: tradeID,
			Field:       database.TierFieldTakeProfit,
			OldValue:    formatPrice(currentTP),
			NewValue:    formatPrice(newTP),
			Source:      1,
			Reason:      strings.TrimSpace(d.Reasoning),
			Timestamp:   now,
		})
	}

	m.appendOperation(ctx, tradeID, d.Symbol, database.OperationAdjust, map[string]any{
		"event_type":  "ADJUST_STOP_LOSS",
		"stop_loss":   newStop,
		"take_profit": newTP,
		"reason":      strings.TrimSpace(d.Reasoning),
	})

	title := "止损已更新 ✅"
	if changedStop && changedTP {
		title = "止损/止盈已更新 ✅"
	} else if !changedStop && changedTP {
		title = "止盈已更新 ✅"
	}
	lines := []string{
		fmt.Sprintf("交易ID: %d", tradeID),
		fmt.Sprintf("标的: %s", strings.ToUpper(d.Symbol)),
		fmt.Sprintf("方向: %s", strings.ToUpper(side)),
	}
	if changedStop {
		lines = append(lines, fmt.Sprintf("止损: %s → %.4f", formatPrice(currentStop), newStop))
	}
	if changedTP {
		lines = append(lines, fmt.Sprintf("止盈: %s → %.4f", formatPrice(currentTP), newTP))
	}
	lines = append(lines, fmt.Sprintf("Trace: %s", m.ensureTrace(traceID)))
	if reason := shortReason(d.Reasoning); reason != "" {
		lines = append(lines, "理由: "+reason)
	}
	m.notify(title, lines...)
	return nil
}

func (m *Manager) adjustTakeProfit(ctx context.Context, traceID string, d decision.Decision) error {
	if d.TakeProfit <= 0 {
		return fmt.Errorf("invalid take_profit for adjust_take_profit")
	}
	tradeID, side, _, ok := m.findActiveTrade(d.Symbol)
	if !ok {
		return fmt.Errorf("freqtrade 没有 %s 的持仓，无法调整止盈", d.Symbol)
	}
	lock := getPositionLock(tradeID)
	lock.Lock()
	defer lock.Unlock()

	now := time.Now()
	orderRec, tierRec, ok, err := m.posRepo.GetPosition(ctx, tradeID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("未找到 freqtrade_id=%d 的仓位", tradeID)
	}
	entry := valOrZero(orderRec.Price)
	currentTP := tierRec.TakeProfit
	currentStop := tierRec.StopLoss
	newTP := d.TakeProfit
	newStop := d.StopLoss
	if newStop <= 0 {
		newStop = currentStop
	}

	changedTP := !floatEqual(currentTP, newTP)
	changedStop := !floatEqual(currentStop, newStop)
	if !changedTP && !changedStop {
		return nil
	}

	tierRec.TakeProfit = newTP
	tierRec.StopLoss = newStop
	if newTP > 0 {
		tierRec.Tier3 = newTP
	} else if tierRec.Tier3 == 0 && tierRec.TakeProfit > 0 {
		tierRec.Tier3 = tierRec.TakeProfit
	}
	tierRec.IsPlaceholder = false
	tierRec.UpdatedAt = now
	if err := m.validateTierRecord(entry, side, tierRec, true); err != nil {
		return err
	}
	_ = m.posRepo.UpsertTiers(ctx, tierRec)
	logger.Infof("freqtrade adjust_take_profit trade=%d symbol=%s changed_tp=%v changed_sl=%v new_tp=%.4f new_sl=%.4f", tradeID, strings.ToUpper(d.Symbol), changedTP, changedStop, newTP, newStop)

	if changedTP {
		m.posRepo.InsertModification(ctx, database.TierModificationLog{
			FreqtradeID: tradeID,
			Field:       database.TierFieldTakeProfit,
			OldValue:    formatPrice(currentTP),
			NewValue:    formatPrice(newTP),
			Source:      1,
			Reason:      strings.TrimSpace(d.Reasoning),
			Timestamp:   now,
		})
	}
	if changedStop {
		m.posRepo.InsertModification(ctx, database.TierModificationLog{
			FreqtradeID: tradeID,
			Field:       database.TierFieldStopLoss,
			OldValue:    formatPrice(currentStop),
			NewValue:    formatPrice(newStop),
			Source:      1,
			Reason:      strings.TrimSpace(d.Reasoning),
			Timestamp:   now,
		})
	}

	m.appendOperation(ctx, tradeID, d.Symbol, database.OperationAdjust, map[string]any{
		"event_type":  "ADJUST_TAKE_PROFIT",
		"take_profit": newTP,
		"stop_loss":   newStop,
		"reason":      strings.TrimSpace(d.Reasoning),
	})

	title := "止盈已更新 ✅"
	if changedTP && changedStop {
		title = "止盈/止损已更新 ✅"
	} else if !changedTP && changedStop {
		title = "止损已更新 ✅"
	}
	lines := []string{
		fmt.Sprintf("交易ID: %d", tradeID),
		fmt.Sprintf("标的: %s", strings.ToUpper(d.Symbol)),
		fmt.Sprintf("方向: %s", strings.ToUpper(side)),
	}
	if changedTP {
		lines = append(lines, fmt.Sprintf("止盈: %s → %.4f", formatPrice(currentTP), newTP))
	}
	if changedStop {
		lines = append(lines, fmt.Sprintf("止损: %s → %.4f", formatPrice(currentStop), newStop))
	}
	lines = append(lines, fmt.Sprintf("Trace: %s", m.ensureTrace(traceID)))
	if reason := shortReason(d.Reasoning); reason != "" {
		lines = append(lines, "理由: "+reason)
	}
	m.notify(title, lines...)
	return nil
}

func (m *Manager) updateTiers(ctx context.Context, traceID string, d decision.Decision, enforceOffset bool) error {
	if d.Tiers == nil {
		return fmt.Errorf("update_tiers 缺少 tiers 字段")
	}
	tradeID, side, _, ok := m.findActiveTrade(d.Symbol)
	if !ok {
		return fmt.Errorf("freqtrade 没有 %s 的持仓，无法调整 tiers", d.Symbol)
	}
	lock := getPositionLock(tradeID)
	lock.Lock()
	defer lock.Unlock()

	now := time.Now()
	orderRec, old, ok, err := m.posRepo.GetPosition(ctx, tradeID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("未找到 freqtrade_id=%d 的仓位", tradeID)
	}
	entry := valOrZero(orderRec.Price)
	newRec := old
	tiersTouched := false
	if d.StopLoss > 0 {
		newRec.StopLoss = d.StopLoss
	}
	if d.TakeProfit > 0 {
		newRec.TakeProfit = d.TakeProfit
	}
	if d.Tiers.Tier1Target > 0 {
		if !floatEqual(old.Tier1, d.Tiers.Tier1Target) {
			tiersTouched = true
		}
		newRec.Tier1 = d.Tiers.Tier1Target
	}
	if d.Tiers.Tier2Target > 0 {
		if !floatEqual(old.Tier2, d.Tiers.Tier2Target) {
			tiersTouched = true
		}
		newRec.Tier2 = d.Tiers.Tier2Target
	}
	if d.Tiers.Tier3Target > 0 {
		if !floatEqual(old.Tier3, d.Tiers.Tier3Target) {
			tiersTouched = true
		}
		newRec.Tier3 = d.Tiers.Tier3Target
		newRec.TakeProfit = d.Tiers.Tier3Target
	}
	if d.Tiers.Tier1Ratio > 0 {
		if !floatEqual(old.Tier1Ratio, d.Tiers.Tier1Ratio) {
			tiersTouched = true
		}
		newRec.Tier1Ratio = d.Tiers.Tier1Ratio
	}
	if d.Tiers.Tier2Ratio > 0 {
		if !floatEqual(old.Tier2Ratio, d.Tiers.Tier2Ratio) {
			tiersTouched = true
		}
		newRec.Tier2Ratio = d.Tiers.Tier2Ratio
	}
	if d.Tiers.Tier3Ratio > 0 {
		if !floatEqual(old.Tier3Ratio, d.Tiers.Tier3Ratio) {
			tiersTouched = true
		}
		newRec.Tier3Ratio = d.Tiers.Tier3Ratio
	}
	if tiersTouched && newRec.Tier3 > 0 && !floatEqual(newRec.TakeProfit, newRec.Tier3) {
		// 每次修改 tier 时，将止盈强制等于 tier3 末段价格，避免沿用旧止盈提前平仓。
		logger.Infof("freqtrade manager: update_tiers force take_profit to tier3 trade=%d old=%.4f new=%.4f", tradeID, newRec.TakeProfit, newRec.Tier3)
		newRec.TakeProfit = newRec.Tier3
	}
	newRec.IsPlaceholder = false
	newRec.UpdatedAt = now
	changed := !(floatEqual(old.TakeProfit, newRec.TakeProfit) &&
		floatEqual(old.StopLoss, newRec.StopLoss) &&
		floatEqual(old.Tier1, newRec.Tier1) &&
		floatEqual(old.Tier2, newRec.Tier2) &&
		floatEqual(old.Tier3, newRec.Tier3) &&
		floatEqual(old.Tier1Ratio, newRec.Tier1Ratio) &&
		floatEqual(old.Tier2Ratio, newRec.Tier2Ratio) &&
		floatEqual(old.Tier3Ratio, newRec.Tier3Ratio))
	if !changed {
		return nil
	}
	if err := m.validateTierRecord(entry, side, newRec, enforceOffset); err != nil {
		logger.Warnf("freqtrade update_tiers: validation failed trade=%d symbol=%s side=%s entry=%.4f err=%v", tradeID, strings.ToUpper(d.Symbol), side, entry, err)
		return err
	}
	if err := m.posRepo.UpsertTiers(ctx, newRec); err != nil {
		return err
	}
	m.updateCacheOrderTiers(database.LiveOrderRecord{FreqtradeID: tradeID, Symbol: strings.ToUpper(d.Symbol), Side: side, Status: orderRec.Status}, newRec)
	logger.Infof("freqtrade update_tiers trade=%d symbol=%s tier_changed=%v", tradeID, strings.ToUpper(d.Symbol), changed)

	writeMod := func(field database.TierField, oldV, newV float64) {
		if floatEqual(oldV, newV) {
			return
		}
		m.posRepo.InsertModification(ctx, database.TierModificationLog{
			FreqtradeID: tradeID,
			Field:       field,
			OldValue:    formatPrice(oldV),
			NewValue:    formatPrice(newV),
			Source:      1,
			Reason:      strings.TrimSpace(d.Reasoning),
			Timestamp:   now,
		})
	}
	writeMod(database.TierFieldTier1, old.Tier1, newRec.Tier1)
	writeMod(database.TierFieldTier2, old.Tier2, newRec.Tier2)
	writeMod(database.TierFieldTier3, old.Tier3, newRec.Tier3)
	writeMod(database.TierFieldTier1Ratio, old.Tier1Ratio, newRec.Tier1Ratio)
	writeMod(database.TierFieldTier2Ratio, old.Tier2Ratio, newRec.Tier2Ratio)
	writeMod(database.TierFieldTier3Ratio, old.Tier3Ratio, newRec.Tier3Ratio)
	writeMod(database.TierFieldTakeProfit, old.TakeProfit, newRec.TakeProfit)
	writeMod(database.TierFieldStopLoss, old.StopLoss, newRec.StopLoss)

	m.appendOperation(ctx, tradeID, d.Symbol, database.OperationUpdateTiers, map[string]any{
		"event_type": "UPDATE_TIERS",
		"tiers":      d.Tiers,
		"reason":     strings.TrimSpace(d.Reasoning),
	})

	slLine := fmt.Sprintf("止损: %s → %.4f", formatPrice(old.StopLoss), newRec.StopLoss)
	tpLine := fmt.Sprintf("止盈: %s → %.4f (强制对齐 tier3)", formatPrice(old.TakeProfit), newRec.TakeProfit)
	tierLine := fmt.Sprintf("T1 %.4f/%.2f%% · T2 %.4f/%.2f%% · T3 %.4f/%.2f%%",
		newRec.Tier1, newRec.Tier1Ratio*100,
		newRec.Tier2, newRec.Tier2Ratio*100,
		newRec.Tier3, newRec.Tier3Ratio*100)

	lines := []string{
		fmt.Sprintf("交易ID: %d", tradeID),
		fmt.Sprintf("标的: %s", strings.ToUpper(d.Symbol)),
		fmt.Sprintf("方向: %s", strings.ToUpper(side)),
		fmt.Sprintf("Trace: %s", m.ensureTrace(traceID)),
	}
	lines = append(lines, slLine, tpLine, tierLine)
	if rs := shortReason(d.Reasoning); rs != "" {
		lines = append(lines, "理由: "+rs)
	}
	m.notify("Tier/止盈止损已更新 ⚙️", lines...)
	return nil
}
