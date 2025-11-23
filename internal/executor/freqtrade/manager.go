package freqtrade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	brcfg "brale/internal/config"
	"brale/internal/decision"
	"brale/internal/executor/freqtrade/storage"
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

// APIPosition 用于 /api/live/freqtrade/positions 返回的数据结构。
type APIPosition struct {
	TradeID        int                  `json:"trade_id"`
	Symbol         string               `json:"symbol"`
	Side           string               `json:"side"`
	EntryPrice     float64              `json:"entry_price"`
	Amount         float64              `json:"amount"`
	Stake          float64              `json:"stake"`
	Leverage       float64              `json:"leverage"`
	OpenedAt       int64                `json:"opened_at"`
	HoldingMs      int64                `json:"holding_ms"`
	StopLoss       float64              `json:"stop_loss,omitempty"`
	TakeProfit     float64              `json:"take_profit,omitempty"`
	CurrentPrice   float64              `json:"current_price,omitempty"`
	PnLRatio       float64              `json:"pnl_ratio,omitempty"`
	PnLUSD         float64              `json:"pnl_usd,omitempty"`
	RemainingRatio float64              `json:"remaining_ratio,omitempty"`
	Tier1          TierInfo             `json:"tier1"`
	Tier2          TierInfo             `json:"tier2"`
	Tier3          TierInfo             `json:"tier3"`
	TierNotes      string               `json:"tier_notes,omitempty"`
	TierLogs       []TierLog            `json:"tier_logs,omitempty"`
	Events         []storage.TradeEvent `json:"events,omitempty"`
	Status         string               `json:"status"`
	ClosedAt       int64                `json:"closed_at,omitempty"`
	ExitPrice      float64              `json:"exit_price,omitempty"`
	ExitReason     string               `json:"exit_reason,omitempty"`
}

type tradeNotFoundError struct {
	Symbol string
	Side   string
}

func (e tradeNotFoundError) Error() string {
	sym := strings.ToUpper(strings.TrimSpace(e.Symbol))
	if sym == "" {
		sym = "UNKNOWN"
	}
	side := strings.ToLower(strings.TrimSpace(e.Side))
	if side == "" {
		side = "unknown"
	}
	return fmt.Sprintf("freqtrade trade_id not found for %s %s", sym, side)
}

type TierInfo struct {
	Target float64 `json:"target"`
	Ratio  float64 `json:"ratio"`
	Done   bool    `json:"done"`
}

type TierUpdateRequest struct {
	TradeID     int     `json:"trade_id"`
	Symbol      string  `json:"symbol"`
	Side        string  `json:"side"`
	Tier1Target float64 `json:"tier1_target"`
	Tier1Ratio  float64 `json:"tier1_ratio"`
	Tier2Target float64 `json:"tier2_target"`
	Tier2Ratio  float64 `json:"tier2_ratio"`
	Tier3Target float64 `json:"tier3_target"`
	Tier3Ratio  float64 `json:"tier3_ratio"`
	Reason      string  `json:"reason"`
}

// TierLog 暴露存储层的三段式记录，供 HTTP 层响应使用。
type TierLog = storage.TierLog
type TradeEvent = storage.TradeEvent

// PositionListOptions 控制 PositionsForAPI 的筛选行为。
type PositionListOptions struct {
	Symbol      string
	Limit       int // 限制“持仓中”列表数量
	ClosedLimit int // 限制最近平仓的数量
	IncludeLogs bool
	LogsLimit   int
}

// TierPriceQuote 表示用于判断 tiers 的最新价格窗口。
type TierPriceQuote struct {
	Last float64
	High float64
	Low  float64
}

type pendingHit struct {
	target float64
	seenAt time.Time
}

func (q TierPriceQuote) isEmpty() bool {
	return q.Last <= 0 && q.High <= 0 && q.Low <= 0
}

// Manager 提供 freqtrade 执行、日志与持仓同步能力。
type Manager struct {
	client        *Client
	cfg           brcfg.FreqtradeConfig
	logger        Logger
	orderRec      market.Recorder
	traceByKey    map[string]int
	traceByID     map[int]string
	positions     map[int]Position
	mu            sync.Mutex
	horizonName   string
	riskStore     *storage.Store
	riskStorePath string
	notifier      TextNotifier
	tierWatchOnce sync.Once
	tierExecMu    sync.Mutex
	tierExec      map[int]bool
	posSyncOnce   sync.Once
	balanceMu     sync.RWMutex
	balance       Balance
	pendingHitsMu sync.Mutex
	pendingHits   map[string]pendingHit
}

const (
	defaultTier1Ratio      = 0.33
	defaultTier2Ratio      = 0.33
	defaultTier3Ratio      = 0.34
	autoCloseRetryInterval = 30 * time.Second
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

// NewManager 创建 freqtrade 执行管理器。
func NewManager(client *Client, cfg brcfg.FreqtradeConfig, horizon string, logStore Logger, orderRec market.Recorder, notifier TextNotifier) *Manager {
	var risk *storage.Store
	if path := strings.TrimSpace(cfg.RiskStorePath); path != "" {
		store, err := storage.Open(path)
		if err != nil {
			logger.Warnf("freqtrade risk store 打开失败: %v", err)
		} else {
			risk = store
		}
	}
	return &Manager{
		client:        client,
		cfg:           cfg,
		logger:        logStore,
		orderRec:      orderRec,
		traceByKey:    make(map[string]int),
		traceByID:     make(map[int]string),
		positions:     make(map[int]Position),
		horizonName:   horizon,
		riskStore:     risk,
		riskStorePath: strings.TrimSpace(cfg.RiskStorePath),
		notifier:      notifier,
		tierExec:      make(map[int]bool),
		pendingHits:   make(map[string]pendingHit),
	}
}

// DecisionInput 用于执行器的输入。
type DecisionInput struct {
	TraceID  string
	Decision decision.Decision
}

// Execute 根据决策调用 freqtrade ForceEnter/ForceExit。
func (m *Manager) Execute(ctx context.Context, input DecisionInput) error {
	if m.client == nil {
		return fmt.Errorf("freqtrade client not initialized")
	}
	d := input.Decision
	switch d.Action {
	case "open_long", "open_short":
		return m.open(ctx, input.TraceID, d)
	case "close_long", "close_short":
		return m.close(ctx, input.TraceID, d)
	case "adjust_stop_loss":
		return m.adjustStopLoss(ctx, input.TraceID, d)
	case "adjust_take_profit":
		return m.adjustTakeProfit(ctx, input.TraceID, d)
	case "update_tiers":
		return m.updateTiers(ctx, input.TraceID, d)
	default:
		return nil
	}
}

// StartTierWatcher 启用三段式监控，按固定间隔检查是否达到目标价。
func (m *Manager) StartTierWatcher(ctx context.Context, priceFn func(symbol string) TierPriceQuote) {
	if m == nil {
		return
	}
	m.tierWatchOnce.Do(func() {
		if priceFn == nil {
			priceFn = func(string) TierPriceQuote { return TierPriceQuote{} }
		}
		go m.runTierWatcher(ctx, priceFn)
	})
}

// StartPositionSync 按固定频率刷新 freqtrade 仓位，与实际持仓保持一致。
func (m *Manager) StartPositionSync(ctx context.Context) {
	if m == nil {
		return
	}
	m.posSyncOnce.Do(func() {
		go m.runPositionSync(ctx)
	})
}

const tierWatchInterval = 5 * time.Second
const positionSyncInterval = 2 * time.Second
const hitConfirmDelay = 2 * time.Second

func (m *Manager) runTierWatcher(ctx context.Context, priceFn func(symbol string) TierPriceQuote) {
	ticker := time.NewTicker(tierWatchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.evaluateTiers(ctx, priceFn)
		}
	}
}

func (m *Manager) runPositionSync(ctx context.Context) {
	ticker := time.NewTicker(positionSyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.refreshOpenPositions(ctx)
		}
	}
}

func (m *Manager) shouldFireHit(tradeID int, kind string, target float64) bool {
	key := fmt.Sprintf("%d:%s", tradeID, kind)
	now := time.Now()
	m.pendingHitsMu.Lock()
	defer m.pendingHitsMu.Unlock()
	ph, ok := m.pendingHits[key]
	if !ok || !floatEqual(ph.target, target) {
		m.pendingHits[key] = pendingHit{target: target, seenAt: now}
		return false
	}
	if now.Sub(ph.seenAt) >= hitConfirmDelay {
		delete(m.pendingHits, key)
		return true
	}
	return false
}

func (m *Manager) clearPendingHit(tradeID int, kind string) {
	key := fmt.Sprintf("%d:%s", tradeID, kind)
	m.pendingHitsMu.Lock()
	delete(m.pendingHits, key)
	m.pendingHitsMu.Unlock()
}

func (m *Manager) evaluateTiers(ctx context.Context, priceFn func(symbol string) TierPriceQuote) {
	m.mu.Lock()
	positions := make([]Position, 0, len(m.positions))
	for _, pos := range m.positions {
		positions = append(positions, pos)
	}
	m.mu.Unlock()
	if len(positions) == 0 {
		return
	}
	for _, pos := range positions {
		if pos.TradeID <= 0 || strings.TrimSpace(pos.Symbol) == "" {
			continue
		}
		rec, ok := m.loadRiskRecord(ctx, pos.TradeID)
		if !ok {
			continue
		}
		var quote TierPriceQuote
		if priceFn != nil {
			quote = priceFn(pos.Symbol)
		}
		if quote.isEmpty() {
			continue
		}
		side := strings.ToLower(pos.Side)
		if price, hit := priceForStopLoss(side, quote, rec.StopLoss); hit {
			if m.shouldFireHit(pos.TradeID, "stop_loss", rec.StopLoss) {
				m.forceClose(ctx, pos, "stop_loss", price, rec)
			}
			continue
		} else {
			m.clearPendingHit(pos.TradeID, "stop_loss")
		}
		if price, hit := priceForTakeProfit(side, quote, rec.TakeProfit); hit {
			if m.shouldFireHit(pos.TradeID, "take_profit", rec.TakeProfit) {
				m.forceClose(ctx, pos, "take_profit", price, rec)
			}
			continue
		} else {
			m.clearPendingHit(pos.TradeID, "take_profit")
		}
		tierName, target, ratio := nextPendingTier(rec)
		if tierName == "" || ratio <= 0 || target <= 0 {
			continue
		}
		if price, hit := priceForTierTrigger(side, quote, target); hit {
			if m.shouldFireHit(pos.TradeID, tierName, target) {
				m.triggerTier(ctx, tierName, price, pos, rec)
			}
		} else {
			m.clearPendingHit(pos.TradeID, tierName)
		}
	}
}

