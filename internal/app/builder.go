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
	"brale/internal/market"
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

type AppBuilder struct {
	cfg *brcfg.Config

	promptManagerFn     func(string) (*strategy.Manager, error)
	marketStackFn       func(context.Context, *brcfg.Config, []string, []string, map[string]int, []string) (*MarketStack, error)
	modelProvidersFn    func(context.Context, brcfg.AIConfig, int) ([]provider.ModelProvider, map[string]bool, bool, error)
	decisionArtifactsFn func(context.Context, brcfg.AIConfig, *decision.DecisionEngine) (*decisionArtifacts, error)
	freqManagerFn       func(brcfg.FreqtradeConfig, string, *database.DecisionLogStore, database.LivePositionStore, store.Store, notifier.TextNotifier) (*freqexec.Manager, error)
	liveHTTPFn          func(brcfg.AppConfig, *database.DecisionLogStore, livehttp.FreqtradeWebhookHandler, []string, map[string]livehttp.SymbolDetail) (*livehttp.Server, error)

	liveStoreOverride     database.LivePositionStore
	strategyStoreOverride exit.StrategyStore
	newStoreOverride      store.Store
}

type AppBuilderOption func(*AppBuilder)

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

func (b *AppBuilder) Build(ctx context.Context) (*App, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if b.cfg == nil {
		return nil, fmt.Errorf("nil config")
	}
	cfg := b.cfg
	logger.SetLevel(cfg.App.LogLevel)

	profiles, err := b.loadProfileSetup(cfg)
	if err != nil {
		return nil, err
	}
	logger.Infof("✓ 已加载 %d 个交易对: %v", len(profiles.symbols), profiles.symbols)
	logger.Infof("✓ Profile 周期: %v", profiles.intervals)
	logger.Infof("[profiles]\n%s", profiles.summary)

	applyDefaultMultiAgentBlocks(cfg, len(profiles.symbols), len(profiles.intervals))

	pm, promptLoader, err := b.loadPromptSetup(cfg, profiles.snapshot)
	if err != nil {
		return nil, err
	}

	marketStack, err := b.marketStackFn(ctx, cfg, profiles.symbols, profiles.intervals, profiles.lookbacks, profiles.derivativeSymbols)
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
		Store:              ks,
		Intervals:          profiles.intervals,
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

	stores, err := b.resolveStores(cfg, decArtifacts)
	if err != nil {
		return nil, err
	}

	freqManager, err := b.freqManagerFn(cfg.Freqtrade, cfg.AI.ActiveHorizon, decArtifacts.store, stores.liveStore, stores.stateStore, textNotifier)
	if err != nil {
		return nil, err
	}

	profileMgr := b.buildProfileManager(cfg, profiles.loader, ks, promptLoader)

	exitRegistry, planHandlers, exitPromptIndex, symbolDetails, err := b.setupExitPlans(cfg, engine, profiles.snapshot)
	if err != nil {
		return nil, err
	}

	liveSvc := agent.NewLiveService(agent.LiveServiceParams{
		Config:          cfg,
		KlineStore:      ks,
		Updater:         updater,
		Engine:          engine,
		Telegram:        tgClient,
		DecisionLogs:    decArtifacts.store,
		Symbols:         profiles.symbols,
		Intervals:       profiles.intervals,
		HorizonName:     cfg.AI.ActiveHorizon,
		HorizonSummary:  profiles.summary,
		WarmupSummary:   warmupSummary,
		ExecManager:     freqManager,
		VisionReady:     visionReady,
		ProfileManager:  profileMgr,
		ExitPlans:       exitRegistry,
		PlanHandlers:    planHandlers,
		StrategyStore:   stores.strategyStore,
		ExitPlanPrompts: exitPromptIndex,
	})

	var freqHandler livehttp.FreqtradeWebhookHandler
	if freqManager != nil {
		freqHandler = liveSvc
	}
	liveHTTPServe, err := b.liveHTTPFn(cfg.App, decArtifacts.store, freqHandler, profiles.symbols, convertSymbolDetails(symbolDetails))
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
				Symbols:   profiles.symbols,
				Intervals: profiles.intervals,
				MaxCached: cfg.Kline.MaxCached,
			},
			EMA:           emaSummary,
			Prompts:       pm.List(),
			SymbolDetails: symbolDetails,
		},
	}, nil
}

