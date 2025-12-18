package app

import (
	"context"
	"fmt"
	"sort"
	"strings"

	brcfg "brale/internal/config"
	cfgloader "brale/internal/config/loader"
	"brale/internal/exitplan"
	"brale/internal/gateway"
	"brale/internal/logger"
	"brale/internal/market"
	"brale/internal/pkg/maputil"
	"brale/internal/store"
)

const defaultIndicatorLookback = 240

type MarketStack struct {
	Store         market.KlineStore
	Updater       *market.WSUpdater
	Metrics       *market.MetricsService
	WarmupSummary string
}

func buildMarketStack(ctx context.Context, cfg *brcfg.Config, symbols []string, intervals []string, lookbacks map[string]int, metricsSymbols []string) (*MarketStack, error) {
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

	preheater := market.NewPreheater(kstore, cfg.Kline.MaxCached, src)
	preheater.Warmup(ctx, symbols, lookbacks)
	preheater.Preheat(ctx, symbols, intervals, cfg.Kline.MaxCached)
	logger.Infof("✓ Warmup 完成，最小条数=%v", lookbacks)
	warmupSummary := fmt.Sprintf("*Warmup 完成*\n```\n%v\n```", lookbacks)

	metricsSvc, err := market.NewMetricsService(src, metricsSymbols, intervals)
	if err != nil {
		return nil, fmt.Errorf("初始化 MetricsService 失败: %w", err)
	}
	if metricsSvc == nil {
		logger.Infof("✓ MetricsService 未启用（profile 未请求衍生品指标）")
	} else {
		logger.Infof("✓ MetricsService 初始化成功")
	}

	success = true
	return &MarketStack{
		Store:         kstore,
		Updater:       updater,
		Metrics:       metricsSvc,
		WarmupSummary: warmupSummary,
	}, nil
}

func collectProfileUniverse(snapshot cfgloader.ProfileSnapshot, defaultLimit int) ([]string, []string, map[string]int, []string, error) {
	if len(snapshot.Profiles) == 0 {
		return nil, nil, nil, nil, fmt.Errorf("profile 配置为空")
	}
	if defaultLimit <= 0 {
		defaultLimit = 200
	}
	symbolSet := make(map[string]struct{})
	intervalSet := make(map[string]struct{})
	lookbacks := make(map[string]int)
	derivativeSet := make(map[string]struct{})

	for name, def := range snapshot.Profiles {
		if err := validateProfileDef(name, def); err != nil {
			return nil, nil, nil, nil, err
		}

		if err := collectTargets(def, symbolSet, derivativeSet); err != nil {
			return nil, nil, nil, nil, fmt.Errorf("profile %s: %w", name, err)
		}

		needBars := estimateProfileLookback(def) + 20
		if err := collectIntervals(def, needBars, intervalSet, lookbacks); err != nil {
			return nil, nil, nil, nil, fmt.Errorf("profile %s: %w", name, err)
		}

		if err := collectMiddlewareNeeds(name, def, intervalSet, lookbacks); err != nil {
			return nil, nil, nil, nil, fmt.Errorf("profile %s middleware: %w", name, err)
		}
	}

	if len(symbolSet) == 0 {
		return nil, nil, nil, nil, fmt.Errorf("profile 未配置任何交易对")
	}
	if len(intervalSet) == 0 {
		return nil, nil, nil, nil, fmt.Errorf("profile 未配置任何周期")
	}

	symbols := setToSortedSlice(symbolSet)
	intervals := setToSortedSlice(intervalSet)
	if len(lookbacks) == 0 {
		lookbacks = make(map[string]int, len(intervals))
	}
	for _, iv := range intervals {
		if lookbacks[iv] <= 0 {
			lookbacks[iv] = defaultLimit
		}
	}
	derivatives := setToSortedSlice(derivativeSet)
	return symbols, intervals, lookbacks, derivatives, nil
}

func validateProfileDef(name string, def cfgloader.ProfileDefinition) error {
	if def.AnalysisSlice <= 0 {
		return fmt.Errorf("profile %s 缺少 analysis_slice 配置", name)
	}
	if def.SliceDropTail < 0 {
		return fmt.Errorf("profile %s.slice_drop_tail 需 >= 0", name)
	}
	if len(def.IntervalsLower()) == 0 {
		return fmt.Errorf("profile %s 缺少 intervals 配置", name)
	}
	if len(def.TargetsUpper()) == 0 {
		return fmt.Errorf("profile %s 缺少 targets 配置", name)
	}
	if estimateProfileLookback(def) <= 0 {
		return fmt.Errorf("profile %s 缺少有效的分析窗口", name)
	}
	// Mandatory prompts validation
	if strings.TrimSpace(def.Prompts.User) == "" {
		return fmt.Errorf("profile %s 缺少 prompts.user 配置，必须为每个 profile 指定 user prompt", name)
	}
	if len(def.Prompts.SystemByModel) == 0 {
		return fmt.Errorf("profile %s 缺少 prompts.system_by_model 配置，必须为每个启用模型指定 system prompt", name)
	}
	return nil
}

