package app

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"brale/internal/ai/parser"
	brcfg "brale/internal/config"
	"brale/internal/decision"
	freqexec "brale/internal/executor/freqtrade"
	"brale/internal/gateway/database"
	"brale/internal/gateway/notifier"
	"brale/internal/logger"
	"brale/internal/market"
	"brale/internal/pkg/text"
	"brale/internal/store"
)

// LiveService 负责实时行情、AI 决策循环与通知。
type LiveService struct {
	cfg                 *brcfg.Config
	ks                  market.KlineStore
	updater             *market.WSUpdater
	engine              decision.Decider
	tg                  *notifier.Telegram
	decLogs             *database.DecisionLogStore
	orderRec            market.Recorder
	lastDec             *lastDecisionCache
	includeLastDecision bool

	symbols       []string
	hIntervals    []string
	horizonName   string
	profile       brcfg.HorizonProfile
	hSummary      string
	warmupSummary string

	lastOpen    map[string]time.Time
	lastRawJSON string

	freqManager *freqexec.Manager
	visionReady bool

	priceCache   map[string]cachedQuote
	priceCacheMu sync.RWMutex
	lastPrice    map[string]lastPriceEntry
	lastPriceMu  sync.RWMutex

	tradeStreamMu sync.Mutex
	tradeStreamUp bool
}

type wsErrorResetter interface {
	ClearLastError()
}

type cachedQuote struct {
	quote freqexec.TierPriceQuote
	ts    int64
}

type lastPriceEntry struct {
	price float64
	ts    int64
}

const lastPriceMaxAge = 10 * time.Second

// Run 启动实时服务，直到 ctx 取消。
func (s *LiveService) Run(ctx context.Context) error {
	if s == nil || s.cfg == nil {
		return fmt.Errorf("live service not initialized")
	}
	if s.updater != nil {
		s.updater.OnEvent = s.onCandleEvent
	}
	if s.freqManager != nil {
		s.freqManager.StartPriceMonitor(ctx)
		s.freqManager.StartPositionSync(ctx)
		s.freqManager.StartFastStatusSync(ctx)
	}

	cfg := s.cfg
	firstWSConnected := false
	s.updater.OnConnected = func() {
		s.clearWSLastError()
		if s.tg == nil {
			return
		}
		if !firstWSConnected {
			firstWSConnected = true
			msg := "*Brale 启动成功* ✅\nWS 已连接并开始订阅"
			if summary := strings.TrimSpace(s.hSummary); summary != "" {
				msg += "\n```text\n" + summary + "\n```"
			}
			if warmup := strings.TrimSpace(s.warmupSummary); warmup != "" {
				msg += "\n" + warmup
			}
			_ = s.tg.SendText(msg)
		}
	}
	s.updater.OnDisconnected = func(err error) {
		if s.tg == nil {
			return
		}
		msg := "WS 断线"
		if err != nil {
			msg = msg + ": " + err.Error()
		}
		_ = s.tg.SendText(msg)
	}
	go func() {
		if err := s.updater.Start(ctx, s.symbols, s.hIntervals); err != nil {
			logger.Errorf("启动行情订阅失败: %v", err)
		}
	}()
	s.startTradePriceStream(ctx)

	decisionInterval := time.Duration(cfg.AI.DecisionIntervalSeconds) * time.Second
	if decisionInterval <= 0 {
		decisionInterval = time.Minute
	}
	return s.runDecisionLoop(ctx, decisionInterval)
}

func (s *LiveService) clearWSLastError() {
	if s == nil || s.updater == nil || s.updater.Source == nil {
		return
	}
	if resetter, ok := s.updater.Source.(wsErrorResetter); ok {
		resetter.ClearLastError()
	}
}

func (s *LiveService) runDecisionLoop(ctx context.Context, decisionInterval time.Duration) error {
	decisionTicker := time.NewTicker(decisionInterval)
	cacheTicker := time.NewTicker(15 * time.Second)
	statsTicker := time.NewTicker(60 * time.Second)
	defer decisionTicker.Stop()
	defer cacheTicker.Stop()
	defer statsTicker.Stop()

	human := fmt.Sprintf("%d 秒", int(decisionInterval.Seconds()))
	if seconds := int(decisionInterval.Seconds()); seconds%60 == 0 {
		human = fmt.Sprintf("%d 分钟", seconds/60)
	}
	logger.Infof("Brale 启动完成。开始订阅 K 线并写入缓存；每 %s 进行一次 AI 决策。按 Ctrl+C 退出。\n", human)

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-cacheTicker.C:
			for _, sym := range s.symbols {
				for _, iv := range s.hIntervals {
					if kl, err := s.ks.Get(ctx, sym, iv); err == nil {
						cnt := len(kl)
						tail := ""
						if cnt > 0 {
							t := time.UnixMilli(kl[cnt-1].CloseTime)
							tail = fmt.Sprintf(" 收=%.4f 结束=%d(%s)", kl[cnt-1].Close, kl[cnt-1].CloseTime, t.UTC().Format(time.RFC3339))
						}
						logger.Debugf("缓存: %s %s 条数=%d%s", sym, iv, cnt, tail)
					}
				}
			}
		case <-statsTicker.C:
			if s.updater != nil {
				stats := s.updater.Stats()
				if stats.LastError != "" {
					logger.Errorf("WS统计: 最后错误=%s", stats.LastError)
				}
				logger.Debugf("ws 统计:重连 = %v,订阅错误=%v", stats.Reconnects, stats.SubscribeErrors)
			}
		case <-decisionTicker.C:
			if err := s.tickDecision(ctx); err != nil {
				logger.Warnf("AI 决策失败: %v", err)
			}
		}
	}
}

