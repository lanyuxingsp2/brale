package freqtrade

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"brale/internal/decision"
	"brale/internal/gateway/database"
	"brale/internal/logger"
	"brale/internal/market"
)

type tradeNotFoundError struct {
	Symbol string
	Side   string
}

func (e tradeNotFoundError) Error() string {
	return "freqtrade trade not found for " + strings.ToUpper(e.Symbol) + " " + strings.ToLower(e.Side)
}

// notify 将消息发送到 TextNotifier（若存在）。
func (m *Manager) notify(title string, lines ...string) {
	if m == nil || m.notifier == nil {
		return
	}
	msg := strings.TrimSpace(title)
	body := strings.TrimSpace(strings.Join(lines, "\n"))
	if body != "" {
		msg = msg + "\n" + body
	}
	_ = m.notifier.SendText(msg)
}

// formatFields 渲染 key/value 行，空值自动跳过。
func formatFields(fields ...[2]string) []string {
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		k := strings.TrimSpace(f[0])
		v := strings.TrimSpace(f[1])
		if k == "" || v == "" {
			continue
		}
		out = append(out, fmt.Sprintf("%s: %s", k, v))
	}
	return out
}

// logExecutor 写入决策日志或输出调试信息。
func (m *Manager) logExecutor(ctx context.Context, traceID string, d decision.Decision, tradeID int, stage string, meta map[string]any, err error) {
	rec := database.DecisionLogRecord{
		TraceID:   m.ensureTrace(traceID),
		Timestamp: time.Now().UnixMilli(),
		Horizon:   m.horizonName,
		Stage:     stage,
		Decisions: []decision.Decision{d},
		Meta:      strings.TrimSpace(stage),
	}
	if err != nil {
		rec.Error = err.Error()
	}
	if len(meta) > 0 {
		if buf, e := json.Marshal(meta); e == nil {
			rec.Meta = string(buf)
		}
	}
	if m.logger != nil {
		if _, e := m.logger.Insert(ctx, rec); e != nil {
			logger.Errorf("freqtrade manager: 记录执行日志失败: %v", e)
		}
		return
	}
	logger.Debugf("freqtrade manager: %s trace=%s trade=%d err=%v", stage, traceID, tradeID, err)
}

// logWebhook 持久化 freqtrade 回调。
func (m *Manager) logWebhook(ctx context.Context, traceID string, tradeID int, symbol, stage string, payload any) {
	rec := database.DecisionLogRecord{
		TraceID:   m.ensureTrace(traceID),
		Timestamp: time.Now().UnixMilli(),
		Horizon:   m.horizonName,
		Stage:     "freqtrade_" + strings.TrimSpace(stage),
	}
	if buf, err := json.Marshal(payload); err == nil {
		rec.RawJSON = string(buf)
	}
	if m.logger != nil {
		if _, err := m.logger.Insert(ctx, rec); err != nil {
			logger.Errorf("freqtrade manager: 写入 webhook 日志失败: %v", err)
		}
	}
}

// recordOrder 将 webhook 转换为执行记录，写入 Recorder。
func (m *Manager) recordOrder(ctx context.Context, msg WebhookMessage, action string, price float64, executedAt time.Time) {
	if m == nil || m.orderRec == nil {
		return
	}
	side := strings.ToLower(strings.TrimSpace(msg.Direction))
	order := &market.Order{
		Symbol:     freqtradePairToSymbol(msg.Pair),
		Action:     action,
		Side:       side,
		Price:      price,
		Quantity:   float64(msg.Amount),
		Notional:   float64(msg.StakeAmount),
		ExecutedAt: executedAt,
		DecidedAt:  executedAt,
	}
	if _, err := m.orderRec.RecordOrder(ctx, order); err != nil {
		logger.Errorf("freqtrade manager: 记录 order 失败: %v", err)
	}
}

