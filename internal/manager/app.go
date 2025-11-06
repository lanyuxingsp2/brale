package manager

import (
	"context"
	"fmt"
	"strings"
	"time"

	"brale/internal/ai"
	"brale/internal/coins"
	brcfg "brale/internal/config"
	"brale/internal/logger"
	brmarket "brale/internal/market"
	"brale/internal/notify"
	"brale/internal/prompt"
	"brale/internal/store"
)

// App 负责应用级编排：加载配置→资源初始化→WS→AI 决策循环与通知。
type App struct {
	cfg     *brcfg.Config
	ks      *store.MemoryKlineStore
	updater *brmarket.WSUpdater
	pm      *prompt.Manager
	engine  ai.Decider
	tg      *notify.Telegram

	symbols []string

	// 内部运行状态
	lastOpen map[string]time.Time // 符号+方向 -> 上次开仓时间
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
	preheater := brmarket.NewPreheater(ks, cfg.Kline.MaxCached)
	preheater.Preheat(ctx, syms, cfg.Kline.Periods, cfg.Kline.MaxCached)

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
		Intervals:      cfg.WS.Periods,
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

	return &App{cfg: cfg, ks: ks, updater: updater, pm: pm, engine: engine, tg: tg, symbols: syms, lastOpen: map[string]time.Time{}}, nil
}

// Run 启动 WS 并进入决策循环（阻塞直到 ctx 取消）
func (a *App) Run(ctx context.Context) error {
	if a == nil || a.cfg == nil {
		return fmt.Errorf("app not initialized")
	}

	// WS 回调：首连成功后通知一次；断线立即告警
	firstWSConnected := false
	a.updater.OnConnected = func() {
		if a.tg == nil {
			return
		}
		if !firstWSConnected {
			firstWSConnected = true
			_ = a.tg.SendText("Brale 启动成功，WS 已连接并开始订阅")
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
	go a.updater.StartRealWS(a.symbols, a.cfg.WS.Periods, a.cfg.Exchange.WSBatchSize)

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
				for _, iv := range a.cfg.WS.Periods {
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
			// Meta 聚合发生分歧时，发送一次 Telegram 说明各模型选择与理由
			if a.tg != nil && a.cfg.AI.Aggregation == "meta" && strings.TrimSpace(res.MetaSummary) != "" {
				msg := "Meta 聚合投票\n" + res.MetaSummary
				if len(msg) > 5000 {
					msg = msg[:5000] + "..."
				}
				if err := a.tg.SendText(msg); err != nil {
					logger.Warnf("Telegram 推送失败(meta): %v", err)
				}
			}
			// 归一化并排序去重（close > open > hold）
			for i := range res.Decisions {
				res.Decisions[i].Action = ai.NormalizeAction(res.Decisions[i].Action)
			}
			res.Decisions = ai.OrderAndDedup(res.Decisions)

			// 选一个用于价格校验的周期
			validateIv := ""
			if len(a.cfg.WS.Periods) > 0 {
				validateIv = a.cfg.WS.Periods[0]
			} else if len(a.cfg.Kline.Periods) > 0 {
				validateIv = a.cfg.Kline.Periods[0]
			}

			newOpens := 0
			for _, d := range res.Decisions {
				// 基础校验
				if err := ai.Validate(&d); err != nil {
					logger.Warnf("AI 决策不合规，已忽略: %v | %+v", err, d)
					continue
				}
				// 带价格的校验（RR、关系）
				if validateIv != "" {
					if kl, _ := a.ks.Get(ctx, d.Symbol, validateIv); len(kl) > 0 {
						price := kl[len(kl)-1].Close
						if err := ai.ValidateWithPrice(&d, price, a.cfg.Advanced.MinRiskReward); err != nil {
							logger.Warnf("AI 决策RR校验失败，已忽略: %v | %+v", err, d)
							continue
						}
					}
				}

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
					if a.tg != nil {
						msg := fmt.Sprintf("开仓信号\n币种: %s\n动作: %s\n杠杆: %dx\n仓位: %.0f USDT\n止损: %.4f\n止盈: %.4f\n信心: %d\n理由: %s",
							d.Symbol, d.Action, d.Leverage, d.PositionSizeUSD, d.StopLoss, d.TakeProfit, d.Confidence, d.Reasoning)
						if err := a.tg.SendText(msg); err != nil {
							logger.Warnf("Telegram 推送失败: %v", err)
						}
					}
				}
			}
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
