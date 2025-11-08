package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"brale/internal/ai"
	"brale/internal/backtest"
	"brale/internal/coins"
	brcfg "brale/internal/config"
	"brale/internal/execution"
	"brale/internal/logger"
	brmarket "brale/internal/market"
	"brale/internal/notify"
	"brale/internal/prompt"
	"brale/internal/store"
)

// App 负责应用级编排：加载配置→资源初始化→WS→AI 决策循环与通知。
type App struct {
	cfg                 *brcfg.Config
	ks                  *store.MemoryKlineStore
	updater             *brmarket.WSUpdater
	pm                  *prompt.Manager
	engine              ai.Decider
	tg                  *notify.Telegram
	decLogs             *ai.DecisionLogStore
	orderRec            execution.Recorder
	lastDec             *lastDecisionCache
	includeLastDecision bool
	lastDecisionTTL     time.Duration

	btStore   *backtest.Store
	btSvc     *backtest.Service
	btResults *backtest.ResultStore
	btSim     *backtest.Simulator
	btServer  *backtest.HTTPServer

	symbols       []string
	horizon       brcfg.HorizonProfile
	hIntervals    []string
	lookbacks     map[string]int
	horizonName   string
	hSummary      string
	warmupSummary string

	// 内部运行状态
	lastOpen    map[string]time.Time // 符号+方向 -> 上次开仓时间
	lastRawJSON string               // 上一次决策的原始 JSON
}

