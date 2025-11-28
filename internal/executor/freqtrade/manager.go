package freqtrade

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	brcfg "brale/internal/config"
	"brale/internal/decision"
	"brale/internal/gateway/database"
	"brale/internal/logger"
	"brale/internal/market"
)

// Logger abstracts decision log writes (live_decision_logs & live_orders).
type Logger interface {
	Insert(ctx context.Context, rec database.DecisionLogRecord) (int64, error)
}

// TextNotifier 描述最小化的文本推送接口（用于 Telegram 等）。
type TextNotifier interface {
	SendText(text string) error
}

// Manager 提供 freqtrade 执行、日志与持仓同步能力（重构版本，基于 live_* 表）。
type Manager struct {
	client           *Client
	cfg              brcfg.FreqtradeConfig
	logger           Logger
	posRepo          *PositionRepo
	posStore         database.LivePositionStore
	orderRec         market.Recorder
	balance          Balance
	traceByKey       map[string]int
	traceByID        map[int]string
	pendingDec       map[string]decision.Decision
	tradeDec         map[int]decision.Decision
	pendingSymbolDec map[string][]queuedDecision
	positions        map[int]Position
	posCache         map[int]database.LiveOrderWithTiers
	posCacheMu       sync.RWMutex
	mu               sync.Mutex
	horizonName      string
	notifier         TextNotifier
	pendingExits     map[int]pendingExit
	tierWatchOnce    sync.Once
	tierExecMu       sync.Mutex
	tierExec         map[int]bool
	posSyncOnce      sync.Once
	locker           *sync.Map
	missingPrice     map[string]bool
	startedAt        time.Time
	hadTradeSnap     bool
	hadOpenTrade     bool
}

const (
	defaultTier1Ratio        = 0.33
	defaultTier2Ratio        = 0.33
	defaultTier3Ratio        = 0.34
	tierWatchInterval        = 2 * time.Second
	positionSyncInterval     = time.Minute
	positionSyncStartupDelay = 10 * time.Second
	hitConfirmDelay          = 2 * time.Second
	autoCloseRetryInterval   = 30 * time.Second
	webhookContextTimeout    = 5 * time.Minute
)

// Position 缓存 freqtrade 持仓信息。
type Position struct {
	TradeID      int
	Symbol       string
	Side         string
	Amount       float64
	Stake        float64
	Leverage     float64
	EntryPrice   float64
	OpenedAt     time.Time
	Closed       bool
	ClosedAt     time.Time
	ExitPrice    float64
	ExitReason   string
	ExitPnLRatio float64
	ExitPnLUSD   float64
}

type queuedDecision struct {
	traceID  string
	decision decision.Decision
}

// NewManager 创建 freqtrade 执行管理器。
func NewManager(client *Client, cfg brcfg.FreqtradeConfig, horizon string, logStore Logger, orderRec market.Recorder, notifier TextNotifier) *Manager {
	var posStore database.LivePositionStore
	if ps, ok := logStore.(database.LivePositionStore); ok {
		posStore = ps
	} else if ps, ok := orderRec.(database.LivePositionStore); ok {
		posStore = ps
	}
	initLiveOrderPnL(posStore)
	return &Manager{
		client:           client,
		cfg:              cfg,
		logger:           logStore,
		posStore:         posStore,
		posRepo:          NewPositionRepo(posStore),
		orderRec:         orderRec,
		traceByKey:       make(map[string]int),
		traceByID:        make(map[int]string),
		pendingDec:       make(map[string]decision.Decision),
		tradeDec:         make(map[int]decision.Decision),
		pendingSymbolDec: make(map[string][]queuedDecision),
		positions:        make(map[int]Position),
		pendingExits:     make(map[int]pendingExit),
		horizonName:      horizon,
		notifier:         notifier,
		tierExec:         make(map[int]bool),
		locker:           positionLocker,
		missingPrice:     make(map[string]bool),
		startedAt:        time.Now(),
	}
}

// initLiveOrderPnL 尝试幂等添加 pnl 列，避免旧库缺列导致写入失败。
func initLiveOrderPnL(store database.LivePositionStore) {
	if store == nil {
		return
	}
	if err := store.AddOrderPnLColumns(); err != nil {
		logger.Warnf("freqtrade manager: 初始化 pnl 列失败: %v", err)
	}
}

