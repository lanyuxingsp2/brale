package freqtrade

import (
	"context"
	"errors"
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
	pendingExits     map[int]*pendingExit
	priceWatchOnce   sync.Once
	tierExec         map[int]bool
	posSyncOnce      sync.Once
	fastSyncOnce     sync.Once
	locker           *sync.Map
	missingPrice     map[string]bool
	priceEvents      chan priceEvent
	startedAt        time.Time
	hadTradeSnap     bool
	hadOpenTrade     bool
	exitConfirmMu    sync.Mutex
	exitConfirms     map[int]exitConfirm
}

type exitConfirm struct {
	Remaining float64
	Timestamp time.Time
}

const (
	defaultTier1Ratio        = 0.33
	defaultTier2Ratio        = 0.33
	defaultTier3Ratio        = 0.34
	positionSyncInterval     = time.Minute
	positionSyncStartupDelay = 10 * time.Second
	fastStatusInterval       = 5 * time.Second
	statusRetryInterval      = 2 * time.Second
	statusRetryMax           = 5
	hitConfirmDelay          = 2 * time.Second
	autoCloseRetryInterval   = 30 * time.Second
	webhookContextTimeout    = 5 * time.Minute
	pendingExitTimeout       = 11 * time.Minute
	pendingAmountEpsilon     = 1e-6
	priceEventBufferSize     = 2048
	priceFlushInterval       = 500 * time.Millisecond
	pendingCheckInterval     = 2 * time.Second
	exitConfirmTTL           = 3 * time.Minute
	positionSummaryDelay     = 3 * time.Minute
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

type priceEvent struct {
	symbol string
	quote  TierPriceQuote
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
		pendingExits:     make(map[int]*pendingExit),
		horizonName:      horizon,
		notifier:         notifier,
		tierExec:         make(map[int]bool),
		locker:           positionLocker,
		missingPrice:     make(map[string]bool),
		exitConfirms:     make(map[int]exitConfirm),
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
	TraceID     string
	Decision    decision.Decision
	MarketPrice float64
}

// Execute 根据决策调用 freqtrade ForceEnter/ForceExit/更新。
func (m *Manager) Execute(ctx context.Context, input DecisionInput) error {
	if m.client == nil {
		return fmt.Errorf("freqtrade client not initialized")
	}
	d := input.Decision
	switch d.Action {
	case "open_long", "open_short":
		return m.forceEnter(ctx, input.TraceID, d, input.MarketPrice)
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
	parent := ctx
	if parent == nil {
		parent = context.Background()
	}
	webhookCtx, cancel := context.WithTimeout(parent, webhookContextTimeout)
	defer cancel()

	typ := strings.ToLower(strings.TrimSpace(msg.Type))
	switch typ {
	case "entry", "entry_fill":
		m.handleEntry(webhookCtx, msg, typ == "entry_fill")
	case "entry_cancel":
		m.handleEntryCancel(webhookCtx, msg)
	case "exit", "exit_fill":
		m.handleExit(webhookCtx, msg, typ)
	case "exit_cancel":
		// TODO: implement exit cancel handling if needed
	default:
		logger.Debugf("freqtrade manager: ignore webhook type %s", msg.Type)
	}
}

// StartPriceMonitor 启动实时成交价监控（由行情事件驱动）。
func (m *Manager) StartPriceMonitor(ctx context.Context) {
	if m == nil {
		return
	}
	m.priceWatchOnce.Do(func() {
		m.priceEvents = make(chan priceEvent, priceEventBufferSize)
		go m.runPriceMonitor(ctx)
	})
}

// PublishPrice 将实时成交价事件写入节流队列。
func (m *Manager) PublishPrice(symbol string, quote TierPriceQuote) {
	if m == nil {
		return
	}
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	if symbol == "" {
		return
	}
	if quote.isEmpty() {
		m.reportMissingPrice(symbol)
		return
	}
	m.clearMissingPrice(symbol)
	m.priceWatchOnce.Do(func() {
		m.priceEvents = make(chan priceEvent, priceEventBufferSize)
	})
	if m.priceEvents == nil {
		return
	}
	evt := priceEvent{symbol: symbol, quote: quote}
	select {
	case m.priceEvents <- evt:
	default:
	}
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

// StartFastStatusSync 启动快速 /status 轮询，刷新盈亏字段。
func (m *Manager) StartFastStatusSync(ctx context.Context) {
	if m == nil {
		return
	}
	m.fastSyncOnce.Do(func() {
		go m.runFastStatusSync(ctx)
	})
}

func (m *Manager) runPriceMonitor(ctx context.Context) {
	ticker := time.NewTicker(priceFlushInterval)
	defer ticker.Stop()
	pending := make(map[string]TierPriceQuote)
	lastPendingCheck := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case evt := <-m.priceEvents:
			if evt.symbol == "" || evt.quote.isEmpty() {
				continue
			}
			curr := pending[evt.symbol]
			if curr.isEmpty() {
				pending[evt.symbol] = evt.quote
				continue
			}
			curr.Last = evt.quote.Last
			if curr.Low == 0 || (evt.quote.Low > 0 && evt.quote.Low < curr.Low) {
				curr.Low = evt.quote.Low
			}
			if evt.quote.High > curr.High {
				curr.High = evt.quote.High
			}
			pending[evt.symbol] = curr
		case <-ticker.C:
			if len(pending) > 0 {
				batch := pending
				pending = make(map[string]TierPriceQuote)
				for sym, quote := range batch {
					m.handlePriceTick(ctx, sym, quote)
				}
			}
			if time.Since(lastPendingCheck) >= pendingCheckInterval {
				m.checkPending(ctx)
				lastPendingCheck = time.Now()
			}
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

	currentAmount := pe.TargetAmount
	if exists {
		currentAmount = tr.Amount
	}
	closedDelta := math.Max(0, pe.PrevAmount-currentAmount)
	success := (!exists && currentAmount <= pendingAmountEpsilon) || closedDelta > pendingAmountEpsilon
	logger.Infof("pending exit 轮询 trade=%d kind=%s state=%s prev=%.6f current=%.6f target=%.6f delta=%.6f exists=%v", tradeID, pe.Kind, pe.stateDesc(), pe.PrevAmount, currentAmount, pe.TargetAmount, closedDelta, exists)

	if !success {
		if confirm, ok := m.peekExitConfirm(tradeID); ok && confirm.Timestamp.After(pe.RequestedAt) {
			expected := pe.TargetAmount
			if expected < 0 {
				expected = 0
			}
			if confirm.Remaining <= expected+pendingAmountEpsilon {
				logger.Infof("pending exit 使用 webhook 确认 trade=%d kind=%s remaining=%.6f", tradeID, pe.Kind, confirm.Remaining)
				currentAmount = confirm.Remaining
				closedDelta = math.Max(0, pe.PrevAmount-confirm.Remaining)
				success = closedDelta > pendingAmountEpsilon || confirm.Remaining <= pendingAmountEpsilon
			}
			if success {
				m.clearExitConfirm(tradeID)
			}
		}
	}

	if !success {
		if time.Since(pe.RequestedAt) < pendingExitTimeout {
			if pe.State == pendingStateQueued {
				pe.markWaiting()
				m.updatePendingExit(pe)
				m.appendOperation(ctx, tradeID, symbol, pe.Operation, map[string]any{
					"event_type": "PENDING_" + strings.ToUpper(pe.describeKind()),
					"status":     pe.stateDesc(),
					"price":      pe.TargetPrice,
					"side":       side,
					"remaining":  currentAmount,
				})
				logger.Infof("pending exit 等待 trade=%d kind=%s 仓位未变化 remaining=%.6f", tradeID, pe.Kind, currentAmount)
			}
			return
		}
		logger.Warnf("pending exit 超时 trade=%d kind=%s remaining=%.6f delta=%.6f", tradeID, pe.Kind, currentAmount, closedDelta)
		pe.markFailed("FREQTRADE_NO_FILL")
		snapshot := m.pendingTradeSnapshot(ctx, exists, tr, tradeID)
		m.restorePendingExitState(ctx, pe, snapshot)
		m.appendOperation(ctx, tradeID, symbol, pe.Operation, map[string]any{
			"event_type": strings.ToUpper(pe.Kind),
			"status":     pe.stateDesc(),
			"price":      pe.TargetPrice,
			"side":       side,
			"remaining":  currentAmount,
			"reason":     pe.FailReason,
		})
		m.notify("自动平仓失败 ❌",
			fmt.Sprintf("交易ID: %d", tradeID),
			fmt.Sprintf("标的: %s", symbol),
			fmt.Sprintf("事件: %s", strings.ToUpper(pe.Kind)),
			fmt.Sprintf("触发价: %s", formatPrice(pe.TargetPrice)),
			"仓位未变化，已恢复监控等待下一次触发",
		)
		m.popPendingExit(tradeID)
		return
	}
	logger.Infof("pending exit 确认 trade=%d kind=%s delta=%.6f remaining=%.6f", tradeID, pe.Kind, closedDelta, currentAmount)
	m.clearExitConfirm(tradeID)

	snapshot := m.pendingTradeSnapshot(ctx, exists, tr, tradeID)

	if snapshot != nil {
		pe.markConfirmed(snapshot)
	} else {
		pe.markConfirmed(nil)
	}
	m.updatePendingExit(pe)

	orderRec, _, actualClosed, err := m.finalizePendingExit(ctx, pe, snapshot)
	if err != nil {
		logger.Warnf("freqtrade pending: finalize trade=%d err=%v", tradeID, err)
		return
	}

	m.appendOperation(ctx, tradeID, orderRec.Symbol, pe.Operation, map[string]any{
		"event_type": strings.ToUpper(pe.describeKind()),
		"status":     "SUCCESS",
		"price":      pe.TargetPrice,
		"side":       orderRec.Side,
		"closed":     actualClosed,
		"remaining":  valOrZero(orderRec.Amount),
		"stake":      pe.Stake,
		"leverage":   pe.Leverage,
	})

	title := "自动平仓完成 ✅"
	switch {
	case strings.EqualFold(pe.Kind, "stop_loss"):
		title = "自动止损完成 ✅"
	case strings.EqualFold(pe.Kind, "take_profit"):
		title = "自动止盈完成 ✅"
	case strings.HasPrefix(strings.ToLower(pe.Kind), "tier"):
		title = fmt.Sprintf("自动%s完成 ✅", strings.ToUpper(pe.describeKind()))
	}
	m.notify(title,
		fmt.Sprintf("交易ID: %d", tradeID),
		fmt.Sprintf("标的: %s", orderRec.Symbol),
		fmt.Sprintf("事件: %s", strings.ToUpper(pe.describeKind())),
		fmt.Sprintf("触发价: %s", formatPrice(pe.TargetPrice)),
		fmt.Sprintf("平仓数量: %s", formatQty(actualClosed)),
		fmt.Sprintf("剩余数量: %s", formatQty(valOrZero(orderRec.Amount))),
	)
	m.schedulePositionSummary(tradeID, strings.ToUpper(pe.describeKind()))

	if valOrZero(orderRec.Amount) <= pendingAmountEpsilon {
		m.markPositionClosed(tradeID, orderRec.Symbol, orderRec.Side, pe.TargetPrice, pe.Stake, "", now, valOrZero(orderRec.RealizedPnLRatio))
	}

	m.popPendingExit(tradeID)
}

func (m *Manager) pendingTradeSnapshot(ctx context.Context, exists bool, cached Trade, tradeID int) *Trade {
	if exists {
		copy := cached
		return &copy
	}
	if trade, err := m.fetchTradeDetailWithRetry(ctx, tradeID); err == nil && trade != nil {
		return trade
	}
	return nil
}

func (m *Manager) finalizePendingExit(ctx context.Context, pe pendingExit, tradeSnapshot *Trade) (database.LiveOrderRecord, database.LiveTierRecord, float64, error) {
	if m == nil || m.posRepo == nil {
		return database.LiveOrderRecord{}, database.LiveTierRecord{}, 0, fmt.Errorf("position repo 未初始化")
	}
	tradeID := pe.TradeID
	var (
		orderRec database.LiveOrderRecord
		tierRec  database.LiveTierRecord
		found    bool
		err      error
	)
	if m.posRepo != nil {
		orderRec, tierRec, found, err = m.posRepo.GetPosition(ctx, tradeID)
		if err != nil {
			return database.LiveOrderRecord{}, database.LiveTierRecord{}, 0, err
		}
	}
	now := time.Now()
	if !found {
		orderRec = database.LiveOrderRecord{
			FreqtradeID: tradeID,
			Symbol:      strings.ToUpper(firstNonEmpty(pe.Symbol)),
			Side:        strings.ToLower(pe.Side),
			CreatedAt:   now,
		}
		tierRec = buildPlaceholderTiers(tradeID, orderRec.Symbol)
	}
	orderRec.Symbol = strings.ToUpper(firstNonEmpty(orderRec.Symbol, pe.Symbol))
	orderRec.Side = strings.ToLower(firstNonEmpty(orderRec.Side, pe.Side))
	if orderRec.CreatedAt.IsZero() {
		orderRec.CreatedAt = now
	}
	orderRec.UpdatedAt = now

	initialAmount := deriveInitialAmount(orderRec, pe)
	currentAmount := pe.TargetAmount
	if tradeSnapshot != nil {
		if tradeSnapshot.IsOpen {
			currentAmount = tradeSnapshot.Amount
		} else {
			// freqtrade /trades 返回的历史记录中 amount 代表初始仓位，
			// 已平仓时不能直接使用，否则会把数量又写回去。
			currentAmount = 0
		}
	}
	if currentAmount < 0 {
		currentAmount = 0
	}
	if initialAmount <= 0 {
		initialAmount = math.Max(pe.PrevAmount, currentAmount)
	}
	totalClosed := math.Max(0, initialAmount-currentAmount)
	closedDelta := math.Max(0, pe.PrevAmount-currentAmount)

	status := database.LiveOrderStatusPartial
	if currentAmount <= pendingAmountEpsilon {
		currentAmount = 0
		status = database.LiveOrderStatusClosed
		end := now
		orderRec.EndTime = &end
	}
	orderRec.Status = status
	orderRec.Amount = ptrFloat(currentAmount)
	orderRec.InitialAmount = ptrFloat(initialAmount)
	orderRec.ClosedAmount = ptrFloat(totalClosed)

	if orderRec.StartTime == nil || orderRec.StartTime.IsZero() {
		start := now
		if tradeSnapshot != nil {
			open := parseFreqtradeTime(tradeSnapshot.OpenDate)
			if !open.IsZero() {
				start = open
			}
		}
		orderRec.StartTime = &start
	}
	if orderRec.Price == nil || *orderRec.Price == 0 {
		if tradeSnapshot != nil && tradeSnapshot.OpenRate > 0 {
			orderRec.Price = ptrFloat(tradeSnapshot.OpenRate)
		} else if pe.EntryPrice > 0 {
			orderRec.Price = ptrFloat(pe.EntryPrice)
		}
	}
	if orderRec.StakeAmount == nil || *orderRec.StakeAmount == 0 {
		if tradeSnapshot != nil && tradeSnapshot.StakeAmount > 0 {
			orderRec.StakeAmount = ptrFloat(tradeSnapshot.StakeAmount)
		} else if pe.Stake > 0 {
			orderRec.StakeAmount = ptrFloat(pe.Stake)
		}
	}
	if orderRec.Leverage == nil || *orderRec.Leverage == 0 {
		if tradeSnapshot != nil && tradeSnapshot.Leverage > 0 {
			orderRec.Leverage = ptrFloat(tradeSnapshot.Leverage)
		} else if pe.Leverage > 0 {
			orderRec.Leverage = ptrFloat(pe.Leverage)
		}
	}
	if orderRec.PositionValue == nil || *orderRec.PositionValue == 0 {
		stake := valOrZero(orderRec.StakeAmount)
		lev := valOrZero(orderRec.Leverage)
		if stake > 0 && lev > 0 {
			orderRec.PositionValue = ptrFloat(stake * lev)
		}
	}

	updatePnLFromSnapshot(&orderRec, tradeSnapshot, status)

	tierRec.FreqtradeID = tradeID
	tierRec.Symbol = orderRec.Symbol
	tierRec.UpdatedAt = now
	if tierRec.Timestamp.IsZero() {
		tierRec.Timestamp = now
	}
	if tierRec.CreatedAt.IsZero() {
		tierRec.CreatedAt = now
	}
	remainingRatio := 0.0
	if initialAmount > 0 {
		remainingRatio = clamp01(currentAmount / initialAmount)
	}
	tierRec.RemainingRatio = remainingRatio

	if status == database.LiveOrderStatusClosed {
		tierRec.Tier1Done, tierRec.Tier2Done, tierRec.Tier3Done = true, true, true
		tierRec.RemainingRatio = 0
		tierRec.Status = statusForClose(pe.Operation)
	} else {
		if len(pe.CoveredTiers) == 0 && strings.HasPrefix(strings.ToLower(pe.Kind), "tier") {
			pe.CoveredTiers = []string{strings.ToLower(pe.Kind)}
		}
		for _, tierName := range pe.CoveredTiers {
			switch strings.ToLower(strings.TrimSpace(tierName)) {
			case "tier1":
				if !tierRec.Tier1Done && pe.EntryPrice > 0 {
					old := tierRec.StopLoss
					tierRec.StopLoss = pe.EntryPrice
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
				tierRec.Tier1Done = true
			case "tier2":
				tierRec.Tier2Done = true
			case "tier3":
				tierRec.Tier3Done = true
			}
		}
		if len(pe.CoveredTiers) > 0 {
			last := pe.CoveredTiers[len(pe.CoveredTiers)-1]
			tierRec.Status = statusForTier(strings.ToLower(last))
		}
	}

	if err := m.posRepo.SavePosition(ctx, orderRec, tierRec); err != nil {
		return orderRec, tierRec, 0, err
	}
	m.updateCacheOrderTiers(orderRec, tierRec)

	m.mu.Lock()
	pos := m.positions[tradeID]
	pos.TradeID = tradeID
	pos.Symbol = orderRec.Symbol
	pos.Side = orderRec.Side
	pos.Amount = currentAmount
	pos.Stake = valOrZero(orderRec.StakeAmount)
	pos.Leverage = valOrZero(orderRec.Leverage)
	pos.EntryPrice = valOrZero(orderRec.Price)
	if orderRec.StartTime != nil {
		pos.OpenedAt = *orderRec.StartTime
	}
	if status == database.LiveOrderStatusClosed {
		pos.Closed = true
		pos.ClosedAt = now
		pos.ExitPrice = pe.TargetPrice
		pos.ExitPnLRatio = valOrZero(orderRec.RealizedPnLRatio)
		pos.ExitPnLUSD = valOrZero(orderRec.RealizedPnLUSD)
	} else {
		pos.Closed = false
	}
	m.positions[tradeID] = pos
	m.mu.Unlock()

	return orderRec, tierRec, closedDelta, nil
}

func (m *Manager) restorePendingExitState(ctx context.Context, pe pendingExit, tradeSnapshot *Trade) {
	if m == nil || m.posRepo == nil {
		return
	}
	tradeID := pe.TradeID
	orderRec, tierRec, found, err := m.posRepo.GetPosition(ctx, tradeID)
	if err != nil {
		logger.Warnf("freqtrade pending: 恢复仓位失败 trade=%d err=%v", tradeID, err)
		return
	}
	now := time.Now()
	if !found {
		orderRec = database.LiveOrderRecord{
			FreqtradeID: tradeID,
			Symbol:      strings.ToUpper(firstNonEmpty(pe.Symbol)),
			Side:        strings.ToLower(firstNonEmpty(pe.Side)),
			CreatedAt:   now,
		}
		tierRec = buildPlaceholderTiers(tradeID, orderRec.Symbol)
	}
	orderRec.Symbol = strings.ToUpper(firstNonEmpty(orderRec.Symbol, pe.Symbol))
	orderRec.Side = strings.ToLower(firstNonEmpty(orderRec.Side, pe.Side))
	if orderRec.CreatedAt.IsZero() {
		orderRec.CreatedAt = now
	}
	orderRec.UpdatedAt = now
	if orderRec.StartTime == nil || orderRec.StartTime.IsZero() {
		start := now
		if tradeSnapshot != nil {
			open := parseFreqtradeTime(tradeSnapshot.OpenDate)
			if !open.IsZero() {
				start = open
			}
		}
		orderRec.StartTime = &start
	}
	if orderRec.Price == nil || *orderRec.Price == 0 {
		if tradeSnapshot != nil && tradeSnapshot.OpenRate > 0 {
			orderRec.Price = ptrFloat(tradeSnapshot.OpenRate)
		} else if pe.EntryPrice > 0 {
			orderRec.Price = ptrFloat(pe.EntryPrice)
		}
	}
	if orderRec.StakeAmount == nil || *orderRec.StakeAmount == 0 {
		if tradeSnapshot != nil && tradeSnapshot.StakeAmount > 0 {
			orderRec.StakeAmount = ptrFloat(tradeSnapshot.StakeAmount)
		} else if pe.Stake > 0 {
			orderRec.StakeAmount = ptrFloat(pe.Stake)
		}
	}
	if orderRec.Leverage == nil || *orderRec.Leverage == 0 {
		if tradeSnapshot != nil && tradeSnapshot.Leverage > 0 {
			orderRec.Leverage = ptrFloat(tradeSnapshot.Leverage)
		} else if pe.Leverage > 0 {
			orderRec.Leverage = ptrFloat(pe.Leverage)
		}
	}

	currentAmount := pe.PrevAmount
	if tradeSnapshot != nil {
		if tradeSnapshot.IsOpen {
			currentAmount = tradeSnapshot.Amount
		} else {
			currentAmount = 0
		}
	}
	if currentAmount < 0 {
		currentAmount = 0
	}
	initialAmount := deriveInitialAmount(orderRec, pe)
	if initialAmount <= 0 {
		initialAmount = math.Max(pe.PrevAmount, currentAmount)
	}
	orderRec.Amount = ptrFloat(currentAmount)
	orderRec.InitialAmount = ptrFloat(initialAmount)
	closedAmount := math.Max(0, initialAmount-currentAmount)
	orderRec.ClosedAmount = ptrFloat(closedAmount)

	status := database.LiveOrderStatusOpen
	if currentAmount <= pendingAmountEpsilon {
		status = database.LiveOrderStatusClosed
		end := now
		orderRec.EndTime = &end
	} else if closedAmount > pendingAmountEpsilon {
		status = database.LiveOrderStatusPartial
	}
	orderRec.Status = status

	tierRec.FreqtradeID = tradeID
	tierRec.Symbol = orderRec.Symbol
	tierRec.UpdatedAt = now
	if tierRec.Timestamp.IsZero() {
		tierRec.Timestamp = now
	}
	if tierRec.CreatedAt.IsZero() {
		tierRec.CreatedAt = now
	}
	if initialAmount > 0 {
		tierRec.RemainingRatio = clamp01(currentAmount / initialAmount)
	} else {
		tierRec.RemainingRatio = 0
	}

	if err := m.posRepo.SavePosition(ctx, orderRec, tierRec); err != nil {
		logger.Warnf("freqtrade pending: 恢复仓位写入失败 trade=%d err=%v", tradeID, err)
		return
	}
	m.updateCacheOrderTiers(orderRec, tierRec)

	m.mu.Lock()
	pos := m.positions[tradeID]
	pos.TradeID = tradeID
	pos.Symbol = orderRec.Symbol
	pos.Side = orderRec.Side
	pos.Amount = currentAmount
	pos.Stake = valOrZero(orderRec.StakeAmount)
	pos.Leverage = valOrZero(orderRec.Leverage)
	pos.EntryPrice = valOrZero(orderRec.Price)
	if status == database.LiveOrderStatusClosed {
		pos.Closed = true
		pos.ClosedAt = now
		pos.ExitPrice = pe.TargetPrice
		pos.ExitReason = pe.Kind
	} else {
		pos.Closed = false
		pos.ClosedAt = time.Time{}
		pos.ExitReason = ""
	}
	m.positions[tradeID] = pos
	m.mu.Unlock()

	logger.Infof("pending exit 回滚 trade=%d 恢复状态=%s amount=%.6f", tradeID, statusText(status), currentAmount)
}

func (m *Manager) pendingFromWebhook(ctx context.Context, tradeID int, symbol, side string, closePrice float64, fill float64, msg WebhookMessage) (pendingExit, error) {
	var orderRec database.LiveOrderRecord
	var err error
	if m.posRepo != nil {
		if order, _, ok, e := m.posRepo.GetPosition(ctx, tradeID); e == nil && ok {
			orderRec = order
		} else if e != nil {
			err = e
		}
	}
	if symbol == "" {
		symbol = strings.ToUpper(strings.TrimSpace(orderRec.Symbol))
	}
	if side == "" {
		side = strings.ToLower(strings.TrimSpace(orderRec.Side))
	}
	if symbol == "" || side == "" {
		return pendingExit{}, fmt.Errorf("缺少 trade=%d 仓位信息，无法同步 exit", tradeID)
	}
	prevAmount := valOrZero(orderRec.Amount)
	prevClosed := valOrZero(orderRec.ClosedAmount)
	initial := valOrZero(orderRec.InitialAmount)
	if initial <= 0 {
		total := prevAmount + prevClosed
		if total > 0 {
			initial = total
		} else {
			initial = prevAmount
		}
	}
	targetAmount := prevAmount
	if fill > 0 && prevAmount > 0 {
		if fill > prevAmount {
			fill = prevAmount
		}
		targetAmount = math.Max(0, prevAmount-fill)
	} else if targetAmount < 0 {
		targetAmount = 0
	}
	reason := strings.ToLower(strings.TrimSpace(msg.ExitReason))
	pe := pendingExit{
		TradeID:       tradeID,
		Symbol:        strings.ToUpper(symbol),
		Side:          side,
		Kind:          reason,
		TargetPrice:   closePrice,
		PrevAmount:    prevAmount,
		PrevClosed:    prevClosed,
		InitialAmount: initial,
		TargetAmount:  targetAmount,
		RequestedAt:   time.Now(),
		EntryPrice:    valOrZero(orderRec.Price),
		Stake:         valOrZero(orderRec.StakeAmount),
		Leverage:      valOrZero(orderRec.Leverage),
		Operation:     operationFromReason(reason, database.OperationFailed),
	}
	if err != nil {
		logger.Warnf("freqtrade pending: 获取 trade=%d 仓位信息失败 err=%v，使用缓存", tradeID, err)
	}
	return pe, nil
}

func deriveInitialAmount(order database.LiveOrderRecord, pe pendingExit) float64 {
	initial := valOrZero(order.InitialAmount)
	if initial > 0 {
		return initial
	}
	sum := valOrZero(order.Amount) + valOrZero(order.ClosedAmount)
	if sum > 0 {
		return sum
	}
	if pe.InitialAmount > 0 {
		return pe.InitialAmount
	}
	total := pe.PrevAmount + pe.PrevClosed
	if total > 0 {
		return total
	}
	return pe.PrevAmount
}

func updatePnLFromSnapshot(order *database.LiveOrderRecord, tradeSnapshot *Trade, status database.LiveOrderStatus) {
	if order == nil || tradeSnapshot == nil {
		return
	}
	pnlRatio := tradeSnapshot.ProfitRatio
	if pnlRatio == 0 {
		pnlRatio = tradeSnapshot.CloseProfit
	}
	pnlAbs := tradeSnapshot.ProfitAbs
	if pnlAbs == 0 {
		pnlAbs = tradeSnapshot.CloseProfitAbs
	}

	if status == database.LiveOrderStatusClosed {
		if tradeSnapshot.CloseRate > 0 {
			order.CurrentPrice = ptrFloat(tradeSnapshot.CloseRate)
		}
		if tradeSnapshot.CloseProfit != 0 {
			order.PnLRatio = ptrFloat(tradeSnapshot.CloseProfit)
			order.RealizedPnLRatio = ptrFloat(tradeSnapshot.CloseProfit)
			order.CurrentProfitRatio = ptrFloat(tradeSnapshot.CloseProfit)
		}
		if tradeSnapshot.CloseProfitAbs != 0 {
			order.PnLUSD = ptrFloat(tradeSnapshot.CloseProfitAbs)
			order.RealizedPnLUSD = ptrFloat(tradeSnapshot.CloseProfitAbs)
			order.CurrentProfitAbs = ptrFloat(tradeSnapshot.CloseProfitAbs)
		}
		order.UnrealizedPnLRatio = ptrFloat(0)
		order.UnrealizedPnLUSD = ptrFloat(0)
	} else {
		realizedRatio := tradeSnapshot.CloseProfit
		realizedAbs := tradeSnapshot.CloseProfitAbs
		unrealizedRatio := pnlRatio
		unrealizedAbs := pnlAbs
		if realizedRatio != 0 {
			unrealizedRatio = pnlRatio - realizedRatio
		}
		if realizedAbs != 0 {
			unrealizedAbs = pnlAbs - realizedAbs
		}
		if tradeSnapshot.CurrentRate > 0 {
			order.CurrentPrice = ptrFloat(tradeSnapshot.CurrentRate)
		} else if tradeSnapshot.CloseRate > 0 {
			order.CurrentPrice = ptrFloat(tradeSnapshot.CloseRate)
		}
		order.UnrealizedPnLRatio = ptrFloat(unrealizedRatio)
		order.UnrealizedPnLUSD = ptrFloat(unrealizedAbs)
		order.RealizedPnLRatio = ptrFloat(realizedRatio)
		order.RealizedPnLUSD = ptrFloat(realizedAbs)
		order.CurrentProfitRatio = ptrFloat(pnlRatio)
		order.CurrentProfitAbs = ptrFloat(pnlAbs)
	}
}

func statusForTier(name string) int {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "tier1":
		return 3
	case "tier2":
		return 4
	case "tier3":
		return 5
	default:
		return 0
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		v = strings.TrimSpace(v)
		if v != "" {
			return v
		}
	}
	return ""
}

func clamp01(val float64) float64 {
	if val < 0 {
		return 0
	}
	if val > 1 {
		return 1
	}
	return val
}

func operationFromReason(reason string, fallback database.OperationType) database.OperationType {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "stop_loss":
		return database.OperationStopLoss
	case "take_profit":
		return database.OperationTakeProfit
	case "force_exit":
		return database.OperationForceExit
	case "tier1":
		return database.OperationTier1
	case "tier2":
		return database.OperationTier2
	case "tier3":
		return database.OperationTier3
	default:
		if fallback != 0 {
			return fallback
		}
		return database.OperationFailed
	}
}

func (m *Manager) schedulePositionSummary(tradeID int, trigger string) {
	if m == nil || m.notifier == nil {
		return
	}
	go func() {
		timer := time.NewTimer(positionSummaryDelay)
		defer timer.Stop()
		<-timer.C
		m.sendPositionSummary(context.Background(), tradeID, trigger)
	}()
}

func (m *Manager) sendPositionSummary(ctx context.Context, tradeID int, trigger string) {
	if m == nil || m.posRepo == nil || m.notifier == nil {
		return
	}
	orderRec, tierRec, ok, err := m.posRepo.GetPosition(ctx, tradeID)
	if err != nil {
		logger.Warnf("freqtrade summary: get position failed trade=%d err=%v", tradeID, err)
		return
	}
	if !ok {
		logger.Warnf("freqtrade summary: position missing trade=%d", tradeID)
		return
	}
	realizedUSD := valOrZero(orderRec.RealizedPnLUSD)
	realizedRatio := valOrZero(orderRec.RealizedPnLRatio)
	unrealizedUSD := valOrZero(orderRec.UnrealizedPnLUSD)
	unrealizedRatio := valOrZero(orderRec.UnrealizedPnLRatio)
	totalUSD := realizedUSD + unrealizedUSD
	totalRatio := realizedRatio + unrealizedRatio
	remainingQty := valOrZero(orderRec.Amount)
	remainingPct := clamp01(tierRec.RemainingRatio) * 100
	leverage := valOrZero(orderRec.Leverage)

	lines := []string{
		fmt.Sprintf("Trade #%d %s · %s x%.2f", tradeID, strings.ToUpper(orderRec.Symbol), strings.ToUpper(orderRec.Side), leverage),
		fmt.Sprintf("事件: %s · 延迟回顾(≈3m)", trigger),
		fmt.Sprintf("已实现 %s (ROE %s)", formatUSD(realizedUSD), fmtPercent(realizedRatio)),
		fmt.Sprintf("待实现 %s (ROE %s)", formatUSD(unrealizedUSD), fmtPercent(unrealizedRatio)),
		fmt.Sprintf("合计 %s (ROE %s)", formatUSD(totalUSD), fmtPercent(totalRatio)),
		fmt.Sprintf("剩余 %.2f%% (%s)", remainingPct, formatQty(remainingQty)),
		fmt.Sprintf("TP %s / SL %s", formatPrice(tierRec.TakeProfit), formatPrice(tierRec.StopLoss)),
		fmt.Sprintf("Tier1 %s (%s) %s", formatPrice(tierRec.Tier1), fmtPercent(tierRec.Tier1Ratio), markTierDone(tierRec.Tier1Done)),
		fmt.Sprintf("Tier2 %s (%s) %s", formatPrice(tierRec.Tier2), fmtPercent(tierRec.Tier2Ratio), markTierDone(tierRec.Tier2Done)),
		fmt.Sprintf("Tier3 %s (%s) %s", formatPrice(tierRec.Tier3), fmtPercent(tierRec.Tier3Ratio), markTierDone(tierRec.Tier3Done)),
	}
	m.notify("仓位回顾 📊", lines...)
}

func formatUSD(val float64) string {
	prefix := "+"
	if val < 0 {
		prefix = "-"
		val = math.Abs(val)
	}
	return fmt.Sprintf("%s$%.2f", prefix, val)
}

func fmtPercent(val float64) string {
	return fmt.Sprintf("%.2f%%", val*100)
}

func markTierDone(done bool) string {
	if done {
		return "✅ 已触发"
	}
	return "… 待触发"
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
	if _, err := m.SyncOpenPositions(ctx); err != nil {
		logger.Errorf("freqtrade manager: 启动同步仓位失败 err=%v", err)
	}
}

func (m *Manager) runFastStatusSync(ctx context.Context) {
	if m == nil {
		return
	}
	ticker := time.NewTicker(fastStatusInterval)
	defer ticker.Stop()
	m.syncFastStatus(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.syncFastStatus(ctx)
		}
	}
}

func (m *Manager) syncFastStatus(ctx context.Context) {
	if m == nil || m.client == nil || m.posRepo == nil {
		return
	}
	trades, err := m.fetchTradesWithRetry(ctx, statusRetryMax, true)
	if err != nil || len(trades) == 0 {
		return
	}
	m.applyFastStatusSnapshot(ctx, trades)
}

func (m *Manager) fetchTradesWithRetry(ctx context.Context, maxRetries int, notify bool) ([]Trade, error) {
	if m == nil || m.client == nil {
		return nil, fmt.Errorf("freqtrade client not initialized")
	}
	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		trades, err := m.client.ListTrades(ctx)
		if err == nil {
			return trades, nil
		}
		lastErr = err
		if attempt < maxRetries-1 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(statusRetryInterval):
			}
		}
	}
	if notify && ctx.Err() == nil {
		logger.Warnf("freqtrade fast sync: /status failed %d times: %v", maxRetries, lastErr)
		m.notify("Freqtrade /status 异常 ⚠️",
			fmt.Sprintf("连续失败 %d 次", maxRetries),
			fmt.Sprintf("原因: %v", lastErr),
		)
	}
	return nil, lastErr
}