// appendOperation 写入 trade_operation_log（忽略失败，避免阻塞主流程）。
func (m *Manager) appendOperation(ctx context.Context, tradeID int, symbol string, op database.OperationType, details map[string]any) {
	if m == nil || m.posRepo == nil {
		return
	}
	if tradeID <= 0 || strings.TrimSpace(symbol) == "" {
		return
	}
	rec := database.TradeOperationRecord{
		FreqtradeID: tradeID,
		Symbol:      strings.ToUpper(strings.TrimSpace(symbol)),
		Operation:   op,
		Details:     details,
		Timestamp:   time.Now(),
	}
	m.posRepo.AppendOperation(ctx, rec)
}

// lookupAmount 返回当前仓位数量，优先内存缓存，其次持久化表。
func (m *Manager) lookupAmount(ctx context.Context, tradeID int) float64 {
	m.mu.Lock()
	if pos, ok := m.positions[tradeID]; ok && pos.Amount > 0 {
		m.mu.Unlock()
		return pos.Amount
	}
	m.mu.Unlock()
	if m.posRepo == nil {
		return 0
	}
	order, _, ok, err := m.posRepo.GetPosition(ctx, tradeID)
	if err != nil || !ok {
		return 0
	}
	return valOrZero(order.Amount)
}

// findActiveTrade 根据 symbol 查找持仓与方向。
func (m *Manager) findActiveTrade(symbol string) (tradeID int, side string, pos Position, ok bool) {
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	if symbol == "" {
		return 0, "", Position{}, false
	}
	m.mu.Lock()
	for _, p := range m.positions {
		if strings.EqualFold(strings.TrimSpace(p.Symbol), symbol) && !p.Closed {
			m.mu.Unlock()
			return p.TradeID, p.Side, p, true
		}
	}
	m.mu.Unlock()
	if m.posRepo == nil {
		return 0, "", Position{}, false
	}
	positions, err := m.posRepo.ActivePositions(context.Background(), 50)
	if err != nil {
		return 0, "", Position{}, false
	}
	for _, p := range positions {
		if strings.EqualFold(strings.TrimSpace(p.Order.Symbol), symbol) {
			side = strings.ToLower(strings.TrimSpace(p.Order.Side))
			pos = Position{
				TradeID:    p.Order.FreqtradeID,
				Symbol:     symbol,
				Side:       side,
				Amount:     valOrZero(p.Order.Amount),
				Stake:      valOrZero(p.Order.StakeAmount),
				Leverage:   valOrZero(p.Order.Leverage),
				EntryPrice: valOrZero(p.Order.Price),
			}
			return pos.TradeID, side, pos, true
		}
	}
	return 0, "", Position{}, false
}

// markPositionClosed 更新内存缓存。
func (m *Manager) markPositionClosed(tradeID int, symbol, side string, closePrice, stake float64, reason string, closedAt time.Time, pnlRatio float64) Position {
	m.mu.Lock()
	defer m.mu.Unlock()
	pos := m.positions[tradeID]
	pos.TradeID = tradeID
	pos.Symbol = symbol
	pos.Side = side
	pos.Closed = true
	pos.ClosedAt = closedAt
	pos.ExitPrice = closePrice
	pos.ExitReason = strings.TrimSpace(reason)
	pos.ExitPnLRatio = pnlRatio
	pos.Stake = stake
	m.positions[tradeID] = pos
	return pos
}

// peTiersFromCache 获取缓存中的 tier 记录（若无则返回空结构）。
func peTiersFromCache(m *Manager, tradeID int) database.LiveTierRecord {
	if m == nil {
		return database.LiveTierRecord{}
	}
	m.posCacheMu.RLock()
	defer m.posCacheMu.RUnlock()
	if m.posCache == nil {
		return database.LiveTierRecord{}
	}
	if p, ok := m.posCache[tradeID]; ok {
		return p.Tiers
	}
	return database.LiveTierRecord{}
}

