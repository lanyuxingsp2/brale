package agent

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"brale/internal/agent/ports"
	"brale/internal/gateway/database"
	"brale/internal/logger"
	"brale/internal/strategy/exit"
)

type PlanExecutor struct {
	repo            *PlanRepository
	execManager     ports.ExecutionManager
	onPlanTriggered func(ctx context.Context, tradeID int) // 触发后通知 scheduler 刷新
}

func NewPlanExecutor(repo *PlanRepository, execManager ports.ExecutionManager, onTriggered func(ctx context.Context, tradeID int)) *PlanExecutor {
	return &PlanExecutor{
		repo:            repo,
		execManager:     execManager,
		onPlanTriggered: onTriggered,
	}
}

func (e *PlanExecutor) EvaluateWatcher(ctx context.Context, watcher *planWatcher, price float64) {
	if watcher == nil || watcher.handler == nil {
		return
	}
	// 若已有 pending（等待 exit_fill 确认），则本轮不再继续评估其它组件，避免多段同时触发导致重复下单。
	if watcherHasPending(watcher) {
		return
	}
	if watcher.rootInst != nil {
		if evt, err := watcher.handler.OnPrice(ctx, *watcher.rootInst, price); err != nil {
			logger.Warnf("PlanExecutor: plan=%s trade=%d 根评估失败: %v", watcher.planID, watcher.tradeID, err)
		} else if evt != nil {
			e.HandlePlanEvent(ctx, watcher, watcher.rootInst, evt, price)
			if isCloseEventType(evt.Type) {
				return
			}
		}
	}
	if len(watcher.components) == 0 {
		return
	}
	keys := make([]string, 0, len(watcher.components))
	for k := range watcher.components {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		inst := watcher.components[k]
		if inst == nil {
			continue
		}
		if inst.Record.Status == database.StrategyStatusDone || inst.Record.Status == database.StrategyStatusPending {
			continue
		}
		if evt, err := watcher.handler.OnPrice(ctx, *inst, price); err != nil {
			logger.Warnf("PlanExecutor: plan=%s trade=%d component=%s 评估失败: %v", watcher.planID, watcher.tradeID, inst.Record.PlanComponent, err)
		} else if evt != nil {
			e.HandlePlanEvent(ctx, watcher, inst, evt, price)
			if isCloseEventType(evt.Type) {
				return
			}
		}
	}
}

func watcherHasPending(watcher *planWatcher) bool {
	if watcher == nil {
		return false
	}
	if watcher.rootInst != nil && watcher.rootInst.Record.Status == database.StrategyStatusPending {
		return true
	}
	for _, inst := range watcher.components {
		if inst != nil && inst.Record.Status == database.StrategyStatusPending {
			return true
		}
	}
	return false
}

func isCloseEventType(t string) bool {
	switch t {
	case exit.PlanEventTypeTierHit,
		exit.PlanEventTypeStopLoss, exit.PlanEventTypeTakeProfit,
		exit.PlanEventTypeFinalStopLoss, exit.PlanEventTypeFinalTakeProfit:
		return true
	default:
		return false
	}
}