func (s *LiveService) prepareDecisionInput(ctx context.Context) (decision.Context, bool) {
	input := decision.Context{Candidates: s.symbols}
	input.Account = s.accountSnapshot()
	if exp, ok := s.ks.(store.SnapshotExporter); ok {
		symbols := append([]string(nil), input.Candidates...)
		if max := 6; len(symbols) > max {
			symbols = symbols[:max]
		}
		input.Analysis = decision.BuildAnalysisContexts(decision.AnalysisBuildInput{
			Context:     ctx,
			Exporter:    exp,
			Symbols:     symbols,
			Intervals:   s.hIntervals,
			Limit:       s.cfg.Kline.MaxCached,
			SliceLength: s.profile.AnalysisSlice,
			SliceDrop:   s.profile.SliceDropTail,
			HorizonName: s.horizonName,
			Indicators:  s.profile.Indicators,
			WithImages:  s.visionReady,
		})
	}
	input.Positions = s.livePositions(input.Account)
	hasPositions := len(input.Positions) > 0
	if !hasPositions {
		if s.lastDec != nil {
			s.lastDec.Reset()
		}
		s.lastRawJSON = ""
		return input, false
	}
	if s.includeLastDecision && s.lastDec != nil {
		snap := s.filterLastDecisionSnapshot(s.lastDec.Snapshot(time.Now()), input.Positions)
		if len(snap) > 0 {
			input.LastDecisions = snap
			input.LastRawJSON = s.lastRawJSON
		}
	}
	return input, true
}

func (s *LiveService) logModelOutput(res decision.DecisionResult) {
	raw := strings.TrimSpace(res.RawOutput)
	if raw == "" {
		return
	}
	_, start, ok := parser.ExtractJSONWithOffset(raw)
	if ok {
		cot := strings.TrimSpace(raw[:start])
		cot = text.Truncate(cot, 4800)
		logger.Infof("")
		logger.InfoBlock(decision.RenderBlockTable("AI[final] 思维链", cot))
		return
	}
	logger.Infof("")
	logger.InfoBlock(decision.RenderBlockTable("AI[final] 思维链", "失败"))
}

func (s *LiveService) notifyMetaSummary(res decision.DecisionResult) {
	if s.tg == nil || !strings.EqualFold(s.cfg.AI.Aggregation, "meta") {
		return
	}
	summary := strings.TrimSpace(res.MetaSummary)
	if summary == "" {
		return
	}
	if err := s.sendMetaSummaryTelegram(summary); err != nil {
		logger.Warnf("Telegram 推送失败(meta): %v", err)
	}
}

func (s *LiveService) prepareDecisions(items []decision.Decision, hasPositions bool) []decision.Decision {
	if len(items) == 0 {
		return nil
	}
	prepared := make([]decision.Decision, len(items))
	copy(prepared, items)
	for i := range prepared {
		prepared[i].Action = decision.NormalizeAction(prepared[i].Action)
	}
	prepared = decision.OrderAndDedup(prepared)
	prepared = s.filterPositionDependentDecisions(prepared, hasPositions)
	if len(prepared) > 0 {
		logger.Infof("")
		logger.InfoBlock(decision.RenderFinalDecisionsTable(prepared, 0))
	}
	return prepared
}

func (s *LiveService) executeDecisions(ctx context.Context, decisions []decision.Decision, traceID string) []decision.Decision {
	if len(decisions) == 0 {
		return nil
	}
	cfg := s.cfg
	validateIv := ""
	if len(s.hIntervals) > 0 {
		validateIv = s.hIntervals[0]
	}
	accepted := make([]decision.Decision, 0, len(decisions))
	newOpens := 0
	for _, original := range decisions {
		d := original
		marketPrice := 0.0
		s.applyTradingDefaults(&d)
		if err := decision.Validate(&d); err != nil {
			logger.Warnf("AI 决策不合规，已忽略: %v | %+v", err, d)
			if strings.EqualFold(d.Action, "update_tiers") && strings.Contains(err.Error(), "需提供 tiers") {
				logger.Warnf("AI 决策缺少 tiers，附: %+v", d.Tiers)
			}
			continue
		}
		if validateIv != "" {
			if kl, _ := s.ks.Get(ctx, d.Symbol, validateIv); len(kl) > 0 {
				price := kl[len(kl)-1].Close
				marketPrice = price
				if err := decision.ValidateWithPrice(&d, price, cfg.Advanced.MinRiskReward); err != nil {
					logger.Warnf("AI 决策RR校验失败，已忽略: %v | %+v", err, d)
					continue
				}
				s.enforceTierDistance(&d, price)
			}
		}
		if s.freqManager != nil {
			if err := s.freqtradeHandleDecision(ctx, traceID, d); err != nil {
				logger.Warnf("freqtrade 执行失败，跳过: %v | %+v", err, d)
				continue
			}
		}
		accepted = append(accepted, d)
		s.logDecision(d)
		if d.Action != "open_long" && d.Action != "open_short" {
			continue
		}
		if newOpens >= cfg.Advanced.MaxOpensPerCycle {
			logger.Infof("跳过超出本周期开仓上限: %s %s", d.Symbol, d.Action)
			continue
		}
		key := d.Symbol + "#" + d.Action
		if prev, ok := s.lastOpen[key]; ok {
			cooldown := time.Duration(cfg.Advanced.OpenCooldownSeconds) * time.Second
			if time.Since(prev) < cooldown {
				remain := float64(cooldown-time.Since(prev)) / float64(time.Second)
				logger.Infof("跳过频繁开仓（冷却中）: %s 剩余 %.0fs", key, remain)
				continue
			}
		}
		s.lastOpen[key] = time.Now()
		newOpens++
		s.recordLiveOrder(ctx, d, marketPrice, validateIv)
		s.notifyOpen(ctx, d, marketPrice, validateIv)
	}
	return accepted
}