func collectTargets(def cfgloader.ProfileDefinition, symbolSet, derivativeSet map[string]struct{}) error {
	for _, sym := range def.TargetsUpper() {
		symbol := strings.ToUpper(sym)
		if symbol == "" {
			return fmt.Errorf("targets 包含空 symbol")
		}
		symbolSet[symbol] = struct{}{}
		if def.DerivativesEnabled() {
			derivativeSet[symbol] = struct{}{}
		}
	}
	return nil
}

func collectIntervals(def cfgloader.ProfileDefinition, needBars int, intervalSet map[string]struct{}, lookbacks map[string]int) error {
	for _, iv := range def.IntervalsLower() {
		norm := strings.ToLower(strings.TrimSpace(iv))
		if norm == "" {
			return fmt.Errorf("intervals 含空值")
		}
		if !brcfg.IsValidInterval(norm) {
			return fmt.Errorf("intervals 包含无效格式: %s", norm)
		}
		intervalSet[norm] = struct{}{}
		if needBars > lookbacks[norm] {
			lookbacks[norm] = needBars
		}
	}
	return nil
}

func collectMiddlewareNeeds(name string, def cfgloader.ProfileDefinition, intervalSet map[string]struct{}, lookbacks map[string]int) error {
	ints := def.IntervalsLower()
	for _, mw := range def.Middlewares {
		switch strings.ToLower(strings.TrimSpace(mw.Name)) {
		case "", "kline_fetcher":
			klineIntervals := maputil.StringSlice(mw.Params, "intervals")
			if len(klineIntervals) == 0 {
				klineIntervals = ints
			}
			if len(klineIntervals) == 0 {
				return fmt.Errorf("kline_fetcher 缺少 intervals")
			}
			limit := maputil.Int(mw.Params, "limit")
			if limit <= 0 {
				return fmt.Errorf("kline_fetcher 缺少 limit")
			}
			for _, iv := range klineIntervals {
				norm := strings.ToLower(strings.TrimSpace(iv))
				if norm == "" {
					return fmt.Errorf("kline_fetcher 含空 interval")
				}
				if !brcfg.IsValidInterval(norm) {
					return fmt.Errorf("kline_fetcher 包含无效 interval: %s", norm)
				}
				intervalSet[norm] = struct{}{}
				if limit > lookbacks[norm] {
					lookbacks[norm] = limit
				}
			}
		case "ema_trend", "rsi_extreme", "macd_trend":
			interval := strings.ToLower(strings.TrimSpace(maputil.String(mw.Params, "interval")))
			if interval == "" {
				return fmt.Errorf("%s 缺少 interval", mw.Name)
			}
			if !brcfg.IsValidInterval(interval) {
				return fmt.Errorf("%s interval 格式无效: %s", mw.Name, interval)
			}
			intervalSet[interval] = struct{}{}

			// Specific validations
			if mw.Name == "ema_trend" {
				if maputil.Int(mw.Params, "fast") <= 0 || maputil.Int(mw.Params, "mid") <= 0 || maputil.Int(mw.Params, "slow") <= 0 {
					return fmt.Errorf("ema_trend 需设置 fast/mid/slow")
				}
			} else if mw.Name == "rsi_extreme" {
				if maputil.Int(mw.Params, "period") <= 0 {
					return fmt.Errorf("rsi_extreme 缺少 period")
				}
				if maputil.Float(mw.Params, "overbought") == 0 || maputil.Float(mw.Params, "oversold") == 0 {
					return fmt.Errorf("rsi_extreme 需设置 overbought/oversold")
				}
			} else if mw.Name == "macd_trend" {
				if maputil.Int(mw.Params, "fast") <= 0 || maputil.Int(mw.Params, "slow") <= 0 || maputil.Int(mw.Params, "signal") <= 0 {
					return fmt.Errorf("macd_trend 需设置 fast/slow/signal")
				}
			}
		}
	}
	return nil
}

func estimateProfileLookback(def cfgloader.ProfileDefinition) int {
	need := def.AnalysisSlice + def.SliceDropTail
	if need < defaultIndicatorLookback {
		need = defaultIndicatorLookback
	}
	return need
}

func logProfileMiddlewares(snapshot cfgloader.ProfileSnapshot) {
	if len(snapshot.Profiles) == 0 {
		return
	}
	names := make([]string, 0, len(snapshot.Profiles))
	for name := range snapshot.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		def := snapshot.Profiles[name]
		logger.Infof("[profile %s] middlewares:", name)
		if len(def.Middlewares) == 0 {
			logger.Infof("  (none)")
			continue
		}
		for _, mw := range def.Middlewares {
			params := formatMiddlewareParams(mw.Params)
			logger.Infof("  - %s stage=%d critical=%v timeout=%ds params=%s", strings.TrimSpace(mw.Name), mw.Stage, mw.Critical, mw.TimeoutSeconds, params)
		}
	}
}

