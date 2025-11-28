package app

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"brale/internal/analysis/visual"
	"brale/internal/coins"
	brcfg "brale/internal/config"
	"brale/internal/decision"
	freqexec "brale/internal/executor/freqtrade"
	"brale/internal/gateway"
	"brale/internal/gateway/database"
	"brale/internal/gateway/notifier"
	"brale/internal/gateway/provider"
	"brale/internal/logger"
	"brale/internal/market"
	brmarket "brale/internal/market"
	"brale/internal/store"
	"brale/internal/strategy"
	livehttp "brale/internal/transport/http/live"
)

// App 负责应用级编排：加载配置→初始化依赖→启动实时与回测服务。
type App struct {
	cfg      *brcfg.Config
	live     *LiveService
	liveHTTP *livehttp.Server
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
	pm := strategy.NewManager(cfg.Prompt.Dir)
	if err := pm.Load(); err != nil {
		return nil, fmt.Errorf("加载提示词模板失败: %w", err)
	}
	if content, ok := pm.Get(cfg.Prompt.SystemTemplate); ok {
		logger.Infof("✓ 提示词模板 '%s' 已就绪，长度=%d 字符", cfg.Prompt.SystemTemplate, len(content))
	} else {
		logger.Warnf("未找到提示词模板 '%s'", cfg.Prompt.SystemTemplate)
	}

	// 存储与 WS 更新器
	src, err := gateway.NewSourceFromConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("初始化行情源失败: %w", err)
	}
	defer func() {
		if err != nil && src != nil {
			_ = src.Close()
		}
	}()

	ks := store.NewMemoryKlineStore()
	updater := brmarket.NewWSUpdater(ks, cfg.Kline.MaxCached, src)

	// 预热
	lookbacks := horizon.LookbackMap(20)
	preheater := brmarket.NewPreheater(ks, cfg.Kline.MaxCached, src)
	preheater.Warmup(ctx, syms, lookbacks)
	preheater.Preheat(ctx, syms, hIntervals, cfg.Kline.MaxCached)
	logger.Infof("✓ Warmup 完成，最小条数=%v", lookbacks)
	warmupSummary := fmt.Sprintf("*Warmup 完成*\n```\n%v\n```", lookbacks)

	// 模型 Providers
	var (
		modelCfgs   []provider.ModelCfg
		visionReady bool
	)
	for _, m := range cfg.AI.MustResolveModelConfigs() {
		modelCfgs = append(modelCfgs, provider.ModelCfg{
			ID:             m.ID,
			Provider:       m.Provider,
			Enabled:        m.Enabled,
			APIURL:         m.APIURL,
			APIKey:         m.APIKey,
			Model:          m.Model,
			Headers:        m.Headers,
			SupportsVision: m.SupportsVision,
			ExpectJSON:     m.ExpectJSON,
		})
		if m.Enabled && m.SupportsVision {
			visionReady = true
		}
	}
	if visionReady {
		if err := visual.EnsureHeadlessAvailable(ctx); err != nil {
			return nil, fmt.Errorf("初始化可视化渲染失败(请安装 headless Chrome): %w", err)
		}
	} else {
		logger.Infof("所有启用模型均不支持图像，跳过可视化渲染初始化")
	}
	providers := provider.BuildProvidersFromConfig(modelCfgs, time.Duration(cfg.MCP.TimeoutSeconds)*time.Second)
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
	var aggregator decision.Aggregator = decision.FirstWinsAggregator{}
	if cfg.AI.Aggregation == "meta" {
		aggregator = decision.MetaAggregator{
			Weights:    cfg.AI.Weights,
			Preference: cfg.AI.ProviderPreference,
		}
	}

	// 引擎
	engine := &decision.LegacyEngineAdapter{
		Providers:             providers,
		Agg:                   aggregator,
		PromptMgr:             pm,
		SystemTemplate:        cfg.Prompt.SystemTemplate,
		KStore:                ks,
		Intervals:             hIntervals,
		Horizon:               horizon,
		HorizonName:           cfg.AI.ActiveHorizon,
		MultiAgent:            cfg.AI.MultiAgent,
		ProviderPreference:    cfg.AI.ProviderPreference,
		Parallel:              true,
		LogEachModel:          cfg.AI.LogEachModel,
		DebugStructuredBlocks: cfg.AI.LogEachModel,
		Metrics:               brmarket.NewDefaultMetricsFetcher(""),
		IncludeOI:             true,
		IncludeFunding:        true,
		TimeoutSeconds:        cfg.MCP.TimeoutSeconds,
	}

	// Telegram（可选）
	var tg *notifier.Telegram
	if cfg.Notify.Telegram.Enabled {
		tg = notifier.NewTelegram(cfg.Notify.Telegram.BotToken, cfg.Notify.Telegram.ChatID)
	}
	if tg != nil {
		engine.AgentNotifier = tg
	}

	var decisionStore *database.DecisionLogStore
	if cfg.AI.DecisionLogPath != "" {
		var err error
		decisionStore, err = database.NewDecisionLogStore(cfg.AI.DecisionLogPath)
		if err != nil {
			return nil, fmt.Errorf("初始化决策日志存储失败: %w", err)
		}
		if obs := database.NewDecisionLogObserver(decisionStore); obs != nil {
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
	lastCache := newLastDecisionCache(time.Duration(cfg.AI.LastDecisionMaxAgeSec) * time.Second)
	if !includeLastDecision {
		lastCache = nil
	}
	orderRecorder := market.Recorder(nil)
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

	var freqManager *freqexec.Manager
	if cfg.Freqtrade.Enabled {
		client, err := freqexec.NewClient(cfg.Freqtrade)
		if err != nil {
			return nil, fmt.Errorf("初始化 freqtrade 执行器失败: %w", err)
		}
		logger.Infof("✓ Freqtrade 执行器已启用: %s", cfg.Freqtrade.APIURL)
		freqManager = freqexec.NewManager(client, cfg.Freqtrade, cfg.AI.ActiveHorizon, decisionStore, orderRecorder, tg)
	}

	liveSvc := &LiveService{
		cfg:                 cfg,
		ks:                  ks,
		updater:             updater,
		engine:              engine,
		tg:                  tg,
		decLogs:             decisionStore,
		orderRec:            orderRecorder,
		lastDec:             lastCache,
		includeLastDecision: includeLastDecision,
		symbols:             syms,
		hIntervals:          append([]string(nil), hIntervals...),
		horizonName:         cfg.AI.ActiveHorizon,
		profile:             horizon,
		hSummary:            hSummary,
		warmupSummary:       warmupSummary,
		lastOpen:            map[string]time.Time{},
		lastRawJSON:         initialLastJSON,
		freqManager:         freqManager,
		visionReady:         visionReady,
		priceCache:          make(map[string]cachedQuote),
	}

	var liveHTTPServe *livehttp.Server
	var freqHandler livehttp.FreqtradeWebhookHandler
	if freqManager != nil {
		freqHandler = liveSvc
	}
	if decisionStore != nil || freqManager != nil {
		var err error
		logPaths := map[string]string{}
		if path := strings.TrimSpace(cfg.App.LogPath); path != "" {
			logPaths["app"] = path
		}
		if path := strings.TrimSpace(cfg.App.LLMLog); path != "" {
			logPaths["llm"] = path
		}
		liveHTTPServe, err = livehttp.NewServer(livehttp.ServerConfig{
			Addr:             cfg.App.HTTPAddr,
			Logs:             decisionStore,
			FreqtradeHandler: freqHandler,
			DefaultSymbols:   syms,
			LogPaths:         logPaths,
		})
		if err != nil {
			return nil, fmt.Errorf("初始化 live HTTP 失败: %w", err)
		}
		logger.Infof("✓ Live HTTP 接口监听 %s", liveHTTPServe.Addr())
	}

	return &App{
		cfg:      cfg,
		live:     liveSvc,
		liveHTTP: liveHTTPServe,
	}, nil
}

// Run 启动回测与实时服务。
func (a *App) Run(ctx context.Context) error {
	if a == nil || a.cfg == nil {
		return fmt.Errorf("app not initialized")
	}
	if a.liveHTTP != nil {
		go func() {
			if err := a.liveHTTP.Start(ctx); err != nil {
				logger.Warnf("Live HTTP 停止: %v", err)
			}
		}()
	}
	if a.live == nil {
		return fmt.Errorf("live service not initialized")
	}
	defer a.live.Close()
	return a.live.Run(ctx)
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

func flattenLastDecisionJSON(records []decision.LastDecisionRecord) string {
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
	all := make([]decision.Decision, 0, total)
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