func (s *LiveService) persistDecisionOutcome(ctx context.Context, accepted []decision.Decision, rawJSON string) {
	if len(accepted) == 0 {
		return
	}
	s.persistLastDecisions(ctx, accepted)
	if raw := strings.TrimSpace(rawJSON); raw != "" {
		s.lastRawJSON = raw
		return
	}
	if buf, err := json.Marshal(accepted); err == nil {
		s.lastRawJSON = string(buf)
	}
}

// Close 释放 LiveService 持有的资源。
func (s *LiveService) Close() {
	if s == nil {
		return
	}
	if s.updater != nil {
		s.updater.Close()
	}
	if s.decLogs != nil {
		_ = s.decLogs.Close()
	}
}

func (s *LiveService) tickDecision(ctx context.Context) error {
	start := time.Now()
	input, hasPositions := s.prepareDecisionInput(ctx)
	logger.Infof("AI 决策循环开始 candidates=%d positions=%d", len(input.Candidates), len(input.Positions))
	res, err := s.engine.Decide(ctx, input)
	if err != nil {
		return err
	}
	traceID := s.ensureTraceID(res.TraceID)
	if len(res.Decisions) == 0 {
		logger.Infof("AI 决策为空（观望） trace=%s 耗时=%s", traceID, time.Since(start))
		return nil
	}
	s.logModelOutput(res)
	s.notifyMetaSummary(res)
	prepared := s.prepareDecisions(res.Decisions, hasPositions)
	accepted := s.executeDecisions(ctx, prepared, traceID)
	s.persistDecisionOutcome(ctx, accepted, res.RawJSON)
	logger.Infof("AI 决策循环结束 trace=%s 原始=%d 接受=%d 耗时=%s", traceID, len(prepared), len(accepted), time.Since(start))
	return nil
}

func (s *LiveService) applyTradingDefaults(d *decision.Decision) {
	if d == nil {
		return
	}
	if d.Action != "open_long" && d.Action != "open_short" {
		return
	}
	if d.Leverage <= 0 {
		if def := s.cfg.Trading.DefaultLeverage; def > 0 {
			logger.Debugf("决策 %s 缺少 leverage，使用默认 %dx", d.Symbol, def)
			d.Leverage = def
		}
	}
	if d.PositionSizeUSD <= 0 {
		if size := s.cfg.Trading.PositionSizeUSD(); size > 0 {
			logger.Debugf("决策 %s 缺少 position_size_usd，使用默认 %.2f USDT", d.Symbol, size)
			d.PositionSizeUSD = size
		}
	}
}

func (s *LiveService) enforceTierDistance(d *decision.Decision, price float64) {
	if d == nil {
		return
	}
	if d.Action != "open_long" && d.Action != "open_short" {
		return
	}
	if price <= 0 || d.TakeProfit <= 0 || d.Tiers == nil || d.Tiers.Tier1Target <= 0 {
		return
	}
	minPct := s.cfg.Advanced.TierMinDistancePct
	if minPct <= 0 {
		return
	}
	oldT1 := d.Tiers.Tier1Target
	diff := math.Abs(oldT1-price) / price
	if diff >= minPct {
		return
	}
	tp := d.TakeProfit
	d.Tiers.Tier1Target = tp
	d.Tiers.Tier2Target = tp
	d.Tiers.Tier3Target = tp
	logger.Infof("tier1 target %.4f 太接近价格 %.4f (%.4f%% < %.4f%%)，已将所有三段统一到止盈价 %.4f", oldT1, price, diff*100, minPct*100, tp)
}

