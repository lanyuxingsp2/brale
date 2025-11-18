package app

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	brcfg "brale/internal/config"
	"brale/internal/decision"
	freqexec "brale/internal/executor/freqtrade"
	"brale/internal/gateway/database"
	"brale/internal/gateway/notifier"
	"brale/internal/logger"
	"brale/internal/market"
	brmarket "brale/internal/market"
	"brale/internal/store"
)

// LiveService 负责实时行情、AI 决策循环与通知。
type LiveService struct {
	cfg                 *brcfg.Config
	ks                  market.KlineStore
	updater             *brmarket.WSUpdater
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
}

// Run 启动实时服务，直到 ctx 取消。
func (s *LiveService) Run(ctx context.Context) error {
	if s == nil || s.cfg == nil {
		return fmt.Errorf("live service not initialized")
	}

	cfg := s.cfg
	firstWSConnected := false
	s.updater.OnConnected = func() {
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
	batchSize := cfg.Market.ResolveActiveSource().WSBatchSize
	if batchSize <= 0 {
		batchSize = 150
	}
	go func() {
		if err := s.updater.Start(ctx, s.symbols, s.hIntervals, batchSize); err != nil {
			logger.Errorf("启动行情订阅失败: %v", err)
		}
	}()

	decisionInterval := time.Duration(cfg.AI.DecisionIntervalSeconds) * time.Second
	if decisionInterval <= 0 {
		decisionInterval = time.Minute
	}
	decisionTicker := time.NewTicker(decisionInterval)
	cacheTicker := time.NewTicker(15 * time.Second)
	statsTicker := time.NewTicker(60 * time.Second)
	defer decisionTicker.Stop()
	defer cacheTicker.Stop()
	defer statsTicker.Stop()

	human := fmt.Sprintf("%d 秒", int(decisionInterval.Seconds()))
	if cfg.AI.DecisionIntervalSeconds%60 == 0 {
		human = fmt.Sprintf("%d 分钟", cfg.AI.DecisionIntervalSeconds/60)
	}
	fmt.Printf("Brale 启动完成。开始订阅 K 线并写入缓存；每 %s 进行一次 AI 决策。按 Ctrl+C 退出。\n", human)

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
	cfg := s.cfg
	input := decision.Context{Candidates: s.symbols}
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
			Limit:       cfg.Kline.MaxCached,
			SliceLength: s.profile.AnalysisSlice,
			SliceDrop:   s.profile.SliceDropTail,
			HorizonName: s.horizonName,
			Indicators:  s.profile.Indicators,
		})
	}
	if snaps := s.livePositions(); len(snaps) > 0 {
		input.Positions = snaps
	}
	if s.includeLastDecision && s.lastDec != nil {
		snap := s.lastDec.Snapshot(time.Now())
		if len(snap) > 0 {
			input.LastDecisions = snap
			input.LastRawJSON = s.lastRawJSON
		}
	}
	res, err := s.engine.Decide(ctx, input)
	if err != nil {
		return err
	}
	traceID := s.ensureTraceID(res.TraceID)
	if len(res.Decisions) == 0 {
		logger.Infof("AI 决策为空（观望）")
		return nil
	}
	if res.RawOutput != "" {
		_, start, ok := decision.ExtractJSONArrayWithIndex(res.RawOutput)
		if ok {
			cot := strings.TrimSpace(res.RawOutput[:start])
			// pretty := decision.PrettyJSON(arr)
			cot = decision.TrimTo(cot, 4800)
			// pretty = decision.TrimTo(pretty, 3600)
			t1 := decision.RenderBlockTable("AI[final] 思维链", cot)
			// t2 := decision.RenderBlockTable("AI[final] 结果(JSON)", pretty)
			logger.Infof("\n%s", t1)
		} else {
			t1 := decision.RenderBlockTable("AI[final] 思维链", "失败")
			// t2 := decision.RenderBlockTable("AI[final] 结果(JSON)", "失败")
			logger.Infof("\n%s", t1)
		}
	}
	if s.tg != nil && cfg.AI.Aggregation == "meta" && strings.TrimSpace(res.MetaSummary) != "" {
		if err := s.sendMetaSummaryTelegram(res.MetaSummary); err != nil {
			logger.Warnf("Telegram 推送失败(meta): %v", err)
		}
	}
	for i := range res.Decisions {
		res.Decisions[i].Action = decision.NormalizeAction(res.Decisions[i].Action)
	}
	res.Decisions = decision.OrderAndDedup(res.Decisions)
	if len(res.Decisions) > 0 {
		tFinal := decision.RenderFinalDecisionsTable(res.Decisions, 180)
		logger.Infof("\n%s", tFinal)
	}

	validateIv := ""
	if len(s.hIntervals) > 0 {
		validateIv = s.hIntervals[0]
	}

	accepted := make([]decision.Decision, 0, len(res.Decisions))
	newOpens := 0
	for _, d := range res.Decisions {
		marketPrice := 0.0
		s.applyTradingDefaults(&d)
		if err := decision.Validate(&d); err != nil {
			logger.Warnf("AI 决策不合规，已忽略: %v | %+v", err, d)
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

		if d.Action == "open_long" || d.Action == "open_short" {
			if newOpens >= cfg.Advanced.MaxOpensPerCycle {
				logger.Infof("跳过超出本周期开仓上限: %s %s", d.Symbol, d.Action)
				continue
			}
			key := d.Symbol + "#" + d.Action
			if prev, ok := s.lastOpen[key]; ok {
				if time.Since(prev) < time.Duration(cfg.Advanced.OpenCooldownSeconds)*time.Second {
					remain := float64(time.Duration(cfg.Advanced.OpenCooldownSeconds)*time.Second-time.Since(prev)) / float64(time.Second)
					logger.Infof("跳过频繁开仓（冷却中）: %s 剩余 %.0fs", key, remain)
					continue
				}
			}
			s.lastOpen[key] = time.Now()
			newOpens++
			s.recordLiveOrder(ctx, d, marketPrice, validateIv)
			s.notifyOpen(ctx, d, marketPrice, validateIv)
		}
	}
	if len(accepted) > 0 {
		s.persistLastDecisions(ctx, accepted)
		if raw := strings.TrimSpace(res.RawJSON); raw != "" {
			s.lastRawJSON = raw
		} else if buf, err := json.Marshal(accepted); err == nil {
			s.lastRawJSON = string(buf)
		}
	}
	return nil
}