// DecisionInput 用于执行器的输入。
type DecisionInput struct {
	TraceID  string
	Decision decision.Decision
}

// Execute 根据决策调用 freqtrade ForceEnter/ForceExit/更新。
func (m *Manager) Execute(ctx context.Context, input DecisionInput) error {
	if m.client == nil {
		return fmt.Errorf("freqtrade client not initialized")
	}
	d := input.Decision
	switch d.Action {
	case "open_long", "open_short":
		return m.forceEnter(ctx, input.TraceID, d)
	case "close_long", "close_short":
		return m.forceExit(ctx, input.TraceID, d)
	case "adjust_stop_loss":
		return m.adjustStopLoss(ctx, input.TraceID, d)
	case "adjust_take_profit":
		return m.adjustTakeProfit(ctx, input.TraceID, d)
	case "update_tiers":
		return m.updateTiers(ctx, input.TraceID, d, true)
	default:
		return nil
	}
}

// HandleWebhook 由 HTTP 路由调用，负责更新持仓与日志。
func (m *Manager) HandleWebhook(ctx context.Context, msg WebhookMessage) {
	// 避免沿用 HTTP 请求 ctx（连接断开/超时会被取消），这里统一切到带超时的后台上下文。
	ctx, cancel := context.WithTimeout(context.Background(), webhookContextTimeout)
	defer cancel()

	typ := strings.ToLower(strings.TrimSpace(msg.Type))
	switch typ {
	case "entry", "entry_fill":
		m.handleEntry(ctx, msg, typ == "entry_fill")
	case "entry_cancel":
		m.handleEntryCancel(ctx, msg)
	case "exit", "exit_fill":
		m.handleExit(ctx, msg, typ)
	case "exit_cancel":
		// TODO: implement exit cancel handling if needed
	default:
		logger.Debugf("freqtrade manager: ignore webhook type %s", msg.Type)
	}
}

// StartTierWatcher 启用三段式监控（重构后将使用 posRepo 数据）。
func (m *Manager) StartTierWatcher(ctx context.Context, priceFn func(symbol string) TierPriceQuote) {
	if m == nil {
		return
	}
	m.tierWatchOnce.Do(func() {
		go m.runTierWatcher(ctx, priceFn)
	})
}

// StartPositionSync 按固定频率刷新 freqtrade 仓位。
func (m *Manager) StartPositionSync(ctx context.Context) {
	if m == nil {
		return
	}
	m.posSyncOnce.Do(func() {
		go m.runPositionSync(ctx)
	})
}

func (m *Manager) runTierWatcher(ctx context.Context, priceFn func(symbol string) TierPriceQuote) {
	ticker := time.NewTicker(tierWatchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.evaluateTiers(ctx, priceFn)
			m.checkPending(ctx)
		}
	}
}

// checkPending 校验 pending exit 是否已在 freqtrade 生效。
func (m *Manager) checkPending(ctx context.Context) {
	if m == nil {
		return
	}
	pending := m.listPendingExits()
	if len(pending) == 0 {
		return
	}
	var trades []Trade
	if m.client != nil {
		if ts, err := m.client.ListTrades(ctx); err == nil {
			trades = ts
		}
	}
	trMap := make(map[int]Trade, len(trades))
	for _, tr := range trades {
		trMap[tr.ID] = tr
	}
	for _, pe := range pending {
		tradeID := pe.TradeID
		lock := getPositionLock(tradeID)
		lock.Lock()
		m.handlePending(ctx, pe, trMap)
		lock.Unlock()
	}
}