func (s *LiveService) notifyOpen(ctx context.Context, d decision.Decision, entryPrice float64, validateIv string) {
	if s.tg == nil {
		return
	}
	rrVal := 0.0
	if entryPrice > 0 {
		var risk, reward float64
		switch d.Action {
		case "open_long":
			risk = entryPrice - d.StopLoss
			reward = d.TakeProfit - entryPrice
		case "open_short":
			risk = d.StopLoss - entryPrice
			reward = entryPrice - d.TakeProfit
		}
		if risk > 0 && reward > 0 {
			rrVal = reward / risk
		}
	}
	if entryPrice > 0 {
		if rrVal > 0 {
			logger.Infof("开仓详情: %s %s entry=%.4f RR=%.2f sl=%.4f tp=%.4f",
				d.Symbol, d.Action, entryPrice, rrVal, d.StopLoss, d.TakeProfit)
		} else {
			logger.Infof("开仓详情: %s %s entry=%.4f sl=%.4f tp=%.4f",
				d.Symbol, d.Action, entryPrice, d.StopLoss, d.TakeProfit)
		}
	}

	ts := time.Now().UTC().Format(time.RFC3339)
	var b strings.Builder
	b.WriteString("📈 开仓信号\n")
	b.WriteString("```\n")
	fmt.Fprintf(&b, "symbol   : %s\n", d.Symbol)
	fmt.Fprintf(&b, "action   : %s\n", d.Action)
	if validateIv != "" {
		fmt.Fprintf(&b, "interval : %s\n", validateIv)
	}
	if entryPrice > 0 {
		fmt.Fprintf(&b, "entry    : %.4f\n", entryPrice)
	}
	fmt.Fprintf(&b, "sl       : %.4f\n", d.StopLoss)
	fmt.Fprintf(&b, "tp       : %.4f\n", d.TakeProfit)
	if rrVal > 0 {
		fmt.Fprintf(&b, "RR       : %.2f\n", rrVal)
	}
	fmt.Fprintf(&b, "leverage : %dx\n", d.Leverage)
	fmt.Fprintf(&b, "size     : %.0f USDT\n", d.PositionSizeUSD)
	if d.Confidence > 0 {
		fmt.Fprintf(&b, "conf     : %d\n", d.Confidence)
	}
	fmt.Fprintf(&b, "time     : %s\n", ts)
	b.WriteString("```\n")
	if reason := strings.TrimSpace(d.Reasoning); reason != "" {
		msg := reason
		if len(msg) > 1500 {
			msg = msg[:1500] + "..."
		}
		msg = strings.ReplaceAll(msg, "```", "'''")
		b.WriteString("理由:\n```\n")
		b.WriteString(msg)
		b.WriteString("\n```")
	}
	msg := b.String()
	if len(msg) > 3800 {
		msg = msg[:3800] + "..."
	}
	if err := s.tg.SendText(msg); err != nil {
		logger.Warnf("Telegram 推送失败: %v", err)
	}
}

func (s *LiveService) recordLiveOrder(ctx context.Context, d decision.Decision, entryPrice float64, timeframe string) {
	if s.orderRec == nil {
		return
	}
	symbol := strings.ToUpper(strings.TrimSpace(d.Symbol))
	if symbol == "" {
		return
	}
	payload := market.Order{
		Symbol:     symbol,
		Action:     d.Action,
		Side:       deriveSide(d.Action),
		Type:       "signal",
		Price:      entryPrice,
		Quantity:   0,
		Notional:   d.PositionSizeUSD,
		Fee:        0,
		Timeframe:  timeframe,
		DecidedAt:  time.Now(),
		TakeProfit: d.TakeProfit,
		StopLoss:   d.StopLoss,
	}
	if data, err := json.Marshal(d); err == nil {
		payload.Decision = data
	}
	if _, err := s.orderRec.RecordOrder(ctx, &payload); err != nil {
		logger.Warnf("记录 live order 失败: %v", err)
	}
}

func (s *LiveService) persistLastDecisions(ctx context.Context, decisions []decision.Decision) {
	if !s.includeLastDecision || len(decisions) == 0 || s.lastDec == nil || s.decLogs == nil {
		return
	}
	now := time.Now()
	for _, d := range decisions {
		symbol := strings.ToUpper(strings.TrimSpace(d.Symbol))
		if symbol == "" {
			continue
		}
		mem := decision.DecisionMemory{
			Symbol:    symbol,
			Horizon:   s.horizonName,
			DecidedAt: now,
			Decisions: []decision.Decision{d},
		}
		s.lastDec.Set(mem)
		rec := decision.LastDecisionRecord{
			Symbol:    symbol,
			Horizon:   s.horizonName,
			DecidedAt: now,
			Decisions: []decision.Decision{d},
		}
		if err := s.decLogs.SaveLastDecision(ctx, rec); err != nil {
			logger.Warnf("保存 LastDecision 失败: %v", err)
		}
	}
}

func (s *LiveService) filterLastDecisionSnapshot(records []decision.DecisionMemory, positions []decision.PositionSnapshot) []decision.DecisionMemory {
	if len(records) == 0 || len(positions) == 0 {
		return nil
	}
	posMap := make(map[string]bool, len(positions))
	for _, p := range positions {
		sym := strings.ToUpper(strings.TrimSpace(p.Symbol))
		if sym != "" {
			posMap[sym] = true
		}
	}
	out := make([]decision.DecisionMemory, 0, len(records))
	for _, mem := range records {
		sym := strings.ToUpper(strings.TrimSpace(mem.Symbol))
		if sym == "" || len(mem.Decisions) == 0 {
			continue
		}
		if !posMap[sym] {
			if s.lastDec != nil {
				s.lastDec.Delete(sym)
			}
			continue
		}
		dup := mem
		dup.Symbol = sym
		dup.Decisions = append([]decision.Decision(nil), mem.Decisions...)
		out = append(out, dup)
	}
	return out
}

func (s *LiveService) filterPositionDependentDecisions(decisions []decision.Decision, hasPositions bool) []decision.Decision {
	if hasPositions || len(decisions) == 0 {
		return decisions
	}
	allowed := decisions[:0]
	dropped := 0
	for _, d := range decisions {
		switch d.Action {
		case "close_long", "close_short", "update_tiers", "adjust_stop_loss", "adjust_take_profit":
			dropped++
			continue
		}
		allowed = append(allowed, d)
	}
	if dropped > 0 {
		logger.Infof("当前无持仓，忽略 %d 条需持仓的决策", dropped)
	}
	return allowed
}