// marshalRaw 将 webhook/raw 对象序列化为紧凑 JSON。
func marshalRaw(payload any) string {
	if payload == nil {
		return ""
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return string(buf)
}

func statusText(status database.LiveOrderStatus) string {
	switch status {
	case database.LiveOrderStatusClosed:
		return "closed"
	case database.LiveOrderStatusPartial:
		return "partial"
	case database.LiveOrderStatusRetrying:
		return "retrying"
	case database.LiveOrderStatusOpening:
		return "opening"
	case database.LiveOrderStatusClosingPartial:
		return "closing_partial"
	case database.LiveOrderStatusClosingFull:
		return "closing_full"
	default:
		return "open"
	}
}

func statusForClose(op database.OperationType) int {
	switch op {
	case database.OperationTakeProfit:
		return 1
	case database.OperationStopLoss:
		return 2
	default:
		return 0
	}
}

// cachePositions returns a snapshot of cached positions.
func (m *Manager) cachePositions() []database.LiveOrderWithTiers {
	if m == nil {
		return nil
	}
	m.posCacheMu.RLock()
	if len(m.posCache) == 0 {
		m.posCacheMu.RUnlock()
		return nil
	}
	out := make([]database.LiveOrderWithTiers, 0, len(m.posCache))
	for _, p := range m.posCache {
		out = append(out, p)
	}
	m.posCacheMu.RUnlock()
	return out
}

// upsertCache updates in-memory cache for a trade.
func (m *Manager) upsertCache(p database.LiveOrderWithTiers) {
	if m == nil {
		return
	}
	m.posCacheMu.Lock()
	if m.posCache == nil {
		m.posCache = make(map[int]database.LiveOrderWithTiers)
	}
	m.posCache[p.Order.FreqtradeID] = p
	m.posCacheMu.Unlock()
}

// deleteCache removes a trade from cache.
func (m *Manager) deleteCache(tradeID int) {
	if m == nil {
		return
	}
	m.posCacheMu.Lock()
	delete(m.posCache, tradeID)
	m.posCacheMu.Unlock()
}

// refreshCache loads active positions into cache (fallback when cache empty).
func (m *Manager) refreshCache(ctx context.Context) []database.LiveOrderWithTiers {
	if m == nil || m.posRepo == nil {
		return nil
	}
	list, err := m.posRepo.ActivePositions(ctx, 200)
	if err != nil {
		return nil
	}
	m.posCacheMu.Lock()
	if m.posCache == nil {
		m.posCache = make(map[int]database.LiveOrderWithTiers, len(list))
	}
	for _, p := range list {
		m.posCache[p.Order.FreqtradeID] = p
	}
	m.posCacheMu.Unlock()
	return list
}

// updateCacheOrderTiers merges order/tier into cache.
func (m *Manager) updateCacheOrderTiers(order database.LiveOrderRecord, tiers database.LiveTierRecord) {
	if m == nil {
		return
	}
	tradeID := order.FreqtradeID
	if tradeID == 0 {
		tradeID = tiers.FreqtradeID
	}
	if tradeID == 0 {
		return
	}
	m.posCacheMu.Lock()
	defer m.posCacheMu.Unlock()
	if m.posCache == nil {
		m.posCache = make(map[int]database.LiveOrderWithTiers)
	}
	cur := m.posCache[tradeID]
	if order.FreqtradeID > 0 {
		cur.Order = order
	}
	if tiers.FreqtradeID > 0 {
		cur.Tiers = tiers
	}
	m.posCache[tradeID] = cur
}

// addPendingExit registers a pending exit action.
func (m *Manager) addPendingExit(pe pendingExit) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.pendingExits[pe.TradeID] = pe
	m.mu.Unlock()
}

func (m *Manager) hasPendingExit(tradeID int) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	_, ok := m.pendingExits[tradeID]
	m.mu.Unlock()
	return ok
}