func (m *Manager) applyFastStatusSnapshot(ctx context.Context, trades []Trade) {
	if m == nil || m.posRepo == nil {
		return
	}
	now := time.Now()
	for _, tr := range trades {
		if (!tr.IsOpen && tr.Amount <= 0) || tr.ID <= 0 {
			continue
		}
		if m.hasPendingExit(tr.ID) {
			continue
		}
		if status, ok := m.cachedOrderStatus(tr.ID); ok && shouldSkipFastSync(status) {
			continue
		}
		symbol := freqtradePairToSymbol(tr.Pair)
		side := strings.ToLower(strings.TrimSpace(tr.Side))
		if side == "" {
			if tr.IsShort {
				side = "short"
			} else {
				side = "long"
			}
		}
		lock := getPositionLock(tr.ID)
		lock.Lock()
		m.applyFastStatusForTrade(ctx, tr, symbol, side, now)
		lock.Unlock()
	}
}

func (m *Manager) applyFastStatusForTrade(ctx context.Context, tr Trade, symbol, side string, ts time.Time) {
	if m == nil || m.posRepo == nil {
		return
	}
	order := database.LiveOrderRecord{
		FreqtradeID:   tr.ID,
		Symbol:        strings.ToUpper(symbol),
		Side:          side,
		Amount:        ptrFloat(tr.Amount),
		InitialAmount: ptrFloat(tr.Amount),
		StakeAmount:   ptrFloat(tr.StakeAmount),
		Leverage:      ptrFloat(tr.Leverage),
		PositionValue: ptrFloat(tr.StakeAmount * tr.Leverage),
		Price:         ptrFloat(tr.OpenRate),
		ClosedAmount:  ptrFloat(0),
		Status:        database.LiveOrderStatusOpen,
		RawData:       marshalRaw(tr),
		CreatedAt:     ts,
		UpdatedAt:     ts,
	}
	lastSync := ts
	order.LastStatusSync = &lastSync
	openTime := parseFreqtradeTime(tr.OpenDate)
	if !openTime.IsZero() {
		order.StartTime = &openTime
	}
	if tr.CurrentRate > 0 {
		order.CurrentPrice = ptrFloat(tr.CurrentRate)
	}
	pnlRatio := tr.ProfitRatio
	pnlAbs := tr.ProfitAbs
	if pnlRatio == 0 && tr.CloseProfit != 0 {
		pnlRatio = tr.CloseProfit
	}
	if pnlAbs == 0 && tr.CloseProfitAbs != 0 {
		pnlAbs = tr.CloseProfitAbs
	}

	order.CurrentProfitRatio = ptrFloat(pnlRatio)
	order.CurrentProfitAbs = ptrFloat(pnlAbs)
	order.UnrealizedPnLRatio = ptrFloat(pnlRatio)
	order.UnrealizedPnLUSD = ptrFloat(pnlAbs)

	if err := m.posRepo.UpsertOrder(ctx, order); err != nil {
		logger.Warnf("freqtrade fast sync: upsert trade=%d err=%v", tr.ID, err)
		return
	}

	m.storeTrade(symbol, side, tr.ID)
	m.mu.Lock()
	pos := m.positions[tr.ID]
	pos.TradeID = tr.ID
	pos.Symbol = symbol
	pos.Side = side
	pos.Amount = tr.Amount
	pos.Stake = tr.StakeAmount
	pos.Leverage = tr.Leverage
	pos.EntryPrice = tr.OpenRate
	if !openTime.IsZero() {
		pos.OpenedAt = openTime
	}
	pos.Closed = false
	m.positions[tr.ID] = pos
	m.mu.Unlock()
}