func (s *LiveService) livePositions(account decision.AccountSnapshot) []decision.PositionSnapshot {
	if s.freqManager == nil {
		return nil
	}
	positions := s.freqManager.Positions()
	if len(positions) == 0 {
		return nil
	}
	total := account.Total
	for i := range positions {
		stake := positions[i].Stake
		if stake <= 0 && positions[i].Quantity > 0 && positions[i].EntryPrice > 0 {
			stake = positions[i].Quantity * positions[i].EntryPrice / positions[i].Leverage
		}
		if total > 0 && stake > 0 {
			positions[i].AccountRatio = stake / total
		}
	}
	return positions
}

func (s *LiveService) startTradePriceStream(ctx context.Context) {
	if s == nil || s.updater == nil || s.updater.Source == nil {
		logger.Warnf("实时成交价订阅跳过：缺少行情源")
		return
	}
	opts := market.SubscribeOptions{
		Buffer: 2048,
		OnConnect: func() {
			if ctx.Err() != nil {
				return
			}
			s.tradeStreamMu.Lock()
			wasUp := s.tradeStreamUp
			s.tradeStreamUp = true
			s.tradeStreamMu.Unlock()
			if s.tg != nil {
				msg := "实时成交价流已建立 ✅"
				if wasUp {
					msg = "实时成交价流已恢复 ✅"
				}
				_ = s.tg.SendText(msg)
			}
		},
		OnDisconnect: func(err error) {
			if ctx.Err() != nil {
				return
			}
			s.tradeStreamMu.Lock()
			s.tradeStreamUp = false
			s.tradeStreamMu.Unlock()
			if s.tg != nil {
				reason := "未知"
				if err != nil && err.Error() != "" {
					reason = err.Error()
				}
				_ = s.tg.SendText(fmt.Sprintf("实时成交价流断线 ⚠️\n错误: %s", reason))
			}
		},
	}
	stream, err := s.updater.Source.SubscribeTrades(ctx, s.symbols, opts)
	if err != nil {
		logger.Warnf("订阅实时成交价失败: %v", err)
		return
	}
	logger.Infof("✓ 实时成交价订阅已启动 (aggTrade)")
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-stream:
				if !ok {
					return
				}
				s.handleTradePrice(ev)
			}
		}
	}()
}

func (s *LiveService) handleTradePrice(ev market.TradeEvent) {
	if s == nil {
		return
	}
	price := ev.Price
	if price <= 0 {
		return
	}
	symbol := strings.ToUpper(strings.TrimSpace(ev.Symbol))
	if symbol == "" {
		return
	}
	ts := ev.EventTime
	if ts == 0 {
		ts = ev.TradeTime
	}
	if ts == 0 {
		ts = time.Now().UnixMilli()
	}
	s.lastPriceMu.Lock()
	if s.lastPrice == nil {
		s.lastPrice = make(map[string]lastPriceEntry)
	}
	s.lastPrice[symbol] = lastPriceEntry{price: price, ts: ts}
	s.lastPriceMu.Unlock()
	s.priceCacheMu.Lock()
	cq := s.priceCache[symbol]
	cq.quote.Last = price
	cq.ts = ts
	s.priceCache[symbol] = cq
	s.priceCacheMu.Unlock()

	if s.freqManager != nil {
		s.freqManager.PublishPrice(symbol, freqexec.TierPriceQuote{
			Last: price,
			High: price,
			Low:  price,
		})
	}
}

func (s *LiveService) freshLastPrice(symbol string) (float64, bool) {
	if s == nil {
		return 0, false
	}
	s.lastPriceMu.RLock()
	entry, ok := s.lastPrice[symbol]
	s.lastPriceMu.RUnlock()
	if !ok || entry.price <= 0 {
		return 0, false
	}
	if entry.ts <= 0 {
		return entry.price, true
	}
	if time.Since(time.UnixMilli(entry.ts)) > lastPriceMaxAge {
		return 0, false
	}
	return entry.price, true
}

func (s *LiveService) latestPrice(ctx context.Context, symbol string) float64 {
	// 优先使用实时 lastPrice（仅在数据新鲜时）
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	if lp, ok := s.freshLastPrice(symbol); ok {
		return lp
	}
	quote := s.latestPriceQuote(ctx, symbol)
	return quote.Last
}

func (s *LiveService) latestPriceQuote(ctx context.Context, symbol string) freqexec.TierPriceQuote {
	var quote freqexec.TierPriceQuote
	if s == nil || s.ks == nil {
		return quote
	}
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	// 带上 lastPrice（仅使用新鲜数据）
	lp, lastPriceFresh := s.freshLastPrice(symbol)
	if cached, ok := s.cachedQuote(symbol); ok {
		quote = cached
		if lastPriceFresh {
			quote.Last = lp
		}
		return quote
	}
	interval := ""
	if len(s.profile.EntryTimeframes) > 0 {
		interval = s.profile.EntryTimeframes[0]
	} else if len(s.hIntervals) > 0 {
		interval = s.hIntervals[0]
	} else {
		interval = "1m"
	}
	klines, err := s.ks.Get(ctx, symbol, interval)
	if err != nil || len(klines) == 0 {
		return quote
	}
	last := klines[len(klines)-1]
	ts := last.CloseTime
	if ts == 0 {
		ts = last.OpenTime
	}
	if ts > 0 {
		const maxAge = 30 * time.Second
		age := time.Since(time.UnixMilli(ts))
		if age > maxAge {
			logger.Warnf("价格回退数据过期，跳过自动触发: %s %s age=%s", symbol, interval, age.Truncate(time.Second))
			return quote
		}
	}
	quote.Last = last.Close
	quote.High = last.High
	quote.Low = last.Low
	if lastPriceFresh {
		quote.Last = lp
	}
	return quote
}

