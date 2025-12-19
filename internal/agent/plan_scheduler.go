package agent

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"brale/internal/agent/interfaces"
	"brale/internal/agent/ports"
	"brale/internal/decision"
	"brale/internal/exitplan"
	"brale/internal/gateway/database"
	"brale/internal/gateway/exchange"
	"brale/internal/gateway/notifier"
	"brale/internal/logger"
	"brale/internal/pkg/utils"
	"brale/internal/strategy/exit"

	"github.com/shopspring/decimal"
)

const (
	defaultPlanRefreshInterval = 5 * time.Second
	planPriceBufferSize        = 1024
	priceDebounceInterval      = 1 * time.Second

	defaultStrategyPendingTimeout      = 12 * time.Minute
	defaultStrategyPendingSweepInterval = 1 * time.Minute

	defaultInactiveTradeSweepInterval = 10 * time.Second
	defaultInactiveTradeMissThreshold = 2
)

type PlanSchedulerParams struct {
	Store           exit.StrategyStore
	Plans           *exitplan.Registry
	Handlers        *exit.HandlerRegistry
	ExecManager     ports.ExecutionManager
	Notifier        notifier.TextNotifier
	RefreshInterval time.Duration
	PendingTimeout  time.Duration
	PendingSweep    time.Duration
	DisableDebounce bool
}

var _ exchange.PlanUpdateHook = (*PlanScheduler)(nil)

// PlanScheduler manages the lifecycle of Exit Strategies (Plans) for all active trades.
//
// Responsibilities:
// - Monitors real-time price ticks from the Market.
// - Triggers plan execution when conditions are met (via PlanExecutor).
// - Handles dynamic updates to plans (from LLM or Admin).
// - Ensures persistence consistency by rebuilding state from DB.
//
// Concurrency:
// - Uses a dedicated priceLoop goroutine to process ticks sequentially per symbol.
// - Uses read/write locks (mu) to protect the in-memory index of active watchers.
type PlanScheduler struct {
	repo        *PlanRepository
	executor    *PlanExecutor
	execManager ports.ExecutionManager
	notifier    notifier.TextNotifier

	interval        time.Duration
	pendingTimeout  time.Duration
	pendingSweep    time.Duration
	startOnce       sync.Once
	priceCh         chan priceTick
	mu              sync.RWMutex
	symbolIndex     map[string][]*planWatcher
	tradeIndex      map[int][]*planWatcher
	pruneMisses     map[int]int
	disableDebounce bool

	lastPriceMu   sync.Mutex
	lastPriceTime map[string]time.Time
}

type priceTick struct {
	symbol string
	price  float64
}

func NewPlanScheduler(params PlanSchedulerParams) *PlanScheduler {
	if params.Store == nil || params.Handlers == nil || params.Plans == nil {
		return nil
	}
	interval := params.RefreshInterval
	if interval <= 0 {
		interval = defaultPlanRefreshInterval
	}
	pendingTimeout := params.PendingTimeout
	if pendingTimeout <= 0 {
		pendingTimeout = defaultStrategyPendingTimeout
	}
	pendingSweep := params.PendingSweep
	if pendingSweep <= 0 {
		pendingSweep = defaultStrategyPendingSweepInterval
	}
	repo := NewPlanRepository(params.Store, params.Plans, params.Handlers)
	s := &PlanScheduler{
		repo:            repo,
		execManager:     params.ExecManager,
		notifier:        params.Notifier,
		interval:        interval,
		pendingTimeout:  pendingTimeout,
		pendingSweep:    pendingSweep,
		priceCh:         make(chan priceTick, planPriceBufferSize),
		symbolIndex:     make(map[string][]*planWatcher),
		tradeIndex:      make(map[int][]*planWatcher),
		pruneMisses:     make(map[int]int),
		lastPriceTime:   make(map[string]time.Time),
		disableDebounce: params.DisableDebounce,
	}

	s.executor = NewPlanExecutor(repo, params.ExecManager, s.rebuildTrade)
	return s
}

func (s *PlanScheduler) Start(ctx context.Context) {
	if s == nil {
		return
	}
	s.startOnce.Do(func() {
		go s.refreshLoop(ctx)
		go s.pendingLoop(ctx)
		go s.pruneLoop(ctx)
		go s.priceLoop(ctx)
	})
}

