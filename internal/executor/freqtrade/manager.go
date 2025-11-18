package freqtrade

import (
	"context"
	"encoding/json"
	"fmt"
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

// APIPosition 用于 /api/live/freqtrade/positions 返回的数据结构。
type APIPosition struct {
	TradeID    int     `json:"trade_id"`
	Symbol     string  `json:"symbol"`
	Side       string  `json:"side"`
	EntryPrice float64 `json:"entry_price"`
	Amount     float64 `json:"amount"`
	Stake      float64 `json:"stake"`
	Leverage   float64 `json:"leverage"`
	OpenedAt   int64   `json:"opened_at"`
	HoldingMs  int64   `json:"holding_ms"`
}

// Manager 提供 freqtrade 执行、日志与持仓同步能力。
type Manager struct {
	client      *Client
	cfg         brcfg.FreqtradeConfig
	logger      Logger
	orderRec    market.Recorder
	traceByKey  map[string]int
	traceByID   map[int]string
	positions   map[int]Position
	mu          sync.Mutex
	horizonName string
}

// Position 缓存 freqtrade 持仓信息。
type Position struct {
	TradeID    int
	Symbol     string
	Side       string
	Amount     float64
	Stake      float64
	Leverage   float64
	EntryPrice float64
	OpenedAt   time.Time
}

// NewManager 创建 freqtrade 执行管理器。
func NewManager(client *Client, cfg brcfg.FreqtradeConfig, horizon string, logStore Logger, orderRec market.Recorder) *Manager {
	return &Manager{
		client:      client,
		cfg:         cfg,
		logger:      logStore,
		orderRec:    orderRec,
		traceByKey:  make(map[string]int),
		traceByID:   make(map[int]string),
		positions:   make(map[int]Position),
		horizonName: horizon,
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
	default:
		return nil
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
		return err
	}
	m.storeTrade(d.Symbol, side, resp.TradeID)
	m.storeTrace(resp.TradeID, traceID)
	m.logExecutor(ctx, traceID, d, resp.TradeID, "forceenter_success", recData, nil)
	return nil
}

func (m *Manager) close(ctx context.Context, traceID string, d decision.Decision) error {
	side := deriveSide(d.Action)
	if side == "" {
		return fmt.Errorf("unsupported action: %s", d.Action)
	}
	tradeID, ok := m.lookupTrade(d.Symbol, side)
	if !ok {
		return fmt.Errorf("freqtrade trade_id not found for %s %s", d.Symbol, side)
	}
	payload := ForceExitPayload{TradeID: fmt.Sprintf("%d", tradeID)}
	if ratio := clampCloseRatio(d.CloseRatio); ratio > 0 && ratio < 1 {
		if amt := m.lookupAmount(tradeID); amt > 0 {
			payload.Amount = amt * ratio
		}
	}
	recData := map[string]any{"payload": payload, "status": "forceexit", "close_ratio": d.CloseRatio}
	if err := m.client.ForceExit(ctx, payload); err != nil {
		m.logExecutor(ctx, traceID, d, tradeID, "forceexit_error", recData, err)
		return err
	}
	m.deleteTrade(d.Symbol, side)
	m.logExecutor(ctx, traceID, d, tradeID, "forceexit_success", recData, nil)
	return nil
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

// Positions 返回当前 freqtrade 持仓快照。
func (m *Manager) Positions() []decision.PositionSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.positions) == 0 {
		return nil
	}
	now := time.Now()
	out := make([]decision.PositionSnapshot, 0, len(m.positions))
	for _, pos := range m.positions {
		holding := int64(0)
		if !pos.OpenedAt.IsZero() {
			holding = now.Sub(pos.OpenedAt).Milliseconds()
		}
		out = append(out, decision.PositionSnapshot{
			Symbol:     strings.ToUpper(pos.Symbol),
			Side:       strings.ToUpper(pos.Side),
			EntryPrice: pos.EntryPrice,
			Quantity:   pos.Amount,
			HoldingMs:  holding,
		})
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

func (m *Manager) handleEntry(ctx context.Context, msg WebhookMessage, filled bool) {
	tradeID := int(msg.TradeID)
	symbol := freqtradePairToSymbol(msg.Pair)
	side := strings.ToLower(strings.TrimSpace(msg.Direction))
	price := firstNonZero(float64(msg.OpenRate), float64(msg.OrderRate))
	amount := float64(msg.Amount)
	stake := float64(msg.StakeAmount)
	openTime := parseFreqtradeTime(msg.OpenDate)
	traceID := m.ensureTrace(fmt.Sprintf("%d", tradeID))
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
	if !filled && amount <= 0 {
		m.storeTrade(symbol, side, tradeID)
		logger.Infof("freqtrade manager: entry submitted trade_id=%d %s %s price=%.4f", tradeID, symbol, side, price)
		return
	}
	m.mu.Lock()
	m.positions[tradeID] = pos
	m.mu.Unlock()
	m.storeTrade(symbol, side, tradeID)
	logger.Infof("freqtrade manager: entry filled trade_id=%d %s %s price=%.4f qty=%.4f", tradeID, symbol, side, price, amount)
	m.logWebhook(ctx, traceID, tradeID, symbol, "entry_fill", msg)
	m.recordOrder(ctx, msg, freqtradeAction(side, false), price, openTime)
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
}

func (m *Manager) handleExit(ctx context.Context, msg WebhookMessage, event string) {
	tradeID := int(msg.TradeID)
	m.mu.Lock()
	pos, ok := m.positions[tradeID]
	if ok {
		delete(m.positions, tradeID)
	}
	m.mu.Unlock()
	symbol := pos.Symbol
	if symbol == "" {
		symbol = freqtradePairToSymbol(msg.Pair)
	}
	side := pos.Side
	if side == "" {
		side = strings.ToLower(strings.TrimSpace(msg.Direction))
	}
	m.deleteTrade(symbol, side)
	closePrice := float64(msg.CloseRate)
	traceID := m.lookupTrace(tradeID)
	m.deleteTrace(tradeID)
	logger.Infof("freqtrade manager: exit trade_id=%d %s %s price=%.4f reason=%s", tradeID, symbol, side, closePrice, msg.ExitReason)
	m.logWebhook(ctx, traceID, tradeID, symbol, event, msg)
	m.recordOrder(ctx, msg, freqtradeAction(side, true), closePrice, parseFreqtradeTime(msg.CloseDate))
}

func (m *Manager) handleExitCancel(ctx context.Context, msg WebhookMessage) {
	tradeID := int(msg.TradeID)
	traceID := m.lookupTrace(tradeID)
	logger.Infof("freqtrade manager: exit cancel trade_id=%d", tradeID)
	m.logWebhook(ctx, traceID, tradeID, freqtradePairToSymbol(msg.Pair), "exit_cancel", msg)
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

func (m *Manager) PositionsForAPI(symbol string, limit int) []APIPosition {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.positions) == 0 {
		return nil
	}
	now := time.Now()
	list := make([]APIPosition, 0, len(m.positions))
	for _, pos := range m.positions {
		if symbol != "" && !strings.EqualFold(pos.Symbol, symbol) {
			continue
		}
		opened := int64(0)
		holding := int64(0)
		if !pos.OpenedAt.IsZero() {
			opened = pos.OpenedAt.UnixMilli()
			holding = now.Sub(pos.OpenedAt).Milliseconds()
		}
		list = append(list, APIPosition{
			TradeID:    pos.TradeID,
			Symbol:     strings.ToUpper(pos.Symbol),
			Side:       strings.ToUpper(pos.Side),
			EntryPrice: pos.EntryPrice,
			Amount:     pos.Amount,
			Stake:      pos.Stake,
			Leverage:   pos.Leverage,
			OpenedAt:   opened,
			HoldingMs:  holding,
		})
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].OpenedAt > list[j].OpenedAt
	})
	if limit > 0 && len(list) > limit {
		list = list[:limit]
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

func (m *Manager) lookupAmount(tradeID int) float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if pos, ok := m.positions[tradeID]; ok {
		return pos.Amount
	}
	return 0
}

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