func (s *LiveService) cachedQuote(symbol string) (freqexec.TierPriceQuote, bool) {
	s.priceCacheMu.RLock()
	cq, ok := s.priceCache[symbol]
	s.priceCacheMu.RUnlock()
	if !ok || (cq.quote.Last <= 0 && cq.quote.High <= 0 && cq.quote.Low <= 0) {
		return freqexec.TierPriceQuote{}, false
	}
	if cq.ts <= 0 {
		return cq.quote, true
	}
	if time.Since(time.UnixMilli(cq.ts)) > 30*time.Second {
		return freqexec.TierPriceQuote{}, false
	}
	return cq.quote, true
}

func (s *LiveService) onCandleEvent(evt market.CandleEvent) {
	if s == nil {
		return
	}
	symbol := strings.ToUpper(strings.TrimSpace(evt.Symbol))
	if symbol == "" {
		return
	}
	c := evt.Candle
	if c.Close <= 0 && c.High <= 0 && c.Low <= 0 {
		return
	}
	ts := c.CloseTime
	if ts == 0 {
		ts = c.OpenTime
	}

	q := freqexec.TierPriceQuote{Last: c.Close, High: c.High, Low: c.Low}
	s.priceCacheMu.Lock()
	s.priceCache[symbol] = cachedQuote{quote: q, ts: ts}
	s.priceCacheMu.Unlock()
}