func (s *PlanScheduler) NotifyPrice(symbol string, price float64) {
	if s == nil || price <= 0 {
		return
	}
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	if symbol == "" {
		return
	}

	if !s.disableDebounce {
		s.lastPriceMu.Lock()
		lastTime, exists := s.lastPriceTime[symbol]
		if exists && time.Since(lastTime) < priceDebounceInterval {
			s.lastPriceMu.Unlock()
			return
		}
		s.lastPriceTime[symbol] = time.Now()
		s.lastPriceMu.Unlock()
	}

	select {
	case s.priceCh <- priceTick{symbol: symbol, price: price}:
	default:
	}
}

func (s *PlanScheduler) refreshLoop(ctx context.Context) {
	if s == nil {
		return
	}

	s.rebuild(ctx)
	<-ctx.Done()
}

func (s *PlanScheduler) pruneLoop(ctx context.Context) {
	if s == nil {
		return
	}
	interval := s.interval
	if interval <= 0 {
		interval = defaultInactiveTradeSweepInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.pruneInactiveTrades(ctx, defaultInactiveTradeMissThreshold)
		}
	}
}

func (s *PlanScheduler) pruneInactiveTrades(ctx context.Context, missThreshold int) {
	if s == nil || s.repo == nil {
		return
	}
	ctx = ensureContext(ctx)
	missThreshold = normalizeMissThreshold(missThreshold)

	active, ok := s.loadActiveTradeSet(ctx)
	if !ok {
		return
	}

	missingActive := s.pruneInactiveLocked(active, missThreshold)
	for _, id := range missingActive {
		s.rebuildTrade(ctx, id)
	}
}

func ensureContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func normalizeMissThreshold(v int) int {
	if v <= 0 {
		return defaultInactiveTradeMissThreshold
	}
	return v
}

func (s *PlanScheduler) loadActiveTradeSet(ctx context.Context) (map[int]struct{}, bool) {
	activeIDs, err := s.repo.ActiveTradeIDs(ctx)
	if err != nil {
		logger.Warnf("PlanScheduler: 查询活跃策略失败(prune): %v", err)
		return nil, false
	}
	active := make(map[int]struct{}, len(activeIDs))
	for _, id := range activeIDs {
		if id > 0 {
			active[id] = struct{}{}
		}
	}
	return active, true
}

func (s *PlanScheduler) pruneInactiveLocked(active map[int]struct{}, missThreshold int) []int {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.pruneMisses == nil {
		s.pruneMisses = make(map[int]int)
	}

	if s.tradeIndex == nil || len(s.tradeIndex) == 0 {
		if len(s.pruneMisses) > 0 {
			s.pruneMisses = make(map[int]int)
		}
		return keysOfActive(active)
	}

	for tradeID := range s.tradeIndex {
		if _, ok := active[tradeID]; ok {
			delete(s.pruneMisses, tradeID)
			continue
		}
		next := s.pruneMisses[tradeID] + 1
		if next >= missThreshold {
			s.removeTradeLocked(tradeID)
			delete(s.pruneMisses, tradeID)
			logger.Infof("PlanScheduler: prune inactive trade watchers trade=%d", tradeID)
			continue
		}
		s.pruneMisses[tradeID] = next
	}

	for tradeID := range s.pruneMisses {
		if _, ok := s.tradeIndex[tradeID]; !ok {
			delete(s.pruneMisses, tradeID)
		}
	}

	return missingActiveTrades(active, s.tradeIndex)
}

func keysOfActive(active map[int]struct{}) []int {
	if len(active) == 0 {
		return nil
	}
	out := make([]int, 0, len(active))
	for id := range active {
		out = append(out, id)
	}
	return out
}

func missingActiveTrades(active map[int]struct{}, tradeIndex map[int][]*planWatcher) []int {
	if len(active) == 0 {
		return nil
	}
	var out []int
	for id := range active {
		if _, ok := tradeIndex[id]; !ok {
			out = append(out, id)
		}
	}
	return out
}

func (s *PlanScheduler) pendingLoop(ctx context.Context) {
	if s == nil {
		return
	}
	interval := s.pendingSweep
	if interval <= 0 {
		interval = defaultStrategyPendingSweepInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.clearStalePending(ctx)
		}
	}
}

