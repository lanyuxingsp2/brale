package app

import (
	"context"
	"fmt"
	"time"

	"brale/internal/coins"
	brcfg "brale/internal/config"
	"brale/internal/decision"
	freqexec "brale/internal/executor/freqtrade"
	"brale/internal/gateway/database"
	"brale/internal/gateway/provider"
	"brale/internal/logger"
	"brale/internal/market"
	"brale/internal/strategy"
	livehttp "brale/internal/transport/http/live"
)

// AppBuilder uses dependency injection to assemble *App instances.
type AppBuilder struct {
	cfg *brcfg.Config

	symbolProviderFn    func(brcfg.SymbolsConfig) coins.SymbolProvider
	promptManagerFn     func(string) (*strategy.Manager, error)
	marketStackFn       func(context.Context, *brcfg.Config, []string, brcfg.HorizonProfile, []string) (*marketStack, error)
	modelProvidersFn    func(context.Context, brcfg.AIConfig, int) ([]provider.ModelProvider, map[string]bool, bool, error)
	decisionArtifactsFn func(context.Context, brcfg.AIConfig, *decision.LegacyEngineAdapter) (*decisionArtifacts, error)
	freqManagerFn       func(brcfg.FreqtradeConfig, string, *database.DecisionLogStore, market.Recorder, freqexec.TextNotifier) (*freqexec.Manager, error)
	liveHTTPFn          func(brcfg.AppConfig, *database.DecisionLogStore, livehttp.FreqtradeWebhookHandler, []string) (*livehttp.Server, error)
}

// AppBuilderOption customizes an AppBuilder dependency.
type AppBuilderOption func(*AppBuilder)

// NewAppBuilder creates a builder with default dependencies.
func NewAppBuilder(cfg *brcfg.Config, opts ...AppBuilderOption) *AppBuilder {
	b := &AppBuilder{
		cfg:                 cfg,
		symbolProviderFn:    buildSymbolProvider,
		promptManagerFn:     loadPromptManager,
		marketStackFn:       buildMarketStack,
		modelProvidersFn:    buildModelProviders,
		decisionArtifactsFn: buildDecisionArtifacts,
		freqManagerFn:       buildFreqManager,
		liveHTTPFn:          buildLiveHTTPServer,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(b)
		}
	}
	return b
}

func loadPromptManager(dir string) (*strategy.Manager, error) {
	pm := strategy.NewManager(dir)
	if err := pm.Load(); err != nil {
		return nil, err
	}
	return pm, nil
}