func (s *LiveService) accountSnapshot() decision.AccountSnapshot {
	if s == nil || s.freqManager == nil {
		return decision.AccountSnapshot{Currency: "USDT"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	bal, err := s.freqManager.RefreshBalance(ctx)
	if err != nil {
		logger.Warnf("获取 freqtrade 余额失败: %v", err)
		bal = s.freqManager.AccountBalance()
	}
	currency := bal.StakeCurrency
	if strings.TrimSpace(currency) == "" {
		currency = "USDT"
	}
	return decision.AccountSnapshot{
		Total:     bal.Total,
		Available: bal.Available,
		Used:      bal.Used,
		Currency:  currency,
		UpdatedAt: bal.UpdatedAt,
	}
}

func (s *LiveService) logDecision(d decision.Decision) {
	base := fmt.Sprintf("AI 决策: %s %s", d.Symbol, d.Action)
	parts := make([]string, 0, 6)
	switch d.Action {
	case "open_long", "open_short":
		parts = append(parts,
			fmt.Sprintf("lev=%d", d.Leverage),
			fmt.Sprintf("size=%.0f", d.PositionSizeUSD),
			fmt.Sprintf("sl=%.4f", d.StopLoss),
			fmt.Sprintf("tp=%.4f", d.TakeProfit),
		)
	case "close_long", "close_short":
		if d.CloseRatio > 0 {
			parts = append(parts, fmt.Sprintf("close_ratio=%.2f", d.CloseRatio))
		}
	}
	if d.Confidence > 0 {
		parts = append(parts, fmt.Sprintf("conf=%d", d.Confidence))
	}
	if reason := strings.TrimSpace(d.Reasoning); reason != "" {
		parts = append(parts, "理由="+reason)
	}
	if len(parts) > 0 {
		base += " " + strings.Join(parts, " ")
	}
	logger.Infof("%s", base)
}

func (s *LiveService) sendMetaSummaryTelegram(summary string) error {
	if s.tg == nil {
		return nil
	}
	header := "🗳️ Meta 聚合投票\n多模型存在分歧，采用加权多数决。\n"
	body := strings.ReplaceAll(summary, "```", "'''")
	lines := strings.Split(body, "\n")
	if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "Meta聚合：多模型存在分歧，采用加权多数决。" {
		lines = lines[1:]
		if len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
			lines = lines[1:]
		}
	}

	headerSent := false
	newChunk := func() (string, int) {
		ch := "```\n"
		if !headerSent {
			ch = header + ch
			headerSent = true
		}
		return ch, len(ch)
	}
	chunk, clen := newChunk()
	for i, ln := range lines {
		if clen+len(ln)+1+3 > 4096 {
			chunk += "```"
			if err := s.tg.SendText(chunk); err != nil {
				return err
			}
			chunk, clen = newChunk()
		}
		chunk += ln + "\n"
		clen += len(ln) + 1
		if i == len(lines)-1 {
			chunk += "```"
			if err := s.tg.SendText(chunk); err != nil {
				return err
			}
		}
	}
	if len(lines) == 0 {
		chunk = header + "```\n```"
		if err := s.tg.SendText(chunk); err != nil {
			return err
		}
	}
	return nil
}

func deriveSide(action string) string {
	switch action {
	case "open_long", "close_long":
		return "long"
	case "open_short", "close_short":
		return "short"
	default:
		return ""
	}
}

func (s *LiveService) freqtradeHandleDecision(ctx context.Context, traceID string, d decision.Decision) error {
	if s.freqManager == nil {
		return nil
	}
	traceID = s.ensureTraceID(traceID)
	logger.Infof("freqtrade: 接收决策 trace=%s symbol=%s action=%s", traceID, strings.ToUpper(strings.TrimSpace(d.Symbol)), d.Action)
	var marketPrice float64
	if d.Action == "open_long" || d.Action == "open_short" {
		price := s.latestPrice(ctx, d.Symbol)
		if price <= 0 {
			err := fmt.Errorf("获取 %s 当前价格失败，无法开仓", strings.ToUpper(d.Symbol))
			logger.Warnf("freqtrade: %v", err)
			return err
		}
		if err := validateDecisionForOpen(d, price, s.cfg.Freqtrade.MinStopDistancePct); err != nil {
			logger.Warnf("freqtrade: 决策非法 symbol=%s action=%s err=%v", d.Symbol, d.Action, err)
			return err
		}
		logger.Infof("freqtrade: 验证通过 trace=%s symbol=%s side=%s price=%.4f sl=%.4f tp=%.4f", traceID, strings.ToUpper(strings.TrimSpace(d.Symbol)), deriveSide(d.Action), price, d.StopLoss, d.TakeProfit)
		marketPrice = price
		traceID = s.freqManager.CacheDecision(traceID, d)
	}
	if err := s.freqManager.Execute(ctx, freqexec.DecisionInput{
		TraceID:     traceID,
		Decision:    d,
		MarketPrice: marketPrice,
	}); err != nil {
		return err
	}
	logger.Infof("freqtrade: 决策已提交 trace=%s symbol=%s action=%s", traceID, strings.ToUpper(strings.TrimSpace(d.Symbol)), d.Action)
	return nil
}

// HandleFreqtradeWebhook implements livehttp.FreqtradeWebhookHandler.
func (s *LiveService) HandleFreqtradeWebhook(ctx context.Context, msg freqexec.WebhookMessage) error {
	if s == nil || s.freqManager == nil {
		return fmt.Errorf("live service 未初始化")
	}
	logger.Infof("收到 freqtrade webhook: type=%s trade_id=%d pair=%s direction=%s",
		strings.ToLower(strings.TrimSpace(msg.Type)),
		int(msg.TradeID),
		strings.ToUpper(strings.TrimSpace(msg.Pair)),
		strings.ToLower(strings.TrimSpace(msg.Direction)))
	s.freqManager.HandleWebhook(ctx, msg)
	return nil
}

// ListFreqtradePositions implements livehttp.FreqtradeWebhookHandler.
func (s *LiveService) ListFreqtradePositions(ctx context.Context, opts freqexec.PositionListOptions) (freqexec.PositionListResult, error) {
	// 默认回传分页参数，避免零值。
	result := freqexec.PositionListResult{
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
	if s == nil || s.freqManager == nil {
		return result, nil
	}
	return s.freqManager.PositionsForAPI(ctx, opts)
}

// CloseFreqtradePosition implements livehttp.FreqtradeWebhookHandler.
func (s *LiveService) CloseFreqtradePosition(ctx context.Context, symbol, side string, closeRatio float64) error {
	if s == nil || s.freqManager == nil {
		return fmt.Errorf("live service 未初始化")
	}
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	if symbol == "" {
		return fmt.Errorf("symbol 不能为空")
	}
	side = strings.ToLower(strings.TrimSpace(side))
	var action string
	switch side {
	case "long":
		action = "close_long"
	case "short":
		action = "close_short"
	default:
		return fmt.Errorf("side 只能是 long 或 short")
	}
	traceID := s.ensureTraceID("")
	decision := decision.Decision{
		Symbol:     symbol,
		Action:     action,
		CloseRatio: closeRatio,
	}
	logger.Infof("freqtrade: 手动平仓请求 symbol=%s side=%s ratio=%.4f", symbol, side, closeRatio)
	return s.freqtradeHandleDecision(ctx, traceID, decision)
}

// UpdateFreqtradeTiers allows manual tier adjustments via HTTP API.
func (s *LiveService) UpdateFreqtradeTiers(ctx context.Context, req freqexec.TierUpdateRequest) error {
	if s == nil || s.freqManager == nil {
		return fmt.Errorf("live service 未初始化")
	}
	if req.Tier3Target > 0 {
		req.TakeProfit = req.Tier3Target
	}
	logger.Infof("freqtrade: 手动 tier 调整 trade_id=%d symbol=%s", req.TradeID, strings.ToUpper(strings.TrimSpace(req.Symbol)))
	return s.freqManager.UpdateTiersManual(ctx, req)
}

// ListFreqtradeTierLogs exposes tier logs for Admin API.
func (s *LiveService) ListFreqtradeTierLogs(ctx context.Context, tradeID int, limit int) ([]freqexec.TierLog, error) {
	if s == nil || s.freqManager == nil {
		return nil, fmt.Errorf("live service 未初始化")
	}
	return s.freqManager.ListTierLogs(ctx, tradeID, limit)
}

// ListFreqtradeEvents implements livehttp.FreqtradeWebhookHandler.
func (s *LiveService) ListFreqtradeEvents(ctx context.Context, tradeID int, limit int) ([]freqexec.TradeEvent, error) {
	if s == nil || s.freqManager == nil {
		return nil, fmt.Errorf("live service 未初始化")
	}
	return s.freqManager.ListTradeEvents(ctx, tradeID, limit)
}

// ManualOpenPosition 提供管理后台手动开仓（免审）的快捷通道。
func (s *LiveService) ManualOpenPosition(ctx context.Context, req freqexec.ManualOpenRequest) error {
	if s == nil || s.freqManager == nil {
		return fmt.Errorf("freqtrade 执行器未启用")
	}
	symbol := strings.ToUpper(strings.TrimSpace(req.Symbol))
	if symbol == "" {
		return fmt.Errorf("symbol 不能为空")
	}
	side := strings.ToLower(strings.TrimSpace(req.Side))
	var action string
	switch side {
	case "long":
		action = "open_long"
	case "short":
		action = "open_short"
	default:
		return fmt.Errorf("side 只能是 long 或 short")
	}
	if req.Leverage <= 0 {
		return fmt.Errorf("leverage 必须大于 0")
	}
	if req.PositionSizeUSD <= 0 {
		return fmt.Errorf("position_size_usd 必须大于 0")
	}
	tiers := &decision.DecisionTiers{
		Tier1Target: req.Tier1Target,
		Tier1Ratio:  req.Tier1Ratio,
		Tier2Target: req.Tier2Target,
		Tier2Ratio:  req.Tier2Ratio,
		Tier3Target: req.Tier3Target,
		Tier3Ratio:  req.Tier3Ratio,
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		reason = "手动开仓（免审）"
	} else {
		reason = fmt.Sprintf("手动开仓: %s", reason)
	}
	logger.Warnf("freqtrade: 管理后台手动开仓 symbol=%s side=%s size=%.2f lev=%d", symbol, side, req.PositionSizeUSD, req.Leverage)
	return s.freqtradeHandleDecision(ctx, "", decision.Decision{
		Symbol:          symbol,
		Action:          action,
		Leverage:        req.Leverage,
		PositionSizeUSD: req.PositionSizeUSD,
		StopLoss:        req.StopLoss,
		TakeProfit:      req.TakeProfit,
		Tiers:           tiers,
		Reasoning:       reason,
	})
}

// GetLatestPriceQuote 返回管理后台所需的最新成交价。
func (s *LiveService) GetLatestPriceQuote(ctx context.Context, symbol string) (freqexec.TierPriceQuote, error) {
	var empty freqexec.TierPriceQuote
	if s == nil {
		return empty, fmt.Errorf("live service 未初始化")
	}
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	if symbol == "" {
		return empty, fmt.Errorf("symbol 不能为空")
	}
	quote := s.latestPriceQuote(ctx, symbol)
	if quote.Last <= 0 {
		return quote, fmt.Errorf("未获取到 %s 的最新价格", symbol)
	}
	return quote, nil
}

func (s *LiveService) ensureTraceID(raw string) string {
	id := strings.TrimSpace(raw)
	if id != "" {
		return id
	}
	return fmt.Sprintf("trace-%d", time.Now().UnixNano())
}

func validateDecisionForOpen(d decision.Decision, price float64, offsetPct float64) error {
	if strings.TrimSpace(d.Symbol) == "" {
		return fmt.Errorf("symbol 不能为空")
	}
	if d.PositionSizeUSD <= 0 {
		return fmt.Errorf("缺少开仓仓位金额")
	}
	if d.Leverage <= 0 {
		return fmt.Errorf("缺少杠杆倍数")
	}
	if price <= 0 {
		return fmt.Errorf("当前价格不可用")
	}
	if offsetPct < 0 {
		offsetPct = 0
	}
	if d.TakeProfit <= 0 || d.StopLoss <= 0 {
		return fmt.Errorf("缺少止盈/止损")
	}
	if d.Tiers == nil {
		return fmt.Errorf("缺少 tiers 配置")
	}
	t := d.Tiers
	if t.Tier1Target <= 0 || t.Tier2Target <= 0 || t.Tier3Target <= 0 {
		return fmt.Errorf("tier 目标价必须大于 0")
	}
	if t.Tier1Ratio <= 0 || t.Tier2Ratio <= 0 || t.Tier3Ratio <= 0 {
		return fmt.Errorf("tier 比例必须大于 0")
	}
	sum := t.Tier1Ratio + t.Tier2Ratio + t.Tier3Ratio
	if math.Abs(sum-1) > 1e-3 {
		return fmt.Errorf("tier 比例之和必须等于 1，当前=%.4f", sum)
	}
	offset := price * offsetPct
	upper := price + offset
	lower := price - offset
	switch d.Action {
	case "open_long":
		if !(d.StopLoss < lower) {
			return fmt.Errorf("多单止损必须低于当前价-偏移, sl=%.4f price=%.4f offset=%.4f", d.StopLoss, price, offset)
		}
		if !(upper <= t.Tier1Target && t.Tier1Target <= t.Tier2Target && t.Tier2Target <= t.Tier3Target) {
			return fmt.Errorf("多单 tier 价格必须递增且高于当前价+偏移")
		}
		if !almostEqual(t.Tier3Target, d.TakeProfit) {
			return fmt.Errorf("多单 tier3 必须等于 take_profit")
		}
	case "open_short":
		if !(d.StopLoss > upper) {
			return fmt.Errorf("空单止损必须高于当前价+偏移, sl=%.4f price=%.4f offset=%.4f", d.StopLoss, price, offset)
		}
		if !(lower >= t.Tier1Target && t.Tier1Target >= t.Tier2Target && t.Tier2Target >= t.Tier3Target) {
			return fmt.Errorf("空单 tier 价格必须递减且低于当前价-偏移")
		}
		if !almostEqual(t.Tier3Target, d.TakeProfit) {
			return fmt.Errorf("空单 tier3 必须等于 take_profit")
		}
	default:
		return fmt.Errorf("不支持的 action: %s", d.Action)
	}
	return nil
}

func almostEqual(a, b float64) bool {
	const eps = 1e-6
	return math.Abs(a-b) <= eps
}