func (s *PlanScheduler) clearStalePending(ctx context.Context) {
	if s == nil || s.repo == nil || s.pendingTimeout <= 0 {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ids, err := s.repo.ActiveTradeIDs(ctx)
	if err != nil {
		logger.Warnf("PlanScheduler: 查询活跃策略失败(pending sweep): %v", err)
		return
	}
	now := time.Now()
	var dirtyTrades []int
	for _, tradeID := range ids {
		recs, err := s.repo.ListStrategyInstances(ctx, tradeID)
		if err != nil {
			logger.Warnf("PlanScheduler: pending sweep 读取策略实例失败 trade=%d err=%v", tradeID, err)
			continue
		}
		updated := false
		for _, rec := range recs {
			if rec.Status != database.StrategyStatusPending {
				continue
			}
			pendingAt := pendingTimestamp(rec)
			if pendingAt.IsZero() || now.Sub(pendingAt) <= s.pendingTimeout {
				continue
			}
			newState := strings.TrimSpace(rec.StateJSON)
			if strings.TrimSpace(rec.PlanComponent) == "" {
				if st, err := exit.DecodeTierPlanState(newState); err == nil {
					st.PendingSince = 0
					st.PendingOrderID = ""
					st.PendingEvent = ""
					newState = exit.EncodeTierPlanState(st)
				}
			} else {
				if st, err := exit.DecodeTierComponentState(newState); err == nil {
					st.Status = "waiting"
					st.PendingSince = 0
					st.PendingOrderID = ""
					st.TriggeredAt = 0
					st.TriggerPrice = 0
					st.LastEvent = "pending_timeout"
					newState = exit.EncodeTierComponentState(st)
				}
			}
			inst := &exit.PlanInstance{Record: rec}
			if ok := s.repo.PersistPlanState(ctx, inst, newState, database.StrategyStatusWaiting); !ok {
				continue
			}
			updated = true
			logger.Warnf("PlanScheduler: 已清理超时 pending trade=%d plan=%s component=%s pending_at=%s",
				tradeID, strings.TrimSpace(rec.PlanID), strings.TrimSpace(rec.PlanComponent), pendingAt.Format(time.RFC3339))
		}
		if updated {
			dirtyTrades = append(dirtyTrades, tradeID)
		}
	}
	for _, tradeID := range dirtyTrades {
		s.rebuildTrade(ctx, tradeID)
	}
}

func pendingTimestamp(rec database.StrategyInstanceRecord) time.Time {
	component := strings.TrimSpace(rec.PlanComponent)
	stateJSON := strings.TrimSpace(rec.StateJSON)
	switch {
	case component == "":
		if st, err := exit.DecodeTierPlanState(stateJSON); err == nil && st.PendingSince > 0 {
			return time.Unix(st.PendingSince, 0)
		}
	default:
		if st, err := exit.DecodeTierComponentState(stateJSON); err == nil && st.PendingSince > 0 {
			return time.Unix(st.PendingSince, 0)
		}
	}
	if !rec.UpdatedAt.IsZero() {
		return rec.UpdatedAt
	}
	return time.Time{}
}

// priceLoop consumes price ticks from the channel and dispatches them to relevant watchers.
//
// Performance:
//   - Ticks are already debounced by NotifyPrice (buffer + time window).
//   - We use a read lock (RLock) to quickly find watchers for the symbol.
//   - Execution of the plan logic happens in s.executor.EvaluateWatcher (synchronously in this loop).
//     This ensures that for a single symbol, plan evaluation is sequential and deterministic.
func (s *PlanScheduler) priceLoop(ctx context.Context) {
	if s == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case tick := <-s.priceCh:
			s.handlePriceTick(ctx, tick)
		}
	}
}

func (s *PlanScheduler) NotifyPlanUpdated(ctx context.Context, tradeID int) {
	if s == nil {
		return
	}

	go s.rebuildTrade(context.Background(), tradeID)
}

func (s *PlanScheduler) rebuild(ctx context.Context) {
	if s.repo == nil {
		return
	}
	ids, err := s.repo.ActiveTradeIDs(ctx)
	if err != nil {
		logger.Warnf("PlanScheduler: 查询活跃策略失败: %v", err)
		return
	}
	newSymbol := make(map[string][]*planWatcher)
	newTrade := make(map[int][]*planWatcher)
	for _, tradeID := range ids {
		recs, err := s.repo.ListStrategyInstances(ctx, tradeID)
		if err != nil {
			logger.Warnf("PlanScheduler: 加载 strategy_instances 失败 trade=%d err=%v", tradeID, err)
			continue
		}
		s.publishPlanSnapshots(tradeID, recs)
		watchers := s.repo.BuildWatchers(recs)
		for _, w := range watchers {
			newSymbol[w.symbol] = append(newSymbol[w.symbol], w)
			newTrade[tradeID] = append(newTrade[tradeID], w)
		}
	}
	s.mu.Lock()
	s.symbolIndex = newSymbol
	s.tradeIndex = newTrade
	s.mu.Unlock()
}

