package freqtrade

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"brale/internal/decision"
	"brale/internal/gateway/database"
	"brale/internal/logger"
	symbolpkg "brale/internal/pkg/symbol"
	"brale/internal/strategy/exit"
	exithandlers "brale/internal/strategy/exit/handlers"
)

const openPlanCacheTTL = 30 * time.Minute

type cachedOpenPlan struct {
	TraceID         string
	Symbol          string
	Side            string
	Plan            *decision.ExitPlanSpec
	ExitPlanVersion int
	Decision        decision.Decision
	CachedAt        time.Time
}

func (m *Manager) cacheOpenExitPlan(traceID string, d decision.Decision) {
	if m == nil {
		return
	}
	action := strings.ToLower(strings.TrimSpace(d.Action))
	if action != "open_long" && action != "open_short" {
		return
	}
	if d.ExitPlan == nil || strings.TrimSpace(d.ExitPlan.ID) == "" {
		return
	}
	symbol := normalizePlanSymbol(d.Symbol)
	if symbol == "" {
		return
	}
	plan := cloneExitPlanSpec(d.ExitPlan)
	if plan == nil || strings.TrimSpace(plan.ID) == "" {
		return
	}
	side := deriveOpenSide(action)
	decisionCopy := d
	decisionCopy.ExitPlan = plan

	now := time.Now()
	entry := cachedOpenPlan{
		TraceID:         strings.TrimSpace(traceID),
		Symbol:          symbol,
		Side:            side,
		Plan:            plan,
		ExitPlanVersion: d.ExitPlanVersion,
		Decision:        decisionCopy,
		CachedAt:        now,
	}

	m.openPlanMu.Lock()
	defer m.openPlanMu.Unlock()
	if m.openPlanCache == nil {
		m.openPlanCache = make(map[string]cachedOpenPlan)
	}
	cleanupOpenPlanCacheLocked(m.openPlanCache, now)
	m.openPlanCache[symbol] = entry
}

func (m *Manager) takeCachedOpenPlan(symbol string) (cachedOpenPlan, bool) {
	if m == nil {
		return cachedOpenPlan{}, false
	}
	key := normalizePlanSymbol(symbol)
	if key == "" {
		return cachedOpenPlan{}, false
	}
	now := time.Now()
	m.openPlanMu.Lock()
	defer m.openPlanMu.Unlock()
	if m.openPlanCache == nil {
		return cachedOpenPlan{}, false
	}
	cleanupOpenPlanCacheLocked(m.openPlanCache, now)
	entry, ok := m.openPlanCache[key]
	if !ok {
		return cachedOpenPlan{}, false
	}
	delete(m.openPlanCache, key)
	return entry, true
}

func (m *Manager) restoreCachedOpenPlan(symbol string, entry cachedOpenPlan) {
	if m == nil {
		return
	}
	key := normalizePlanSymbol(symbol)
	if key == "" {
		return
	}
	now := time.Now()
	m.openPlanMu.Lock()
	defer m.openPlanMu.Unlock()
	if m.openPlanCache == nil {
		m.openPlanCache = make(map[string]cachedOpenPlan)
	}
	cleanupOpenPlanCacheLocked(m.openPlanCache, now)
	if _, exists := m.openPlanCache[key]; exists {
		return
	}
	m.openPlanCache[key] = entry
}

func cleanupOpenPlanCacheLocked(cache map[string]cachedOpenPlan, now time.Time) {
	if len(cache) == 0 {
		return
	}
	cutoff := now.Add(-openPlanCacheTTL)
	for k, v := range cache {
		if v.CachedAt.IsZero() || v.CachedAt.Before(cutoff) {
			delete(cache, k)
		}
	}
}

func normalizePlanSymbol(raw string) string {
	if norm := symbolpkg.Normalize(raw); norm != "" {
		return norm
	}
	return strings.ToUpper(strings.TrimSpace(raw))
}

func deriveOpenSide(action string) string {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "open_long":
		return "long"
	case "open_short":
		return "short"
	default:
		return ""
	}
}

func cloneExitPlanSpec(spec *decision.ExitPlanSpec) *decision.ExitPlanSpec {
	if spec == nil {
		return nil
	}
	data, err := json.Marshal(spec)
	if err != nil {
		return nil
	}
	var out decision.ExitPlanSpec
	if err := json.Unmarshal(data, &out); err != nil {
		return nil
	}
	return &out
}