// popPendingExit removes and returns pending exit by tradeID.
func (m *Manager) popPendingExit(tradeID int) (pendingExit, bool) {
	if m == nil {
		return pendingExit{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	pe, ok := m.pendingExits[tradeID]
	if ok {
		delete(m.pendingExits, tradeID)
	}
	return pe, ok
}

// listPendingExits returns a snapshot of pending exits.
func (m *Manager) listPendingExits() []pendingExit {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]pendingExit, 0, len(m.pendingExits))
	for _, pe := range m.pendingExits {
		out = append(out, pe)
	}
	return out
}

// entryTag 生成 freqtrade entry tag。
func (m *Manager) entryTag() string {
	tag := strings.TrimSpace(m.cfg.EntryTag)
	if tag != "" {
		return tag
	}
	if h := strings.TrimSpace(m.horizonName); h != "" {
		return h
	}
	return "brale"
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

// notifyReconcileAdded 提示通过对账补齐的仓位（占位 tiers）。
func (m *Manager) notifyReconcileAdded(order database.LiveOrderRecord, tier database.LiveTierRecord) {
	title := "仓位对账：创建待确认仓位 ⚠️"
	lines := formatFields(
		[2]string{"交易ID", fmt.Sprintf("%d", order.FreqtradeID)},
		[2]string{"标的", strings.ToUpper(order.Symbol)},
		[2]string{"方向", strings.ToUpper(order.Side)},
		[2]string{"入场价", formatPrice(valOrZero(order.Price))},
		[2]string{"仓位", fmt.Sprintf("%s USDT", formatQty(valOrZero(order.StakeAmount)))},
		[2]string{"杠杆", fmt.Sprintf("x%.2f", valOrZero(order.Leverage))},
		[2]string{"状态", statusText(order.Status)},
	)
	lines = append(lines, "请在后台补齐止盈/止损与 tier1/2/3，保存后状态将切换为“正在持仓”。")
	m.notify(title, lines...)
}

// notifyReconcileClosed 提示幽灵仓位被标记关闭。
func (m *Manager) notifyReconcileClosed(order database.LiveOrderRecord, tier database.LiveTierRecord) {
	title := "仓位对账：本地已关闭 ✅"
	lines := formatFields(
		[2]string{"交易ID", fmt.Sprintf("%d", order.FreqtradeID)},
		[2]string{"标的", strings.ToUpper(order.Symbol)},
		[2]string{"方向", strings.ToUpper(order.Side)},
		[2]string{"状态", statusText(order.Status)},
		[2]string{"关闭时间", formatTime(time.Now())},
	)
	if tier.Status == 1 {
		lines = append(lines, "原因: freqtrade 无该仓位，已标记为止盈/完成")
	}
	m.notify(title, lines...)
}

func parseProfitRatio(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case float32:
		return float64(t)
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case uint64:
		return float64(t)
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		if err == nil {
			return f
		}
	case json.Number:
		f, err := t.Float64()
		if err == nil {
			return f
		}
	}
	return 0
}