// NewApp 根据配置构建应用对象（不启动）
func NewApp(cfg *brcfg.Config) (*App, error) {
	if cfg == nil {
		return nil, fmt.Errorf("nil config")
	}
	logger.SetLevel(cfg.App.LogLevel)

	// 符号提供者
	var sp coins.SymbolProvider
	if cfg.Symbols.Provider == "http" {
		sp = coins.NewHTTPSymbolProvider(cfg.Symbols.APIURL)
	} else {
		sp = coins.NewDefaultProvider(cfg.Symbols.DefaultList)
	}

	ctx := context.Background()
	syms, err := sp.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("获取币种列表失败: %w", err)
	}
	logger.Infof("✓ 已加载 %d 个交易对: %v", len(syms), syms)

	// 选定持仓周期 profile
	horizon, ok := cfg.AI.HoldingProfiles[cfg.AI.ActiveHorizon]
	if !ok {
		return nil, fmt.Errorf("未找到持仓周期配置: %s", cfg.AI.ActiveHorizon)
	}
	hIntervals := horizon.AllTimeframes()
	if len(hIntervals) == 0 {
		return nil, fmt.Errorf("持仓周期 %s 未配置任何 k 线周期", cfg.AI.ActiveHorizon)
	}
	logger.Infof("✓ 启用持仓周期 %s，K线周期=%v", cfg.AI.ActiveHorizon, hIntervals)
	hSummary := formatHorizonSummary(cfg.AI.ActiveHorizon, horizon, hIntervals)
	logger.Infof("[horizon]\n%s", hSummary)

	// 提示词
	pm := prompt.NewManager(cfg.Prompt.Dir)
	if err := pm.Load(); err != nil {
		return nil, fmt.Errorf("加载提示词模板失败: %w", err)
	}
	if content, ok := pm.Get(cfg.Prompt.SystemTemplate); ok {
		logger.Infof("✓ 提示词模板 '%s' 已就绪，长度=%d 字符", cfg.Prompt.SystemTemplate, len(content))
	} else {
		logger.Warnf("未找到提示词模板 '%s'", cfg.Prompt.SystemTemplate)
	}

	// 存储与 WS 更新器
	ks := store.NewMemoryKlineStore()
	updater := brmarket.NewWSUpdater(ks, cfg.Kline.MaxCached)

	// 预热
	lookbacks := horizon.LookbackMap(20)
	preheater := brmarket.NewPreheater(ks, cfg.Kline.MaxCached)
	preheater.Warmup(ctx, syms, lookbacks)
	preheater.Preheat(ctx, syms, hIntervals, cfg.Kline.MaxCached)
	logger.Infof("✓ Warmup 完成，最小条数=%v", lookbacks)
	warmupSummary := fmt.Sprintf("*Warmup 完成*\n```\n%v\n```", lookbacks)

	// 模型 Providers
	var modelCfgs []ai.ModelCfg
	for _, m := range cfg.AI.Models {
		modelCfgs = append(modelCfgs, ai.ModelCfg{ID: m.ID, Provider: m.Provider, Enabled: m.Enabled, APIURL: m.APIURL, APIKey: m.APIKey, Model: m.Model, Headers: m.Headers})
	}
	providers := ai.BuildProvidersFromConfig(modelCfgs)
	if len(providers) == 0 {
		logger.Warnf("未启用任何 AI 模型（请检查 ai.models 配置）")
	} else {
		ids := make([]string, 0, len(providers))
		for _, p := range providers {
			if p != nil && p.Enabled() {
				ids = append(ids, p.ID())
			}
		}
		logger.Infof("✓ 已启用 %d 个 AI 模型: %v", len(ids), ids)
	}

	// 聚合器
	var aggregator ai.Aggregator = ai.FirstWinsAggregator{}
	if cfg.AI.Aggregation == "meta" {
		aggregator = ai.MetaAggregator{Weights: cfg.AI.Weights}
	}

	// 引擎
	engine := &ai.LegacyEngineAdapter{
		Providers:      providers,
		Agg:            aggregator,
		PromptMgr:      pm,
		SystemTemplate: cfg.Prompt.SystemTemplate,
		KStore:         ks,
		Intervals:      hIntervals,
		Horizon:        horizon,
		HorizonName:    cfg.AI.ActiveHorizon,
		Parallel:       true,
		LogEachModel:   cfg.AI.LogEachModel,
		Metrics:        brmarket.NewDefaultMetricsFetcher(""),
		IncludeOI:      true,
		IncludeFunding: true,
		TimeoutSeconds: cfg.MCP.TimeoutSeconds,
	}

	// Telegram（可选）
	var tg *notify.Telegram
	if cfg.Notify.Telegram.Enabled {
		tg = notify.NewTelegram(cfg.Notify.Telegram.BotToken, cfg.Notify.Telegram.ChatID)
	}

	var decisionStore *ai.DecisionLogStore
	if cfg.AI.DecisionLogPath != "" {
		var err error
		decisionStore, err = ai.NewDecisionLogStore(cfg.AI.DecisionLogPath)
		if err != nil {
			return nil, fmt.Errorf("初始化决策日志存储失败: %w", err)
		}
		if obs := ai.NewDecisionLogObserver(decisionStore); obs != nil {
			engine.Observer = obs
		}
		logPath := cfg.AI.DecisionLogPath
		if abs, err := filepath.Abs(logPath); err == nil {
			logPath = abs
		}
		logger.Infof("✓ 实盘决策日志写入 %s", logPath)
	}

	includeLastDecision := cfg.AI.IncludeLastDecision
	initialLastJSON := ""
	lastTTL := time.Duration(cfg.AI.LastDecisionMaxAgeSec) * time.Second
	lastCache := newLastDecisionCache(lastTTL)
	if !includeLastDecision {
		lastCache = nil
	}
	orderRecorder := execution.Recorder(nil)
	if decisionStore != nil {
		orderRecorder = decisionStore
		if lastCache != nil {
			records, err := decisionStore.LoadLastDecisions(ctx)
			if err != nil {
				logger.Warnf("加载 LastDecision 失败: %v", err)
			} else {
				lastCache.Load(records)
				initialLastJSON = flattenLastDecisionJSON(records)
			}
		}
	}

	var btStore *backtest.Store
	var btResults *backtest.ResultStore
	var btSvc *backtest.Service
	var btSim *backtest.Simulator
	var btHTTP *backtest.HTTPServer
	if cfg.Backtest.Enabled {
		var err error
		btStore, err = backtest.NewStore(cfg.Backtest.DataDir)
		if err != nil {
			return nil, fmt.Errorf("初始化回测存储失败: %w", err)
		}
		btResults, err = backtest.NewResultStore(cfg.Backtest.DataDir)
		if err != nil {
			btStore.Close()
			return nil, fmt.Errorf("初始化回测结果库失败: %w", err)
		}
		sources := map[string]backtest.CandleSource{
			"binance": backtest.NewBinanceSource(""),
		}
		btSvc, err = backtest.NewService(backtest.ServiceConfig{
			Store:           btStore,
			Sources:         sources,
			DefaultExchange: cfg.Backtest.DefaultExchange,
			RateLimitPerMin: cfg.Backtest.RateLimitPerMin,
			MaxBatch:        cfg.Backtest.MaxBatch,
			MaxConcurrent:   cfg.Backtest.MaxConcurrent,
		})
		if err != nil {
			btResults.Close()
			btStore.Close()
			return nil, fmt.Errorf("初始化回测服务失败: %w", err)
		}
		simFactory := &backtest.AIProxyFactory{
			Prompt:         pm,
			SystemTemplate: cfg.Prompt.SystemTemplate,
			Models:         modelCfgs,
			Aggregator:     aggregator,
			Parallel:       true,
			TimeoutSeconds: cfg.MCP.TimeoutSeconds,
		}
		btSim, err = backtest.NewSimulator(backtest.SimulatorConfig{
			CandleStore:    btStore,
			ResultStore:    btResults,
			Fetcher:        btSvc,
			Profiles:       cfg.AI.HoldingProfiles,
			Lookbacks:      lookbacks,
			DefaultProfile: cfg.AI.ActiveHorizon,
			Strategy:       simFactory,
			Notifier:       tg,
			MaxConcurrent:  cfg.Backtest.MaxConcurrent,
		})
		if err != nil {
			btResults.Close()
			btStore.Close()
			return nil, fmt.Errorf("初始化回测模拟器失败: %w", err)
		}
		btHTTP, err = backtest.NewHTTPServer(backtest.HTTPConfig{
			Addr:      cfg.Backtest.HTTPAddr,
			Svc:       btSvc,
			Simulator: btSim,
			Results:   btResults,
			LiveLogs:  decisionStore,
		})
		if err != nil {
			btResults.Close()
			btStore.Close()
			return nil, fmt.Errorf("初始化回测 HTTP 失败: %w", err)
		}
		logger.Infof("✓ 回测 HTTP 接口监听 %s", cfg.Backtest.HTTPAddr)
	}

	return &App{
		cfg:                 cfg,
		ks:                  ks,
		updater:             updater,
		pm:                  pm,
		engine:              engine,
		tg:                  tg,
		decLogs:             decisionStore,
		orderRec:            orderRecorder,
		lastDec:             lastCache,
		includeLastDecision: includeLastDecision,
		lastDecisionTTL:     lastTTL,
		symbols:             syms,
		horizon:             horizon,
		hIntervals:          append([]string(nil), hIntervals...),
		lookbacks:           lookbacks,
		horizonName:         cfg.AI.ActiveHorizon,
		hSummary:            hSummary,
		warmupSummary:       warmupSummary,
		lastOpen:            map[string]time.Time{},
		lastRawJSON:         initialLastJSON,
		btStore:             btStore,
		btSvc:               btSvc,
		btResults:           btResults,
		btSim:               btSim,
		btServer:            btHTTP,
	}, nil
}

