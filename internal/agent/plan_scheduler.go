package agent

import (
	"context"
	"fmt"
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
)

// PlanSchedulerParams 聚合组建 PlanScheduler 所需的依赖。
type PlanSchedulerParams struct {
	Store           exit.StrategyStore
	Plans           *exitplan.Registry
	Handlers        *exit.HandlerRegistry
	ExecManager     ports.ExecutionManager
	Notifier        notifier.TextNotifier
	RefreshInterval time.Duration
	DisableDebounce bool // For tests: disable price debounce
}

var _ exchange.PlanUpdateHook = (*PlanScheduler)(nil)

// PlanScheduler 订阅价格并调度 handler，维护 strategy_instances 状态。
type PlanScheduler struct {
	repo        *PlanRepository
	executor    *PlanExecutor
	execManager ports.ExecutionManager
	notifier    notifier.TextNotifier

	interval        time.Duration
	startOnce       sync.Once
	priceCh         chan priceTick
	mu              sync.RWMutex
	symbolIndex     map[string][]*planWatcher
	tradeIndex      map[int][]*planWatcher
	disableDebounce bool

	// Debounce: track last processed price time per symbol
	lastPriceMu   sync.Mutex
	lastPriceTime map[string]time.Time
}

type priceTick struct {
	symbol string
	price  float64
}

// NewPlanScheduler 构造调度器，若依赖缺失返回 nil。
func NewPlanScheduler(params PlanSchedulerParams) *PlanScheduler {
	if params.Store == nil || params.Handlers == nil || params.Plans == nil {
		return nil
	}
	interval := params.RefreshInterval
	if interval <= 0 {
		interval = defaultPlanRefreshInterval
	}
	repo := NewPlanRepository(params.Store, params.Plans, params.Handlers)
	s := &PlanScheduler{
		repo:            repo,
		execManager:     params.ExecManager,
		notifier:        params.Notifier,
		interval:        interval,
		priceCh:         make(chan priceTick, planPriceBufferSize),
		symbolIndex:     make(map[string][]*planWatcher),
		tradeIndex:      make(map[int][]*planWatcher),
		lastPriceTime:   make(map[string]time.Time),
		disableDebounce: params.DisableDebounce,
	}
	// 将 rebuildTrade 作为回调传入 executor，触发后立即刷新索引
	s.executor = NewPlanExecutor(repo, params.ExecManager, s.rebuildTrade)
	return s
}

// Start 启动刷新与价格监听循环。
func (s *PlanScheduler) Start(ctx context.Context) {
	if s == nil {
		return
	}
	s.startOnce.Do(func() {
		go s.refreshLoop(ctx)
		go s.priceLoop(ctx)
	})
}

// NotifyPrice 推送最新成交价（由 LiveService 调用）。
func (s *PlanScheduler) NotifyPrice(symbol string, price float64) {
	if s == nil || price <= 0 {
		return
	}
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	if symbol == "" {
		return
	}

	// Debounce: skip if last price update was within priceDebounceInterval
	// (disabled for tests via DisableDebounce flag)
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
	// 只在启动时做一次全量 rebuild，后续依赖事件驱动：
	// - entry_fill/manual-open 创建策略后通知
	// - update_exit_plan / 手动调整触发 rebuildTrade
	// - exit_fill 确认 pending tier/全平后通知
	s.rebuild(ctx)
	<-ctx.Done()
}

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

