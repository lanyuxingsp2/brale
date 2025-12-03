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
	"brale/internal/store"
	"brale/internal/strategy"
	livehttp "brale/internal/transport/http/live"

	"golang.org/x/sync/errgroup"
)

// App 负责应用级编排：加载配置→初始化依赖→启动实时与回测服务。
type App struct {
	cfg        *brcfg.Config
	live       *LiveService
	liveHTTP   *livehttp.Server
	metricsSvc *market.MetricsService // 新增：持有 MetricsService 实例
}

// NewApp 根据配置构建应用对象（不启动）
func NewApp(cfg *brcfg.Config) (*App, error) {
	if cfg == nil {
		return nil, fmt.Errorf("nil config")
	}
	logger.SetLevel(cfg.App.LogLevel)
	return buildAppWithWire(context.Background(), cfg)
}

// Run 启动回测与实时服务。
func (a *App) Run(ctx context.Context) error {
	if a == nil || a.cfg == nil {
		return fmt.Errorf("app not initialized")
	}

	if a.live == nil {
		return fmt.Errorf("live service not initialized")
	}
	group, ctx := errgroup.WithContext(ctx)

	if a.metricsSvc != nil {
		group.Go(func() error {
			a.metricsSvc.Start(ctx)
			return nil
		})
	}

	if a.liveHTTP != nil {
		group.Go(func() error {
			if err := a.liveHTTP.Start(ctx); err != nil && ctx.Err() == nil {
				logger.Warnf("Live HTTP 停止: %v", err)
			}
			return nil
		})
	}

	group.Go(func() error {
		defer a.live.Close()
		return a.live.Run(ctx)
	})

	return group.Wait()
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

type decisionArtifacts struct {
	store       *database.DecisionLogStore
	recorder    market.Recorder
	cache       *lastDecisionCache
	initialJSON string
}

type marketStack struct {
	store         market.KlineStore
	updater       *market.WSUpdater
	metrics       *market.MetricsService
	warmupSummary string
}

type engineConfig struct {
	Providers          []provider.ModelProvider
	Aggregator         decision.Aggregator
	PromptMgr          *strategy.Manager
	SystemTemplate     string
	Store              market.KlineStore
	Intervals          []string
	Horizon            brcfg.HorizonProfile
	HorizonName        string
	MultiAgent         brcfg.MultiAgentConfig
	ProviderPreference []string
	LogEachModel       bool
	Metrics            *market.MetricsService
	TimeoutSeconds     int
}

func buildSymbolProvider(cfg brcfg.SymbolsConfig) coins.SymbolProvider {
	if strings.EqualFold(cfg.Provider, "http") {
		return coins.NewHTTPSymbolProvider(cfg.APIURL)
	}
	return coins.NewDefaultProvider(cfg.DefaultList)
}

func newTelegram(cfg brcfg.NotifyConfig) *notifier.Telegram {
	if !cfg.Telegram.Enabled {
		return nil
	}
	return notifier.NewTelegram(cfg.Telegram.BotToken, cfg.Telegram.ChatID)
}

func buildDecisionArtifacts(ctx context.Context, cfg brcfg.AIConfig, engine *decision.LegacyEngineAdapter) (*decisionArtifacts, error) {
	artifacts := &decisionArtifacts{}
	maxAge := time.Duration(cfg.LastDecisionMaxAgeSec) * time.Second
	if strings.TrimSpace(cfg.DecisionLogPath) == "" {
		if cfg.IncludeLastDecision {
			artifacts.cache = newLastDecisionCache(maxAge)
		}
		return artifacts, nil
	}
	store, err := database.NewDecisionLogStore(cfg.DecisionLogPath)
	if err != nil {
		return nil, fmt.Errorf("初始化决策日志存储失败: %w", err)
	}
	artifacts.store = store
	artifacts.recorder = store
	if engine != nil {
		if obs := database.NewDecisionLogObserver(store); obs != nil {
			engine.Observer = obs
		}
	}
	logPath := cfg.DecisionLogPath
	if abs, err := filepath.Abs(logPath); err == nil {
		logPath = abs
	}
	logger.Infof("✓ 实盘决策日志写入 %s", logPath)
	if cfg.IncludeLastDecision {
		cache := newLastDecisionCache(maxAge)
		artifacts.cache = cache
		records, err := store.LoadLastDecisions(ctx)
		if err != nil {
			logger.Warnf("加载 LastDecision 失败: %v", err)
		} else {
			cache.Load(records)
			artifacts.initialJSON = flattenLastDecisionJSON(records)
		}
	}
	return artifacts, nil
}

func buildFreqManager(cfg brcfg.FreqtradeConfig, horizon string, logStore *database.DecisionLogStore, recorder market.Recorder, notifier freqexec.TextNotifier) (*freqexec.Manager, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	client, err := freqexec.NewClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("初始化 freqtrade 执行器失败: %w", err)
	}
	logger.Infof("✓ Freqtrade 执行器已启用: %s", cfg.APIURL)
	manager := freqexec.NewManager(client, cfg, horizon, logStore, recorder, notifier)
	return manager, nil
}