// rebuildTrade refreshes the in-memory watchers for a specific trade.
// Called when a plan is updated (AdjustPlan) or initialized.
//
// Logic:
// 1. Fetches all strategy instances for the trade from DB.
// 2. Checks if all strategies are "Done". If so, and trade is closed, removes it from memory.
// 3. If active strategies exist, rebuilds the "watchers" (indexes).
// 4. Updates symbolIndex and tradeIndex atomically.
func (s *PlanScheduler) rebuildTrade(ctx context.Context, tradeID int) {
	if s.repo == nil {
		return
	}
	if tradeID <= 0 {
		s.rebuild(ctx)
		return
	}
	recs, err := s.repo.ListStrategyInstances(ctx, tradeID)
	if err != nil {
		logger.Warnf("PlanScheduler: 局部刷新失败 trade=%d err=%v", tradeID, err)
		return
	}
	s.publishPlanSnapshots(tradeID, recs)

	allDone := true
	for _, rec := range recs {
		if rec.Status != database.StrategyStatusDone {
			allDone = false
			break
		}
	}
	if !allDone {

		if ids, err := s.repo.ActiveTradeIDs(ctx); err == nil {
			active := false
			for _, id := range ids {
				if id == tradeID {
					active = true
					break
				}
			}
			if !active {
				allDone = true
			}
		}
	}

	watchers := []*planWatcher(nil)
	if !allDone {
		watchers = s.repo.BuildWatchers(recs)
	}
	s.mu.Lock()
	if s.symbolIndex == nil {
		s.symbolIndex = make(map[string][]*planWatcher)
	}
	if s.tradeIndex == nil {
		s.tradeIndex = make(map[int][]*planWatcher)
	}
	s.removeTradeLocked(tradeID)
	if len(watchers) > 0 {
		s.tradeIndex[tradeID] = watchers
		for _, w := range watchers {
			if w == nil {
				continue
			}
			s.symbolIndex[w.symbol] = append(s.symbolIndex[w.symbol], w)
		}
	}
	s.mu.Unlock()
}

func (s *PlanScheduler) handlePriceTick(ctx context.Context, tick priceTick) {
	s.mu.RLock()
	watchers := append([]*planWatcher(nil), s.symbolIndex[tick.symbol]...)
	s.mu.RUnlock()
	if len(watchers) == 0 {
		return
	}
	if s.executor == nil {
		return
	}
	for _, watcher := range watchers {
		s.executor.EvaluateWatcher(ctx, watcher, tick.price)
	}
}

func (s *PlanScheduler) removeTradeLocked(tradeID int) {
	if s.tradeIndex == nil {
		return
	}
	delete(s.tradeIndex, tradeID)
	if len(s.symbolIndex) == 0 {
		return
	}
	for symbol, list := range s.symbolIndex {
		if len(list) == 0 {
			continue
		}
		filtered := list[:0]
		for _, w := range list {
			if w == nil || w.tradeID != tradeID {
				filtered = append(filtered, w)
			}
		}
		if len(filtered) == 0 {
			delete(s.symbolIndex, symbol)
		} else {
			s.symbolIndex[symbol] = filtered
		}
	}
}

func (s *PlanScheduler) publishPlanSnapshots(tradeID int, recs []database.StrategyInstanceRecord) {
	if s == nil || s.execManager == nil || tradeID <= 0 || len(recs) == 0 {
		return
	}
	snapshots := buildPlanSnapshots(recs)
	if len(snapshots) == 0 {
		return
	}
	go func() {
		if err := s.execManager.SyncStrategyPlans(context.Background(), tradeID, snapshots); err != nil {
			logger.Warnf("PlanScheduler: SyncStrategyPlans failed trade=%d err=%v", tradeID, err)
		}
	}()
}