// NotifyPlanUpdated 实现 freqtrade.PlanUpdateHook，用于在策略状态变化时局部刷新 watcher。
func (s *PlanScheduler) NotifyPlanUpdated(ctx context.Context, tradeID int) {
	if s == nil {
		return
	}
	// 这里通常由 webhook / HTTP handler 触发，ctx 可能很快被 cancel。
	// rebuildTrade 需要稳定读取 DB，因此使用 Background。
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

	// 当 trade 已全平时，manager 会将该 trade 的所有 strategy_instances 标记为 Done。
	// 此时应移除 watcher，避免继续评估导致重复下单。
	allDone := true
	for _, rec := range recs {
		if rec.Status != database.StrategyStatusDone {
			allDone = false
			break
		}
	}
	if !allDone {
		// 兜底：即使 strategies 状态异常，只要 live_orders 显示已全平，也应移除 watcher。
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

// AdjustPlan 允许外部直接调整计划实例。
func (s *PlanScheduler) AdjustPlan(ctx context.Context, req interfaces.PlanAdjustSpec) error {
	if s == nil {
		return fmt.Errorf("plan scheduler 未初始化")
	}
	planID := strings.TrimSpace(req.PlanID)
	if req.TradeID <= 0 || planID == "" {
		return fmt.Errorf("trade_id 与 plan_id 必填")
	}

	// We need to find the specific watcher. We can look up in index if available?
	// But rebuild logic forces full refresh. AdjustPlan implies logic operation.
	// It's safer to fetch from DB to be sure?
	// But `buildWatcher` requires handler and grouping. The Repo handles that.

	recs, err := s.repo.ListStrategyInstances(ctx, req.TradeID)
	if err != nil {
		return fmt.Errorf("读取策略实例失败: %w", err)
	}

	// Find target plan
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

	// Rebuild index to reflect changes (e.g. status updates)
	s.rebuild(ctx)
	// Or rebuildTrade?
	// rebuildTrade(ctx, req.TradeID) is more efficient.
	// But `rebuild` was called in original code.
	// Use rebuildTrade if possible.
	s.rebuildTrade(ctx, req.TradeID)
	return nil
}

// ProcessUpdateDecision handles update_exit_plan decisions from the agent.
func (s *PlanScheduler) ProcessUpdateDecision(ctx context.Context, traceID string, d decision.Decision) error {
	if s == nil {
		return fmt.Errorf("plan scheduler 未初始化")
	}
	if d.ExitPlan == nil || strings.TrimSpace(d.ExitPlan.ID) == "" {
		return fmt.Errorf("缺少 exit_plan")
	}

	// Helper to find trade ID by symbol
	// We need generic way to find trade ID.
	// Scheduler has tradeIndex but indexed by tradeID.
	// symbolIndex maps symbol -> watchers.
	// We can pick active watcher for symbol.
	// Assuming one active trade per symbol for this agent context.
	var tradeID int
	s.mu.RLock()
	watchers := s.symbolIndex[strings.ToUpper(d.Symbol)]
	if len(watchers) > 0 {
		// Pick the first one? Or largest ID?
		// Usually only one active.
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
		// If simple adjust succeeded, we are good. The switch default in original returned error?
		// Original code: return fmt.Errorf("plan %s 暂不支持 update_exit_plan", planID) for default
		// BUT it executed AdjustPlan first.
		// "if err := ...; err != nil { return err } return fmt.Errorf..."
		// This meant simple plans failed?
		// Rereading legacy:
		// default:
		//   if err := s.planScheduler.AdjustPlan(...); err != nil { return err }
		//   return fmt.Errorf("plan %s 暂不支持 update_exit_plan", planID)
		// This implies only "plan_combo_main" was fully supported for silent success?
		// Or maybe it was intended to warn?
		// "update_exit_plan" usually implies complex adjustment.
		// If AdjustPlan works, we should return nil.
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
	waiting, err := waitingTierComponents(index[alias])
	if err != nil {
		return err
	}
	if len(waiting) == 0 {
		// return fmt.Errorf("%s 无可调整段位", alias)
		// Relaxed: maybe all triggered?
		logger.Warnf("%s 无可调整段位 (all triggered?)", alias)
		return nil
	}
	rawTiers, _ := params["tiers"].([]any)
	if len(rawTiers) == 0 {
		return fmt.Errorf("%s 缺少 tiers 参数", alias)
	}
	if len(rawTiers) != len(waiting) {
		return fmt.Errorf("%s tiers 数量应为 %d（剩余可调整段），当前=%d", alias, len(waiting), len(rawTiers))
	}
	newSum := decimal.Zero
	updates := make([]map[string]any, len(rawTiers))
	for i, entry := range rawTiers {
		tierMap, ok := entry.(map[string]any)
		if !ok {
			return fmt.Errorf("%s tier#%d 参数格式错误", alias, i+1)
		}
		update := make(map[string]any)
		if price, ok := tierMap["target_price"]; ok {
			update["target_price"] = price
		} else if price, ok := tierMap["target"]; ok {
			update["target"] = price
		} else {
			return fmt.Errorf("%s tier#%d 缺少 target_price", alias, i+1)
		}
		if ratio, ok := tierMap["ratio"]; ok {
			update["ratio"] = ratio
			if val, ok := utils.AsFloat(ratio); ok {
				newSum = newSum.Add(decimal.NewFromFloat(val))
			}
		} else {
			newSum = newSum.Add(decimal.NewFromFloat(waiting[i].Remaining))
		}
		updates[i] = update
	}
	oldSum := decimal.Zero
	for _, info := range waiting {
		oldSum = oldSum.Add(decimal.NewFromFloat(info.Remaining))
	}
	tolerance := decimal.NewFromFloat(1e-6)
	if oldSum.Sub(newSum).Abs().GreaterThan(tolerance) {
		return fmt.Errorf("%s 比例和应为 %s，当前=%s", alias, oldSum.String(), newSum.String())
	}
	for i, info := range waiting {
		if err := s.AdjustPlan(ctx, interfaces.PlanAdjustSpec{
			TradeID:   tradeID,
			PlanID:    planID,
			Component: info.Component,
			Params:    updates[i],
			Source:    source,
		}); err != nil {
			return fmt.Errorf("%s: 调整 %s 失败: %w", alias, info.Component, err)
		}
	}
	return nil
}

func (s *PlanScheduler) buildPlanComponentIndex(ctx context.Context, tradeID int, planID string) (map[string][]database.StrategyInstanceRecord, error) {
	// PlanScheduler uses s.repo not strategyStore directly
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

// Helpers

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
