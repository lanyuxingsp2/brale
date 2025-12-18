package app

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"brale/internal/agent"
	brcfg "brale/internal/config"
	cfgloader "brale/internal/config/loader"
	"brale/internal/decision"
	"brale/internal/exitplan"
	"brale/internal/gateway/database"
	freqexec "brale/internal/gateway/freqtrade"
	"brale/internal/gateway/notifier"
	"brale/internal/gateway/provider"
	"brale/internal/logger"
	"brale/internal/pipeline/factory"
	"brale/internal/profile"
	promptkit "brale/internal/prompt"
	"brale/internal/store"
	"brale/internal/store/gormstore"
	"brale/internal/store/sqlite"
	"brale/internal/strategy"
	"brale/internal/strategy/exit"
	exitHandlers "brale/internal/strategy/exit/handlers"
	livehttp "brale/internal/transport/http/live"

	"gorm.io/gorm"
)

// AppBuilder uses dependency injection to assemble *App instances.
type AppBuilder struct {
	cfg *brcfg.Config

	promptManagerFn     func(string) (*strategy.Manager, error)
	marketStackFn       func(context.Context, *brcfg.Config, []string, []string, map[string]int, []string) (*MarketStack, error)
	modelProvidersFn    func(context.Context, brcfg.AIConfig, int) ([]provider.ModelProvider, map[string]bool, bool, error)
	decisionArtifactsFn func(context.Context, brcfg.AIConfig, *decision.LegacyEngineAdapter) (*decisionArtifacts, error)
	freqManagerFn       func(brcfg.FreqtradeConfig, string, *database.DecisionLogStore, database.LivePositionStore, store.Store, notifier.TextNotifier) (*freqexec.Manager, error)
	liveHTTPFn          func(brcfg.AppConfig, *database.DecisionLogStore, livehttp.FreqtradeWebhookHandler, []string, map[string]livehttp.SymbolDetail) (*livehttp.Server, error)

	liveStoreOverride     database.LivePositionStore
	strategyStoreOverride exit.StrategyStore
	newStoreOverride      store.Store
}

// AppBuilderOption customizes an AppBuilder dependency.
type AppBuilderOption func(*AppBuilder)