// AdjustPlan handles a request to modify an active strategy's parameters.
// This is used for manual overrides (Admin UI) or dynamic LLM adjustments.
//
// Logic:
// 1. Locates the target plan instance in DB.
// 2. Validates the existence of the handler.
// 3. Calls the handler's OnAdjust method to calculate the new state/event.
//   - e.g., "Trailing Stop" handler might update its activation price.
//
// 4. Persists the new state.
// 5. Rebuilds the in-memory watchers for consistency.
func (s *PlanScheduler) AdjustPlan(ctx context.Context, req interfaces.PlanAdjustSpec) error {
	if s == nil {
		return fmt.Errorf("plan scheduler 未初始化")
	}
	planID := strings.TrimSpace(req.PlanID)
	if req.TradeID <= 0 || planID == "" {
		return fmt.Errorf("trade_id 与 plan_id 必填")
	}

	recs, err := s.repo.ListStrategyInstances(ctx, req.TradeID)
	if err != nil {
		return fmt.Errorf("读取策略实例失败: %w", err)
	}

	var targetRecs []database.StrategyInstanceRecord
	for _, rec := range recs {
		if strings.TrimSpace(rec.PlanID) == planID {
			targetRecs = append(targetRecs, rec)
		}
	}
	if len(targetRecs) == 0 {
		return fmt.Errorf("未找到 plan: %s", planID)
	}

	handler := s.repo.LookupHandler(planID)
	if handler == nil {
		return fmt.Errorf("handler 未注册: %s", planID)
	}

	watcher := s.repo.BuildWatcher(planID, handler, targetRecs)
	if watcher == nil {
		return fmt.Errorf("plan 记录异常: %s", planID)
	}

	reason, err := s.executor.HandleAdjust(ctx, watcher, req.Component, req.Params, req.Source)
	if err != nil {
		return err
	}
	if s.notifier != nil && strings.TrimSpace(reason) != "" {
		comp := strings.TrimSpace(req.Component)
		if comp == "" {
			comp = "ROOT"
		}
		msg := fmt.Sprintf("🛠 策略调整：%s (TradeID %d)\nPlan %s · Component %s\n来源: %s\n\n%s",
			watcher.symbol, req.TradeID, planID, comp, strings.TrimSpace(req.Source), strings.TrimSpace(reason))
		if err := s.notifier.SendText(msg); err != nil {
			logger.Warnf("Telegram 推送失败(plan_adjust): %v", err)
		}
	}

	s.rebuildTrade(ctx, req.TradeID)
	return nil
}

// ProcessUpdateDecision processes an "update_exit_plan" decision from the LLM.
//
// Logic:
// 1. Validates the decision contains an ExitPlan spec.
// 2. Finds the active TradeID for the symbol.
// 3. Dispatches to AdjustPlan (or specific combo logic).
//   - This bridges the gap between LLM's high-level intent and low-level strategy state.
func (s *PlanScheduler) ProcessUpdateDecision(ctx context.Context, traceID string, d decision.Decision) error {
	if s == nil {
		return fmt.Errorf("plan scheduler 未初始化")
	}
	if d.ExitPlan == nil || strings.TrimSpace(d.ExitPlan.ID) == "" {
		return fmt.Errorf("缺少 exit_plan")
	}

	var tradeID int
	s.mu.RLock()
	watchers := s.symbolIndex[strings.ToUpper(d.Symbol)]
	if len(watchers) > 0 {

		tradeID = watchers[0].tradeID
	}
	s.mu.RUnlock()

	if tradeID <= 0 {
		return fmt.Errorf("未找到 symbol=%s 的活跃策略", d.Symbol)
	}

	planID := strings.TrimSpace(d.ExitPlan.ID)
	adjustSource := fmt.Sprintf("llm:update_exit_plan:%s", strings.TrimSpace(traceID))

	switch planID {
	case "plan_combo_main":
		if err := s.applyComboAdjustments(ctx, tradeID, planID, d.ExitPlan.Params, adjustSource); err != nil {
			return err
		}
	default:
		if err := s.AdjustPlan(ctx, interfaces.PlanAdjustSpec{
			TradeID: tradeID,
			PlanID:  planID,
			Params:  cloneMapAny(d.ExitPlan.Params),
			Source:  adjustSource,
		}); err != nil {
			return err
		}

		return nil
	}
	logger.Infof("update_exit_plan 成功: symbol=%s plan=%s trade=%d", strings.ToUpper(strings.TrimSpace(d.Symbol)), planID, tradeID)
	return nil
}