func (s *LiveService) applyTradingDefaults(d *decision.Decision) {
	if s == nil || s.cfg == nil || d == nil {
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

func (s *LiveService) livePositions() []decision.PositionSnapshot {
	if s.freqManager != nil {
		return s.freqManager.Positions()
	}
	return nil
}

func (s *LiveService) logDecision(d decision.Decision) {
	switch d.Action {
	case "open_long", "open_short":
		if d.Reasoning != "" {
			logger.Infof("AI 决策: %s %s lev=%d size=%.0f sl=%.4f tp=%.4f conf=%d 理由=%s",
				d.Symbol, d.Action, d.Leverage, d.PositionSizeUSD, d.StopLoss, d.TakeProfit, d.Confidence, d.Reasoning)
		} else {
			logger.Infof("AI 决策: %s %s lev=%d size=%.0f sl=%.4f tp=%.4f conf=%d",
				d.Symbol, d.Action, d.Leverage, d.PositionSizeUSD, d.StopLoss, d.TakeProfit, d.Confidence)
		}
	case "close_long", "close_short":
		if d.Reasoning != "" {
			if d.Confidence > 0 {
				logger.Infof("AI 决策: %s %s conf=%d 理由=%s", d.Symbol, d.Action, d.Confidence, d.Reasoning)
			} else {
				logger.Infof("AI 决策: %s %s 理由=%s", d.Symbol, d.Action, d.Reasoning)
			}
		} else {
			if d.Confidence > 0 {
				logger.Infof("AI 决策: %s %s conf=%d", d.Symbol, d.Action, d.Confidence)
			} else {
				logger.Infof("AI 决策: %s %s", d.Symbol, d.Action)
			}
		}
	default:
		if d.Reasoning != "" {
			if d.Confidence > 0 {
				logger.Infof("AI 决策: %s %s conf=%d 理由=%s", d.Symbol, d.Action, d.Confidence, d.Reasoning)
			} else {
				logger.Infof("AI 决策: %s %s 理由=%s", d.Symbol, d.Action, d.Reasoning)
			}
		} else {
			if d.Confidence > 0 {
				logger.Infof("AI 决策: %s %s conf=%d", d.Symbol, d.Action, d.Confidence)
			} else {
				logger.Infof("AI 决策: %s %s", d.Symbol, d.Action)
			}
		}
	}
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

	const maxLen = 3900
	prefix := header
	chunk := prefix + "```\n"
	clen := len(chunk)
	for i, ln := range lines {
		if clen+len(ln)+1+3 > 4096 {
			chunk += "```"
			if err := s.tg.SendText(chunk); err != nil {
				return err
			}
			prefix = ""
			chunk = "```\n"
			clen = len(chunk)
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
	return s.freqManager.Execute(ctx, freqexec.DecisionInput{
		TraceID:  traceID,
		Decision: d,
	})
}

// HandleFreqtradeWebhook implements livehttp.FreqtradeWebhookHandler.
func (s *LiveService) HandleFreqtradeWebhook(ctx context.Context, msg freqexec.WebhookMessage) error {
	if s == nil || s.freqManager == nil {
		return fmt.Errorf("live service 未初始化")
	}
	s.freqManager.HandleWebhook(ctx, msg)
	return nil
}

// ListFreqtradePositions implements livehttp.FreqtradeWebhookHandler.
func (s *LiveService) ListFreqtradePositions(ctx context.Context, symbol string, limit int) []freqexec.APIPosition {
	if s == nil || s.freqManager == nil {
		return nil
	}
	return s.freqManager.PositionsForAPI(symbol, limit)
}

func (s *LiveService) ensureTraceID(raw string) string {
	id := strings.TrimSpace(raw)
	if id != "" {
		return id
	}
	return fmt.Sprintf("trace-%d", time.Now().UnixNano())
}