func (m *Manager) open(ctx context.Context, traceID string, d decision.Decision) error {
	pair := formatFreqtradePair(d.Symbol)
	if pair == "" {
		return fmt.Errorf("unable to convert symbol %s to freqtrade pair", d.Symbol)
	}
	side := deriveSide(d.Action)
	if side == "" {
		return fmt.Errorf("unsupported action: %s", d.Action)
	}
	stake := d.PositionSizeUSD
	if stake <= 0 {
		if m.cfg.DefaultStakeUSD > 0 {
			stake = m.cfg.DefaultStakeUSD
		}
	}
	if stake <= 0 {
		return fmt.Errorf("stake amount missing")
	}
	lev := float64(d.Leverage)
	if lev <= 0 {
		if m.cfg.DefaultLeverage > 0 {
			lev = float64(m.cfg.DefaultLeverage)
		}
	}

	payload := ForceEnterPayload{
		Pair:        pair,
		Side:        side,
		StakeAmount: stake,
		EntryTag:    m.entryTag(),
	}
	if lev > 0 {
		payload.Leverage = lev
	}

	recData := map[string]any{"payload": payload, "status": "forceenter"}
	resp, err := m.client.ForceEnter(ctx, payload)
	if err != nil {
		m.logExecutor(ctx, traceID, d, 0, "forceenter_error", recData, err)
		m.notify("Freqtrade 建仓请求失败 ❌",
			fmt.Sprintf("标的: %s (%s)", strings.ToUpper(d.Symbol), pair),
			fmt.Sprintf("方向: %s  杠杆: x%.2f", strings.ToUpper(side), lev),
			fmt.Sprintf("仓位: %.2f USDT", stake),
			fmt.Sprintf("Trace: %s", m.ensureTrace(traceID)),
			fmt.Sprintf("错误: %v", err),
		)
		return err
	}
	m.storeTrade(d.Symbol, side, resp.TradeID)
	m.storeTrace(resp.TradeID, traceID)
	m.logExecutor(ctx, traceID, d, resp.TradeID, "forceenter_success", recData, nil)
	initialRisk := storage.RiskRecord{
		TradeID:        resp.TradeID,
		Symbol:         strings.ToUpper(strings.TrimSpace(d.Symbol)),
		Pair:           pair,
		Side:           side,
		Leverage:       lev,
		StopLoss:       d.StopLoss,
		TakeProfit:     d.TakeProfit,
		Reason:         strings.TrimSpace(d.Reasoning),
		Source:         "open",
		Status:         "forceenter_success",
		UpdatedAt:      time.Now(),
		Stake:          stake,
		RemainingRatio: 1,
	}
	m.applyTiersFromDecision(&initialRisk, d.Tiers)
	m.recordRisk(initialRisk)
	m.recordEvent(initialRisk, "open", "forceenter_success", map[string]any{
		"trace_id":   m.ensureTrace(traceID),
		"leverage":   lev,
		"stake":      stake,
		"entry_side": side,
	})
	lines := []string{
		fmt.Sprintf("交易ID: %d", resp.TradeID),
		fmt.Sprintf("标的: %s (%s)", strings.ToUpper(d.Symbol), pair),
		fmt.Sprintf("方向: %s  杠杆: x%.2f", strings.ToUpper(side), lev),
		fmt.Sprintf("仓位: %.2f USDT", stake),
		fmt.Sprintf("止损/止盈: %.4f / %.4f", d.StopLoss, d.TakeProfit),
		fmt.Sprintf("三段: T1 %.4f(%.0f%%) T2 %.4f(%.0f%%) T3 %.4f(%.0f%%)",
			initialRisk.Tier1Target, initialRisk.Tier1Ratio*100,
			initialRisk.Tier2Target, initialRisk.Tier2Ratio*100,
			initialRisk.Tier3Target, initialRisk.Tier3Ratio*100),
		fmt.Sprintf("Trace: %s", m.ensureTrace(traceID)),
	}
	if reason := shortReason(d.Reasoning); reason != "" {
		lines = append(lines, "理由: "+reason)
	}
	m.notify("Freqtrade 建仓请求已发送 ✅", lines...)
	return nil
}

func (m *Manager) close(ctx context.Context, traceID string, d decision.Decision) error {
	side := deriveSide(d.Action)
	if side == "" {
		return fmt.Errorf("unsupported action: %s", d.Action)
	}
	tradeID, ok := m.lookupTrade(d.Symbol, side)
	if !ok {
		return tradeNotFoundError{Symbol: d.Symbol, Side: side}
	}
	payload := ForceExitPayload{TradeID: fmt.Sprintf("%d", tradeID)}
	ratio := clampCloseRatio(d.CloseRatio)
	if ratio > 0 && ratio < 1 {
		if amt := m.lookupAmount(ctx, tradeID); amt > 0 {
			payload.Amount = amt * ratio
		}
	}
	recData := map[string]any{"payload": payload, "status": "forceexit", "close_ratio": d.CloseRatio}
	if err := m.client.ForceExit(ctx, payload); err != nil {
		m.logExecutor(ctx, traceID, d, tradeID, "forceexit_error", recData, err)
		m.notify("Freqtrade 平仓指令失败 ❌",
			fmt.Sprintf("交易ID: %d", tradeID),
			fmt.Sprintf("标的: %s", strings.ToUpper(d.Symbol)),
			fmt.Sprintf("方向: %s", strings.ToUpper(side)),
			fmt.Sprintf("平仓比例: %.2f", clampCloseRatio(d.CloseRatio)),
			fmt.Sprintf("Trace: %s", m.ensureTrace(traceID)),
			fmt.Sprintf("错误: %v", err),
		)
		return err
	}
	m.logExecutor(ctx, traceID, d, tradeID, "forceexit_success", recData, nil)
	partial := ratio > 0 && ratio < 1 && payload.Amount > 0
	if !partial {
		m.deleteTrade(d.Symbol, side)
	}
	return nil
}

func (m *Manager) guardStopLossChange(pos Position, rec storage.RiskRecord, newStop float64) error {
	entry := pos.EntryPrice
	if entry <= 0 || newStop <= 0 {
		return nil
	}
	side := strings.ToLower(strings.TrimSpace(rec.Side))
	isBreakevenMove := false
	switch side {
	case "short":
		isBreakevenMove = newStop <= entry
	default: // long as default
		isBreakevenMove = newStop >= entry
	}
	if isBreakevenMove && !rec.Tier1Done {
		return fmt.Errorf("tier1 未完成，拒绝将止损调整至入场价/保本")
	}
	if !rec.Tier1Done && m.cfg.MinStopDistancePct > 0 {
		dist := math.Abs(entry-newStop) / entry
		if dist < m.cfg.MinStopDistancePct {
			return fmt.Errorf("止损距离入场过近(%.2f%% < %.2f%%)", dist*100, m.cfg.MinStopDistancePct*100)
		}
	}
	return nil
}

func (m *Manager) adjustStopLoss(ctx context.Context, traceID string, d decision.Decision) error {
	if d.StopLoss <= 0 {
		return fmt.Errorf("invalid stop_loss for adjust_stop_loss")
	}
	tradeID, side, pos, ok := m.findActiveTrade(d.Symbol)
	if !ok {
		return fmt.Errorf("freqtrade 没有 %s 的持仓，无法调整止损", d.Symbol)
	}
	pair := formatFreqtradePair(d.Symbol)
	if pair == "" {
		return fmt.Errorf("无法转换 symbol %s 为 freqtrade pair", d.Symbol)
	}
	existing, _ := m.loadRiskRecord(ctx, tradeID)
	prevStop := existing.StopLoss
	prevTP := existing.TakeProfit
	newStop := d.StopLoss
	newTP := prevTP
	if d.TakeProfit > 0 {
		newTP = d.TakeProfit
	}
	changedStop := !floatEqual(prevStop, newStop)
	changedTP := newTP > 0 && !floatEqual(prevTP, newTP)
	if !changedStop && !changedTP {
		logger.Infof("freqtrade stop-loss 调整无变化 trade_id=%d", tradeID)
		return nil
	}
	if changedStop {
		if err := m.guardStopLossChange(pos, existing, newStop); err != nil {
			logger.Warnf("freqtrade stop-loss 调整被拒绝 trade_id=%d: %v", tradeID, err)
			return err
		}
	}
	status := "stoploss_recorded"
	lines := []string{
		fmt.Sprintf("交易ID: %d", tradeID),
		fmt.Sprintf("标的: %s", strings.ToUpper(d.Symbol)),
		fmt.Sprintf("方向: %s", strings.ToUpper(side)),
	}
	if changedStop {
		lines = append(lines, fmt.Sprintf("止损: %s → %.4f", formatPrice(prevStop), newStop))
	}
	if changedTP {
		lines = append(lines, fmt.Sprintf("止盈: %s → %.4f", formatPrice(prevTP), newTP))
	}
	lines = append(lines, fmt.Sprintf("Trace: %s", m.ensureTrace(traceID)))
	if reason := shortReason(d.Reasoning); reason != "" {
		lines = append(lines, "理由: "+reason)
	}
	updatedRec := storage.RiskRecord{
		TradeID:    tradeID,
		Symbol:     strings.ToUpper(strings.TrimSpace(d.Symbol)),
		Pair:       pair,
		Side:       side,
		EntryPrice: pos.EntryPrice,
		Stake:      pos.Stake,
		Amount:     pos.Amount,
		Leverage:   pos.Leverage,
		StopLoss:   newStop,
		TakeProfit: newTP,
		Reason:     strings.TrimSpace(d.Reasoning),
		Source:     "adjust_stop_loss",
		Status:     status,
		UpdatedAt:  time.Now(),
	}
	copyTierState(&updatedRec, existing)
	m.recordRisk(updatedRec)
	m.recordEvent(updatedRec, "adjust_stop_loss", strings.TrimSpace(d.Reasoning), map[string]any{
		"prev_stop": prevStop,
		"new_stop":  newStop,
		"prev_tp":   prevTP,
		"new_tp":    newTP,
	})
	logger.Infof("freqtrade stop-loss 记录已更新 trade_id=%d %s stop=%.4f tp=%.4f", tradeID, d.Symbol, newStop, newTP)
	title := "止损已更新 ✅"
	if changedStop && changedTP {
		title = "止损/止盈已更新 ✅"
	} else if !changedStop && changedTP {
		title = "止盈已更新 ✅"
	}
	m.notify(title, lines...)
	recData := map[string]any{
		"trade_id":  tradeID,
		"symbol":    strings.ToUpper(strings.TrimSpace(d.Symbol)),
		"prev_stop": formatPrice(prevStop),
		"new_stop":  newStop,
		"prev_tp":   formatPrice(prevTP),
		"new_tp":    newTP,
	}
	m.logExecutor(ctx, traceID, d, tradeID, status, recData, nil)
	return nil
}