func (e *PlanExecutor) HandlePlanEvent(ctx context.Context, watcher *planWatcher, inst *exit.PlanInstance, evt *exit.PlanEvent, price float64) {
	prevState := inst.Record.StateJSON
	prevStatus := inst.Record.Status
	updated := false
	switch evt.Type {
	case exit.PlanEventTypeTierHit:
		updated = e.markTierTriggered(ctx, inst, evt, price)
	case exit.PlanEventTypeStopLoss, exit.PlanEventTypeTakeProfit,
		exit.PlanEventTypeFinalStopLoss, exit.PlanEventTypeFinalTakeProfit:
		updated = e.markPlanTriggered(ctx, inst, evt)
	case exit.PlanEventTypeAdjust:
		updated = e.applyAdjustEvent(ctx, inst, evt)
	default:
		logger.Warnf("PlanExecutor: 未处理的事件类型 trade=%d plan=%s type=%s", evt.TradeID, evt.PlanID, evt.Type)
	}
	if !updated {
		return
	}
	if e.execManager != nil {
		var ratio float64
		doClose := false

		switch evt.Type {
		case exit.PlanEventTypeTierHit:
			if r, ok := extractExecutorFloat(evt.Details, "ratio"); ok && r > 0 {
				ratio = r
				doClose = true
			}
		case exit.PlanEventTypeStopLoss, exit.PlanEventTypeTakeProfit,
			exit.PlanEventTypeFinalStopLoss, exit.PlanEventTypeFinalTakeProfit:
			ratio = 1.0
			doClose = true
		}

		if doClose {
			if err := e.execManager.CloseFreqtradePosition(ctx, watcher.tradeID, watcher.symbol, watcher.side, ratio); err != nil {
				logger.Errorf("PlanExecutor: 执行平仓失败 symbol=%s side=%s ratio=%.2f err=%v", watcher.symbol, watcher.side, ratio, err)
			}
		}
	}
	var changeDetails map[string]any
	if evt != nil {
		if details, ok := evt.Details["changes"].(map[string]any); ok {
			changeDetails = details
		}
	}
	if e.repo != nil {
		e.repo.LogStateChange(ctx, inst, prevState, prevStatus, evt.Type, "", changeDetails)
		e.repo.LogTradeOperation(ctx, inst, evt)
	}
	// 触发后立即通知 scheduler 刷新索引，避免延迟
	if e.onPlanTriggered != nil {
		e.onPlanTriggered(ctx, watcher.tradeID)
	}
}

func (e *PlanExecutor) markTierTriggered(ctx context.Context, inst *exit.PlanInstance, evt *exit.PlanEvent, price float64) bool {
	state, err := exit.DecodeTierComponentState(inst.Record.StateJSON)
	if err != nil {
		logger.Warnf("PlanExecutor: 解析 tier 状态失败 trade=%d component=%s err=%v", inst.Record.TradeID, inst.Record.PlanComponent, err)
		return false
	}
	if strings.EqualFold(state.Status, "done") || strings.EqualFold(state.Status, "triggered") {
		return false
	}
	state.Status = "triggered"
	state.TriggerPrice = price
	state.TriggeredAt = time.Now().Unix()
	state.PendingSince = state.TriggeredAt
	state.PendingOrderID = ""
	state.LastEvent = evt.Type
	raw := exit.EncodeTierComponentState(state)
	if e.repo != nil {
		// Tier 触发成功后标记为 Pending（等待 exit_fill webhook 确认后改为 Done）
		if !e.repo.PersistPlanState(ctx, inst, raw, database.StrategyStatusPending) {
			logger.Warnf("PlanExecutor: 更新 tier 状态失败 trade=%d component=%s", inst.Record.TradeID, inst.Record.PlanComponent)
			return false
		}
		// Refresh plan cache logic was here in Scheduler. It called Scheduler.refreshPlanCache which did publishPlanSnapshots.
		// Executor shouldn't know about scheduler refresh. Maybe return result?
		// Or executor calls Repo (which doesn't do publish).
		// We should move publish logic?
		// Currently skip publishing here? Or rely on periodic sync?
		// Original code called s.refreshPlanCache(ctx, inst.Record.TradeID).
		// We can emit event or just do sync here if Executor has execManager.
		// Let's add Sync here?
		// e.execManager.SyncStrategyPlans... but we need all plans snapshots.
		// Executor doesn't have all plans for trade? Watcher has them! watcher.components.
		// So we can rebuild snapshots from watcher?
		// Watcher has `rootInst` and `components` map.
		// But watcher might be partial (grouped by plan ID). A trade can have multiple plans? (Usually one active plan tree).
		// Original `refreshPlanCache` re-fetched from DB.
		// Here we can optimize or just skip for now pending Periodic Sync.
		// Let's skip heavy sync on every trigger for now, rely on periodic?
		// But trigger implies UI update fast.
		// Let's defer sync.
		logger.Infof("PlanExecutor: trade=%d plan=%s component=%s 触发 tier 事件 price=%.4f", inst.Record.TradeID, inst.Record.PlanID, inst.Record.PlanComponent, price)
		return true
	}
	return false
}