func buildLiveHTTPServer(cfg brcfg.AppConfig, logs *database.DecisionLogStore, freqHandler livehttp.FreqtradeWebhookHandler, defaultSymbols []string) (*livehttp.Server, error) {
	if logs == nil && freqHandler == nil {
		return nil, nil
	}
	logPaths := map[string]string{}
	if path := strings.TrimSpace(cfg.LogPath); path != "" {
		logPaths["app"] = path
	}
	if path := strings.TrimSpace(cfg.LLMLog); path != "" {
		logPaths["llm"] = path
	}
	server, err := livehttp.NewServer(livehttp.ServerConfig{
		Addr:             cfg.HTTPAddr,
		Logs:             logs,
		FreqtradeHandler: freqHandler,
		DefaultSymbols:   defaultSymbols,
		LogPaths:         logPaths,
	})
	if err != nil {
		return nil, fmt.Errorf("初始化 live HTTP 失败: %w", err)
	}
	logger.Infof("✓ Live HTTP 接口监听 %s", server.Addr())
	return server, nil
}

func buildAggregator(cfg brcfg.AIConfig) decision.Aggregator {
	if strings.EqualFold(cfg.Aggregation, "meta") {
		return decision.MetaAggregator{
			Weights:    cfg.Weights,
			Preference: cfg.ProviderPreference,
		}
	}
	return decision.FirstWinsAggregator{}
}

func buildModelProviders(ctx context.Context, cfg brcfg.AIConfig, timeoutSeconds int) ([]provider.ModelProvider, bool, error) {
	var (
		modelCfgs   []provider.ModelCfg
		visionReady bool
	)
	for _, m := range cfg.MustResolveModelConfigs() {
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
			return nil, false, fmt.Errorf("初始化可视化渲染失败(请安装 headless Chrome): %w", err)
		}
	} else {
		logger.Infof("所有启用模型均不支持图像，跳过可视化渲染初始化")
	}
	timeout := time.Duration(timeoutSeconds) * time.Second
	providers := provider.BuildProvidersFromConfig(modelCfgs, timeout)
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
	return providers, visionReady, nil
}

func buildDecisionEngine(cfg engineConfig) *decision.LegacyEngineAdapter {
	agg := cfg.Aggregator
	if agg == nil {
		agg = decision.FirstWinsAggregator{}
	}
	return &decision.LegacyEngineAdapter{
		Providers:             cfg.Providers,
		Agg:                   agg,
		PromptMgr:             cfg.PromptMgr,
		SystemTemplate:        cfg.SystemTemplate,
		KStore:                cfg.Store,
		Intervals:             append([]string(nil), cfg.Intervals...),
		Horizon:               cfg.Horizon,
		HorizonName:           cfg.HorizonName,
		MultiAgent:            cfg.MultiAgent,
		ProviderPreference:    append([]string(nil), cfg.ProviderPreference...),
		Parallel:              true,
		LogEachModel:          cfg.LogEachModel,
		DebugStructuredBlocks: cfg.LogEachModel,
		Metrics:               cfg.Metrics,
		IncludeOI:             true,
		IncludeFunding:        true,
		TimeoutSeconds:        cfg.TimeoutSeconds,
	}
}

func buildMarketStack(ctx context.Context, cfg *brcfg.Config, symbols []string, horizon brcfg.HorizonProfile, intervals []string) (*marketStack, error) {
	src, err := gateway.NewSourceFromConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("初始化行情源失败: %w", err)
	}
	success := false
	defer func() {
		if !success {
			_ = src.Close()
		}
	}()

	kstore := store.NewMemoryKlineStore()
	updater := market.NewWSUpdater(kstore, cfg.Kline.MaxCached, src)

	lookbacks := horizon.LookbackMap(20)
	preheater := market.NewPreheater(kstore, cfg.Kline.MaxCached, src)
	preheater.Warmup(ctx, symbols, lookbacks)
	preheater.Preheat(ctx, symbols, intervals, cfg.Kline.MaxCached)
	logger.Infof("✓ Warmup 完成，最小条数=%v", lookbacks)
	warmupSummary := fmt.Sprintf("*Warmup 完成*\n```\n%v\n```", lookbacks)

	metricsSvc := market.NewMetricsService(src, symbols, cfg.AI.DecisionIntervalSeconds, horizon)
	if metricsSvc == nil {
		return nil, fmt.Errorf("初始化 MetricsService 失败")
	}
	logger.Infof("✓ MetricsService 初始化成功")

	success = true
	return &marketStack{
		store:         kstore,
		updater:       updater,
		metrics:       metricsSvc,
		warmupSummary: warmupSummary,
	}, nil
}