func (m *Manager) adjustTakeProfit(ctx context.Context, traceID string, d decision.Decision) error {
	if d.TakeProfit <= 0 {
		return fmt.Errorf("invalid take_profit for adjust_take_profit")
	}
	tradeID, side, pos, ok := m.findActiveTrade(d.Symbol)
	if !ok {
		return fmt.Errorf("freqtrade 没有 %s 的持仓，无法调整止盈", d.Symbol)
	}
	pair := formatFreqtradePair(d.Symbol)
	if pair == "" {
		return fmt.Errorf("无法转换 symbol %s 为 freqtrade pair", d.Symbol)
	}
	existing, _ := m.loadRiskRecord(ctx, tradeID)
	prevTP := existing.TakeProfit
	prevStop := existing.StopLoss
	newTP := d.TakeProfit
	newStop := prevStop
	if d.StopLoss > 0 {
		newStop = d.StopLoss
	}
	changedTP := !floatEqual(prevTP, newTP)
	changedStop := newStop > 0 && !floatEqual(prevStop, newStop)
	if !changedTP && !changedStop {
		logger.Infof("freqtrade take-profit 调整无变化 trade_id=%d", tradeID)
		return nil
	}
	status := "takeprofit_recorded"
	lines := []string{
		fmt.Sprintf("交易ID: %d", tradeID),
		fmt.Sprintf("标的: %s", strings.ToUpper(d.Symbol)),
		fmt.Sprintf("方向: %s", strings.ToUpper(side)),
	}
	if changedTP {
		lines = append(lines, fmt.Sprintf("止盈: %s → %.4f", formatPrice(prevTP), newTP))
	}
	if changedStop {
		lines = append(lines, fmt.Sprintf("止损: %s → %.4f", formatPrice(prevStop), newStop))
	}
	lines = append(lines, fmt.Sprintf("Trace: %s", m.ensureTrace(traceID)))
	if reason := shortReason(d.Reasoning); reason != "" {
		lines = append(lines, "理由: "+reason)
	}
	updatedRec := storage.RiskRecord{
		TradeID:    tradeID,
		Symbol:     strings.ToUpper(strings.TrimSpace(d.Symbol)),
		Pair:       pair,
		Side:       side,
		EntryPrice: pos.EntryPrice,
		Stake:      pos.Stake,
		Amount:     pos.Amount,
		Leverage:   pos.Leverage,
		StopLoss:   newStop,
		TakeProfit: newTP,
		Reason:     strings.TrimSpace(d.Reasoning),
		Source:     "adjust_take_profit",
		Status:     status,
		UpdatedAt:  time.Now(),
	}
	copyTierState(&updatedRec, existing)
	m.recordRisk(updatedRec)
	m.recordEvent(updatedRec, "adjust_take_profit", strings.TrimSpace(d.Reasoning), map[string]any{
		"prev_tp":   prevTP,
		"new_tp":    newTP,
		"prev_stop": prevStop,
		"new_stop":  newStop,
	})
	logger.Infof("freqtrade take-profit 记录已更新 trade_id=%d %s tp=%.4f sl=%.4f", tradeID, d.Symbol, newTP, newStop)
	title := "止盈已更新 ✅"
	if changedTP && changedStop {
		title = "止盈/止损已更新 ✅"
	} else if !changedTP && changedStop {
		title = "止损已更新 ✅"
	}
	m.notify(title, lines...)
	recData := map[string]any{
		"trade_id":  tradeID,
		"symbol":    strings.ToUpper(strings.TrimSpace(d.Symbol)),
		"prev_tp":   formatPrice(prevTP),
		"new_tp":    newTP,
		"prev_stop": formatPrice(prevStop),
		"new_stop":  newStop,
	}
	m.logExecutor(ctx, traceID, d, tradeID, status, recData, nil)
	return nil
}

func (m *Manager) updateTiers(ctx context.Context, traceID string, d decision.Decision) error {
	if d.Tiers == nil {
		return fmt.Errorf("update_tiers 缺少 tiers 字段")
	}
	return m.applyTierUpdate(ctx, traceID, d.Symbol, "", d.Tiers, strings.TrimSpace(d.Reasoning), "update_tiers", true)
}

func (m *Manager) UpdateTiersManual(ctx context.Context, req TierUpdateRequest) error {
	if req.TradeID <= 0 {
		return fmt.Errorf("trade_id 必填")
	}
	tiers := &decision.DecisionTiers{
		Tier1Target: req.Tier1Target,
		Tier1Ratio:  req.Tier1Ratio,
		Tier2Target: req.Tier2Target,
		Tier2Ratio:  req.Tier2Ratio,
		Tier3Target: req.Tier3Target,
		Tier3Ratio:  req.Tier3Ratio,
	}
	symbol := req.Symbol
	if symbol == "" {
		if rec, ok := m.loadRiskRecord(ctx, req.TradeID); ok && rec.Symbol != "" {
			symbol = rec.Symbol
		}
	}
	traceID := m.ensureTrace(fmt.Sprintf("manual-tier-%d", time.Now().UnixNano()))
	return m.applyTierUpdate(ctx, traceID, symbol, req.Side, tiers, strings.TrimSpace(req.Reason), "manual", false)
}

// UpdateFreqtradeTiers implements the interface expected by LiveService/router.
func (m *Manager) UpdateFreqtradeTiers(ctx context.Context, req TierUpdateRequest) error {
	return m.UpdateTiersManual(ctx, req)
}

// ListTierLogs exposes tier change history for HTTP layer.
func (m *Manager) ListTierLogs(ctx context.Context, tradeID int, limit int) ([]storage.TierLog, error) {
	return m.loadTierLogs(ctx, tradeID, limit)
}

// ListTradeEvents exposes the timeline of a trade for HTTP layer.
func (m *Manager) ListTradeEvents(ctx context.Context, tradeID int, limit int) ([]storage.TradeEvent, error) {
	store := m.ensureRiskStore()
	if store == nil {
		return nil, fmt.Errorf("risk store 未初始化")
	}
	if limit <= 0 {
		limit = 100
	}
	return store.ListEvents(ctx, tradeID, limit)
}

// HandleWebhook 由 HTTP 路由调用，负责更新持仓与日志。
func (m *Manager) HandleWebhook(ctx context.Context, msg WebhookMessage) {
	typ := strings.ToLower(strings.TrimSpace(msg.Type))
	switch typ {
	case "entry", "entry_fill":
		m.handleEntry(ctx, msg, typ == "entry_fill")
	case "entry_cancel":
		m.handleEntryCancel(ctx, msg)
	case "exit", "exit_fill":
		m.handleExit(ctx, msg, typ)
	case "exit_cancel":
		m.handleExitCancel(ctx, msg)
	default:
		logger.Debugf("freqtrade manager: ignore webhook type %s", msg.Type)
	}
}

// SyncOpenPositions loads existing open trades from freqtrade to seed the cache.
func (m *Manager) SyncOpenPositions(ctx context.Context) (int, error) {
	if m == nil || m.client == nil {
		return 0, fmt.Errorf("freqtrade client 未初始化")
	}
	trades, err := m.client.ListTrades(ctx)
	if err != nil {
		return 0, err
	}
	count := m.applyTradeSnapshot(trades, true)
	m.refreshBalance(ctx)
	return count, nil
}

func (m *Manager) refreshOpenPositions(ctx context.Context) {
	if m == nil || m.client == nil {
		return
	}
	callCtx := ctx
	if callCtx == nil {
		callCtx = context.Background()
	}
	cctx, cancel := context.WithTimeout(callCtx, 5*time.Second)
	defer cancel()
	trades, err := m.client.ListTrades(cctx)
	if err != nil {
		logger.Warnf("freqtrade 仓位刷新失败: %v", err)
		return
	}
	m.applyTradeSnapshot(trades, true)
	m.refreshBalance(ctx)
}

func (m *Manager) refreshBalance(ctx context.Context) {
	if m == nil || m.client == nil {
		return
	}
	callCtx := ctx
	if callCtx == nil {
		callCtx = context.Background()
	}
	cctx, cancel := context.WithTimeout(callCtx, 3*time.Second)
	defer cancel()
	bal, err := m.client.GetBalance(cctx)
	if err != nil {
		logger.Warnf("freqtrade balance 查询失败: %v", err)
		return
	}
	m.balanceMu.Lock()
	m.balance = bal
	m.balanceMu.Unlock()
}