func (m *Manager) handlePending(ctx context.Context, pe pendingExit, trMap map[int]Trade) {
	tradeID := pe.TradeID
	symbol := pe.Symbol
	side := pe.Side
	tr, exists := trMap[tradeID]
	now := time.Now()

	currentAmount := pe.ExpectedAmount
	if exists && tr.Amount > 0 {
		currentAmount = tr.Amount
	}
	prevAmt := pe.PrevAmount
	closedDelta := math.Max(0, prevAmt-currentAmount)
	success := false
	if !exists && pe.ExpectedAmount <= 0 {
		success = true // 完全平仓
	} else if exists && closedDelta > 0 {
		success = true // 数量减少
	}

	if !success {
		m.appendOperation(ctx, tradeID, symbol, pe.Operation, map[string]any{
			"event_type": strings.ToUpper(pe.Kind),
			"status":     "FAILED",
			"price":      pe.TargetPrice,
			"side":       side,
		})
		m.notify("自动平仓失败 ❌",
			fmt.Sprintf("交易ID: %d", tradeID),
			fmt.Sprintf("标的: %s", symbol),
			fmt.Sprintf("事件: %s", strings.ToUpper(pe.Kind)),
			fmt.Sprintf("触发价: %s", formatPrice(pe.TargetPrice)),
			"仓位未变化，等待下轮检测",
		)
		m.popPendingExit(tradeID)
		return
	}

	totalClosed := pe.PrevClosed + closedDelta
	status := database.LiveOrderStatusPartial
	var endTime *time.Time
	if currentAmount <= 0.0000001 {
		currentAmount = 0
		status = database.LiveOrderStatusClosed
		tmp := now
		endTime = &tmp
	}

	orderRec := database.LiveOrderRecord{
		FreqtradeID:  tradeID,
		Symbol:       symbol,
		Side:         side,
		Amount:       ptrFloat(currentAmount),
		ClosedAmount: ptrFloat(totalClosed),
		Status:       status,
		EndTime:      endTime,
		UpdatedAt:    now,
	}
	_ = m.posRepo.UpsertOrder(ctx, orderRec)

	tier := peTiersFromCache(m, tradeID)
	remainingRatio := tier.RemainingRatio
	switch pe.Kind {
	case "stop_loss":
		tier.Tier1Done, tier.Tier2Done, tier.Tier3Done = true, true, true
		tier.RemainingRatio = 0
		tier.Status = 2
	case "take_profit":
		tier.Tier1Done, tier.Tier2Done, tier.Tier3Done = true, true, true
		tier.RemainingRatio = 0
		tier.Status = 1
	case "tier1":
		tier.Tier1Done = true
		tier.Status = 3
		if pe.EntryPrice > 0 {
			old := tier.StopLoss
			tier.StopLoss = pe.EntryPrice
			m.posRepo.InsertModification(ctx, database.TierModificationLog{
				FreqtradeID: tradeID,
				Field:       database.TierFieldStopLoss,
				OldValue:    formatPrice(old),
				NewValue:    formatPrice(pe.EntryPrice),
				Source:      3,
				Reason:      "价格到达 tier1，上移止损到入场价",
				Timestamp:   now,
			})
		}
		tier.RemainingRatio = math.Max(0, remainingRatio-pe.Ratio)
	case "tier2":
		tier.Tier2Done = true
		tier.Status = 4
		tier.RemainingRatio = math.Max(0, remainingRatio-pe.Ratio)
	case "tier3":
		tier.Tier3Done = true
		tier.Status = 5
		tier.RemainingRatio = math.Max(0, remainingRatio-pe.Ratio)
	}
	if tier.RemainingRatio < 0 {
		tier.RemainingRatio = 0
	}
	if status == database.LiveOrderStatusClosed {
		tier.Tier1Done, tier.Tier2Done, tier.Tier3Done = true, true, true
		tier.RemainingRatio = 0
	}
	tier.Timestamp = now
	tier.UpdatedAt = now
	if tier.FreqtradeID == 0 {
		tier.FreqtradeID = tradeID
		tier.Symbol = symbol
	}
	_ = m.posRepo.UpsertTiers(ctx, tier)
	m.updateCacheOrderTiers(orderRec, tier)

	m.appendOperation(ctx, tradeID, symbol, pe.Operation, map[string]any{
		"event_type": strings.ToUpper(pe.Kind),
		"status":     "SUCCESS",
		"price":      pe.TargetPrice,
		"side":       side,
		"closed":     closedDelta,
		"remaining":  currentAmount,
		"stake":      pe.Stake,
		"leverage":   pe.Leverage,
	})

	title := "自动平仓完成 ✅"
	switch pe.Kind {
	case "tier1", "tier2", "tier3":
		title = fmt.Sprintf("自动%s完成 ✅", strings.ToUpper(pe.Kind))
	case "stop_loss":
		title = "自动止损完成 ✅"
	case "take_profit":
		title = "自动止盈完成 ✅"
	}
	m.notify(title,
		fmt.Sprintf("交易ID: %d", tradeID),
		fmt.Sprintf("标的: %s", symbol),
		fmt.Sprintf("事件: %s", strings.ToUpper(pe.Kind)),
		fmt.Sprintf("触发价: %s", formatPrice(pe.TargetPrice)),
		fmt.Sprintf("平仓数量: %s", formatQty(closedDelta)),
		fmt.Sprintf("剩余数量: %s", formatQty(currentAmount)),
	)

	if status == database.LiveOrderStatusClosed {
		m.markPositionClosed(tradeID, symbol, side, pe.TargetPrice, pe.Stake, "", now, 0)
	} else {
		m.mu.Lock()
		if pos, ok := m.positions[tradeID]; ok {
			pos.Amount = currentAmount
			m.positions[tradeID] = pos
		}
		m.mu.Unlock()
	}

	m.popPendingExit(tradeID)
}