func collectSymbolDetails(snapshot cfgloader.ProfileSnapshot, exitReg *exitplan.Registry) map[string]SymbolDetail {
	out := make(map[string]SymbolDetail)

	for name, def := range snapshot.Profiles {
		// Middlewares
		mws := summarizeMiddlewares(def.Middlewares)

		// Strategies
		strategies, exitSummary, exitCombos := summarizeExitPlans(def.ExitPlans.ComboKeys(), exitReg)

		// Assign to each target symbol
		sysPrompt := summarizeSystemPromptRefs(def.Prompts.SystemByModel)
		userPrompt := strings.TrimSuffix(def.Prompts.User, ".txt")

		for _, sym := range def.TargetsUpper() {
			out[sym] = SymbolDetail{
				ProfileName:  name,
				Middlewares:  mws,
				Strategies:   strategies,
				ExitSummary:  exitSummary,
				ExitCombos:   exitCombos,
				SystemPrompt: sysPrompt,
				UserPrompt:   userPrompt,
			}
		}
	}
	return out
}

func summarizeSystemPromptRefs(refs map[string]string) string {
	if len(refs) == 0 {
		return ""
	}
	keys := make([]string, 0, len(refs))
	for modelID := range refs {
		modelID = strings.TrimSpace(modelID)
		if modelID == "" {
			continue
		}
		keys = append(keys, modelID)
	}
	if len(keys) == 0 {
		return ""
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, modelID := range keys {
		ref := strings.TrimSpace(refs[modelID])
		ref = strings.TrimSuffix(ref, ".txt")
		if ref == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s:%s", modelID, ref))
	}
	return strings.Join(parts, ", ")
}

func setToSortedSlice(set map[string]struct{}) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func summarizeMiddlewares(list []cfgloader.MiddlewareConfig) []string {
	if len(list) == 0 {
		return nil
	}
	type key struct {
		name   string
		params string
	}
	groups := make(map[key][]string)

	for _, mw := range list {
		baseParams := cloneParams(mw.Params)
		delete(baseParams, "interval")
		paramsText := formatMiddlewareParams(baseParams)
		interval := ""
		if v, ok := mw.Params["interval"]; ok {
			interval = fmt.Sprint(v)
		}
		k := key{name: strings.TrimSpace(mw.Name), params: paramsText}
		groups[k] = append(groups[k], interval)
	}

	var out []string
	for k, intervals := range groups {
		label := k.name
		iv := dedupAndSort(intervals)
		if len(iv) > 0 {
			label += fmt.Sprintf(" intervals: %s", strings.Join(iv, "/"))
		}
		if k.params != "" && k.params != "{}" {
			label += fmt.Sprintf(" · %s", k.params)
		}
		out = append(out, label)
	}
	sort.Strings(out)
	return out
}

func formatMiddlewareParams(params map[string]interface{}) string {
	if len(params) == 0 {
		return "{}"
	}
	ordered := make([]string, 0, len(params))
	for k := range params {
		ordered = append(ordered, k)
	}
	sort.Strings(ordered)
	parts := make([]string, 0, len(ordered))
	for _, k := range ordered {
		parts = append(parts, fmt.Sprintf("%s=%v", k, params[k]))
	}
	return strings.Join(parts, ", ")
}

// cloneParams 做一份浅拷贝，避免修改原始配置。
func cloneParams(src map[string]interface{}) map[string]interface{} {
	if len(src) == 0 {
		return map[string]interface{}{}
	}
	dst := make(map[string]interface{}, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

func dedupAndSort(list []string) []string {
	set := make(map[string]struct{})
	for _, v := range list {
		trim := strings.TrimSpace(v)
		if trim == "" {
			continue
		}
		set[trim] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func summarizeExitPlans(comboKeys []string, exitReg *exitplan.Registry) (strategies []string, exitSummary string, exitCombos []string) {
	if len(comboKeys) == 0 {
		return []string{"plan_combo_main: 通用组合模板"}, "通用组合模板", nil
	}
	labels := make([]string, 0, len(comboKeys))
	for _, key := range comboKeys {
		label := comboLabel(key, exitReg)
		labels = append(labels, label)
		strategies = append(strategies, label)
		exitCombos = append(exitCombos, key)
	}
	exitSummary = strings.Join(labels, " / ")
	return
}

func comboLabel(key string, exitReg *exitplan.Registry) string {
	switch strings.ToLower(key) {
	case "tp_tiers__sl_single":
		return "分段止盈 + 固定止损"
	case "tp_tiers__sl_tiers":
		return "分段止盈 + 分段止损"
	case "tp_single__sl_single":
		return "单止盈 + 单止损"
	case "tp_atr__sl_atr":
		return "ATR 追踪止盈 + ATR 追踪止损"
	case "sl_atr__tp_tiers":
		return "ATR 止损 + 分段止盈"
	case "tp_atr__sl_tiers":
		return "ATR 止盈 + 分段止损"
	case "sl_atr__tp_single":
		return "ATR 止损 + 单止盈"
	case "tp_atr__sl_single":
		return "ATR 止盈 + 单止损"
	default:
		if exitReg != nil {
			if tpl, ok := exitReg.Template(key); ok && tpl.Description != "" {
				return tpl.Description
			}
		}
		return key
	}
}