func (m *Manager) applyTradeSnapshot(trades []Trade, recordStore bool) int {
	if m == nil {
		return 0
	}
	if len(trades) == 0 {
		m.mu.Lock()
		hasCached := len(m.positions) > 0
		m.mu.Unlock()
		if hasCached {
			logger.Debugf("freqtrade manager: ListTrades 返回空，保留本地持仓缓存以防误清")
		}
		return 0
	}
	openIDs := make(map[int]struct{})
	count := 0
	for _, tr := range trades {
		if !tr.IsOpen {
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
		if symbol == "" || side == "" {
			continue
		}
		openIDs[tr.ID] = struct{}{}
		m.mergePositionFromTrade(tr)
		if recordStore {
			m.storeTrade(symbol, side, tr.ID)
		}
		count++
	}
	m.markPositionsClosed(openIDs)
	m.retryPendingAutoCloses(openIDs)
	return count
}

// Positions 返回当前 freqtrade 持仓快照。
func (m *Manager) Positions() []decision.PositionSnapshot {
	list := m.PositionsForAPI(context.Background(), PositionListOptions{})
	if len(list) == 0 {
		return nil
	}
	out := make([]decision.PositionSnapshot, 0, len(list))
	for _, pos := range list {
		if !strings.EqualFold(pos.Status, "open") {
			continue
		}
		snap := apiPositionToSnapshot(pos)
		out = append(out, snap)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (m *Manager) entryTag() string {
	tag := fmt.Sprintf("brale_%s", strings.ToLower(strings.TrimSpace(m.horizonName)))
	if tag == "" || tag == "brale_" {
		return "brale"
	}
	return tag
}

// AccountBalance 返回最近一次同步的账户余额信息。
func (m *Manager) AccountBalance() Balance {
	m.balanceMu.RLock()
	defer m.balanceMu.RUnlock()
	return m.balance
}

func (m *Manager) findActiveTrade(symbol string) (int, string, Position, bool) {
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	if symbol == "" {
		return 0, "", Position{}, false
	}
	if id, ok := m.lookupTrade(symbol, "long"); ok {
		if pos, ok := m.positionByID(id); ok {
			return id, "long", pos, true
		}
		return id, "long", Position{}, true
	}
	if id, ok := m.lookupTrade(symbol, "short"); ok {
		if pos, ok := m.positionByID(id); ok {
			return id, "short", pos, true
		}
		return id, "short", Position{}, true
	}
	return 0, "", Position{}, false
}

func (m *Manager) handleEntry(ctx context.Context, msg WebhookMessage, filled bool) {
	tradeID := int(msg.TradeID)
	symbol := freqtradePairToSymbol(msg.Pair)
	side := strings.ToLower(strings.TrimSpace(msg.Direction))
	price := firstNonZero(float64(msg.OpenRate), float64(msg.OrderRate))
	amount := float64(msg.Amount)
	stake := float64(msg.StakeAmount)
	openTime := parseFreqtradeTime(msg.OpenDate)
	traceID := m.lookupTrace(tradeID)
	if traceID == "" {
		traceID = m.ensureTrace(fmt.Sprintf("%d", tradeID))
	}
	pos := Position{
		TradeID:    tradeID,
		Symbol:     symbol,
		Side:       side,
		Amount:     amount,
		Stake:      stake,
		Leverage:   float64(msg.Leverage),
		EntryPrice: price,
		OpenedAt:   openTime,
	}
	pos.Closed = false
	pos.ClosedAt = time.Time{}
	pos.ExitPrice = 0
	pos.ExitReason = ""
	pos.ExitPnLRatio = 0
	pos.ExitPnLUSD = 0
	if !filled && amount <= 0 {
		m.storeTrade(symbol, side, tradeID)
		logger.Infof("freqtrade manager: entry submitted trade_id=%d %s %s price=%.4f", tradeID, symbol, side, price)
		m.notify("Freqtrade 建仓下单 ⌛",
			fmt.Sprintf("交易ID: %d", tradeID),
			fmt.Sprintf("标的: %s (%s)", strings.ToUpper(symbol), msg.Pair),
			fmt.Sprintf("方向: %s  杠杆: x%.2f", strings.ToUpper(side), float64(msg.Leverage)),
			fmt.Sprintf("委托: %.2f USDT 价格: %s", stake, formatPrice(price)),
			fmt.Sprintf("Trace: %s", traceID),
		)
		return
	}
	m.mu.Lock()
	m.positions[tradeID] = pos
	m.mu.Unlock()
	m.storeTrade(symbol, side, tradeID)
	logger.Infof("freqtrade manager: entry filled trade_id=%d %s %s price=%.4f qty=%.4f", tradeID, symbol, side, price, amount)
	m.logWebhook(ctx, traceID, tradeID, symbol, "entry_fill", msg)
	m.recordOrder(ctx, msg, freqtradeAction(side, false), price, openTime)
	m.notify("Freqtrade 建仓成交 ✅",
		fmt.Sprintf("交易ID: %d", tradeID),
		fmt.Sprintf("标的: %s (%s)", strings.ToUpper(symbol), msg.Pair),
		fmt.Sprintf("方向: %s  杠杆: x%.2f", strings.ToUpper(side), pos.Leverage),
		fmt.Sprintf("数量: %s  仓位: %.2f USDT", formatQty(amount), stake),
		fmt.Sprintf("成交价: %s", formatPrice(price)),
		fmt.Sprintf("开仓时间: %s", formatTime(openTime)),
		fmt.Sprintf("Trace: %s", traceID),
	)
}

func (m *Manager) handleEntryCancel(ctx context.Context, msg WebhookMessage) {
	tradeID := int(msg.TradeID)
	symbol := freqtradePairToSymbol(msg.Pair)
	side := strings.ToLower(strings.TrimSpace(msg.Direction))
	m.deleteTrade(symbol, side)
	m.mu.Lock()
	delete(m.positions, tradeID)
	m.mu.Unlock()
	traceID := m.lookupTrace(tradeID)
	m.deleteTrace(tradeID)
	logger.Infof("freqtrade manager: entry cancel trade_id=%d %s %s reason=%s", tradeID, symbol, side, msg.Reason)
	m.logWebhook(ctx, traceID, tradeID, symbol, "entry_cancel", msg)
	m.notify("Freqtrade 建仓被取消 ⚠️",
		fmt.Sprintf("交易ID: %d", tradeID),
		fmt.Sprintf("标的: %s (%s)", strings.ToUpper(symbol), msg.Pair),
		fmt.Sprintf("方向: %s", strings.ToUpper(side)),
		fmt.Sprintf("原因: %s", strings.TrimSpace(msg.Reason)),
		fmt.Sprintf("Trace: %s", traceID),
	)
}

func (m *Manager) handleExit(ctx context.Context, msg WebhookMessage, event string) {
	tradeID := int(msg.TradeID)
	m.mu.Lock()
	pos, _ := m.positions[tradeID]
	m.mu.Unlock()
	symbol := pos.Symbol
	if symbol == "" {
		symbol = freqtradePairToSymbol(msg.Pair)
	}
	side := pos.Side
	if side == "" {
		side = strings.ToLower(strings.TrimSpace(msg.Direction))
	}
	closePrice := firstNonZero(float64(msg.CloseRate), float64(msg.OrderRate))
	stake := float64(msg.StakeAmount)
	closedAt := parseFreqtradeTime(msg.CloseDate)
	pnlRatio := parseProfitRatio(msg.ProfitRatio)
	pos = m.markPositionClosed(tradeID, symbol, side, closePrice, stake, msg.ExitReason, closedAt, pnlRatio)
	traceID := m.lookupTrace(tradeID)
	m.deleteTrace(tradeID)
	logger.Infof("freqtrade manager: exit trade_id=%d %s %s price=%.4f reason=%s", tradeID, symbol, side, closePrice, msg.ExitReason)
	m.logWebhook(ctx, traceID, tradeID, symbol, event, msg)
	m.recordOrder(ctx, msg, freqtradeAction(side, true), closePrice, parseFreqtradeTime(msg.CloseDate))
	m.finalizeRiskAfterClose(tradeID, msg.ExitReason, closePrice)
}

func (m *Manager) handleExitCancel(ctx context.Context, msg WebhookMessage) {
	tradeID := int(msg.TradeID)
	traceID := m.lookupTrace(tradeID)
	logger.Infof("freqtrade manager: exit cancel trade_id=%d", tradeID)
	m.logWebhook(ctx, traceID, tradeID, freqtradePairToSymbol(msg.Pair), "exit_cancel", msg)
	m.notify("Freqtrade 平仓被取消 ⚠️",
		fmt.Sprintf("交易ID: %d", tradeID),
		fmt.Sprintf("标的: %s", freqtradePairToSymbol(msg.Pair)),
		fmt.Sprintf("原因: %s", strings.TrimSpace(msg.Reason)),
		fmt.Sprintf("Trace: %s", traceID),
	)
}

func (m *Manager) logExecutor(ctx context.Context, traceID string, d decision.Decision, tradeID int, status string, extra map[string]any, execErr error) {
	if m.logger == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	sym := strings.ToUpper(strings.TrimSpace(d.Symbol))
	rec := database.DecisionLogRecord{
		TraceID:    m.ensureTrace(traceID),
		Timestamp:  time.Now().UnixMilli(),
		Horizon:    m.horizonName,
		ProviderID: "freqtrade",
		Stage:      "executor",
		Symbols:    []string{sym},
		Decisions:  []decision.Decision{d},
		Note:       status,
	}
	if extra == nil {
		extra = make(map[string]any)
	}
	extra["status"] = status
	extra["trade_id"] = tradeID
	extra["symbol"] = sym
	extra["action"] = d.Action
	rec.RawOutput = fmt.Sprintf("freqtrade %s %s %s", status, sym, d.Action)
	if execErr != nil {
		rec.Error = execErr.Error()
		extra["error"] = execErr.Error()
	}
	if data, err := json.Marshal(extra); err == nil {
		rec.RawJSON = string(data)
	}
	if _, err := m.logger.Insert(ctx, rec); err != nil {
		logger.Warnf("freqtrade executor log failed: %v", err)
	}
}

func (m *Manager) logWebhook(ctx context.Context, traceID string, tradeID int, symbol, event string, msg WebhookMessage) {
	if m.logger == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	rec := database.DecisionLogRecord{
		TraceID:    m.ensureTrace(traceID),
		Timestamp:  time.Now().UnixMilli(),
		Horizon:    m.horizonName,
		ProviderID: "freqtrade",
		Stage:      "freqtrade",
		Symbols:    []string{strings.ToUpper(symbol)},
		Note:       strings.ToLower(event),
	}
	rec.RawOutput = fmt.Sprintf("%s trade_id=%d symbol=%s amount=%.4f price=%.4f reason=%s",
		strings.ToUpper(event), tradeID, symbol, float64(msg.Amount), firstNonZero(float64(msg.CloseRate), float64(msg.OpenRate)), strings.TrimSpace(msg.Reason))
	if data, err := json.Marshal(msg); err == nil {
		rec.RawJSON = string(data)
	}
	if _, err := m.logger.Insert(ctx, rec); err != nil {
		logger.Warnf("freqtrade webhook log failed: %v", err)
	}
}

func (m *Manager) PositionsForAPI(ctx context.Context, opts PositionListOptions) []APIPosition {
	symbol := strings.ToUpper(strings.TrimSpace(opts.Symbol))
	openLimit := opts.Limit
	if openLimit < 0 {
		openLimit = 0
	}
	closedLimit := opts.ClosedLimit
	if closedLimit < 0 {
		closedLimit = 0
	}
	logsLimit := opts.LogsLimit
	if logsLimit <= 0 {
		logsLimit = 20
	}
	includeLogs := opts.IncludeLogs
	m.mu.Lock()
	positions := make([]Position, 0, len(m.positions))
	for _, pos := range m.positions {
		positions = append(positions, pos)
	}
	m.mu.Unlock()
	if len(positions) == 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	now := time.Now()
	list := make([]APIPosition, 0, len(positions))
	for _, pos := range positions {
		if symbol != "" && !strings.EqualFold(pos.Symbol, symbol) {
			continue
		}
		opened := int64(0)
		holding := int64(0)
		closedAt := int64(0)
		status := "open"
		if !pos.OpenedAt.IsZero() {
			opened = pos.OpenedAt.UnixMilli()
			holding = now.Sub(pos.OpenedAt).Milliseconds()
		}
		if pos.Closed {
			status = "closed"
			if !pos.ClosedAt.IsZero() {
				closedAt = pos.ClosedAt.UnixMilli()
				if !pos.OpenedAt.IsZero() {
					holding = pos.ClosedAt.Sub(pos.OpenedAt).Milliseconds()
				}
			}
		}
		rec, _ := m.loadRiskRecord(ctx, pos.TradeID)
		api := APIPosition{
			TradeID:        pos.TradeID,
			Symbol:         strings.ToUpper(pos.Symbol),
			Side:           strings.ToUpper(pos.Side),
			EntryPrice:     pos.EntryPrice,
			Amount:         pos.Amount,
			Stake:          pos.Stake,
			Leverage:       pos.Leverage,
			OpenedAt:       opened,
			HoldingMs:      holding,
			StopLoss:       rec.StopLoss,
			TakeProfit:     rec.TakeProfit,
			RemainingRatio: rec.RemainingRatio,
			Tier1:          TierInfo{Target: rec.Tier1Target, Ratio: rec.Tier1Ratio, Done: rec.Tier1Done},
			Tier2:          TierInfo{Target: rec.Tier2Target, Ratio: rec.Tier2Ratio, Done: rec.Tier2Done},
			Tier3:          TierInfo{Target: rec.Tier3Target, Ratio: rec.Tier3Ratio, Done: rec.Tier3Done},
			TierNotes:      rec.TierNotes,
			Status:         status,
			ClosedAt:       closedAt,
			ExitPrice:      pos.ExitPrice,
			ExitReason:     pos.ExitReason,
		}
		if pos.Closed {
			api.PnLRatio = pos.ExitPnLRatio
			api.PnLUSD = pos.ExitPnLUSD
			api.RemainingRatio = 0
		}
		var logs []TierLog
		var events []storage.TradeEvent
		if includeLogs || rec.TierNotes == "" {
			if fetched, err := m.loadTierLogs(ctx, pos.TradeID, logsLimit); err == nil {
				logs = fetched
			}
		}
		if includeLogs {
			if evs, err := m.ListTradeEvents(ctx, pos.TradeID, logsLimit); err == nil {
				events = evs
			}
		}
		if api.TierNotes == "" && len(logs) > 0 {
			api.TierNotes = logs[0].Reason
		}
		if includeLogs && len(logs) > 0 {
			api.TierLogs = logs
		}
		if includeLogs && len(events) > 0 {
			api.Events = events
		}
		list = append(list, api)
	}
	if len(list) == 0 {
		return nil
	}
	sort.Slice(list, func(i, j int) bool {
		rank := func(status string) int {
			if strings.EqualFold(status, "open") {
				return 0
			}
			return 1
		}
		ri := rank(list[i].Status)
		rj := rank(list[j].Status)
		if ri != rj {
			return ri < rj
		}
		if ri == 1 {
			if list[i].ClosedAt != list[j].ClosedAt {
				return list[i].ClosedAt > list[j].ClosedAt
			}
			return list[i].OpenedAt > list[j].OpenedAt
		}
		return list[i].OpenedAt > list[j].OpenedAt
	})
	if openLimit > 0 || closedLimit > 0 {
		opens := make([]APIPosition, 0, len(list))
		closeds := make([]APIPosition, 0, len(list))
		for _, pos := range list {
			if strings.EqualFold(pos.Status, "open") {
				opens = append(opens, pos)
			} else {
				closeds = append(closeds, pos)
			}
		}
		if openLimit > 0 && len(opens) > openLimit {
			opens = opens[:openLimit]
		}
		if closedLimit > 0 && len(closeds) > closedLimit {
			closeds = closeds[:closedLimit]
		}
		list = append(opens, closeds...)
	} else if openLimit > 0 && len(list) > openLimit {
		list = list[:openLimit]
	}
	return list
}

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
}

func (m *Manager) positionByID(tradeID int) (Position, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	pos, ok := m.positions[tradeID]
	return pos, ok
}

func (m *Manager) ensureRiskStore() *storage.Store {
	m.mu.Lock()
	store := m.riskStore
	path := m.riskStorePath
	m.mu.Unlock()
	if store != nil || strings.TrimSpace(path) == "" {
		return store
	}
	newStore, err := storage.Open(path)
	if err != nil {
		logger.Warnf("freqtrade risk store 初始化失败: %v", err)
		return nil
	}
	m.mu.Lock()
	if m.riskStore == nil {
		m.riskStore = newStore
		store = newStore
	} else {
		store = m.riskStore
		newStore.Close()
	}
	m.mu.Unlock()
	return store
}

func (m *Manager) recordRisk(rec storage.RiskRecord) {
	store := m.ensureRiskStore()
	if store == nil {
		return
	}
	if rec.UpdatedAt.IsZero() {
		rec.UpdatedAt = time.Now()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := store.Upsert(ctx, rec); err != nil {
		logger.Warnf("freqtrade risk store upsert 失败: %v", err)
	}
}

func (m *Manager) insertTierLog(ctx context.Context, log storage.TierLog) {
	store := m.ensureRiskStore()
	if store == nil {
		return
	}
	if log.CreatedAt.IsZero() {
		log.CreatedAt = time.Now()
	}
	if err := store.InsertTierLog(ctx, log); err != nil {
		logger.Warnf("freqtrade tier log 记录失败: %v", err)
	}
}

func (m *Manager) recordEvent(rec storage.RiskRecord, eventType, detail string, extra map[string]any) {
	store := m.ensureRiskStore()
	if store == nil || rec.TradeID == 0 {
		return
	}
	payload := storage.TradeEvent{
		TradeID:        rec.TradeID,
		Event:          strings.TrimSpace(eventType),
		Detail:         strings.TrimSpace(detail),
		StopLoss:       rec.StopLoss,
		TakeProfit:     rec.TakeProfit,
		Tier1Target:    rec.Tier1Target,
		Tier2Target:    rec.Tier2Target,
		Tier3Target:    rec.Tier3Target,
		RemainingRatio: rec.RemainingRatio,
		Source:         strings.TrimSpace(rec.Source),
		CreatedAt:      time.Now(),
	}
	if extra != nil {
		if buf, err := json.Marshal(extra); err == nil {
			payload.Extra = string(buf)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := store.InsertEvent(ctx, payload); err != nil {
		logger.Warnf("freqtrade event 记录失败 trade_id=%d event=%s: %v", rec.TradeID, eventType, err)
	}
}

func (m *Manager) loadRiskRecord(ctx context.Context, tradeID int) (storage.RiskRecord, bool) {
	store := m.ensureRiskStore()
	if store == nil {
		return storage.RiskRecord{}, false
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	rec, ok, err := store.Get(cctx, tradeID)
	if err != nil {
		logger.Warnf("freqtrade risk store 查询失败: %v", err)
		return storage.RiskRecord{}, false
	}
	return rec, ok
}

func (m *Manager) loadTierLogs(ctx context.Context, tradeID int, limit int) ([]storage.TierLog, error) {
	store := m.ensureRiskStore()
	if store == nil {
		return nil, fmt.Errorf("risk store 未初始化")
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if limit <= 0 {
		limit = 20
	}
	return store.ListTierLogs(cctx, tradeID, limit)
}

func (m *Manager) ensureLivePositionForAuto(pos Position, rec storage.RiskRecord, reasonType string) bool {
	symbol := strings.ToUpper(strings.TrimSpace(pos.Symbol))
	side := strings.ToLower(strings.TrimSpace(pos.Side))
	if pos.TradeID <= 0 || symbol == "" || side == "" {
		m.markAutoActionSkipped(rec, reasonType, "仓位标识缺失，跳过自动执行")
		return false
	}
	if pos.Closed {
		m.markAutoActionSkipped(rec, reasonType, "仓位已平仓，跳过自动执行")
		return false
	}
	if _, ok := m.lookupTrade(symbol, side); !ok {
		m.markAutoActionSkipped(rec, reasonType, "Freqtrade 未找到对应仓位，跳过自动执行")
		return false
	}
	return true
}

func (m *Manager) markAutoActionSkipped(rec storage.RiskRecord, reasonType, detail string) {
	if rec.TradeID <= 0 {
		return
	}
	status := fmt.Sprintf("%s_skipped_no_position", reasonType)
	if rec.Status == status {
		return
	}
	note := strings.TrimSpace(detail)
	if note == "" {
		note = "仓位不存在，跳过自动执行"
	}
	if prev := strings.TrimSpace(rec.TierNotes); prev != "" {
		note = prev + " | " + note
	}
	rec.Status = status
	rec.Source = fmt.Sprintf("%s_auto", reasonType)
	rec.TierNotes = note
	rec.RemainingRatio = 0
	rec.Tier1Done = true
	rec.Tier2Done = true
	rec.Tier3Done = true
	if strings.EqualFold(reasonType, "stop_loss") {
		rec.StopLoss = 0
	}
	if strings.EqualFold(reasonType, "take_profit") {
		rec.TakeProfit = 0
	}
	rec.UpdatedAt = time.Now()
	m.recordRisk(rec)
	logger.Infof("freqtrade manager: %s 跳过 trade_id=%d (%s)", reasonType, rec.TradeID, detail)
}

func (m *Manager) markAutoActionPending(rec storage.RiskRecord, reasonType, detail string) {
	if rec.TradeID <= 0 {
		return
	}
	reasonType = normalizeExitReason(reasonType)
	if reasonType == "" {
		reasonType = "close"
	}
	rec.Status = fmt.Sprintf("%s_pending", reasonType)
	rec.Source = fmt.Sprintf("%s_auto", reasonType)
	rec.Tier1Done = true
	rec.Tier2Done = true
	rec.Tier3Done = true
	if strings.EqualFold(reasonType, "stop_loss") {
		rec.StopLoss = 0
	}
	if strings.EqualFold(reasonType, "take_profit") {
		rec.TakeProfit = 0
	}
	rec.TierNotes = appendTierNote(rec.TierNotes, detail)
	rec.UpdatedAt = time.Now()
	m.recordRisk(rec)
}

func (m *Manager) finalizeRiskAfterClose(tradeID int, exitReason string, exitPrice float64) {
	ctx := context.Background()
	rec, ok := m.loadRiskRecord(ctx, tradeID)
	if !ok || rec.TradeID == 0 {
		return
	}
	origStatus := rec.Status
	reason := pendingReasonFromStatus(origStatus)
	if reason == "" {
		reason = reasonFromStatus(origStatus)
	}
	if reason == "" {
		reason = normalizeExitReason(exitReason)
	}
	if reason == "" {
		reason = "close"
	}
	rec.Status = fmt.Sprintf("%s_done", reason)
	source := strings.ToLower(strings.TrimSpace(rec.Source))
	if strings.Contains(source, "_auto") || pendingReasonFromStatus(origStatus) != "" {
		rec.Source = fmt.Sprintf("%s_auto", reason)
	} else if source == "" || source == "open" {
		rec.Source = reason
	}
	rec.Tier1Done = true
	rec.Tier2Done = true
	rec.Tier3Done = true
	rec.RemainingRatio = 0
	rec.StopLoss = 0
	rec.TakeProfit = 0
	rec.TierNotes = appendTierNote(rec.TierNotes, fmt.Sprintf("%s 成交 %.4f", strings.ToUpper(reason), exitPrice))
	rec.UpdatedAt = time.Now()
	m.recordRisk(rec)
	m.recordEvent(rec, "close", fmt.Sprintf("exit_reason=%s price=%.4f", exitReason, exitPrice), map[string]any{
		"exit_reason": reason,
		"exit_price":  exitPrice,
	})
}

func pendingReasonFromStatus(status string) string {
	status = strings.ToLower(strings.TrimSpace(status))
	for _, suf := range []string{"_pending", "_sent"} {
		if strings.HasSuffix(status, suf) {
			return strings.TrimSuffix(status, suf)
		}
	}
	return ""
}

func reasonFromStatus(status string) string {
	status = strings.ToLower(strings.TrimSpace(status))
	for _, suf := range []string{"_pending", "_sent", "_done", "_skipped_no_position"} {
		if strings.HasSuffix(status, suf) {
			return strings.TrimSuffix(status, suf)
		}
	}
	return status
}

func normalizeExitReason(reason string) string {
	r := strings.ToLower(strings.TrimSpace(reason))
	switch r {
	case "stoploss", "stop_loss", "stoploss_on_exchange":
		return "stop_loss"
	case "roi", "take_profit", "tp":
		return "take_profit"
	case "force_exit", "forceexit":
		return "force_exit"
	}
	return r
}

func appendTierNote(prev, addition string) string {
	addition = strings.TrimSpace(addition)
	if addition == "" {
		return prev
	}
	if strings.TrimSpace(prev) == "" {
		return addition
	}
	return prev + " | " + addition
}

func (m *Manager) lookupAmount(ctx context.Context, tradeID int) float64 {
	m.mu.Lock()
	if pos, ok := m.positions[tradeID]; ok && pos.Amount > 0 {
		amt := pos.Amount
		m.mu.Unlock()
		return amt
	}
	m.mu.Unlock()
	if m == nil || m.client == nil || tradeID <= 0 {
		return 0
	}
	callCtx := ctx
	if callCtx == nil {
		callCtx = context.Background()
	}
	cctx, cancel := context.WithTimeout(callCtx, 3*time.Second)
	defer cancel()
	trades, err := m.client.ListTrades(cctx)
	if err != nil {
		logger.Warnf("freqtrade manager: 获取 trade_id=%d 仓位失败: %v", tradeID, err)
		return 0
	}
	for _, tr := range trades {
		if tr.ID == tradeID {
			return m.mergePositionFromTrade(tr)
		}
	}
	return 0
}

func (m *Manager) mergePositionFromTrade(tr Trade) float64 {
	if m == nil || tr.ID == 0 {
		return 0
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
	openedAt := parseFreqtradeTime(tr.OpenDate)
	m.mu.Lock()
	defer m.mu.Unlock()
	pos, ok := m.positions[tr.ID]
	if !ok {
		pos = Position{TradeID: tr.ID}
	}
	if symbol != "" {
		pos.Symbol = symbol
	}
	if side != "" {
		pos.Side = side
	}
	if tr.Amount > 0 {
		pos.Amount = tr.Amount
	}
	if tr.StakeAmount > 0 {
		pos.Stake = tr.StakeAmount
	}
	if tr.Leverage > 0 {
		pos.Leverage = tr.Leverage
	}
	if tr.OpenRate > 0 {
		pos.EntryPrice = tr.OpenRate
	}
	if !openedAt.IsZero() {
		pos.OpenedAt = openedAt
	}
	pos.Closed = false
	pos.ClosedAt = time.Time{}
	pos.ExitPrice = 0
	pos.ExitReason = ""
	pos.ExitPnLRatio = 0
	pos.ExitPnLUSD = 0
	m.positions[tr.ID] = pos
	return pos.Amount
}

func (m *Manager) markPositionsClosed(openIDs map[int]struct{}) {
	if m == nil {
		return
	}
	now := time.Now()
	type key struct{ symbol, side string }
	toDelete := make([]key, 0)
	closedIDs := make([]int, 0)
	m.mu.Lock()
	for id, pos := range m.positions {
		if pos.Closed {
			continue
		}
		if _, ok := openIDs[id]; ok {
			continue
		}
		pos.Closed = true
		if pos.ClosedAt.IsZero() {
			pos.ClosedAt = now
		}
		if pos.ExitReason == "" {
			pos.ExitReason = "freqtrade_sync_closed"
		}
		pos.Amount = 0
		m.positions[id] = pos
		toDelete = append(toDelete, key{symbol: pos.Symbol, side: pos.Side})
		closedIDs = append(closedIDs, id)
	}
	m.mu.Unlock()
	for _, item := range toDelete {
		if item.symbol == "" || item.side == "" {
			continue
		}
		m.deleteTrade(item.symbol, item.side)
	}
	for _, id := range closedIDs {
		m.clearPendingHit(id, "stop_loss")
		m.clearPendingHit(id, "take_profit")
		m.clearPendingHit(id, "tier1")
		m.clearPendingHit(id, "tier2")
		m.clearPendingHit(id, "tier3")
	}
}

func (m *Manager) retryPendingAutoCloses(openIDs map[int]struct{}) {
	if len(openIDs) == 0 {
		return
	}
	ctx := context.Background()
	now := time.Now()
	for id := range openIDs {
		rec, ok := m.loadRiskRecord(ctx, id)
		if !ok || rec.TradeID == 0 {
			continue
		}
		reason := pendingReasonFromStatus(rec.Status)
		if reason == "" {
			continue
		}
		if rec.UpdatedAt.IsZero() || now.Sub(rec.UpdatedAt) < autoCloseRetryInterval {
			continue
		}
		pos, ok := m.positionByID(id)
		if !ok || pos.TradeID == 0 {
			continue
		}
		logger.Warnf("freqtrade manager: %s pending trade_id=%d 超过 %s 未确认，重试强平", reason, id, autoCloseRetryInterval)
		go m.retryForceClose(reason, pos, rec)
	}
}

func (m *Manager) retryForceClose(reasonType string, pos Position, rec storage.RiskRecord) {
	ctx := context.Background()
	action := "close_long"
	if strings.EqualFold(pos.Side, "short") {
		action = "close_short"
	}
	traceID := m.ensureTrace(fmt.Sprintf("retry-%s-%d", reasonType, time.Now().UnixNano()))
	reasoning := fmt.Sprintf("%s pending retry", strings.ToUpper(strings.TrimSpace(reasonType)))
	dec := decision.Decision{
		Symbol:    strings.ToUpper(pos.Symbol),
		Action:    action,
		Reasoning: reasoning,
	}
	if err := m.close(ctx, traceID, dec); err != nil {
		logger.Warnf("freqtrade manager: %s pending 重试失败 trade_id=%d: %v", reasonType, pos.TradeID, err)
		return
	}
	m.markAutoActionPending(rec, reasonType, "自动重试强平指令已发送")
}

func (m *Manager) markPositionClosed(tradeID int, symbol, side string, exitPrice, stake float64, reason string, closedAt time.Time, pnlRatio float64) Position {
	if m == nil || tradeID <= 0 {
		return Position{}
	}
	if closedAt.IsZero() {
		closedAt = time.Now()
	}
	reason = strings.TrimSpace(reason)
	m.mu.Lock()
	pos, ok := m.positions[tradeID]
	if !ok {
		pos = Position{TradeID: tradeID}
	}
	if symbol != "" {
		pos.Symbol = strings.ToUpper(strings.TrimSpace(symbol))
	}
	if side != "" {
		pos.Side = strings.ToLower(strings.TrimSpace(side))
	}
	if stake > 0 {
		pos.Stake = stake
	}
	if exitPrice > 0 {
		pos.ExitPrice = exitPrice
	}
	if reason != "" {
		pos.ExitReason = reason
	}
	pos.ExitPnLRatio = pnlRatio
	if pos.Stake > 0 && pnlRatio != 0 {
		pos.ExitPnLUSD = pnlRatio * pos.Stake
	}
	pos.Closed = true
	pos.ClosedAt = closedAt
	pos.Amount = 0
	m.positions[tradeID] = pos
	m.mu.Unlock()
	m.deleteTrade(pos.Symbol, pos.Side)
	return pos
}

// removePosition 已废弃，保留历史仓位用于前端展示。

func (m *Manager) recordOrder(ctx context.Context, msg WebhookMessage, action string, price float64, executedAt time.Time) {
	if m.orderRec == nil || action == "" {
		return
	}
	symbol := freqtradePairToSymbol(msg.Pair)
	if symbol == "" {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if executedAt.IsZero() {
		executedAt = time.Now()
	}
	order := market.Order{
		Symbol:     symbol,
		Action:     action,
		Side:       strings.ToLower(strings.TrimSpace(msg.Direction)),
		Type:       "freqtrade",
		Price:      price,
		Quantity:   float64(msg.Amount),
		Notional:   float64(msg.StakeAmount),
		DecidedAt:  executedAt,
		ExecutedAt: executedAt,
	}
	if data, err := json.Marshal(msg); err == nil {
		order.Meta = data
	}
	if _, err := m.orderRec.RecordOrder(ctx, &order); err != nil {
		logger.Warnf("freqtrade manager: record order failed: %v", err)
	}
}

func (m *Manager) notify(title string, lines ...string) {
	if m == nil || m.notifier == nil {
		return
	}
	title = strings.TrimSpace(title)
	bodyLines := make([]string, 0, len(lines))
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		bodyLines = append(bodyLines, ln)
	}
	body := strings.Join(bodyLines, "\n")
	if body == "" {
		body = "-"
	}
	msg := fmt.Sprintf("*%s*\n```text\n%s\n```", title, body)
	if err := m.notifier.SendText(msg); err != nil {
		logger.Warnf("freqtrade notifier 推送失败: %v", err)
	}
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

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04:05")
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

func formatPercent(val float64) string {
	return fmt.Sprintf("%.0f%%", val*100)
}

func floatEqual(a, b float64) bool {
	return math.Abs(a-b) <= 1e-6*math.Max(1, math.Max(math.Abs(a), math.Abs(b)))
}

func ratioOrDefault(val, def float64) float64 {
	if val <= 0 {
		return def
	}
	return val
}

func parseProfitRatio(v interface{}) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case float32:
		return float64(t)
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case string:
		if t == "" {
			return 0
		}
		if val, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
			return val
		}
	}
	return 0
}

func copyTierState(dst *storage.RiskRecord, src storage.RiskRecord) {
	if dst == nil {
		return
	}
	dst.Tier1Target = src.Tier1Target
	dst.Tier1Ratio = src.Tier1Ratio
	dst.Tier1Done = src.Tier1Done
	dst.Tier2Target = src.Tier2Target
	dst.Tier2Ratio = src.Tier2Ratio
	dst.Tier2Done = src.Tier2Done
	dst.Tier3Target = src.Tier3Target
	dst.Tier3Ratio = src.Tier3Ratio
	dst.Tier3Done = src.Tier3Done
	dst.RemainingRatio = src.RemainingRatio
	it := strings.TrimSpace(dst.TierNotes)
	if it == "" {
		dst.TierNotes = src.TierNotes
	}
	if dst.EntryPrice == 0 {
		dst.EntryPrice = src.EntryPrice
	}
	if dst.Stake == 0 {
		dst.Stake = src.Stake
	}
	if dst.Amount == 0 {
		dst.Amount = src.Amount
	}
}

func nextPendingTier(rec storage.RiskRecord) (string, float64, float64) {
	if !rec.Tier1Done && rec.Tier1Target > 0 {
		return "tier1", rec.Tier1Target, ratioOrDefault(rec.Tier1Ratio, defaultTier1Ratio)
	}
	if !rec.Tier2Done && rec.Tier2Target > 0 {
		return "tier2", rec.Tier2Target, ratioOrDefault(rec.Tier2Ratio, defaultTier2Ratio)
	}
	if !rec.Tier3Done && rec.Tier3Target > 0 {
		return "tier3", rec.Tier3Target, ratioOrDefault(rec.Tier3Ratio, defaultTier3Ratio)
	}
	return "", 0, 0
}

func priceForTierTrigger(side string, quote TierPriceQuote, target float64) (float64, bool) {
	if target <= 0 {
		return 0, false
	}
	side = strings.ToLower(strings.TrimSpace(side))
	switch side {
	case "long":
		price := maxPositive(quote.High, quote.Last)
		if price > 0 && price >= target {
			return price, true
		}
	case "short":
		price := minPositive(quote.Low, quote.Last)
		if price > 0 && price <= target {
			return price, true
		}
	}
	return 0, false
}

func priceForStopLoss(side string, quote TierPriceQuote, stop float64) (float64, bool) {
	if stop <= 0 {
		return 0, false
	}
	side = strings.ToLower(strings.TrimSpace(side))
	switch side {
	case "long":
		price := minPositive(quote.Low, quote.Last)
		if price > 0 && price <= stop {
			return price, true
		}
	case "short":
		price := maxPositive(quote.High, quote.Last)
		if price > 0 && price >= stop {
			return price, true
		}
	}
	return 0, false
}

func priceForTakeProfit(side string, quote TierPriceQuote, tp float64) (float64, bool) {
	if tp <= 0 {
		return 0, false
	}
	side = strings.ToLower(strings.TrimSpace(side))
	switch side {
	case "long":
		price := maxPositive(quote.High, quote.Last)
		if price > 0 && price >= tp {
			return price, true
		}
	case "short":
		price := minPositive(quote.Low, quote.Last)
		if price > 0 && price <= tp {
			return price, true
		}
	}
	return 0, false
}

func maxPositive(vals ...float64) float64 {
	max := 0.0
	for _, v := range vals {
		if v <= 0 {
			continue
		}
		if v > max {
			max = v
		}
	}
	return max
}

func minPositive(vals ...float64) float64 {
	min := 0.0
	for _, v := range vals {
		if v <= 0 {
			continue
		}
		if min == 0 || v < min {
			min = v
		}
	}
	return min
}

func (m *Manager) triggerTier(ctx context.Context, tierName string, price float64, pos Position, rec storage.RiskRecord) {
	if !m.ensureLivePositionForAuto(pos, rec, tierName) {
		return
	}
	if !m.beginTierExecution(pos.TradeID) {
		return
	}
	traceID := m.ensureTrace(fmt.Sprintf("tier-%s-%d", tierName, time.Now().UnixNano()))
	go func() {
		defer m.finishTierExecution(pos.TradeID)
		if err := m.handleTierHit(ctx, traceID, tierName, price, pos, rec); err != nil {
			logger.Warnf("自动执行 %s 失败 trade_id=%d: %v", tierName, pos.TradeID, err)
		}
	}()
}

func (m *Manager) beginTierExecution(tradeID int) bool {
	m.tierExecMu.Lock()
	defer m.tierExecMu.Unlock()
	if m.tierExec[tradeID] {
		return false
	}
	m.tierExec[tradeID] = true
	return true
}

func (m *Manager) finishTierExecution(tradeID int) {
	m.tierExecMu.Lock()
	delete(m.tierExec, tradeID)
	m.tierExecMu.Unlock()
}

func (m *Manager) handleTierHit(ctx context.Context, traceID, tierName string, price float64, pos Position, rec storage.RiskRecord) error {
	if !m.ensureLivePositionForAuto(pos, rec, tierName) {
		return nil
	}
	closePortion := tierRatioForName(rec, tierName)
	if closePortion <= 0 {
		return nil
	}
	remain := rec.RemainingRatio
	if remain <= 0 {
		remain = 1
	}
	if closePortion > remain {
		closePortion = remain
	}
	closeRatio := closePortion / remain
	if closeRatio <= 0 {
		return nil
	}
	action := "close_long"
	if strings.EqualFold(pos.Side, "short") {
		action = "close_short"
	}
	reason := fmt.Sprintf("自动%s减仓 %.4f", strings.ToUpper(tierName), price)
	dec := decision.Decision{
		Symbol:     strings.ToUpper(pos.Symbol),
		Action:     action,
		CloseRatio: closeRatio,
		Reasoning:  reason,
	}
	if err := m.close(ctx, traceID, dec); err != nil {
		var nfErr tradeNotFoundError
		if errors.As(err, &nfErr) {
			m.markAutoActionSkipped(rec, tierName, "Freqtrade 未找到仓位，跳过自动执行")
			logger.Warnf("自动执行 %s 跳过：仓位不存在 trade_id=%d symbol=%s", strings.ToUpper(tierName), pos.TradeID, strings.ToUpper(pos.Symbol))
			return nil
		}
		return err
	}
	oldStop := rec.StopLoss
	rec.RemainingRatio = math.Max(0, remain-closePortion)
	switch tierName {
	case "tier1":
		rec.Tier1Done = true
		if rec.EntryPrice > 0 {
			rec.StopLoss = rec.EntryPrice
		}
	case "tier2":
		rec.Tier2Done = true
		if rec.Tier1Target > 0 {
			rec.StopLoss = rec.Tier1Target
		}
	case "tier3":
		rec.Tier3Done = true
	}
	rec.Source = fmt.Sprintf("%s_auto", tierName)
	rec.Status = "tier_hit"
	rec.Reason = reason
	rec.TierNotes = fmt.Sprintf("%s 命中 %.4f", strings.ToUpper(tierName), price)
	rec.UpdatedAt = time.Now()
	m.recordRisk(rec)
	m.insertTierLog(context.Background(), storage.TierLog{
		TradeID:   pos.TradeID,
		TierName:  tierName,
		OldTarget: targetForName(rec, tierName),
		NewTarget: price,
		OldRatio:  closePortion,
		NewRatio:  closePortion,
		Reason:    fmt.Sprintf("命中 %.4f", price),
		Source:    "auto_tier_hit",
		CreatedAt: time.Now(),
	})
	if rec.StopLoss > 0 && !floatEqual(oldStop, rec.StopLoss) && tierName != "tier3" {
		adj := decision.Decision{Symbol: strings.ToUpper(pos.Symbol), Action: "adjust_stop_loss", StopLoss: rec.StopLoss, TakeProfit: rec.TakeProfit, Reasoning: reason}
		if err := m.adjustStopLoss(ctx, traceID, adj); err != nil {
			logger.Warnf("tier 调整止损失败 trade_id=%d: %v", pos.TradeID, err)
		}
	}
	m.recordEvent(rec, fmt.Sprintf("%s_hit", tierName), reason, map[string]any{
		"hit_price":   price,
		"close_ratio": closeRatio,
	})
	m.notifyTierHit(tierName, rec, pos, price, closePortion, oldStop)
	return nil
}

func tierRatioForName(rec storage.RiskRecord, name string) float64 {
	switch name {
	case "tier1":
		return ratioOrDefault(rec.Tier1Ratio, defaultTier1Ratio)
	case "tier2":
		return ratioOrDefault(rec.Tier2Ratio, defaultTier2Ratio)
	case "tier3":
		return ratioOrDefault(rec.Tier3Ratio, defaultTier3Ratio)
	default:
		return 0
	}
}

func apiPositionToSnapshot(pos APIPosition) decision.PositionSnapshot {
	snap := decision.PositionSnapshot{
		Symbol:          strings.ToUpper(pos.Symbol),
		Side:            strings.ToUpper(pos.Side),
		EntryPrice:      pos.EntryPrice,
		Quantity:        pos.Amount,
		Stake:           pos.Stake,
		Leverage:        pos.Leverage,
		CurrentPrice:    pos.CurrentPrice,
		TakeProfit:      pos.TakeProfit,
		StopLoss:        pos.StopLoss,
		HoldingMs:       pos.HoldingMs,
		RemainingRatio:  pos.RemainingRatio,
		TierNotes:       pos.TierNotes,
		UnrealizedPnPct: pos.PnLRatio,
		UnrealizedPn:    pos.PnLUSD,
	}
	if pos.Tier1.Target > 0 {
		snap.Tier1Target = pos.Tier1.Target
		snap.Tier1Ratio = pos.Tier1.Ratio
		snap.Tier1Done = pos.Tier1.Done
	}
	if pos.Tier2.Target > 0 {
		snap.Tier2Target = pos.Tier2.Target
		snap.Tier2Ratio = pos.Tier2.Ratio
		snap.Tier2Done = pos.Tier2.Done
	}
	if pos.Tier3.Target > 0 {
		snap.Tier3Target = pos.Tier3.Target
		snap.Tier3Ratio = pos.Tier3.Ratio
		snap.Tier3Done = pos.Tier3.Done
	}
	if snap.RemainingRatio == 0 {
		snap.RemainingRatio = 1
	}
	if snap.CurrentPrice <= 0 {
		snap.CurrentPrice = pos.EntryPrice
	}
	if snap.UnrealizedPnPct == 0 && snap.EntryPrice > 0 && snap.CurrentPrice > 0 {
		if strings.EqualFold(snap.Side, "SHORT") {
			snap.UnrealizedPnPct = (snap.EntryPrice - snap.CurrentPrice) / snap.EntryPrice
		} else {
			snap.UnrealizedPnPct = (snap.CurrentPrice - snap.EntryPrice) / snap.EntryPrice
		}
	}
	if snap.UnrealizedPn == 0 && snap.UnrealizedPnPct != 0 && snap.Stake > 0 {
		snap.UnrealizedPn = snap.UnrealizedPnPct * snap.Stake
	}
	value := snap.Stake
	if value == 0 && snap.CurrentPrice > 0 && snap.Quantity > 0 {
		value = snap.CurrentPrice * snap.Quantity
	}
	snap.PositionValue = value
	return snap
}

func targetForName(rec storage.RiskRecord, name string) float64 {
	switch name {
	case "tier1":
		return rec.Tier1Target
	case "tier2":
		return rec.Tier2Target
	case "tier3":
		return rec.Tier3Target
	default:
		return 0
	}
}

func (m *Manager) forceClose(ctx context.Context, pos Position, reasonType string, price float64, rec storage.RiskRecord) {
	if !m.ensureLivePositionForAuto(pos, rec, reasonType) {
		return
	}
	if !m.beginTierExecution(pos.TradeID) {
		return
	}
	defer m.finishTierExecution(pos.TradeID)
	action := "close_long"
	if strings.EqualFold(pos.Side, "short") {
		action = "close_short"
	}
	reason := fmt.Sprintf("触发%s %.4f", reasonType, price)
	dec := decision.Decision{
		Symbol:    strings.ToUpper(pos.Symbol),
		Action:    action,
		Reasoning: reason,
	}
	traceID := m.ensureTrace(fmt.Sprintf("force-%s-%d", reasonType, time.Now().UnixNano()))
	if err := m.close(ctx, traceID, dec); err != nil {
		var nfErr tradeNotFoundError
		if errors.As(err, &nfErr) {
			skipNote := fmt.Sprintf("Freqtrade 未找到仓位，跳过 %s", strings.ToUpper(reasonType))
			m.markAutoActionSkipped(rec, reasonType, skipNote)
			logger.Warnf("强制%s跳过：仓位不存在 trade_id=%d symbol=%s", strings.ToUpper(reasonType), pos.TradeID, strings.ToUpper(pos.Symbol))
			return
		}
		logger.Warnf("强制%s失败 trade_id=%d: %v", reasonType, pos.TradeID, err)
		return
	}
	rec.Tier1Done = true
	rec.Tier2Done = true
	rec.Tier3Done = true
	note := fmt.Sprintf("%s 触发 %.4f", strings.ToUpper(reasonType), price)
	m.markAutoActionPending(rec, reasonType, note)
	m.recordEvent(rec, fmt.Sprintf("force_%s", reasonType), note, map[string]any{
		"trigger_price": price,
	})
	m.notify(fmt.Sprintf("%s 已触发 ⚠️", strings.ToUpper(reasonType)),
		fmt.Sprintf("交易ID: %d", pos.TradeID),
		fmt.Sprintf("标的: %s", strings.ToUpper(pos.Symbol)),
		fmt.Sprintf("方向: %s", strings.ToUpper(pos.Side)),
		fmt.Sprintf("触发价: %.4f", price))
}

func (m *Manager) notifyTierHit(tierName string, rec storage.RiskRecord, pos Position, price float64, portion float64, oldStop float64) {
	nextName, nextTarget, nextRatio := nextPendingTier(rec)
	remain := rec.RemainingRatio
	pnlRatio := 0.0
	if rec.EntryPrice > 0 {
		if strings.EqualFold(pos.Side, "short") {
			pnlRatio = (rec.EntryPrice - price) / rec.EntryPrice
		} else {
			pnlRatio = (price - rec.EntryPrice) / rec.EntryPrice
		}
	}
	stake := rec.Stake
	if stake == 0 {
		stake = pos.Stake
	}
	pnlUSD := pnlRatio * stake
	lines := []string{
		fmt.Sprintf("交易ID: %d", pos.TradeID),
		fmt.Sprintf("标的: %s (%s)", strings.ToUpper(rec.Symbol), strings.ToUpper(rec.Pair)),
		fmt.Sprintf("方向: %s", strings.ToUpper(pos.Side)),
		fmt.Sprintf("阶段: %s", strings.ToUpper(tierName)),
		fmt.Sprintf("触发价: %.4f", price),
		fmt.Sprintf("减仓比例: %s", formatPercent(portion)),
		fmt.Sprintf("止损: %s → %s", formatPrice(oldStop), formatPrice(rec.StopLoss)),
		fmt.Sprintf("剩余仓位: %s", formatPercent(remain)),
		fmt.Sprintf("当前浮盈: %.2f USDT (%.2f%%)", pnlUSD, pnlRatio*100),
	}
	if nextName != "" {
		lines = append(lines, fmt.Sprintf("下一阶段: %s %.4f (%s)", strings.ToUpper(nextName), nextTarget, formatPercent(nextRatio)))
	}
	m.notify(fmt.Sprintf("%s 已执行 ✅", strings.ToUpper(tierName)), lines...)
}

func (m *Manager) applyTiersFromDecision(rec *storage.RiskRecord, tiers *decision.DecisionTiers) {
	if rec == nil {
		return
	}
	if tiers == nil {
		tiers = &decision.DecisionTiers{}
	}
	applyDefaultTierRatios(tiers)
	rec.Tier1Target = tiers.Tier1Target
	rec.Tier1Ratio = ratioOrDefault(tiers.Tier1Ratio, defaultTier1Ratio)
	rec.Tier2Target = tiers.Tier2Target
	rec.Tier2Ratio = ratioOrDefault(tiers.Tier2Ratio, defaultTier2Ratio)
	rec.Tier3Target = tiers.Tier3Target
	rec.Tier3Ratio = ratioOrDefault(tiers.Tier3Ratio, defaultTier3Ratio)
	rec.TierNotes = fmt.Sprintf("T1 %.4f(%.2f), T2 %.4f(%.2f), T3 %.4f(%.2f)",
		rec.Tier1Target, rec.Tier1Ratio, rec.Tier2Target, rec.Tier2Ratio, rec.Tier3Target, rec.Tier3Ratio)
	if rec.RemainingRatio == 0 {
		rec.RemainingRatio = 1
	}
}

func (m *Manager) applyTierUpdate(ctx context.Context, traceID, symbol, side string, tiers *decision.DecisionTiers, reason, source string, logExec bool) error {
	if tiers == nil {
		return fmt.Errorf("tiers 不能为空")
	}
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	tradeID, sideResolved, pos, ok := m.findActiveTrade(symbol)
	if !ok {
		return fmt.Errorf("freqtrade 没有 %s 的持仓，无法调整 tiers", symbol)
	}
	if side == "" {
		side = sideResolved
	}
	rec, has := m.loadRiskRecord(ctx, tradeID)
	if !has {
		rec = storage.RiskRecord{
			TradeID:        tradeID,
			Symbol:         strings.ToUpper(symbol),
			Pair:           formatFreqtradePair(symbol),
			Side:           side,
			EntryPrice:     pos.EntryPrice,
			Stake:          pos.Stake,
			Amount:         pos.Amount,
			Leverage:       pos.Leverage,
			RemainingRatio: 1,
			UpdatedAt:      time.Now(),
		}
	}
	old := rec
	if err := validateTierRatios(old, tiers); err != nil {
		m.notify("Tier 更新被拒绝 ⚠️",
			fmt.Sprintf("交易ID: %d", tradeID),
			fmt.Sprintf("标的: %s", strings.ToUpper(symbol)),
			fmt.Sprintf("方向: %s", strings.ToUpper(side)),
			fmt.Sprintf("请求来源: %s", source),
			"缺少比例: "+err.Error(),
		)
		return err
	}
	ignored, blocked := sanitizeCompletedTiers(old, tiers)
	if blocked {
		upper := strings.ToUpper(strings.Join(ignored, ", "))
		lines := []string{
			fmt.Sprintf("交易ID: %d", tradeID),
			fmt.Sprintf("标的: %s", strings.ToUpper(symbol)),
			fmt.Sprintf("方向: %s", strings.ToUpper(side)),
			fmt.Sprintf("请求来源: %s", source),
			fmt.Sprintf("无法调整: %s 已执行", upper),
		}
		if rs := shortReason(reason); rs != "" {
			lines = append(lines, "理由: "+rs)
		}
		m.notify("Tier 更新被拒绝 ⚠️", lines...)
		return fmt.Errorf("tiers 已完成: %s", upper)
	}
	rec = applyPendingTierUpdates(rec, tiers, old)
	rec.Source = source
	rec.Status = "tiers_updated"
	rec.Reason = reason
	note := fmt.Sprintf("%s %s", source, strings.TrimSpace(reason))
	if strings.TrimSpace(rec.TierNotes) != "" {
		note = fmt.Sprintf("%s | %s", note, strings.TrimSpace(rec.TierNotes))
	}
	rec.TierNotes = note
	m.recordRisk(rec)
	m.recordEvent(rec, "update_tiers", reason, map[string]any{
		"source":    source,
		"ignored":   ignored,
		"request":   tiers,
		"old_tiers": old,
	})
	ctxLog, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	m.logTierChange(ctxLog, tradeID, "tier1", old.Tier1Target, rec.Tier1Target, old.Tier1Ratio, rec.Tier1Ratio, reason, source)
	m.logTierChange(ctxLog, tradeID, "tier2", old.Tier2Target, rec.Tier2Target, old.Tier2Ratio, rec.Tier2Ratio, reason, source)
	m.logTierChange(ctxLog, tradeID, "tier3", old.Tier3Target, rec.Tier3Target, old.Tier3Ratio, rec.Tier3Ratio, reason, source)
	lines := []string{
		fmt.Sprintf("交易ID: %d", tradeID),
		fmt.Sprintf("标的: %s", strings.ToUpper(symbol)),
		fmt.Sprintf("方向: %s", strings.ToUpper(side)),
		fmt.Sprintf("Tier1: %s → %.4f (%s → %s)", formatPrice(old.Tier1Target), rec.Tier1Target, formatPercent(old.Tier1Ratio), formatPercent(rec.Tier1Ratio)),
		fmt.Sprintf("Tier2: %s → %.4f (%s → %s)", formatPrice(old.Tier2Target), rec.Tier2Target, formatPercent(old.Tier2Ratio), formatPercent(rec.Tier2Ratio)),
		fmt.Sprintf("Tier3: %s → %.4f (%s → %s)", formatPrice(old.Tier3Target), rec.Tier3Target, formatPercent(old.Tier3Ratio), formatPercent(rec.Tier3Ratio)),
		fmt.Sprintf("Trace: %s", m.ensureTrace(traceID)),
	}
	if len(ignored) > 0 {
		upper := strings.ToUpper(strings.Join(ignored, ", "))
		lines = append(lines, fmt.Sprintf("提示: %s 已完成，保持原配置", upper))
	}
	if rs := shortReason(reason); rs != "" {
		lines = append(lines, "理由: "+rs)
	}
	m.notify("Tier 配置已更新 ⚙️", lines...)
	if logExec {
		m.logExecutor(ctx, traceID, decision.Decision{Symbol: symbol, Action: "update_tiers", Reasoning: reason}, tradeID, "tiers_updated", map[string]any{"tiers": tiers}, nil)
	}
	return nil
}

func (m *Manager) logTierChange(ctx context.Context, tradeID int, name string, oldTarget, newTarget, oldRatio, newRatio float64, reason, source string) {
	if floatEqual(oldTarget, newTarget) && floatEqual(oldRatio, newRatio) {
		return
	}
	m.insertTierLog(ctx, storage.TierLog{
		TradeID:   tradeID,
		TierName:  name,
		OldTarget: oldTarget,
		NewTarget: newTarget,
		OldRatio:  oldRatio,
		NewRatio:  newRatio,
		Reason:    reason,
		Source:    source,
		CreatedAt: time.Now(),
	})
}

func applyDefaultTierRatios(t *decision.DecisionTiers) {
	if t == nil {
		return
	}
	if t.Tier1Ratio <= 0 {
		t.Tier1Ratio = defaultTier1Ratio
	}
	if t.Tier2Ratio <= 0 {
		t.Tier2Ratio = defaultTier2Ratio
	}
	if t.Tier3Ratio <= 0 {
		t.Tier3Ratio = defaultTier3Ratio
	}
}

func sanitizeCompletedTiers(existing storage.RiskRecord, tiers *decision.DecisionTiers) ([]string, bool) {
	ignored := make([]string, 0, 3)
	if tiers == nil {
		return ignored, false
	}
	requests := 0
	if tierChangeRequested(tiers.Tier1Target, tiers.Tier1Ratio, existing.Tier1Target, existing.Tier1Ratio) {
		requests++
		if existing.Tier1Done {
			ignored = append(ignored, "tier1")
		}
	}
	if existing.Tier1Done {
		tiers.Tier1Target = existing.Tier1Target
		if existing.Tier1Ratio > 0 {
			tiers.Tier1Ratio = existing.Tier1Ratio
		}
	}
	if tierChangeRequested(tiers.Tier2Target, tiers.Tier2Ratio, existing.Tier2Target, existing.Tier2Ratio) {
		requests++
		if existing.Tier2Done {
			ignored = append(ignored, "tier2")
		}
	}
	if existing.Tier2Done {
		tiers.Tier2Target = existing.Tier2Target
		if existing.Tier2Ratio > 0 {
			tiers.Tier2Ratio = existing.Tier2Ratio
		}
	}
	if tierChangeRequested(tiers.Tier3Target, tiers.Tier3Ratio, existing.Tier3Target, existing.Tier3Ratio) {
		requests++
		if existing.Tier3Done {
			ignored = append(ignored, "tier3")
		}
	}
	if existing.Tier3Done {
		tiers.Tier3Target = existing.Tier3Target
		if existing.Tier3Ratio > 0 {
			tiers.Tier3Ratio = existing.Tier3Ratio
		}
	}
	blocked := requests > 0 && len(ignored) == requests
	return ignored, blocked
}

func tierChangeRequested(newTarget, newRatio, oldTarget, oldRatio float64) bool {
	targetChanged := newTarget > 0 && (oldTarget <= 0 || !floatEqual(newTarget, oldTarget))
	ratioChanged := newRatio > 0 && (oldRatio <= 0 || !floatEqual(newRatio, oldRatio))
	return targetChanged || ratioChanged
}

func validateTierRatios(existing storage.RiskRecord, tiers *decision.DecisionTiers) error {
	if tiers == nil {
		return nil
	}
	missing := make([]string, 0, 3)
	if needsRatio(existing.Tier1Done, tiers.Tier1Target, tiers.Tier1Ratio, existing.Tier1Target, existing.Tier1Ratio) && tiers.Tier1Ratio <= 0 {
		missing = append(missing, "tier1")
	}
	if needsRatio(existing.Tier2Done, tiers.Tier2Target, tiers.Tier2Ratio, existing.Tier2Target, existing.Tier2Ratio) && tiers.Tier2Ratio <= 0 {
		missing = append(missing, "tier2")
	}
	if needsRatio(existing.Tier3Done, tiers.Tier3Target, tiers.Tier3Ratio, existing.Tier3Target, existing.Tier3Ratio) && tiers.Tier3Ratio <= 0 {
		missing = append(missing, "tier3")
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(missing, ", "))
}

func needsRatio(done bool, newTarget, newRatio, oldTarget, oldRatio float64) bool {
	if done {
		return false
	}
	return tierChangeRequested(newTarget, newRatio, oldTarget, oldRatio)
}

func applyPendingTierUpdates(rec storage.RiskRecord, tiers *decision.DecisionTiers, existing storage.RiskRecord) storage.RiskRecord {
	if tiers == nil {
		return rec
	}
	if !existing.Tier1Done {
		if tiers.Tier1Target > 0 {
			rec.Tier1Target = tiers.Tier1Target
		}
		if tiers.Tier1Ratio > 0 {
			rec.Tier1Ratio = tiers.Tier1Ratio
		}
	}
	if !existing.Tier2Done {
		if tiers.Tier2Target > 0 {
			rec.Tier2Target = tiers.Tier2Target
		}
		if tiers.Tier2Ratio > 0 {
			rec.Tier2Ratio = tiers.Tier2Ratio
		}
	}
	if !existing.Tier3Done {
		if tiers.Tier3Target > 0 {
			rec.Tier3Target = tiers.Tier3Target
		}
		if tiers.Tier3Ratio > 0 {
			rec.Tier3Ratio = tiers.Tier3Ratio
		}
	}
	return rec
}