func (m *Manager) initExitPlanOnEntryFill(ctx context.Context, tradeID int, symbol string, entryPrice float64) {
	if m == nil || m.posStore == nil || tradeID <= 0 {
		return
	}
	baseCtx := context.Background()
	if m.exitPlanAlreadyInitialized(baseCtx, tradeID) {
		return
	}
	keySymbol := normalizePlanSymbol(symbol)
	entry, ok := m.takeCachedOpenPlan(keySymbol)
	if !ok || entry.Plan == nil {
		return
	}
	planID := strings.TrimSpace(entry.Plan.ID)
	if planID == "" {
		return
	}

	args := buildComboInstantiateArgs(tradeID, keySymbol, entry, entryPrice, m.lookupEntryPrice)
	if args.PlanSpec == nil {
		args.PlanSpec = map[string]any{}
	}

	handler := comboGroupHandler()
	if handler == nil {
		m.restoreCachedOpenPlan(keySymbol, entry)
		logger.Warnf("initExitPlanOnEntryFill: handler combo_group 未注册 trade=%d symbol=%s", tradeID, keySymbol)
		return
	}

	workCtx, cancel := context.WithTimeout(baseCtx, 8*time.Second)
	defer cancel()
	records, err := instantiateComboPlanRecords(workCtx, handler, planID, args)
	if err != nil {
		m.restoreCachedOpenPlan(keySymbol, entry)
		logger.Warnf("initExitPlanOnEntryFill: instantiate 失败 trade=%d symbol=%s plan=%s err=%v", tradeID, keySymbol, planID, err)
		return
	}
	if len(records) == 0 {
		return
	}
	if err := m.posStore.InsertStrategyInstances(workCtx, records); err != nil {
		m.restoreCachedOpenPlan(keySymbol, entry)
		logger.Warnf("initExitPlanOnEntryFill: insert 失败 trade=%d symbol=%s plan=%s err=%v", tradeID, keySymbol, planID, err)
		return
	}

	m.logPlanInit(workCtx, tradeID, planID, entry.TraceID, "entry_fill")
	_ = m.SyncStrategyPlans(workCtx, tradeID, buildPlanSnapshots(records))
	if m.planUpdateHook != nil {
		m.planUpdateHook.NotifyPlanUpdated(baseCtx, tradeID)
	}
}

func (m *Manager) exitPlanAlreadyInitialized(ctx context.Context, tradeID int) bool {
	checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	recs, err := m.posStore.ListStrategyInstances(checkCtx, tradeID)
	cancel()
	return err == nil && len(recs) > 0
}

type entryPriceLookupFn func(context.Context, int, string) float64

func buildComboInstantiateArgs(tradeID int, symbol string, entry cachedOpenPlan, entryPrice float64, lookup entryPriceLookupFn) exit.InstantiateArgs {
	planVersion := entry.ExitPlanVersion
	if planVersion <= 0 {
		planVersion = 1
	}
	side := strings.ToLower(strings.TrimSpace(entry.Side))
	if side == "" {
		side = deriveOpenSide(entry.Decision.Action)
	}
	if entryPrice <= 0 && lookup != nil {
		entryPrice = lookup(context.Background(), tradeID, symbol)
	}
	spec := entry.Plan.Params
	if spec == nil {
		spec = map[string]any{}
	}
	return exit.InstantiateArgs{
		TradeID:       tradeID,
		PlanVersion:   planVersion,
		PlanSpec:      spec,
		Decision:      entry.Decision,
		EntryPrice:    entryPrice,
		Side:          side,
		Symbol:        symbol,
		DecisionTrace: entry.TraceID,
	}
}

func comboGroupHandler() exit.PlanHandler {
	reg := exit.NewHandlerRegistry()
	exithandlers.RegisterCoreHandlers(reg)
	handler, _ := reg.Handler("combo_group")
	return handler
}

func instantiateComboPlanRecords(ctx context.Context, handler exit.PlanHandler, planID string, args exit.InstantiateArgs) ([]database.StrategyInstanceRecord, error) {
	if handler == nil {
		return nil, fmt.Errorf("handler missing")
	}
	args.PlanID = planID
	insts, err := handler.Instantiate(ctx, args)
	if err != nil {
		return nil, err
	}
	if len(insts) == 0 {
		return nil, nil
	}
	records := make([]database.StrategyInstanceRecord, 0, len(insts))
	for _, inst := range insts {
		records = append(records, inst.Record)
	}
	return records, nil
}