func (e *PlanExecutor) markPlanTriggered(ctx context.Context, inst *exit.PlanInstance, evt *exit.PlanEvent) bool {
	state, err := exit.DecodeTierPlanState(inst.Record.StateJSON)
	if err != nil {
		logger.Warnf("PlanExecutor: 解析根状态失败 trade=%d plan=%s err=%v", inst.Record.TradeID, inst.Record.PlanID, err)
		return false
	}
	switch evt.Type {
	case exit.PlanEventTypeStopLoss:
		state.StopLossTriggered = true
	case exit.PlanEventTypeTakeProfit:
		state.TakeProfitTriggered = true
	case exit.PlanEventTypeFinalStopLoss:
		state.FinalStopLossTriggered = true
	case exit.PlanEventTypeFinalTakeProfit:
		state.FinalTakeProfitTriggered = true
	}
	state.LastUpdatedAt = time.Now().Unix()
	state.PendingEvent = evt.Type
	state.PendingSince = state.LastUpdatedAt
	state.PendingOrderID = ""
	state.LastEvent = evt.Type
	raw := exit.EncodeTierPlanState(state)
	if e.repo != nil {
		if !e.repo.PersistPlanState(ctx, inst, raw, database.StrategyStatusPending) {
			logger.Warnf("PlanExecutor: 更新根状态失败 trade=%d plan=%s", inst.Record.TradeID, inst.Record.PlanID)
			return false
		}
		logger.Infof("PlanExecutor: trade=%d plan=%s 触发事件 type=%s", inst.Record.TradeID, inst.Record.PlanID, evt.Type)
		return true
	}
	return false
}

func (e *PlanExecutor) applyAdjustEvent(ctx context.Context, inst *exit.PlanInstance, evt *exit.PlanEvent) bool {
	stateJSON, _ := evt.Details["state_json"].(string)
	stateJSON = strings.TrimSpace(stateJSON)
	if stateJSON == "" {
		return false
	}
	if e.repo != nil {
		if !e.repo.PersistPlanState(ctx, inst, stateJSON, database.StrategyStatusWaiting) {
			logger.Warnf("PlanExecutor: 应用调整事件失败 trade=%d plan=%s", inst.Record.TradeID, inst.Record.PlanID)
			return false
		}
		return true
	}
	return false
}

func (e *PlanExecutor) HandleAdjust(ctx context.Context, watcher *planWatcher, component string, params map[string]any, source string) (string, error) {
	var inst exit.PlanInstance
	if component == "" {
		if watcher.rootInst == nil {
			return "", fmt.Errorf("plan 缺少 root 记录")
		}
		inst = *watcher.rootInst
	} else if target, ok := watcher.components[component]; ok {
		inst = *target
	} else {
		return "", fmt.Errorf("未找到组件: %s", component)
	}
	if params == nil {
		params = map[string]any{}
	}
	evt, err := watcher.handler.OnAdjust(ctx, inst, params)
	if err != nil {
		return "", err
	}
	if evt == nil {
		// No-op adjustment (no changes).
		return "", nil
	}
	if evt.Type != exit.PlanEventTypeAdjust {
		return "", fmt.Errorf("handler 未返回 plan_adjust 事件")
	}
	prevState := inst.Record.StateJSON
	prevStatus := inst.Record.Status
	if !e.applyAdjustEvent(ctx, &inst, evt) {
		return "", fmt.Errorf("更新 plan 状态失败")
	}
	var changeDetails map[string]any
	if details, ok := evt.Details["changes"].(map[string]any); ok {
		changeDetails = details
	}
	if e.repo != nil {
		e.repo.LogStateChange(ctx, &inst, prevState, prevStatus, evt.Type, source, changeDetails)
	}
	reason, _ := buildPlanChangeReason(&inst, prevState, inst.Record.StateJSON, prevStatus, inst.Record.Status, changeDetails)
	return strings.TrimSpace(reason), nil
}

func extractExecutorFloat(ctx map[string]interface{}, key string) (float64, bool) {
	if ctx == nil {
		return 0, false
	}
	val, ok := ctx[key]
	if !ok {
		return 0, false
	}
	switch v := val.(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case string:
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f, true
		}
	}
	return 0, false
}