type profileSetup struct {
	loader            *cfgloader.ProfileLoader
	snapshot          cfgloader.ProfileSnapshot
	symbols           []string
	intervals         []string
	lookbacks         map[string]int
	derivativeSymbols []string
	summary           string
}

func (b *AppBuilder) loadProfileSetup(cfg *brcfg.Config) (profileSetup, error) {
	if strings.TrimSpace(cfg.AI.ProfilesPath) == "" {
		return profileSetup{}, fmt.Errorf("ai.profiles_path 未配置，无法加载 profile")
	}
	profileLoader, err := cfgloader.NewProfileLoader(cfg.AI.ProfilesPath)
	if err != nil {
		return profileSetup{}, fmt.Errorf("加载 profile 配置失败: %w", err)
	}
	snapshot := profileLoader.Snapshot()
	syms, intervals, lookbacks, derivativeSymbols, err := collectProfileUniverse(snapshot, cfg.Kline.MaxCached)
	if err != nil {
		return profileSetup{}, err
	}
	return profileSetup{
		loader:            profileLoader,
		snapshot:          snapshot,
		symbols:           syms,
		intervals:         intervals,
		lookbacks:         lookbacks,
		derivativeSymbols: derivativeSymbols,
		summary:           formatProfileSummary(syms, intervals),
	}, nil
}

func (b *AppBuilder) loadPromptSetup(cfg *brcfg.Config, snapshot cfgloader.ProfileSnapshot) (*strategy.Manager, profile.PromptLoader, error) {
	pm, err := b.promptManagerFn(cfg.Prompt.Dir)
	if err != nil {
		return nil, nil, fmt.Errorf("加载提示词模板失败: %w", err)
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
		return nil, nil, err
	}
	if err := validateProfilePrompts(snapshot, promptLoader, requiredModelIDs); err != nil {
		return nil, nil, err
	}
	return pm, promptLoader, nil
}

type storeSetup struct {
	strategyStore exit.StrategyStore
	liveStore     database.LivePositionStore
	stateStore    store.Store
	sharedGorm    *gorm.DB
}

func (b *AppBuilder) resolveStores(cfg *brcfg.Config, decArtifacts *decisionArtifacts) (storeSetup, error) {
	out := applyStoreOverrides(b)
	if out.strategyStore != nil && out.liveStore != nil && out.stateStore != nil {
		return out, nil
	}

	path := strings.TrimSpace(cfg.AI.DecisionLogPath)
	if path == "" {
		return storeSetup{}, fmt.Errorf("ai.decision_log_path 未配置，无法初始化存储")
	}
	gormStore, err := gormstore.NewGormStore(path)
	if err != nil {
		return storeSetup{}, fmt.Errorf("初始化 gorm 存储失败: %w", err)
	}
	out.strategyStore = gormStore
	out.liveStore = gormStore
	out.sharedGorm = gormStore.GormDB()

	if err := attachDecisionLogDB(gormStore, decArtifacts); err != nil {
		return storeSetup{}, err
	}

	if out.stateStore == nil {
		ns, err := initStateStore(path, out.sharedGorm)
		if err != nil {
			return storeSetup{}, err
		}
		out.stateStore = ns
	}
	return out, nil
}

func applyStoreOverrides(b *AppBuilder) storeSetup {
	var out storeSetup
	if b == nil {
		return out
	}
	if b.strategyStoreOverride != nil {
		out.strategyStore = b.strategyStoreOverride
	}
	if b.liveStoreOverride != nil {
		out.liveStore = b.liveStoreOverride
	} else if out.strategyStore != nil {
		if ls, ok := out.strategyStore.(database.LivePositionStore); ok {
			out.liveStore = ls
		}
	}
	if b.newStoreOverride != nil {
		out.stateStore = b.newStoreOverride
	}
	return out
}

func attachDecisionLogDB(gormStore *gormstore.GormStore, decArtifacts *decisionArtifacts) error {
	if decArtifacts == nil || decArtifacts.store == nil || gormStore == nil {
		return nil
	}
	sqlDB, err := gormStore.SQLDB()
	if err != nil {
		return fmt.Errorf("获取 SQL DB 失败: %w", err)
	}
	if err := decArtifacts.store.UseExternalDB(sqlDB); err != nil {
		return fmt.Errorf("绑定决策日志存储失败: %w", err)
	}
	return nil
}