// Build assembles the application using injected dependencies.
func (b *AppBuilder) Build(ctx context.Context) (*App, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if b.cfg == nil {
		return nil, fmt.Errorf("nil config")
	}
	cfg := b.cfg
	logger.SetLevel(cfg.App.LogLevel)

	sp := b.symbolProviderFn(cfg.Symbols)
	syms, err := sp.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("获取币种列表失败: %w", err)
	}
	logger.Infof("✓ 已加载 %d 个交易对: %v", len(syms), syms)

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

	pm, err := b.promptManagerFn(cfg.Prompt.Dir)
	if err != nil {
		return nil, fmt.Errorf("加载提示词模板失败: %w", err)
	}
	if content, ok := pm.Get(cfg.Prompt.SystemTemplate); ok {
		logger.Infof("✓ 提示词模板 '%s' 已就绪，长度=%d 字符", cfg.Prompt.SystemTemplate, len(content))
	} else {
		logger.Warnf("未找到提示词模板 '%s'", cfg.Prompt.SystemTemplate)
	}

	marketStack, err := b.marketStackFn(ctx, cfg, syms, horizon, hIntervals)
	if err != nil {
		return nil, err
	}
	ks := marketStack.store
	updater := marketStack.updater
	warmupSummary := marketStack.warmupSummary
	metricsSvc := marketStack.metrics

	providers, finalDisabled, visionReady, err := b.modelProvidersFn(ctx, cfg.AI, cfg.MCP.TimeoutSeconds)
	if err != nil {
		return nil, err
	}

	engine := buildDecisionEngine(engineConfig{
		Providers:          providers,
		Aggregator:         buildAggregator(cfg.AI),
		PromptMgr:          pm,
		SystemTemplate:     cfg.Prompt.SystemTemplate,
		Store:              ks,
		Intervals:          hIntervals,
		Horizon:            horizon,
		HorizonName:        cfg.AI.ActiveHorizon,
		MultiAgent:         cfg.AI.MultiAgent,
		ProviderPreference: cfg.AI.ProviderPreference,
		FinalDisabled:      finalDisabled,
		LogEachModel:       cfg.AI.LogEachModel,
		Metrics:            metricsSvc,
		TimeoutSeconds:     cfg.MCP.TimeoutSeconds,
	})

	tg := newTelegram(cfg.Notify)
	if tg != nil {
		engine.AgentNotifier = tg
	}

	decArtifacts, err := b.decisionArtifactsFn(ctx, cfg.AI, engine)
	if err != nil {
		return nil, err
	}
	includeLastDecision := cfg.AI.IncludeLastDecision
	orderRecorder := decArtifacts.recorder
	freqManager, err := b.freqManagerFn(cfg.Freqtrade, cfg.AI.ActiveHorizon, decArtifacts.store, orderRecorder, tg)
	if err != nil {
		return nil, err
	}

	liveSvc := &LiveService{
		cfg:                 cfg,
		ks:                  ks,
		updater:             updater,
		engine:              engine,
		tg:                  tg,
		decLogs:             decArtifacts.store,
		orderRec:            orderRecorder,
		lastDec:             decArtifacts.cache,
		includeLastDecision: includeLastDecision,
		symbols:             syms,
		hIntervals:          append([]string(nil), hIntervals...),
		horizonName:         cfg.AI.ActiveHorizon,
		profile:             horizon,
		hSummary:            hSummary,
		warmupSummary:       warmupSummary,
		lastOpen:            map[string]time.Time{},
		lastRawJSON:         decArtifacts.initialJSON,
		freqManager:         freqManager,
		visionReady:         visionReady,
		priceCache:          make(map[string]cachedQuote),
	}

	var freqHandler livehttp.FreqtradeWebhookHandler
	if freqManager != nil {
		freqHandler = liveSvc
	}
	liveHTTPServe, err := b.liveHTTPFn(cfg.App, decArtifacts.store, freqHandler, syms)
	if err != nil {
		return nil, err
	}

	return &App{
		cfg:        cfg,
		live:       liveSvc,
		liveHTTP:   liveHTTPServe,
		metricsSvc: metricsSvc,
	}, nil
}

// WithSymbolProvider overrides the symbol provider factory (useful for tests).
func WithSymbolProvider(fn func(brcfg.SymbolsConfig) coins.SymbolProvider) AppBuilderOption {
	return func(b *AppBuilder) {
		if fn != nil {
			b.symbolProviderFn = fn
		}
	}
}

// WithPromptManager overrides the prompt manager constructor.
func WithPromptManager(fn func(string) (*strategy.Manager, error)) AppBuilderOption {
	return func(b *AppBuilder) {
		if fn != nil {
			b.promptManagerFn = fn
		}
	}
}

// WithMarketStack overrides the market stack builder.
func WithMarketStack(fn func(context.Context, *brcfg.Config, []string, brcfg.HorizonProfile, []string) (*marketStack, error)) AppBuilderOption {
	return func(b *AppBuilder) {
		if fn != nil {
			b.marketStackFn = fn
		}
	}
}

// WithModelProviders overrides the model provider builder.
func WithModelProviders(fn func(context.Context, brcfg.AIConfig, int) ([]provider.ModelProvider, map[string]bool, bool, error)) AppBuilderOption {
	return func(b *AppBuilder) {
		if fn != nil {
			b.modelProvidersFn = fn
		}
	}
}

// WithDecisionArtifacts overrides the decision artifact loader.
func WithDecisionArtifacts(fn func(context.Context, brcfg.AIConfig, *decision.LegacyEngineAdapter) (*decisionArtifacts, error)) AppBuilderOption {
	return func(b *AppBuilder) {
		if fn != nil {
			b.decisionArtifactsFn = fn
		}
	}
}

// WithFreqManager overrides the freqtrade manager builder.
func WithFreqManager(fn func(brcfg.FreqtradeConfig, string, *database.DecisionLogStore, market.Recorder, freqexec.TextNotifier) (*freqexec.Manager, error)) AppBuilderOption {
	return func(b *AppBuilder) {
		if fn != nil {
			b.freqManagerFn = fn
		}
	}
}

// WithLiveHTTP overrides the live HTTP server builder.
func WithLiveHTTP(fn func(brcfg.AppConfig, *database.DecisionLogStore, livehttp.FreqtradeWebhookHandler, []string) (*livehttp.Server, error)) AppBuilderOption {
	return func(b *AppBuilder) {
		if fn != nil {
			b.liveHTTPFn = fn
		}
	}
}