// validateTierRecord 校验止盈止损与 tier 的完整性。
// enforceOffset 控制是否应用 min_stop_distance_pct 的偏移约束。
func (m *Manager) validateTierRecord(entry float64, side string, tier database.LiveTierRecord, enforceOffset bool) error {
	if m == nil {
		return fmt.Errorf("freqtrade manager 未初始化")
	}
	if entry <= 0 {
		return fmt.Errorf("开仓价格缺失，无法校验 tiers")
	}
	if tier.TakeProfit <= 0 || tier.StopLoss <= 0 || tier.Tier1 <= 0 || tier.Tier2 <= 0 || tier.Tier3 <= 0 {
		return fmt.Errorf("缺少止盈/止损/分段价格")
	}
	sum := tier.Tier1Ratio + tier.Tier2Ratio + tier.Tier3Ratio
	if math.Abs(sum-1) > 1e-3 {
		return fmt.Errorf("tier 比例之和必须等于 1，当前=%.4f", sum)
	}
	if !floatEqual(tier.Tier3, tier.TakeProfit) {
		return fmt.Errorf("tier3 必须等于 take_profit")
	}
	offsetPct := m.cfg.MinStopDistancePct
	if offsetPct < 0 {
		offsetPct = 0
	}
	offset := entry * offsetPct
	upper := entry + offset
	lower := entry - offset
	side = strings.ToLower(strings.TrimSpace(side))
	switch side {
	case "long":
		if enforceOffset && !(tier.StopLoss < lower) {
			return fmt.Errorf("多单止损必须低于当前价-偏移")
		}
		if enforceOffset {
			if !(upper <= tier.Tier1 && tier.Tier1 <= tier.Tier2 && tier.Tier2 <= tier.Tier3) {
				return fmt.Errorf("多单 tier 需递增且高于当前价+偏移")
			}
		} else if !(tier.Tier1 <= tier.Tier2 && tier.Tier2 <= tier.Tier3) {
			return fmt.Errorf("多单 tier 需递增")
		}
	case "short":
		if enforceOffset && !(tier.StopLoss > upper) {
			return fmt.Errorf("空单止损必须高于当前价+偏移")
		}
		if enforceOffset {
			if !(lower >= tier.Tier1 && tier.Tier1 >= tier.Tier2 && tier.Tier2 >= tier.Tier3) {
				return fmt.Errorf("空单 tier 需递减且低于当前价-偏移")
			}
		} else if !(tier.Tier1 >= tier.Tier2 && tier.Tier2 >= tier.Tier3) {
			return fmt.Errorf("空单 tier 需递减")
		}
	default:
		return fmt.Errorf("未知方向: %s", side)
	}
	return nil
}

// hasCompleteTier 判断 tier 是否包含全量价格与比例。
func hasCompleteTier(tier database.LiveTierRecord) bool {
	return tier.TakeProfit > 0 && tier.StopLoss > 0 &&
		tier.Tier1 > 0 && tier.Tier2 > 0 && tier.Tier3 > 0 &&
		tier.Tier1Ratio > 0 && tier.Tier2Ratio > 0 && tier.Tier3Ratio > 0
}

// recordTierInit 写入初始 tier 修改流水。
func (m *Manager) recordTierInit(ctx context.Context, tradeID int, tier database.LiveTierRecord, reason string) {
	if m == nil || m.posRepo == nil || tradeID <= 0 {
		return
	}
	if !hasCompleteTier(tier) {
		return
	}
	reason = strings.TrimSpace(reason)
	now := time.Now()
	fields := []struct {
		field database.TierField
		val   float64
	}{
		{database.TierFieldTakeProfit, tier.TakeProfit},
		{database.TierFieldStopLoss, tier.StopLoss},
		{database.TierFieldTier1, tier.Tier1},
		{database.TierFieldTier2, tier.Tier2},
		{database.TierFieldTier3, tier.Tier3},
		{database.TierFieldTier1Ratio, tier.Tier1Ratio},
		{database.TierFieldTier2Ratio, tier.Tier2Ratio},
		{database.TierFieldTier3Ratio, tier.Tier3Ratio},
	}
	for _, f := range fields {
		m.posRepo.InsertModification(ctx, database.TierModificationLog{
			FreqtradeID: tradeID,
			Field:       f.field,
			OldValue:    "",
			NewValue:    formatPrice(f.val),
			Source:      1,
			Reason:      reason,
			Timestamp:   now,
		})
	}
}

// fetchTradeByID 从 freqtrade 查询指定 trade_id。
func (m *Manager) fetchTradeByID(ctx context.Context, tradeID int) (*Trade, error) {
	if m == nil || m.client == nil {
		return nil, fmt.Errorf("freqtrade client 未初始化")
	}
	trades, err := m.client.ListTrades(ctx)
	if err != nil {
		return nil, err
	}
	for i := range trades {
		if trades[i].ID == tradeID {
			return &trades[i], nil
		}
	}
	return nil, nil
}