// NewAppBuilder creates a builder with default dependencies.
func NewAppBuilder(cfg *brcfg.Config, opts ...AppBuilderOption) *AppBuilder {
	b := &AppBuilder{
		cfg:                 cfg,
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

	var profileLoader *cfgloader.ProfileLoader
	if strings.TrimSpace(cfg.AI.ProfilesPath) == "" {
		return nil, fmt.Errorf("ai.profiles_path 未配置，无法加载 profile")
	}
	profileLoader, err := cfgloader.NewProfileLoader(cfg.AI.ProfilesPath)
	if err != nil {
		return nil, fmt.Errorf("加载 profile 配置失败: %w", err)
	}
	profileSnapshot := profileLoader.Snapshot()
	syms, profileIntervals, lookbacks, derivativeSymbols, err := collectProfileUniverse(profileSnapshot, cfg.Kline.MaxCached)
	if err != nil {
		return nil, err
	}
	logger.Infof("✓ 已加载 %d 个交易对: %v", len(syms), syms)
	logger.Infof("✓ Profile 周期: %v", profileIntervals)
	hSummary := formatProfileSummary(syms, profileIntervals)
	logger.Infof("[profiles]\n%s", hSummary)
	// logProfileMiddlewares(profileSnapshot)

	applyDefaultMultiAgentBlocks(cfg, len(syms), len(profileIntervals))

	pm, err := b.promptManagerFn(cfg.Prompt.Dir)
	if err != nil {
		return nil, fmt.Errorf("加载提示词模板失败: %w", err)
	}
	if content, ok := pm.Get(cfg.Prompt.SystemTemplate); ok {
		logger.Infof("✓ 提示词模板 '%s' 已就绪，长度=%d 字符", cfg.Prompt.SystemTemplate, len(content))
	} else {
		logger.Warnf("未找到提示词模板 '%s'", cfg.Prompt.SystemTemplate)
	}
	var promptLoader profile.PromptLoader
	if pm != nil {
		promptLoader = profile.NewPromptLoader(pm, ".", cfg.Prompt.Dir)
	}

	requiredModelIDs, err := enabledFinalModelIDs(cfg.AI)
	if err != nil {
		return nil, err
	}
	if err := validateProfilePrompts(profileSnapshot, promptLoader, requiredModelIDs); err != nil {
		return nil, err
	}

	marketStack, err := b.marketStackFn(ctx, cfg, syms, profileIntervals, lookbacks, derivativeSymbols)
	if err != nil {
		return nil, err
	}
	ks := marketStack.Store
	updater := marketStack.Updater
	warmupSummary := marketStack.WarmupSummary
	metricsSvc := marketStack.Metrics

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
		Intervals:          profileIntervals,
		HorizonName:        cfg.AI.ActiveHorizon,
		MultiAgent:         cfg.AI.MultiAgent,
		ProviderPreference: cfg.AI.ProviderPreference,
		FinalDisabled:      finalDisabled,
		LogEachModel:       cfg.AI.LogEachModel,
		Metrics:            metricsSvc,
		TimeoutSeconds:     cfg.MCP.TimeoutSeconds,
	})

	tgClient := newTelegram(cfg.Notify)
	var textNotifier notifier.TextNotifier
	if tgClient != nil {
		textNotifier = tgClient
	}
	if textNotifier != nil {
		engine.AgentNotifier = textNotifier
	}

	decArtifacts, err := b.decisionArtifactsFn(ctx, cfg.AI, engine)
	if err != nil {
		return nil, err
	}

	var (
		strategyStore exit.StrategyStore
		liveStore     database.LivePositionStore
		newStore      store.Store
		sharedGorm    *gorm.DB
	)
	if b.strategyStoreOverride != nil {
		strategyStore = b.strategyStoreOverride
	}
	if b.liveStoreOverride != nil {
		liveStore = b.liveStoreOverride
	} else if b.strategyStoreOverride != nil {
		if ls, ok := strategyStore.(database.LivePositionStore); ok {
			liveStore = ls
		}
	}
	if b.newStoreOverride != nil {
		newStore = b.newStoreOverride
	}

	if strategyStore == nil || liveStore == nil || newStore == nil {
		path := strings.TrimSpace(cfg.AI.DecisionLogPath)
		if path == "" {
			return nil, fmt.Errorf("ai.decision_log_path 未配置，无法初始化存储")
		}
		gormStore, err := gormstore.NewGormStore(path)
		if err != nil {
			return nil, fmt.Errorf("初始化 gorm 存储失败: %w", err)
		}
		strategyStore = gormStore
		liveStore = gormStore
		sharedGorm = gormStore.GormDB()
		if decArtifacts.store != nil {
			sqlDB, err := gormStore.SQLDB()
			if err != nil {
				return nil, fmt.Errorf("获取 SQL DB 失败: %w", err)
			}
			if err := decArtifacts.store.UseExternalDB(sqlDB); err != nil {
				return nil, fmt.Errorf("绑定决策日志存储失败: %w", err)
			}
		}
		if newStore == nil {
			if sharedGorm != nil {
				ns, err := sqlite.NewSqliteStoreFromDB(sharedGorm)
				if err != nil {
					return nil, fmt.Errorf("初始化 sqlite 存储失败: %w", err)
				}
				newStore = ns
			} else {
				ns, err := sqlite.NewSqliteStore(path)
				if err != nil {
					return nil, fmt.Errorf("初始化 sqlite 存储失败: %w", err)
				}
				newStore = ns
			}
		}
	}

	freqManager, err := b.freqManagerFn(cfg.Freqtrade, cfg.AI.ActiveHorizon, decArtifacts.store, liveStore, newStore, textNotifier)
	if err != nil {
		return nil, err
	}

	var profileMgr *profile.Manager
	if exporter, ok := ks.(store.SnapshotExporter); ok {
		pipeFactory := &factory.Factory{
			Exporter:     exporter,
			DefaultLimit: cfg.Kline.MaxCached,
		}
		profileMgr = profile.NewManager(profileLoader, pipeFactory, promptLoader)
	} else {
		logger.Warnf("K 线存储不支持快照导出，Pipeline 功能被禁用")
	}

	var exitRegistry *exitplan.Registry
	var planHandlers *exit.HandlerRegistry
	if path := strings.TrimSpace(cfg.AI.ExitPlanPath); path != "" {
		reg, err := exitplan.NewRegistry(path)
		if err != nil {
			return nil, fmt.Errorf("加载 exit plan 配置失败: %w", err)
		}
		exitRegistry = reg
		engine.ExitPlans = reg
	}
	planHandlers = exit.NewHandlerRegistry()
	exitHandlers.RegisterCoreHandlers(planHandlers)

	exitPromptIndex := promptkit.BuildPromptsFromCombos(collectProfileCombos(profileSnapshot))
	if len(exitPromptIndex) == 0 {
		return nil, fmt.Errorf("profile 未配置 exit_plan 组合，启动中止")
	}

	symbolDetails := collectSymbolDetails(profileSnapshot, exitRegistry)

	liveSvc := agent.NewLiveService(agent.LiveServiceParams{
		Config:          cfg,
		KlineStore:      ks,
		Updater:         updater,
		Engine:          engine,
		Telegram:        tgClient,
		DecisionLogs:    decArtifacts.store,
		Symbols:         syms,
		Intervals:       profileIntervals,
		HorizonName:     cfg.AI.ActiveHorizon,
		HorizonSummary:  hSummary,
		WarmupSummary:   warmupSummary,
		ExecManager:     freqManager,
		VisionReady:     visionReady,
		ProfileManager:  profileMgr,
		ExitPlans:       exitRegistry,
		PlanHandlers:    planHandlers,
		StrategyStore:   strategyStore,
		ExitPlanPrompts: exitPromptIndex,
	})

	var freqHandler livehttp.FreqtradeWebhookHandler
	if freqManager != nil {
		freqHandler = liveSvc
	}
	liveHTTPServe, err := b.liveHTTPFn(cfg.App, decArtifacts.store, freqHandler, syms, convertSymbolDetails(symbolDetails))
	if err != nil {
		return nil, err
	}

	var emaSummary EMASummary
	if metricsSvc != nil {
		emaSummary = EMASummary{
			TargetTimeframes: metricsSvc.GetTargetTimeframes(),
			BasePeriod:       metricsSvc.BaseOIHistoryPeriod(),
			HistoryLimit:     metricsSvc.OIHistoryLimit(),
		}
	}

	return &App{
		cfg:        cfg,
		live:       liveSvc,
		liveHTTP:   liveHTTPServe,
		metricsSvc: metricsSvc,
		Summary: &StartupSummary{
			KLine: KLineSummary{
				Symbols:   syms,
				Intervals: profileIntervals,
				MaxCached: cfg.Kline.MaxCached,
			},
			EMA:           emaSummary,
			Prompts:       pm.List(),
			SymbolDetails: symbolDetails,
		},
	}, nil
}