// Run 启动 WS 并进入决策循环（阻塞直到 ctx 取消）
func (a *App) Run(ctx context.Context) error {
	if a == nil || a.cfg == nil {
		return fmt.Errorf("app not initialized")
	}
	if a.decLogs != nil {
		defer a.decLogs.Close()
	}
	if a.btStore != nil {
		defer a.btStore.Close()
	}
	if a.btResults != nil {
		defer a.btResults.Close()
	}
	if a.btSvc != nil {
		a.btSvc.SetContext(ctx)
	}
	if a.btSim != nil {
		a.btSim.SetContext(ctx)
	}
	if a.btServer != nil {
		go func() {
			if err := a.btServer.Start(ctx); err != nil {
				logger.Warnf("回测 HTTP 停止: %v", err)
			}
		}()
	}

	// WS 回调：首连成功后通知一次；断线立即告警
	firstWSConnected := false
	a.updater.OnConnected = func() {
		if a.tg == nil {
			return
		}
		if !firstWSConnected {
			firstWSConnected = true
			msg := "*Brale 启动成功* ✅\nWS 已连接并开始订阅"
			if summary := strings.TrimSpace(a.hSummary); summary != "" {
				msg = msg + "\n```text\n" + summary + "\n```"
			}
			if warmup := strings.TrimSpace(a.warmupSummary); warmup != "" {
				msg = msg + "\n" + warmup
			}
			_ = a.tg.SendText(msg)
		}
	}
	a.updater.OnDisconnected = func(err error) {
		if a.tg == nil {
			return
		}
		msg := "WS 断线"
		if err != nil {
			msg = msg + ": " + err.Error()
		}
		_ = a.tg.SendText(msg)
	}
	// 启动真实 WS 订阅
	go a.updater.StartRealWS(a.symbols, a.hIntervals, a.cfg.Exchange.WSBatchSize)

	// 决策与心跳 ticker
	decisionInterval := time.Duration(a.cfg.AI.DecisionIntervalSeconds) * time.Second
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
	if a.cfg.AI.DecisionIntervalSeconds%60 == 0 {
		human = fmt.Sprintf("%d 分钟", a.cfg.AI.DecisionIntervalSeconds/60)
	}
	fmt.Println(fmt.Sprintf("Brale 启动完成。开始订阅 K 线并写入缓存；每 %s 进行一次 AI 决策。按 Ctrl+C 退出。", human))

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-cacheTicker.C:
			// 打印缓存状态
			for _, sym := range a.symbols {
				for _, iv := range a.hIntervals {
					if kl, err := a.ks.Get(ctx, sym, iv); err == nil {
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
			if a.updater != nil && a.updater.Client != nil {
				r, s, last := a.updater.Client.Stats()
				if last != "" {
					logger.Errorf("WS统计: 最后错误=%s", last)
				}
				logger.Debugf("ws 统计:重连 = %v,订阅错误=%v", r, s)
			}
		case <-decisionTicker.C:
			// 构建最小上下文并进行决策
			input := ai.Context{Candidates: a.symbols}
			if a.includeLastDecision && a.lastDec != nil {
				snap := a.lastDec.Snapshot(time.Now())
				if len(snap) > 0 {
					input.LastDecisions = snap
					input.LastRawJSON = a.lastRawJSON
				}
			}
			res, err := a.engine.Decide(ctx, input)
			if err != nil {
				logger.Warnf("AI 决策失败: %v", err)
				continue
			}
			if len(res.Decisions) == 0 {
				logger.Infof("AI 决策为空（观望）")
				continue
			}
			// 打印思维链与结果JSON（表格展示）
			if res.RawOutput != "" {
				arr, start, ok := ai.ExtractJSONArrayWithIndex(res.RawOutput)
				if ok {
					cot := strings.TrimSpace(res.RawOutput[:start])
					pretty := ai.PrettyJSON(arr)
					cot = ai.TrimTo(cot, 2400)
					pretty = ai.TrimTo(pretty, 3600)
					t1 := ai.RenderBlockTable("AI[final] 思维链", cot)
					t2 := ai.RenderBlockTable("AI[final] 结果(JSON)", pretty)
					logger.Infof("\n%s\n%s", t1, t2)
				} else {
					t1 := ai.RenderBlockTable("AI[final] 思维链", "失败")
					t2 := ai.RenderBlockTable("AI[final] 结果(JSON)", "失败")
					logger.Infof("\n%s\n%s", t1, t2)
				}
			}
			// Meta 聚合发生分歧时，发送一次 Telegram 说明各模型选择与理由（完整且优雅格式）
			if a.tg != nil && a.cfg.AI.Aggregation == "meta" && strings.TrimSpace(res.MetaSummary) != "" {
				if err := a.sendMetaSummaryTelegram(res.MetaSummary); err != nil {
					logger.Warnf("Telegram 推送失败(meta): %v", err)
				}
			}
			// 归一化并排序去重（close > open > hold）
			for i := range res.Decisions {
				res.Decisions[i].Action = ai.NormalizeAction(res.Decisions[i].Action)
			}
			res.Decisions = ai.OrderAndDedup(res.Decisions)
			accepted := make([]ai.Decision, 0, len(res.Decisions))

			// 新增：最终聚合决策表（标红）
			if len(res.Decisions) > 0 {
				tFinal := ai.RenderFinalDecisionsTable(res.Decisions, 180)
				logger.Infof("\n%s", tFinal)
			}

			// 选一个用于价格校验的周期
			validateIv := ""
			if len(a.hIntervals) > 0 {
				validateIv = a.hIntervals[0]
			} else if len(a.cfg.Kline.Periods) > 0 {
				validateIv = a.cfg.Kline.Periods[0]
			}

			newOpens := 0
			for _, d := range res.Decisions {
				// 记录入场价格（用于通知/展示）
				entryPrice := 0.0
				// 基础校验
				if err := ai.Validate(&d); err != nil {
					logger.Warnf("AI 决策不合规，已忽略: %v | %+v", err, d)
					continue
				}
				// 带价格的校验（RR、关系）
				if validateIv != "" {
					if kl, _ := a.ks.Get(ctx, d.Symbol, validateIv); len(kl) > 0 {
						price := kl[len(kl)-1].Close
						entryPrice = price
						if err := ai.ValidateWithPrice(&d, price, a.cfg.Advanced.MinRiskReward); err != nil {
							logger.Warnf("AI 决策RR校验失败，已忽略: %v | %+v", err, d)
							continue
						}
					}
				}
				accepted = append(accepted, d)

				// 打印决策
				a.logDecision(d)

				// 开仓限制与推送
				if d.Action == "open_long" || d.Action == "open_short" {
					if newOpens >= a.cfg.Advanced.MaxOpensPerCycle {
						logger.Infof("跳过超出本周期开仓上限: %s %s", d.Symbol, d.Action)
						continue
					}
					key := d.Symbol + "#" + d.Action
					if prev, ok := a.lastOpen[key]; ok {
						if time.Since(prev) < time.Duration(a.cfg.Advanced.OpenCooldownSeconds)*time.Second {
							remain := float64(time.Duration(a.cfg.Advanced.OpenCooldownSeconds)*time.Second-time.Since(prev)) / float64(time.Second)
							logger.Infof("跳过频繁开仓（冷却中）: %s 剩余 %.0fs", key, remain)
							continue
						}
					}
					a.lastOpen[key] = time.Now()
					newOpens++
					a.recordLiveOrder(ctx, d, entryPrice, validateIv)
					if a.tg != nil {
						// 可选的入场价与RR
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
						// 控制台额外打印入场与RR
						if entryPrice > 0 {
							if rrVal > 0 {
								logger.Infof("开仓详情: %s %s entry=%.4f RR=%.2f sl=%.4f tp=%.4f",
									d.Symbol, d.Action, entryPrice, rrVal, d.StopLoss, d.TakeProfit)
							} else {
								logger.Infof("开仓详情: %s %s entry=%.4f sl=%.4f tp=%.4f",
									d.Symbol, d.Action, entryPrice, d.StopLoss, d.TakeProfit)
							}
						}

						// Telegram 结构化输出（Markdown 代码块 + 理由）
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
						if strings.TrimSpace(d.Reasoning) != "" {
							reason := strings.TrimSpace(d.Reasoning)
							if len(reason) > 1500 {
								reason = reason[:1500] + "..."
							}
							reason = strings.ReplaceAll(reason, "```", "'''")
							b.WriteString("理由:\n```\n")
							b.WriteString(reason)
							b.WriteString("\n```")
						}
						msg := b.String()
						if len(msg) > 3800 {
							msg = msg[:3800] + "..."
						}
						if err := a.tg.SendText(msg); err != nil {
							logger.Warnf("Telegram 推送失败: %v", err)
						}
					}
				}
			}
			if len(accepted) > 0 {
				a.persistLastDecisions(ctx, accepted)
				if raw := strings.TrimSpace(res.RawJSON); raw != "" {
					a.lastRawJSON = raw
				} else if buf, err := json.Marshal(accepted); err == nil {
					a.lastRawJSON = string(buf)
				}
			}
		}
	}
}

func formatHorizonSummary(name string, profile brcfg.HorizonProfile, intervals []string) string {
	toList := func(items []string) string {
		if len(items) == 0 {
			return "-"
		}
		return strings.Join(items, ", ")
	}
	ind := profile.Indicators
	lines := []string{
		fmt.Sprintf("持仓周期：%s", name),
		fmt.Sprintf("- 入场周期：%s", toList(profile.EntryTimeframes)),
		fmt.Sprintf("- 确认周期：%s", toList(profile.ConfirmTimeframes)),
		fmt.Sprintf("- 背景周期：%s", toList(profile.BackgroundTimeframes)),
		fmt.Sprintf("- 订阅/缓存周期：%s", toList(intervals)),
		fmt.Sprintf("- EMA(fast/mid/slow) = %d / %d / %d", ind.EMA.Fast, ind.EMA.Mid, ind.EMA.Slow),
		fmt.Sprintf("- RSI(period=%d, oversold=%.0f, overbought=%.0f)", ind.RSI.Period, ind.RSI.Oversold, ind.RSI.Overbought),
	}
	return strings.Join(lines, "\n")
}

func flattenLastDecisionJSON(records []ai.LastDecisionRecord) string {
	if len(records) == 0 {
		return ""
	}
	total := 0
	for _, rec := range records {
		total += len(rec.Decisions)
	}
	if total == 0 {
		return ""
	}
	all := make([]ai.Decision, 0, total)
	for _, rec := range records {
		if len(rec.Decisions) == 0 {
			continue
		}
		all = append(all, rec.Decisions...)
	}
	buf, err := json.Marshal(all)
	if err != nil {
		return ""
	}
	return string(buf)
}

func (a *App) recordLiveOrder(ctx context.Context, d ai.Decision, entryPrice float64, timeframe string) {
	if a.orderRec == nil {
		return
	}
	symbol := strings.ToUpper(strings.TrimSpace(d.Symbol))
	if symbol == "" {
		return
	}
	payload := execution.Order{
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
		ExpectedRR: 0,
	}
	if data, err := json.Marshal(d); err == nil {
		payload.Decision = data
	}
	if _, err := a.orderRec.RecordOrder(ctx, &payload); err != nil {
		logger.Warnf("记录 live order 失败: %v", err)
	}
}

func deriveSide(action string) string {
	switch action {
	case "open_long", "close_long", "adjust_stop_loss":
		return "long"
	case "open_short", "close_short", "partial_close_short":
		return "short"
	default:
		return ""
	}
}

func (a *App) persistLastDecisions(ctx context.Context, decisions []ai.Decision) {
	if !a.includeLastDecision || len(decisions) == 0 || a.lastDec == nil || a.decLogs == nil {
		return
	}
	now := time.Now()
	for _, d := range decisions {
		symbol := strings.ToUpper(strings.TrimSpace(d.Symbol))
		if symbol == "" {
			continue
		}
		mem := ai.DecisionMemory{
			Symbol:    symbol,
			Horizon:   a.horizonName,
			DecidedAt: now,
			Decisions: []ai.Decision{d},
		}
		a.lastDec.Set(mem)
		rec := ai.LastDecisionRecord{
			Symbol:    symbol,
			Horizon:   a.horizonName,
			DecidedAt: now,
			Decisions: []ai.Decision{d},
		}
		if err := a.decLogs.SaveLastDecision(ctx, rec); err != nil {
			logger.Warnf("保存 LastDecision 失败: %v", err)
		}
	}
}

func (a *App) logDecision(d ai.Decision) {
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
	default: // hold
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

// sendMetaSummaryTelegram 将 Meta 聚合摘要以代码块形式完整发送到 Telegram。
// 若文本超过单条消息限制，将自动分多条消息发送，保证不截断内容。
func (a *App) sendMetaSummaryTelegram(summary string) error {
	if a.tg == nil {
		return nil
	}
	header := "🗳️ Meta 聚合投票\n多模型存在分歧，采用加权多数决。\n"
	// 清理可能干扰 Markdown 的围栏
	body := strings.ReplaceAll(summary, "```", "'''")
	// 切分为行，便于按行分包
	lines := strings.Split(body, "\n")
	if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	// 若首行已包含聚合器生成的说明，则去重该行
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "Meta聚合：多模型存在分歧，采用加权多数决。" {
		lines = lines[1:]
		// 同时去掉紧随其后的空行
		if len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
			lines = lines[1:]
		}
	}

	// Telegram sendMessage 文本限制约 4096 字符（Markdown 可能略少），留出余量
	const maxLen = 3900
	prefix := header
	chunk := prefix + "```\n"
	clen := len(chunk)
	for i, ln := range lines {
		// 每行末尾 +1 换行；再加上结尾的 ```
		if clen+len(ln)+1+3 > 4096 {
			chunk += "```"
			if err := a.tg.SendText(chunk); err != nil {
				return err
			}
			prefix = "" // 后续包不再重复头部
			chunk = "```\n"
			clen = len(chunk)
		}
		chunk += ln + "\n"
		clen += len(ln) + 1
		// 最后一行发送
		if i == len(lines)-1 {
			chunk += "```"
			if err := a.tg.SendText(chunk); err != nil {
				return err
			}
		}
	}
	// 处理 lines 为空的情况
	if len(lines) == 0 {
		chunk = header + "```\n```"
		if err := a.tg.SendText(chunk); err != nil {
			return err
		}
	}
	return nil
}