func (s *PlanScheduler) applyComboAdjustments(ctx context.Context, tradeID int, planID string, params map[string]any, source string) error {
	childrenRaw, _ := params["children"].([]any)
	if len(childrenRaw) == 0 {
		return fmt.Errorf("combo plan 缺少 children")
	}
	index, err := s.buildPlanComponentIndex(ctx, tradeID, planID)
	if err != nil {
		return err
	}
	for _, raw := range childrenRaw {
		child, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		alias := strings.TrimSpace(utils.AsString(child["component"]))
		if alias == "" {
			continue
		}
		handler := strings.TrimSpace(utils.AsString(child["handler"]))
		childParams, _ := child["params"].(map[string]any)
		switch handler {
		case "tier_take_profit", "tier_stop_loss":
			if err := s.adjustTierLevelsPlan(ctx, tradeID, planID, alias, childParams, source, index); err != nil {
				return err
			}
		case "atr_trailing":
			if err := s.adjustATRComponent(ctx, tradeID, planID, alias, childParams, source); err != nil {
				return err
			}
		default:
			logger.Warnf("update_exit_plan: 组件 %s handler=%s 暂未支持", alias, handler)
		}
	}
	return nil
}

func (s *PlanScheduler) adjustATRComponent(ctx context.Context, tradeID int, planID, alias string, params map[string]any, source string) error {
	if alias == "" {
		return fmt.Errorf("atr 组件缺少 component")
	}
	if params == nil {
		return fmt.Errorf("atr 组件缺少 params")
	}
	update := cloneMapAny(params)
	return s.AdjustPlan(ctx, interfaces.PlanAdjustSpec{
		TradeID:   tradeID,
		PlanID:    planID,
		Component: alias,
		Params:    update,
		Source:    source,
	})
}

func (s *PlanScheduler) adjustTierLevelsPlan(ctx context.Context, tradeID int, planID, alias string, params map[string]any, source string, index map[string][]database.StrategyInstanceRecord) error {
	if alias == "" {
		return fmt.Errorf("tier 组件缺少 component")
	}
	tierStates, err := tierComponentStates(index[alias])
	if err != nil {
		return err
	}
	waiting := filterWaitingTiers(tierStates)
	if len(waiting) == 0 {
		logger.Warnf("%s 无可调整段位 (all triggered?)", alias)
		return nil
	}
	rawTiers, _ := params["tiers"].([]any)
	if len(rawTiers) == 0 {
		return fmt.Errorf("%s 缺少 tiers 参数", alias)
	}
	if len(rawTiers) != len(tierStates) {
		return fmt.Errorf("%s tiers 数量应为 %d（全部段位，包括已触发），当前=%d", alias, len(tierStates), len(rawTiers))
	}
	newSum := decimal.Zero
	updates := make([]map[string]any, len(rawTiers))
	for i, entry := range rawTiers {
		tierMap, ok := entry.(map[string]any)
		if !ok {
			return fmt.Errorf("%s tier#%d 参数格式错误", alias, i+1)
		}
		update, ratioContribution, err := buildTierUpdate(alias, i, tierMap, tierStates[i])
		if err != nil {
			return err
		}
		updates[i] = update
		if ratioContribution != 0 {
			newSum = newSum.Add(decimal.NewFromFloat(ratioContribution))
		}
	}
	oldSum := decimal.Zero
	for _, info := range waiting {
		oldSum = oldSum.Add(decimal.NewFromFloat(info.Remaining))
	}
	tolerance := decimal.NewFromFloat(1e-6)
	if oldSum.Sub(newSum).Abs().GreaterThan(tolerance) {
		return fmt.Errorf("%s 比例和应为 %s，当前=%s", alias, oldSum.String(), newSum.String())
	}
	for _, info := range waiting {
		if err := s.AdjustPlan(ctx, interfaces.PlanAdjustSpec{
			TradeID:   tradeID,
			PlanID:    planID,
			Component: info.Component,
			Params:    updates[info.Index],
			Source:    source,
		}); err != nil {
			return fmt.Errorf("%s: 调整 %s 失败: %w", alias, info.Component, err)
		}
	}
	return nil
}

