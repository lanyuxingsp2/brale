package main

import (
	"context"
	"fmt"
	"log"
	"os"
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

// 入口程序：
// 1) 加载 TOML 配置
// 2) 初始化符号提供者与提示词管理器
// 3) 启动 WS 合并流订阅，将 K 线写入内存缓存
// 4) 周期输出缓存状态（便于观察数据流）
func main() {
	ctx := context.Background()

	// 从环境变量或默认路径读取配置文件路径
	cfgPath := os.Getenv("BRALE_CONFIG")
	if cfgPath == "" {
		cfgPath = "configs/config.toml"
	}

	cfg, err := brcfg.Load(cfgPath)
	if err != nil {
		log.Fatalf("读取配置失败: %v", err)
	}
	logger.SetLevel(cfg.App.LogLevel)
	logger.Infof("✓ 配置加载成功（环境=%s，WS订阅周期=%v，K线周期=%v）", cfg.App.Env, cfg.WS.Periods, cfg.Kline.Periods)

	// 初始化符号提供者（默认/HTTP）
	var sp coins.SymbolProvider
	if cfg.Symbols.Provider == "http" {
		sp = coins.NewHTTPSymbolProvider(cfg.Symbols.APIURL)
	} else {
		sp = coins.NewDefaultProvider(cfg.Symbols.DefaultList)
	}
	syms, err := sp.List(ctx)
	if err != nil {
		log.Fatalf("获取币种列表失败: %v", err)
	}
	logger.Infof("✓ 已加载 %d 个交易对: %v", len(syms), syms)

	// 加载提示词模板（用于 AI 决策的系统提示词）
	pm := prompt.NewManager(cfg.Prompt.Dir)
	if err := pm.Load(); err != nil {
		log.Fatalf("加载提示词模板失败: %v", err)
	}
	if content, ok := pm.Get(cfg.Prompt.SystemTemplate); ok {
		logger.Infof("✓ 提示词模板 '%s' 已就绪，长度=%d 字符", cfg.Prompt.SystemTemplate, len(content))
	} else {
		logger.Warnf("未找到提示词模板 '%s'", cfg.Prompt.SystemTemplate)
	}

	// 初始化内存 K 线存储与 WS 更新器
	ks := store.NewMemoryKlineStore()
	updater := brmarket.NewWSUpdater(ks, cfg.Kline.MaxCached)

	// 启动前 REST 预热：按 kline.periods 拉取最近 N 根，避免冷启动空窗
	preheater := brmarket.NewPreheater(ks, cfg.Kline.MaxCached)
	preheater.Preheat(ctx, syms, cfg.Kline.Periods, cfg.Kline.MaxCached)

	// 启动真实 WS 订阅（复用旧版合并流客户端）：多符号 + 多周期
	go updater.StartRealWS(syms, cfg.WS.Periods, cfg.Exchange.WSBatchSize)

	// 构造 AI 模型提供方与引擎适配器（first-wins 聚合）
	/*enableDeepSeek := false
	enableQwen := false
	enableOpenAI := false
	for _, m := range cfg.AI.Models {
		if !m.Enabled {
			continue
		}
		switch m.Provider {
		case "deepseek":
			enableDeepSeek = true
		case "qwen":
			enableQwen = true
		case "openai":
			enableOpenAI = true
		}
	}*/
	// 从配置构造模型提供方（不使用环境变量）
	var modelCfgs []ai.ModelCfg
    for _, m := range cfg.AI.Models {
        modelCfgs = append(modelCfgs, ai.ModelCfg{ID: m.ID, Provider: m.Provider, Enabled: m.Enabled, APIURL: m.APIURL, APIKey: m.APIKey, Model: m.Model, Headers: m.Headers})
    }
	providers := ai.BuildProvidersFromConfig(modelCfgs)
	// 启用模型列表日志，便于确认已加载的决策模型
	{
		ids := make([]string, 0, len(providers))
		for _, p := range providers {
			if p != nil && p.Enabled() {
				ids = append(ids, p.ID())
			}
		}
		if len(ids) > 0 {
			logger.Infof("✓ 已启用 %d 个 AI 模型: %v", len(ids), ids)
		} else {
			logger.Warnf("未启用任何 AI 模型（请检查 ai.models 配置）")
		}
	}
	// 选择聚合策略（默认 first-wins，与旧项目一致）
	var aggregator ai.Aggregator = ai.FirstWinsAggregator{}
	switch cfg.AI.Aggregation {
	case "majority":
		aggregator = ai.MajorityAggregator{}
	case "weighted":
		aggregator = ai.WeightedAggregator{Weights: map[string]float64{"deepseek": 1.0, "qwen": 1.0}}
	}
	engine := &ai.LegacyEngineAdapter{
		Providers:      providers,
		Agg:            aggregator,
		PromptMgr:      pm,
		SystemTemplate: cfg.Prompt.SystemTemplate,
		KStore:         ks,
		Intervals:      cfg.WS.Periods,
		Parallel:       true,
		Metrics:        brmarket.NewDefaultMetricsFetcher(""),
		IncludeOI:      true,
		IncludeFunding: true,
		TimeoutSeconds: cfg.MCP.TimeoutSeconds,
	}

	// Telegram 通知器（可选）
	var tg *notify.Telegram
	if cfg.Notify.Telegram.Enabled {
		tg = notify.NewTelegram(os.Getenv("TELEGRAM_BOT_TOKEN"), os.Getenv("TELEGRAM_CHAT_ID"))
	}

    // 决策周期：可配置（单位：秒）
    decisionInterval := time.Duration(cfg.AI.DecisionIntervalSeconds) * time.Second
    if decisionInterval <= 0 { decisionInterval = time.Minute }
    decisionTicker := time.NewTicker(decisionInterval)
    cacheTicker := time.NewTicker(15 * time.Second) // 打印缓存心跳
    statsTicker := time.NewTicker(60 * time.Second) // WS 统计
	defer decisionTicker.Stop()
	defer cacheTicker.Stop()
	defer statsTicker.Stop()

    // 友好展示：按分钟或秒打印
    human := fmt.Sprintf("%d 秒", int(decisionInterval.Seconds()))
    if cfg.AI.DecisionIntervalSeconds%60 == 0 { human = fmt.Sprintf("%d 分钟", cfg.AI.DecisionIntervalSeconds/60) }
    fmt.Println(fmt.Sprintf("Brale 启动完成。开始订阅 K 线并写入缓存；每 %s 进行一次 AI 决策。按 Ctrl+C 退出。", human))
	// 简单开仓冷却（避免频繁开仓）：符号+方向 -> 上次开仓时间
	lastOpen := map[string]time.Time{}

    for {
		select {
		case <-ctx.Done():
			return
		case <-cacheTicker.C:
			// 打印缓存状态
			for _, sym := range syms {
				for _, iv := range cfg.WS.Periods {
                        if kl, err := ks.Get(ctx, sym, iv); err == nil {
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
            if updater != nil && updater.Client != nil {
                r, s, last := updater.Client.Stats()
                logger.Infof("WS统计: 重连=%d 订阅错误=%d 最后错误=%s", r, s, last)
            }
		case <-decisionTicker.C:
			// 构建最小上下文并进行决策
			input := ai.Context{Candidates: syms}
            res, err := engine.Decide(ctx, input)
            if err != nil {
                logger.Warnf("AI 决策失败: %v", err)
                continue
            }
            if len(res.Decisions) == 0 {
                logger.Infof("AI 决策为空（观望）")
                continue
            }
            // 调试：输出原始 JSON 决策片段（需将 log_level 设为 debug 才可见）
            if res.RawOutput != "" {
                snippet := res.RawOutput
                if len(snippet) > 1000 { snippet = snippet[:1000] + "..." }
                logger.Debugf("AI 原始JSON: %s", snippet)
            }
			// 排序与去重（close > open > hold/wait）
			res.Decisions = ai.OrderAndDedup(res.Decisions)
			// 打印并通知
			// 周期新开仓上限统计
			newOpens := 0
			// 选一个用于价格校验的周期（优先 WS 周期首个，否则 kline 首个）
			validateIv := ""
			if len(cfg.WS.Periods) > 0 {
				validateIv = cfg.WS.Periods[0]
			} else if len(cfg.Kline.Periods) > 0 {
				validateIv = cfg.Kline.Periods[0]
			}
			for _, d := range res.Decisions {
				// 基础校验
				if err := ai.Validate(&d); err != nil {
					logger.Warnf("AI 决策不合规，已忽略: %v | %+v", err, d)
					continue
				}
				// 带价格的校验（RR、关系）；若无法获取价格，则仅执行基础校验
				if validateIv != "" {
					if kl, _ := ks.Get(ctx, d.Symbol, validateIv); len(kl) > 0 {
						price := kl[len(kl)-1].Close
						if err := ai.ValidateWithPrice(&d, price, cfg.Advanced.MinRiskReward); err != nil {
							logger.Warnf("AI 决策RR校验失败，已忽略: %v | %+v", err, d)
							continue
						}
					}
				}
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
                default: // hold / wait
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
				if d.Action == "open_long" || d.Action == "open_short" {
					if newOpens >= cfg.Advanced.MaxOpensPerCycle {
						logger.Infof("跳过超出本周期开仓上限: %s %s", d.Symbol, d.Action)
						continue
					}
					// 冷却限制（默认3分钟，可配置）
					key := d.Symbol + "#" + d.Action
					if prev, ok := lastOpen[key]; ok {
						if time.Since(prev) < time.Duration(cfg.Advanced.OpenCooldownSeconds)*time.Second {
							logger.Infof("跳过频繁开仓（冷却中）: %s 剩余 %.0fs", key, float64(time.Duration(cfg.Advanced.OpenCooldownSeconds)*time.Second-time.Since(prev))/float64(time.Second))
							continue
						}
					}
					lastOpen[key] = time.Now()
					newOpens++
				}
				if tg != nil && (d.Action == "open_long" || d.Action == "open_short") {
					msg := fmt.Sprintf("开仓信号\n币种: %s\n动作: %s\n杠杆: %dx\n仓位: %.0f USDT\n止损: %.4f\n止盈: %.4f\n信心: %d\n理由: %s",
						d.Symbol, d.Action, d.Leverage, d.PositionSizeUSD, d.StopLoss, d.TakeProfit, d.Confidence, d.Reasoning)
					if err := tg.SendText(msg); err != nil {
						logger.Warnf("Telegram 推送失败: %v", err)
					}
				}
			}
		}
	}
}