func initStateStore(path string, shared *gorm.DB) (store.Store, error) {
	if shared != nil {
		ns, err := sqlite.NewSqliteStoreFromDB(shared)
		if err != nil {
			return nil, fmt.Errorf("初始化 sqlite 存储失败: %w", err)
		}
		return ns, nil
	}
	ns, err := sqlite.NewSqliteStore(path)
	if err != nil {
		return nil, fmt.Errorf("初始化 sqlite 存储失败: %w", err)
	}
	return ns, nil
}

func (b *AppBuilder) buildProfileManager(cfg *brcfg.Config, loader *cfgloader.ProfileLoader, ks market.KlineStore, promptLoader profile.PromptLoader) *profile.Manager {
	exporter, ok := ks.(store.SnapshotExporter)
	if !ok {
		logger.Warnf("K 线存储不支持快照导出，Pipeline 功能被禁用")
		return nil
	}
	pipeFactory := &factory.Factory{Exporter: exporter, DefaultLimit: cfg.Kline.MaxCached}
	return profile.NewManager(loader, pipeFactory, promptLoader)
}

func (b *AppBuilder) setupExitPlans(cfg *brcfg.Config, engine *decision.DecisionEngine, snapshot cfgloader.ProfileSnapshot) (*exitplan.Registry, *exit.HandlerRegistry, map[string]promptkit.ExitPlanPrompt, map[string]SymbolDetail, error) {
	var exitRegistry *exitplan.Registry
	if path := strings.TrimSpace(cfg.AI.ExitPlanPath); path != "" {
		reg, err := exitplan.NewRegistry(path)
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("加载 exit plan 配置失败: %w", err)
		}
		exitRegistry = reg
		engine.ExitPlans = reg
	}
	planHandlers := exit.NewHandlerRegistry()
	exitHandlers.RegisterCoreHandlers(planHandlers)

	exitPromptIndex := promptkit.BuildPromptsFromCombos(collectProfileCombos(snapshot))
	if len(exitPromptIndex) == 0 {
		return nil, nil, nil, nil, fmt.Errorf("profile 未配置 exit_plan 组合，启动中止")
	}
	symbolDetails := collectSymbolDetails(snapshot, exitRegistry)
	return exitRegistry, planHandlers, exitPromptIndex, symbolDetails, nil
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

func WithPromptManager(fn func(string) (*strategy.Manager, error)) AppBuilderOption {
	return func(b *AppBuilder) {
		if fn != nil {
			b.promptManagerFn = fn
		}
	}
}

func WithMarketStack(fn func(context.Context, *brcfg.Config, []string, []string, map[string]int, []string) (*MarketStack, error)) AppBuilderOption {
	return func(b *AppBuilder) {
		if fn != nil {
			b.marketStackFn = fn
		}
	}
}

func WithModelProviders(fn func(context.Context, brcfg.AIConfig, int) ([]provider.ModelProvider, map[string]bool, bool, error)) AppBuilderOption {
	return func(b *AppBuilder) {
		if fn != nil {
			b.modelProvidersFn = fn
		}
	}
}

func WithDecisionArtifacts(fn func(context.Context, brcfg.AIConfig, *decision.DecisionEngine) (*decisionArtifacts, error)) AppBuilderOption {
	return func(b *AppBuilder) {
		if fn != nil {
			b.decisionArtifactsFn = fn
		}
	}
}

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

func WithFreqManager(fn func(brcfg.FreqtradeConfig, string, *database.DecisionLogStore, database.LivePositionStore, store.Store, notifier.TextNotifier) (*freqexec.Manager, error)) AppBuilderOption {
	return func(b *AppBuilder) {
		if fn != nil {
			b.freqManagerFn = fn
		}
	}
}

func WithLiveHTTP(fn func(brcfg.AppConfig, *database.DecisionLogStore, livehttp.FreqtradeWebhookHandler, []string, map[string]livehttp.SymbolDetail) (*livehttp.Server, error)) AppBuilderOption {
	return func(b *AppBuilder) {
		if fn != nil {
			b.liveHTTPFn = fn
		}
	}
}
