package app

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"brale/internal/analysis/visual"
	brcfg "brale/internal/config"
	"brale/internal/decision"
	"brale/internal/gateway/database"
	"brale/internal/gateway/provider"
	"brale/internal/logger"
	"brale/internal/market"
	"brale/internal/strategy"
)

type engineConfig struct {
	Providers          []provider.ModelProvider
	Aggregator         decision.Aggregator
	PromptMgr          *strategy.Manager
	Store              market.KlineStore
	Intervals          []string
	HorizonName        string
	MultiAgent         brcfg.MultiAgentConfig
	ProviderPreference []string
	FinalDisabled      map[string]bool
	LogEachModel       bool
	Metrics            *market.MetricsService
	TimeoutSeconds     int
}

type decisionArtifacts struct {
	store *database.DecisionLogStore
}

func buildDecisionArtifacts(ctx context.Context, cfg brcfg.AIConfig, engine *decision.DecisionEngine) (*decisionArtifacts, error) {
	artifacts := &decisionArtifacts{}
	if strings.TrimSpace(cfg.DecisionLogPath) == "" {
		return artifacts, nil
	}
	store, err := database.NewDecisionLogStore(cfg.DecisionLogPath)
	if err != nil {
		return nil, fmt.Errorf("初始化决策日志存储失败: %w", err)
	}
	artifacts.store = store
	if engine != nil {
		if obs := database.NewDecisionLogObserver(store); obs != nil {
			engine.Observer = obs
		}
		engine.AgentHistory = store
	}
	logPath := cfg.DecisionLogPath
	if abs, err := filepath.Abs(logPath); err == nil {
		logPath = abs
	}
	logger.Infof("✓ 实盘决策日志写入 %s", logPath)
	return artifacts, nil
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

func buildModelProviders(ctx context.Context, cfg brcfg.AIConfig, timeoutSeconds int) ([]provider.ModelProvider, map[string]bool, bool, error) {
	var (
		modelCfgs   []provider.ModelCfg
		visionReady bool
	)
	finalDisabled := make(map[string]bool)
	for _, m := range cfg.MustResolveModelConfigs() {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			provider := strings.TrimSpace(m.Provider)
			if provider == "" {
				provider = "provider"
			}
			if model := strings.TrimSpace(m.Model); model != "" {
				id = provider + ":" + model
			} else {
				id = provider
			}
			logger.Warnf("未配置 ai.models.id，已为 %q 生成 ID: %s", m.Provider, id)
		}
		modelCfgs = append(modelCfgs, provider.ModelCfg{
			ID:             id,
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
		if m.FinalDisabled {
			finalDisabled[id] = true
		}
	}
	if visionReady {
		if err := visual.EnsureHeadlessAvailable(ctx); err != nil {
			return nil, nil, false, fmt.Errorf("初始化可视化渲染失败(请安装 headless Chrome): %w", err)
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
	return providers, finalDisabled, visionReady, nil
}

func buildDecisionEngine(cfg engineConfig) *decision.DecisionEngine {
	agg := cfg.Aggregator
	if agg == nil {
		agg = decision.FirstWinsAggregator{}
	}
	var finalDisabled map[string]bool
	if len(cfg.FinalDisabled) > 0 {
		finalDisabled = make(map[string]bool, len(cfg.FinalDisabled))
		for k, v := range cfg.FinalDisabled {
			if v {
				finalDisabled[k] = true
			}
		}
	}
	engine := &decision.DecisionEngine{
		Providers:          cfg.Providers,
		Agg:                agg,
		PromptMgr:          cfg.PromptMgr,
		KStore:             cfg.Store,
		Intervals:          append([]string(nil), cfg.Intervals...),
		HorizonName:        cfg.HorizonName,
		MultiAgent:         cfg.MultiAgent,
		ProviderPreference: append([]string(nil), cfg.ProviderPreference...),
		FinalDisabled:      finalDisabled,
		Parallel:           true,
		LogEachModel:       cfg.LogEachModel,
		TimeoutSeconds:     cfg.TimeoutSeconds,
	}
	engine.PromptBuilder = decision.NewDefaultPromptBuilder(cfg.PromptMgr, cfg.Metrics, cfg.Intervals, cfg.LogEachModel)
	return engine
}

func applyDefaultMultiAgentBlocks(cfg *brcfg.Config, symbolCount, intervalCount int) {
	if cfg == nil {
		return
	}
	if cfg.AI.MultiAgent.MaxBlocks > 0 {
		return
	}
	auto := symbolCount * intervalCount
	if auto <= 0 {
		if intervalCount > 0 {
			auto = intervalCount
		} else if symbolCount > 0 {
			auto = symbolCount
		} else {
			auto = 4
		}
	}
	cfg.AI.MultiAgent.MaxBlocks = auto
	logger.Infof("✓ Multi-Agent max_blocks 未配置，自动使用 %d（%d 个币种 × %d 个周期）", auto, symbolCount, intervalCount)
}