func (m *Manager) runPositionSync(ctx context.Context) {
	// 启动后等待一段时间，避免 freqtrade 尚未返回仓位时误判关闭。
	if delay := positionSyncStartupDelay - time.Since(m.startedAt); delay > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
	// 启动时同步一次，当前关闭周期性对账，避免频繁写入。
	_, _ = m.SyncOpenPositions(ctx)
}

// Positions 返回当前 freqtrade 持仓快照（基于内存缓存）。
func (m *Manager) Positions() []decision.PositionSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.positions) == 0 {
		return nil
	}
	out := make([]decision.PositionSnapshot, 0, len(m.positions))
	for _, pos := range m.positions {
		if strings.TrimSpace(pos.Symbol) == "" {
			continue
		}
		if pos.Closed {
			continue
		}
		tiers := peTiersFromCache(m, pos.TradeID)
		holdingMs := time.Since(pos.OpenedAt).Milliseconds()
		if pos.Closed && !pos.ClosedAt.IsZero() {
			holdingMs = pos.ClosedAt.Sub(pos.OpenedAt).Milliseconds()
		}
		snap := decision.PositionSnapshot{
			Symbol:         strings.ToUpper(pos.Symbol),
			Side:           strings.ToUpper(pos.Side),
			EntryPrice:     pos.EntryPrice,
			Quantity:       pos.Amount,
			Stake:          pos.Stake,
			Leverage:       pos.Leverage,
			HoldingMs:      holdingMs,
			PositionValue:  pos.EntryPrice * pos.Amount,
			TakeProfit:     tiers.TakeProfit,
			StopLoss:       tiers.StopLoss,
			RemainingRatio: tiers.RemainingRatio,
			Tier1Target:    tiers.Tier1,
			Tier1Ratio:     tiers.Tier1Ratio,
			Tier1Done:      tiers.Tier1Done,
			Tier2Target:    tiers.Tier2,
			Tier2Ratio:     tiers.Tier2Ratio,
			Tier2Done:      tiers.Tier2Done,
			Tier3Target:    tiers.Tier3,
			Tier3Ratio:     tiers.Tier3Ratio,
			Tier3Done:      tiers.Tier3Done,
			TierNotes:      tiers.TierNotes,
		}
		out = append(out, snap)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Symbol < out[j].Symbol })
	return out
}