func buildTierUpdate(alias string, idx int, tierMap map[string]any, state tierComponentState) (map[string]any, float64, error) {
	update := make(map[string]any)
	if price, ok := tierMap["target_price"]; ok {
		update["target_price"] = price
	} else if price, ok := tierMap["target"]; ok {
		update["target"] = price
	} else {
		return nil, 0, fmt.Errorf("%s tier#%d 缺少 target_price", alias, idx+1)
	}

	ratioContribution := 0.0
	if ratio, ok := tierMap["ratio"]; ok {
		update["ratio"] = ratio
	}
	if state.Status == database.StrategyStatusWaiting {
		if ratio, ok := tierMap["ratio"]; ok {
			if val, ok := utils.AsFloat(ratio); ok {
				ratioContribution = val
			}
		} else {
			ratioContribution = state.Remaining
		}
	} else {
		if err := ensureTriggeredRatioImmutable(alias, state, update); err != nil {
			return nil, 0, err
		}
	}
	return update, ratioContribution, nil
}

func ensureTriggeredRatioImmutable(alias string, state tierComponentState, update map[string]any) error {
	ratio, _ := utils.AsFloat(update["ratio"])
	if ratio == 0 {
		ratio = state.Ratio
	}
	const tol = 1e-9
	if ratio != 0 && math.Abs(ratio-state.Ratio) > tol {
		return fmt.Errorf("%s 已触发段 %s 的 ratio 不可修改", alias, state.Component)
	}
	return nil
}

func (s *PlanScheduler) buildPlanComponentIndex(ctx context.Context, tradeID int, planID string) (map[string][]database.StrategyInstanceRecord, error) {

	recs, err := s.repo.ListStrategyInstances(ctx, tradeID)
	if err != nil {
		return nil, fmt.Errorf("读取策略实例失败: %w", err)
	}
	index := make(map[string][]database.StrategyInstanceRecord)
	for _, rec := range recs {
		if strings.TrimSpace(rec.PlanID) != planID {
			continue
		}
		alias := componentAlias(rec.PlanComponent)
		index[alias] = append(index[alias], rec)
	}
	return index, nil
}

func cloneMapAny(src map[string]any) map[string]any {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]any, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

type tierComponentInfo struct {
	Component string
	Index     int
	Remaining float64
}

func waitingTierComponents(recs []database.StrategyInstanceRecord) ([]tierComponentInfo, error) {
	if len(recs) == 0 {
		return nil, nil
	}
	waiting := make([]tierComponentInfo, 0, len(recs))
	for _, rec := range recs {
		if !strings.Contains(rec.PlanComponent, ".tier") {
			continue
		}
		if rec.Status != database.StrategyStatusWaiting {
			continue
		}
		state, err := exit.DecodeTierComponentState(rec.StateJSON)
		if err != nil {
			return nil, fmt.Errorf("解析组件 %s 状态失败: %w", rec.PlanComponent, err)
		}
		waiting = append(waiting, tierComponentInfo{
			Component: strings.TrimSpace(rec.PlanComponent),
			Remaining: state.RemainingRatio,
		})
	}
	sort.Slice(waiting, func(i, j int) bool { return waiting[i].Component < waiting[j].Component })
	return waiting, nil
}

type tierComponentState struct {
	Component string
	Status    database.StrategyStatus
	Target    float64
	Ratio     float64
	Remaining float64
}

func tierComponentStates(recs []database.StrategyInstanceRecord) ([]tierComponentState, error) {
	if len(recs) == 0 {
		return nil, nil
	}
	out := make([]tierComponentState, 0, len(recs))
	for _, rec := range recs {
		if !strings.Contains(rec.PlanComponent, ".tier") {
			continue
		}
		state, err := exit.DecodeTierComponentState(rec.StateJSON)
		if err != nil {
			return nil, fmt.Errorf("解析组件 %s 状态失败: %w", rec.PlanComponent, err)
		}
		out = append(out, tierComponentState{
			Component: strings.TrimSpace(rec.PlanComponent),
			Status:    rec.Status,
			Target:    state.TargetPrice,
			Ratio:     state.Ratio,
			Remaining: state.RemainingRatio,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Component < out[j].Component })
	return out, nil
}

func filterWaitingTiers(list []tierComponentState) []tierComponentInfo {
	waiting := make([]tierComponentInfo, 0, len(list))
	for i, t := range list {
		if t.Status != database.StrategyStatusWaiting {
			continue
		}
		waiting = append(waiting, tierComponentInfo{
			Component: t.Component,
			Index:     i,
			Remaining: t.Remaining,
		})
	}
	return waiting
}

func componentAlias(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if idx := strings.Index(name, "."); idx != -1 {
		return name[:idx]
	}
	return name
}