func enabledFinalModelIDs(cfg brcfg.AIConfig) ([]string, error) {
	models := cfg.MustResolveModelConfigs()
	ids := make([]string, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	for _, m := range models {
		if !m.Enabled || m.FinalDisabled {
			continue
		}
		id := strings.TrimSpace(m.ID)
		if id == "" {
			return nil, fmt.Errorf("ai.models.id 不能为空（enabled && !final_disabled 的模型必须配置 id）")
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

func validateProfilePrompts(snapshot cfgloader.ProfileSnapshot, loader profile.PromptLoader, modelIDs []string) error {
	if loader == nil {
		return fmt.Errorf("prompt loader 未初始化")
	}
	for name, def := range snapshot.Profiles {
		for _, modelID := range modelIDs {
			ref := strings.TrimSpace(def.Prompts.SystemByModel[modelID])
			if ref == "" {
				return fmt.Errorf("profile %s 缺少 prompts.system_by_model.%s 配置", name, modelID)
			}
			sys, err := loader.Load(ref)
			if err != nil {
				return fmt.Errorf("profile %s 加载 system prompt 失败 model=%s: %w", name, modelID, err)
			}
			if strings.TrimSpace(sys) == "" {
				return fmt.Errorf("profile %s 的 system prompt 内容为空 model=%s", name, modelID)
			}
		}
		user, err := loader.Load(def.Prompts.User)
		if err != nil {
			return fmt.Errorf("profile %s 加载 user prompt 失败: %w", name, err)
		}
		if strings.TrimSpace(user) == "" {
			return fmt.Errorf("profile %s 的 user prompt 内容为空", name)
		}
	}
	return nil
}

func collectProfileCombos(snapshot cfgloader.ProfileSnapshot) map[string]string {
	result := make(map[string]string)
	for _, def := range snapshot.Profiles {
		planID := "plan_combo_main"
		if len(def.ExitPlans.Allowed) > 0 {
			planID = strings.TrimSpace(def.ExitPlans.Allowed[0])
		}
		for _, combo := range def.ExitPlans.ComboKeys() {
			norm := promptkit.NormalizeComboKey(combo)
			if norm == "" {
				continue
			}
			if _, ok := result[norm]; ok {
				continue
			}
			result[norm] = planID
		}
	}
	return result
}

func convertSymbolDetails(src map[string]SymbolDetail) map[string]livehttp.SymbolDetail {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]livehttp.SymbolDetail, len(src))
	for sym, detail := range src {
		out[sym] = livehttp.SymbolDetail{
			Profile:      detail.ProfileName,
			Middlewares:  append([]string(nil), detail.Middlewares...),
			Strategies:   append([]string(nil), detail.Strategies...),
			ExitSummary:  detail.ExitSummary,
			ExitCombos:   append([]string(nil), detail.ExitCombos...),
			SystemPrompt: detail.SystemPrompt,
			UserPrompt:   detail.UserPrompt,
		}
	}
	return out
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
func WithMarketStack(fn func(context.Context, *brcfg.Config, []string, []string, map[string]int, []string) (*MarketStack, error)) AppBuilderOption {
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

// WithStorageOverrides allows tests to inject custom stores for strategy/live/state data.
func WithStorageOverrides(live database.LivePositionStore, strategy exit.StrategyStore, state store.Store) AppBuilderOption {
	return func(b *AppBuilder) {
		if live != nil {
			b.liveStoreOverride = live
		}
		if strategy != nil {
			b.strategyStoreOverride = strategy
		}
		if state != nil {
			b.newStoreOverride = state
		}
	}
}

// WithFreqManager overrides the freqtrade manager builder.
func WithFreqManager(fn func(brcfg.FreqtradeConfig, string, *database.DecisionLogStore, database.LivePositionStore, store.Store, notifier.TextNotifier) (*freqexec.Manager, error)) AppBuilderOption {
	return func(b *AppBuilder) {
		if fn != nil {
			b.freqManagerFn = fn
		}
	}
}

// WithLiveHTTP overrides the live HTTP server builder.
func WithLiveHTTP(fn func(brcfg.AppConfig, *database.DecisionLogStore, livehttp.FreqtradeWebhookHandler, []string, map[string]livehttp.SymbolDetail) (*livehttp.Server, error)) AppBuilderOption {
	return func(b *AppBuilder) {
		if fn != nil {
			b.liveHTTPFn = fn
		}
	}
}