// PositionsForAPI 使用内存缓存构造 APIPosition（待优化为直接读取 live_*）。
func (m *Manager) PositionsForAPI(ctx context.Context, opts PositionListOptions) (PositionListResult, error) {
	result := PositionListResult{
		Page:     opts.Page,
		PageSize: opts.PageSize,
	}
	if result.Page < 1 {
		result.Page = 1
	}
	if result.PageSize <= 0 {
		result.PageSize = 10
	}
	if result.PageSize > 500 {
		result.PageSize = 500
	}
	if m.posRepo == nil {
		return result, fmt.Errorf("position repo 未初始化")
	}
	offset := (result.Page - 1) * result.PageSize
	symbol := strings.ToUpper(strings.TrimSpace(opts.Symbol))
	positions, total, err := m.posRepo.RecentPositionsPaged(ctx, symbol, result.PageSize, offset)
	if err != nil {
		return result, err
	}
	result.TotalCount = total
	if len(positions) == 0 {
		return result, nil
	}
	list := make([]APIPosition, 0, len(positions))
	for _, p := range positions {
		// opening 也要展示，供后台补齐 tier
		api := APIPosition{
			TradeID:        p.Order.FreqtradeID,
			Symbol:         strings.ToUpper(p.Order.Symbol),
			Side:           strings.ToUpper(p.Order.Side),
			EntryPrice:     valOrZero(p.Order.Price),
			Amount:         valOrZero(p.Order.Amount),
			InitialAmount:  valOrZero(p.Order.InitialAmount),
			Stake:          valOrZero(p.Order.StakeAmount),
			Leverage:       valOrZero(p.Order.Leverage),
			PositionValue:  valOrZero(p.Order.PositionValue),
			OpenedAt:       timeToMillis(p.Order.StartTime),
			HoldingMs:      millisSince(p.Order.StartTime),
			StopLoss:       p.Tiers.StopLoss,
			TakeProfit:     p.Tiers.TakeProfit,
			RemainingRatio: p.Tiers.RemainingRatio,
			Tier1:          TierInfo{Target: p.Tiers.Tier1, Ratio: p.Tiers.Tier1Ratio, Done: p.Tiers.Tier1Done},
			Tier2:          TierInfo{Target: p.Tiers.Tier2, Ratio: p.Tiers.Tier2Ratio, Done: p.Tiers.Tier2Done},
			Tier3:          TierInfo{Target: p.Tiers.Tier3, Ratio: p.Tiers.Tier3Ratio, Done: p.Tiers.Tier3Done},
			TierNotes:      p.Tiers.TierNotes,
			Placeholder:    p.Tiers.IsPlaceholder,
			Status:         statusText(p.Order.Status),
		}
		if p.Order.EndTime != nil {
			api.ClosedAt = timeToMillis(p.Order.EndTime)
		}
		baseValue := PositionPnLValue(api.Stake, api.Leverage, api.PositionValue)
		realizedUSD := valOrZero(p.Order.PnLUSD)
		realizedRatio := valOrZero(p.Order.PnLRatio)
		if pos, ok := m.positions[p.Order.FreqtradeID]; ok {
			if pos.ExitPrice > 0 {
				api.ExitPrice = pos.ExitPrice
			}
			if pos.ExitReason != "" {
				api.ExitReason = pos.ExitReason
			}
			if pos.ExitPnLRatio != 0 {
				realizedRatio = pos.ExitPnLRatio
			}
			if pos.ExitPnLUSD != 0 {
				realizedUSD = pos.ExitPnLUSD
			}
			if pos.Closed && !pos.OpenedAt.IsZero() && !pos.ClosedAt.IsZero() {
				api.HoldingMs = pos.ClosedAt.Sub(pos.OpenedAt).Milliseconds()
				api.RemainingRatio = 0
				api.Tier1.Done, api.Tier2.Done, api.Tier3.Done = true, true, true
			}
		}
		if realizedRatio == 0 && baseValue > 0 && realizedUSD != 0 {
			realizedRatio = realizedUSD / baseValue
		}
		api.RealizedPnLUSD = realizedUSD
		api.RealizedPnLRatio = realizedRatio
		api.PnLUSD = realizedUSD
		api.PnLRatio = realizedRatio
		// CurrentPrice/ExitPrice 可结合行情或事件补充，这里以存量信息为准。
		if opts.IncludeLogs && m.posRepo != nil {
			limit := opts.LogsLimit
			if limit <= 0 {
				limit = 50
			}
			if logs, err := m.posRepo.TierLogs(ctx, p.Order.FreqtradeID, limit); err == nil {
				api.TierLogs = logs
			}
			if events, err := m.posRepo.TradeEvents(ctx, p.Order.FreqtradeID, limit); err == nil {
				api.Events = events
			}
		}
		list = append(list, api)
	}
	result.Positions = list
	return result, nil
}

// AccountBalance 返回最近一次同步的账户余额信息（占位）。
func (m *Manager) AccountBalance() Balance {
	return m.balance
}

// RefreshBalance 主动从 freqtrade 获取账户余额并缓存。
func (m *Manager) RefreshBalance(ctx context.Context) (Balance, error) {
	if m == nil || m.client == nil {
		return Balance{}, fmt.Errorf("freqtrade client not initialized")
	}
	bal, err := m.client.GetBalance(ctx)
	if err != nil {
		return Balance{}, err
	}
	m.balance = bal
	return bal, nil
}

// trace helpers
func (m *Manager) storeTrade(symbol, side string, tradeID int) {
	if tradeID == 0 {
		return
	}
	key := freqtradeKey(symbol, side)
	m.mu.Lock()
	m.traceByKey[key] = tradeID
	m.mu.Unlock()
}