func (m *Manager) fetchTradeDetailWithRetry(ctx context.Context, tradeID int) (*Trade, error) {
	if m == nil || m.client == nil {
		return nil, fmt.Errorf("freqtrade client not initialized")
	}
	var lastErr error
	for attempt := 0; attempt < statusRetryMax; attempt++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		tr, err := m.client.GetTrade(ctx, tradeID)
		if err == nil {
			return tr, nil
		}
		if errors.Is(err, errTradeNotFound) {
			return nil, err
		}
		lastErr = err
		if attempt < statusRetryMax-1 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(statusRetryInterval):
			}
		}
	}
	if ctx.Err() == nil {
		logger.Warnf("freqtrade trade detail: trade=%d failed %d times: %v", tradeID, statusRetryMax, lastErr)
		m.notify("Freqtrade /trade 异常 ⚠️",
			fmt.Sprintf("trade_id=%d 连续失败 %d 次", tradeID, statusRetryMax),
			fmt.Sprintf("原因: %v", lastErr),
		)
	}
	return nil, lastErr
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
		realizedUSD := valOrZero(p.Order.RealizedPnLUSD)
		if realizedUSD == 0 {
			realizedUSD = valOrZero(p.Order.PnLUSD)
		}
		realizedRatio := valOrZero(p.Order.RealizedPnLRatio)
		if realizedRatio == 0 {
			realizedRatio = valOrZero(p.Order.PnLRatio)
		}
		unrealizedUSD := valOrZero(p.Order.UnrealizedPnLUSD)
		unrealizedRatio := valOrZero(p.Order.UnrealizedPnLRatio)
		currentPrice := valOrZero(p.Order.CurrentPrice)
		if currentPrice > 0 {
			api.CurrentPrice = currentPrice
		}
		if pos, ok := m.positions[p.Order.FreqtradeID]; ok {
			if pos.ExitPrice > 0 {
				api.ExitPrice = pos.ExitPrice
				if api.CurrentPrice == 0 {
					api.CurrentPrice = pos.ExitPrice
				}
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
		api.UnrealizedPnLUSD = unrealizedUSD
		api.UnrealizedPnLRatio = unrealizedRatio
		api.PnLUSD = realizedUSD + unrealizedUSD
		if baseValue > 0 {
			api.PnLRatio = api.PnLUSD / baseValue
		} else if api.PnLRatio == 0 {
			api.PnLRatio = realizedRatio + unrealizedRatio
		}
		if strings.EqualFold(api.Status, "closed") {
			api.UnrealizedPnLUSD = 0
			api.UnrealizedPnLRatio = 0
			api.PnLUSD = realizedUSD
			api.PnLRatio = realizedRatio
			if api.CurrentPrice == 0 && api.ExitPrice > 0 {
				api.CurrentPrice = api.ExitPrice
			}
		}
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