func (m *Manager) lookupTrade(symbol, side string) (int, bool) {
	key := freqtradeKey(symbol, side)
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.traceByKey[key]
	return id, ok
}

func (m *Manager) deleteTrade(symbol, side string) {
	key := freqtradeKey(symbol, side)
	m.mu.Lock()
	delete(m.traceByKey, key)
	m.mu.Unlock()
}

func (m *Manager) storeTrace(tradeID int, traceID string) {
	if tradeID <= 0 || traceID == "" {
		return
	}
	m.mu.Lock()
	m.traceByID[tradeID] = traceID
	m.mu.Unlock()
	m.bindDecision(tradeID, traceID)
}

func (m *Manager) lookupTrace(tradeID int) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.traceByID[tradeID]
}

func (m *Manager) ensureTrace(raw string) string {
	id := strings.TrimSpace(raw)
	if id != "" {
		return id
	}
	return fmt.Sprintf("freqtrade-%d", time.Now().UnixNano())
}

func (m *Manager) deleteTrace(tradeID int) {
	m.mu.Lock()
	delete(m.traceByID, tradeID)
	m.mu.Unlock()
	m.forgetDecision(tradeID)
}

func (m *Manager) CacheDecision(traceID string, d decision.Decision) string {
	id := m.ensureTrace(traceID)
	m.mu.Lock()
	m.pendingDec[id] = d
	if side := deriveSide(d.Action); side != "" {
		if key := freqtradeKey(d.Symbol, side); key != "" {
			m.pendingSymbolDec[key] = append(m.pendingSymbolDec[key], queuedDecision{traceID: id, decision: d})
		}
	}
	m.mu.Unlock()
	return id
}

func (m *Manager) bindDecision(tradeID int, traceID string) {
	if tradeID <= 0 {
		return
	}
	id := m.ensureTrace(traceID)
	m.mu.Lock()
	if dec, ok := m.pendingDec[id]; ok {
		m.attachDecisionLocked(tradeID, id, dec)
	}
	m.mu.Unlock()
}

func (m *Manager) decisionForTrade(tradeID int) (decision.Decision, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	dec, ok := m.tradeDec[tradeID]
	return dec, ok
}

func (m *Manager) forgetDecision(tradeID int) {
	if tradeID <= 0 {
		return
	}
	m.mu.Lock()
	delete(m.tradeDec, tradeID)
	m.mu.Unlock()
}

func (m *Manager) bindDecisionBySymbol(tradeID int, symbol, side string) bool {
	key := freqtradeKey(symbol, side)
	if key == "" {
		return false
	}
	m.mu.Lock()
	queue := m.pendingSymbolDec[key]
	if len(queue) == 0 {
		m.mu.Unlock()
		return false
	}
	entry := queue[0]
	if len(queue) == 1 {
		delete(m.pendingSymbolDec, key)
	} else {
		m.pendingSymbolDec[key] = queue[1:]
	}
	m.attachDecisionLocked(tradeID, entry.traceID, entry.decision)
	m.mu.Unlock()
	return true
}

func (m *Manager) attachDecisionLocked(tradeID int, traceID string, dec decision.Decision) {
	m.tradeDec[tradeID] = dec
	if traceID != "" {
		m.traceByID[tradeID] = traceID
		delete(m.pendingDec, traceID)
	}
	if traceID != "" {
		m.removeQueuedDecisionLocked(traceID, dec)
	}
}

func (m *Manager) removeQueuedDecisionLocked(traceID string, dec decision.Decision) {
	if traceID == "" {
		return
	}
	side := deriveSide(dec.Action)
	key := freqtradeKey(dec.Symbol, side)
	if key == "" {
		return
	}
	queue := m.pendingSymbolDec[key]
	if len(queue) == 0 {
		return
	}
	idx := -1
	for i, entry := range queue {
		if entry.traceID == traceID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return
	}
	queue = append(queue[:idx], queue[idx+1:]...)
	if len(queue) == 0 {
		delete(m.pendingSymbolDec, key)
	} else {
		m.pendingSymbolDec[key] = queue
	}
}

func (m *Manager) discardQueuedDecision(traceID string) {
	id := m.ensureTrace(traceID)
	m.mu.Lock()
	if dec, ok := m.pendingDec[id]; ok {
		delete(m.pendingDec, id)
		m.removeQueuedDecisionLocked(id, dec)
	}
	m.mu.Unlock()
}
