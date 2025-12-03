package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
)

const (
	defaultAppEnv            = "dev"                                          // config: app.env
	defaultAppLogLevel       = "info"                                         // config: app.log_level
	defaultAppHTTPAddr       = ":9991"                                        // config: app.http_addr
	defaultAppLogPath        = "/data/logs/brale-live.log"                    // config: app.log_path
	defaultAppLLMLogPath     = "/data/logs/brale-llm.log"                     // config: app.llm_log_path
	defaultSymbolsProvider   = "default"                                      // config: symbols.provider
	defaultKlineMaxCached    = 300                                            // config: kline.max_cached
	defaultMarketName        = "binance"                                      // config: market.sources.name
	defaultMarketREST        = "https://fapi.binance.com"                     // config: market.sources.rest_base_url
	defaultAIAggregation     = "meta"                                         // config: ai.aggregation
	defaultAIDecisionLog     = "/data/live/decisions.db"                      // config: ai.decision_log_path
	defaultAIDecisionAge     = 3600                                           // config: ai.last_decision_max_age_seconds
	defaultAIDecisionEvery   = 600                                            // config: ai.decision_interval_seconds
	defaultAIMultiBlocks     = 4                                              // config: ai.multi_agent.max_blocks
	defaultMCPTimeout        = 120                                            // config: mcp.timeout_seconds
	defaultFreqtradeAPI      = "http://freqtrade:8080/api/v1"                 // config: freqtrade.api_url
	defaultFreqtradeStake    = 100                                            // config: freqtrade.default_stake_usd
	defaultFreqtradeLev      = 1                                              // config: freqtrade.default_leverage
	defaultFreqtradeTimeout  = 15                                             // config: freqtrade.timeout_seconds
	defaultFreqtradeWebhook  = "http://brale:9991/api/live/freqtrade/webhook" // config: freqtrade.webhook_url
	defaultFreqtradeRiskDB   = "/data/db/trade_risk.db"                       // config: freqtrade.risk_store_path
	defaultAdvancedLiquidity = 15                                             // config: advanced.liquidity_filter_usd_m
	defaultAdvancedRR        = 1                                              // config: advanced.min_risk_reward
	defaultAdvancedCooldown  = 180                                            // config: advanced.open_cooldown_seconds
	defaultAdvancedMaxOpen   = 3                                              // config: advanced.max_opens_per_cycle
	defaultTradingMode       = "static"                                       // config: trading.mode
	defaultTradingMaxPct     = 0.01                                           // config: trading.max_position_pct
	defaultTradingLeverage   = 10                                             // config: trading.default_leverage
)

// Config 是 Brale 的主配置载体。
type Config struct {
	App       AppConfig       `toml:"app"`
	Symbols   SymbolsConfig   `toml:"symbols"`
	Kline     KlineConfig     `toml:"kline"`
	Market    MarketConfig    `toml:"market"`
	AI        AIConfig        `toml:"ai"`
	MCP       MCPConfig       `toml:"mcp"`
	Prompt    PromptConfig    `toml:"prompt"`
	Notify    NotifyConfig    `toml:"notify"`
	Freqtrade FreqtradeConfig `toml:"freqtrade"`
	Advanced  AdvancedConfig  `toml:"advanced"`
	Trading   TradingConfig   `toml:"trading"`
}

type AppConfig struct {
	Env      string `toml:"env"`
	LogLevel string `toml:"log_level"`
	HTTPAddr string `toml:"http_addr"`
	LogPath  string `toml:"log_path"`
	LLMLog   string `toml:"llm_log_path"`
	LLMDump  bool   `toml:"llm_dump_payload"`
}

func (a *AppConfig) applyDefaults(keys keySet) {
	if a == nil {
		return
	}
	applyFieldDefaults(keys,
		stringFieldDefault("app.env", &a.Env, defaultAppEnv),
		stringFieldDefault("app.log_level", &a.LogLevel, defaultAppLogLevel),
		stringFieldDefault("app.http_addr", &a.HTTPAddr, defaultAppHTTPAddr),
		stringFieldDefault("app.log_path", &a.LogPath, defaultAppLogPath),
		stringFieldDefault("app.llm_log_path", &a.LLMLog, defaultAppLLMLogPath),
	)
}

type SymbolsConfig struct {
	Provider    string   `toml:"provider"`
	DefaultList []string `toml:"default_list"`
	APIURL      string   `toml:"api_url"`
}

func (s *SymbolsConfig) applyDefaults(keys keySet) {
	if s == nil {
		return
	}
	applyFieldDefaults(keys,
		stringFieldDefault("symbols.provider", &s.Provider, defaultSymbolsProvider),
	)
}

type KlineConfig struct {
	MaxCached int `toml:"max_cached"`
}

func (k *KlineConfig) applyDefaults(keys keySet) {
	if k == nil {
		return
	}
	applyFieldDefaults(keys,
		fieldDefault{
			key:   "kline.max_cached",
			need:  func() bool { return k.MaxCached <= 0 },
			apply: func() { k.MaxCached = defaultKlineMaxCached },
		},
	)
}

type MCPConfig struct {
	TimeoutSeconds int `toml:"timeout_seconds"`
}

func (m *MCPConfig) applyDefaults(keys keySet) {
	if m == nil {
		return
	}
	applyFieldDefaults(keys,
		fieldDefault{
			key:   "mcp.timeout_seconds",
			need:  func() bool { return m.TimeoutSeconds <= 0 },
			apply: func() { m.TimeoutSeconds = defaultMCPTimeout },
		},
	)
}

type PromptConfig struct {
	Dir            string `toml:"dir"`
	SystemTemplate string `toml:"system_template"`
}

func (p *PromptConfig) applyDefaults(keys keySet) {
	if p == nil {
		return
	}
	applyFieldDefaults(keys,
		stringFieldDefault("prompt.dir", &p.Dir, "prompts"),
		stringFieldDefault("prompt.system_template", &p.SystemTemplate, "default"),
	)
}

type NotifyConfig struct {
	Telegram TelegramConfig `toml:"telegram"`
}

type TelegramConfig struct {
	Enabled  bool   `toml:"enabled"`
	BotToken string `toml:"bot_token"`
	ChatID   string `toml:"chat_id"`
}

type AdvancedConfig struct {
	LiquidityFilterUSDM int     `toml:"liquidity_filter_usd_m"`
	MinRiskReward       float64 `toml:"min_risk_reward"`
	OpenCooldownSeconds int     `toml:"open_cooldown_seconds"`
	MaxOpensPerCycle    int     `toml:"max_opens_per_cycle"`
	TierMinDistancePct  float64 `toml:"tier_min_distance_pct"`
}

func (a *AdvancedConfig) applyDefaults() {
	if a == nil {
		return
	}
	if a.LiquidityFilterUSDM <= 0 {
		a.LiquidityFilterUSDM = defaultAdvancedLiquidity // config: advanced.liquidity_filter_usd_m
	}
	if a.MinRiskReward <= 0 {
		a.MinRiskReward = defaultAdvancedRR // config: advanced.min_risk_reward
	}
	if a.OpenCooldownSeconds <= 0 {
		a.OpenCooldownSeconds = defaultAdvancedCooldown // config: advanced.open_cooldown_seconds
	}
	if a.MaxOpensPerCycle <= 0 {
		a.MaxOpensPerCycle = defaultAdvancedMaxOpen // config: advanced.max_opens_per_cycle
	}
	if a.TierMinDistancePct <= 0 {
		a.TierMinDistancePct = 0.002
	}
}

// TradingConfig 控制模拟/实盘资金来源与默认仓位策略。
type TradingConfig struct {
	Mode               string  `toml:"mode"`                 // "static" | 后续扩展 "live"
	MaxPositionPct     float64 `toml:"max_position_pct"`     // 单笔最大占用比例 0~1
	DefaultPositionUSD float64 `toml:"default_position_usd"` // 若>0，直接作为 fallback 仓位
	DefaultLeverage    int     `toml:"default_leverage"`     // 缺省杠杆
}

func (t *TradingConfig) applyDefaults() {
	if t == nil {
		return
	}
	if strings.TrimSpace(t.Mode) == "" {
		t.Mode = defaultTradingMode // config: trading.mode
	}
	if t.MaxPositionPct <= 0 || t.MaxPositionPct > 1 {
		t.MaxPositionPct = defaultTradingMaxPct // config: trading.max_position_pct
	}
	if t.DefaultLeverage <= 0 {
		t.DefaultLeverage = defaultTradingLeverage // config: trading.default_leverage
	}
	if t.DefaultPositionUSD < 0 {
		t.DefaultPositionUSD = 0
	}
}

// FreqtradeConfig 描述外部执行引擎的访问方式。
type FreqtradeConfig struct {
	Enabled            bool    `toml:"enabled"`
	APIURL             string  `toml:"api_url"`
	Username           string  `toml:"username"`
	Password           string  `toml:"password"`
	APIToken           string  `toml:"api_token"`
	DefaultStakeUSD    float64 `toml:"default_stake_usd"`
	DefaultLeverage    int     `toml:"default_leverage"`
	TimeoutSeconds     int     `toml:"timeout_seconds"`
	InsecureSkipVerify bool    `toml:"insecure_skip_verify"`
	WebhookURL         string  `toml:"webhook_url"`
	RiskStorePath      string  `toml:"risk_store_path"`
	MinStopDistancePct float64 `toml:"min_stop_distance_pct"`
	EntrySlipPct       float64 `toml:"entry_slip_pct"`
	EntryTag           string  `toml:"entry_tag"`
}

func (f *FreqtradeConfig) applyDefaults(keys keySet) {
	if f == nil {
		return
	}
	applyFieldDefaults(keys,
		boolFieldDefault("freqtrade.enabled", &f.Enabled, true),
		stringFieldDefault("freqtrade.api_url", &f.APIURL, defaultFreqtradeAPI),
		stringFieldDefault("freqtrade.webhook_url", &f.WebhookURL, defaultFreqtradeWebhook),
		stringFieldDefault("freqtrade.risk_store_path", &f.RiskStorePath, defaultFreqtradeRiskDB),
		fieldDefault{
			key:   "freqtrade.default_stake_usd",
			need:  func() bool { return f.DefaultStakeUSD <= 0 },
			apply: func() { f.DefaultStakeUSD = defaultFreqtradeStake },
		},
		fieldDefault{
			key:   "freqtrade.default_leverage",
			need:  func() bool { return f.DefaultLeverage <= 0 },
			apply: func() { f.DefaultLeverage = defaultFreqtradeLev },
		},
		fieldDefault{
			key:   "freqtrade.timeout_seconds",
			need:  func() bool { return f.TimeoutSeconds <= 0 },
			apply: func() { f.TimeoutSeconds = defaultFreqtradeTimeout },
		},
	)
	if f.DefaultStakeUSD < 0 {
		f.DefaultStakeUSD = 0
	}
	if f.MinStopDistancePct < 0 {
		f.MinStopDistancePct = 0
	}
	if f.EntrySlipPct < 0 {
		f.EntrySlipPct = 0
	}
}

// AIConfig 包含与模型、持仓周期相关的所有设置。
type AIConfig struct {
	Aggregation             string                    `toml:"aggregation"`
	LogEachModel            bool                      `toml:"log_each_model"`
	Weights                 map[string]float64        `toml:"weights"`
	ProviderPreference      []string                  `toml:"provider_preference"`
	DecisionIntervalSeconds int                       `toml:"decision_interval_seconds"`
	DecisionLogPath         string                    `toml:"decision_log_path"`
	IncludeLastDecision     bool                      `toml:"include_last_decision"`
	LastDecisionMaxAgeSec   int                       `toml:"last_decision_max_age_seconds"`
	ActiveHorizon           string                    `toml:"active_horizon"`
	HoldingProfiles         map[string]HorizonProfile `toml:"holding_horizon_profiles"`
	ProfileDefaults         HorizonProfileDefaults    `toml:"profile_defaults"`
	ProviderPresets         map[string]ModelPreset    `toml:"provider_presets"`
	Models                  []AIModelConfig           `toml:"models"`
	MultiAgent              MultiAgentConfig          `toml:"multi_agent"`
}

func (a *AIConfig) applyDefaults(keys keySet) {
	if a == nil {
		return
	}
	if a.ProviderPresets == nil {
		a.ProviderPresets = make(map[string]ModelPreset)
	}
	applyFieldDefaults(keys,
		stringFieldDefault("ai.aggregation", &a.Aggregation, defaultAIAggregation),
		stringFieldDefault("ai.decision_log_path", &a.DecisionLogPath, defaultAIDecisionLog),
		boolFieldDefault("ai.log_each_model", &a.LogEachModel, true),
		boolFieldDefault("ai.include_last_decision", &a.IncludeLastDecision, true),
	)
	if a.LastDecisionMaxAgeSec <= 0 {
		a.LastDecisionMaxAgeSec = defaultAIDecisionAge // config: ai.last_decision_max_age_seconds
	}
	if a.DecisionIntervalSeconds <= 0 {
		a.DecisionIntervalSeconds = defaultAIDecisionEvery // config: ai.decision_interval_seconds
	}
	a.ProviderPreference = normalizePreferenceList(a.ProviderPreference)
	profileDefaults := a.ProfileDefaults
	profileDefaults.normalize()
	if profileDefaults.AnalysisSlice <= 0 {
		profileDefaults.AnalysisSlice = 46
		if profileDefaults.SliceDropTail <= 0 {
			profileDefaults.SliceDropTail = 3
		}
	}
	a.ProfileDefaults = profileDefaults
	if len(a.HoldingProfiles) == 0 {
		a.HoldingProfiles = defaultHorizonProfiles(profileDefaults)
	} else {
		normalized := make(map[string]HorizonProfile, len(a.HoldingProfiles))
		for name, profile := range a.HoldingProfiles {
			normalized[name] = normalizeHorizonProfile(profile, profileDefaults)
		}
		a.HoldingProfiles = normalized
	}
	if strings.TrimSpace(a.ActiveHorizon) == "" {
		a.ActiveHorizon = "one_hour"
	}
	if _, ok := a.HoldingProfiles[a.ActiveHorizon]; !ok {
		for name := range a.HoldingProfiles {
			a.ActiveHorizon = name
			break
		}
	}
	a.MultiAgent.applyDefaults(keys)
}

// ModelPreset 描述可复用的 API 连接配置。
type ModelPreset struct {
	APIURL         string            `toml:"api_url"`
	APIKey         string            `toml:"api_key"`
	Headers        map[string]string `toml:"headers"`
	SupportsVision bool              `toml:"supports_vision"`
	ExpectJSON     bool              `toml:"expect_json"`
}

// AIModelConfig 代表一个最终参与投票的模型条目。
type AIModelConfig struct {
	ID       string            `toml:"id"`
	Provider string            `toml:"provider"`
	Preset   string            `toml:"preset"`
	Enabled  bool              `toml:"enabled"`
	APIURL   string            `toml:"api_url"`
	APIKey   string            `toml:"api_key"`
	Model    string            `toml:"model"`
	Headers  map[string]string `toml:"headers"`
	// SupportsVision/ExpectJSON 使用指针以区分“显式 false”与“沿用预设值”。
	SupportsVision *bool `toml:"supports_vision"`
	ExpectJSON     *bool `toml:"expect_json"`
}

// ResolvedModelConfig 是合并预设后的最终模型配置。
type ResolvedModelConfig struct {
	ID             string
	Provider       string
	Enabled        bool
	APIURL         string
	APIKey         string
	Model          string
	Headers        map[string]string
	SupportsVision bool
	ExpectJSON     bool
}

// MultiAgentConfig 描述多阶段 Agent 编排的配置。
type MultiAgentConfig struct {
	Enabled           bool   `toml:"enabled"`
	IndicatorProvider string `toml:"indicator_provider"`
	PatternProvider   string `toml:"pattern_provider"`
	TrendProvider     string `toml:"trend_provider"`
	IndicatorTemplate string `toml:"indicator_template"`
	PatternTemplate   string `toml:"pattern_template"`
	TrendTemplate     string `toml:"trend_template"`
	MaxBlocks         int    `toml:"max_blocks"`
}

func (m *MultiAgentConfig) applyDefaults(keys keySet) {
	if m == nil {
		return
	}
	applyFieldDefaults(keys,
		boolFieldDefault("ai.multi_agent.enabled", &m.Enabled, true),
		stringFieldDefault("ai.multi_agent.indicator_template", &m.IndicatorTemplate, "agent_indicator"),
		stringFieldDefault("ai.multi_agent.pattern_template", &m.PatternTemplate, "agent_pattern"),
		stringFieldDefault("ai.multi_agent.trend_template", &m.TrendTemplate, "agent_trend"),
	)
	if strings.TrimSpace(m.IndicatorProvider) == "" {
		m.IndicatorProvider = "deepseek"
	}
	if strings.TrimSpace(m.PatternProvider) == "" {
		m.PatternProvider = "qwen"
	}
	if strings.TrimSpace(m.TrendProvider) == "" {
		m.TrendProvider = "vanchin"
	}
	if m.MaxBlocks <= 0 {
		m.MaxBlocks = defaultAIMultiBlocks
	}
}

type MarketConfig struct {
	ActiveSource string         `toml:"active_source"`
	Sources      []MarketSource `toml:"sources"`
}

type MarketSource struct {
	Name        string      `toml:"name"`
	Enabled     bool        `toml:"enabled"`
	RESTBaseURL string      `toml:"rest_base_url"`
	Proxy       ProxyConfig `toml:"proxy"`
}

type ProxyConfig struct {
	Enabled bool   `toml:"enabled"`
	RESTURL string `toml:"rest_url"`
	WSURL   string `toml:"ws_url"`
}

func (m *MarketConfig) applyDefaults() {
	if m == nil {
		return
	}
	if len(m.Sources) == 0 {
		m.Sources = []MarketSource{{
			Name:        defaultMarketName,
			Enabled:     true,
			RESTBaseURL: defaultMarketREST,
		}}
	}
	for i := range m.Sources {
		src := &m.Sources[i]
		src.Proxy.normalize()
		if strings.TrimSpace(src.Name) == "" {
			if i == 0 {
				src.Name = defaultMarketName
			} else {
				src.Name = fmt.Sprintf("market_%d", i)
			}
		}
		if src.RESTBaseURL == "" {
			src.RESTBaseURL = defaultMarketREST
		}
	}
	if strings.TrimSpace(m.ActiveSource) == "" {
		m.ActiveSource = firstEnabledMarket(m.Sources)
	}
}

func (p *ProxyConfig) normalize() {
	if p == nil {
		return
	}
	p.RESTURL = strings.TrimSpace(p.RESTURL)
	p.WSURL = strings.TrimSpace(p.WSURL)
}

// HorizonProfile 描述特定持仓周期所需的时间段与指标参数。
type HorizonProfile struct {
	EntryTimeframes      []string          `toml:"entry_timeframes"`
	ConfirmTimeframes    []string          `toml:"confirm_timeframes"`
	BackgroundTimeframes []string          `toml:"background_timeframes"`
	Indicators           HorizonIndicators `toml:"indicators"`
	AnalysisSlice        int               `toml:"analysis_slice"`
	SliceDropTail        int               `toml:"slice_drop_tail"`
}

// HorizonProfileDefaults 定义“持仓模板”的共享指标与窗口设置。
type HorizonProfileDefaults struct {
	Indicators    HorizonIndicators `toml:"indicators"`
	AnalysisSlice int               `toml:"analysis_slice"`
	SliceDropTail int               `toml:"slice_drop_tail"`
}

func (d *HorizonProfileDefaults) normalize() {
	if d.Indicators.EMA.Fast <= 0 {
		d.Indicators.EMA.Fast = 21
	}
	if d.Indicators.EMA.Mid <= 0 {
		d.Indicators.EMA.Mid = 50
	}
	if d.Indicators.EMA.Slow <= 0 {
		d.Indicators.EMA.Slow = 200
	}
	if d.Indicators.RSI.Period <= 0 {
		d.Indicators.RSI.Period = 14
	}
	if d.Indicators.RSI.Oversold == 0 {
		d.Indicators.RSI.Oversold = 33
	}
	if d.Indicators.RSI.Overbought == 0 {
		d.Indicators.RSI.Overbought = 68
	}
	if d.AnalysisSlice <= 0 {
		d.AnalysisSlice = 46
	}
	minSlice := d.Indicators.LookbackBars()
	if d.AnalysisSlice > 0 && d.AnalysisSlice < minSlice {
		d.AnalysisSlice = minSlice
	}
	if d.SliceDropTail < 0 {
		d.SliceDropTail = 0
	}
	if d.SliceDropTail == 0 && d.AnalysisSlice > 0 {
		// 默认丢弃最近 3 根，避免未收盘蜡烛干扰。
		d.SliceDropTail = 3
		if d.SliceDropTail >= d.AnalysisSlice {
			d.SliceDropTail = d.AnalysisSlice / 2
		}
	}
}

// ResolveModelConfigs 合并 provider 预设与模型条目，返回最终配置。
func (a AIConfig) ResolveModelConfigs() ([]ResolvedModelConfig, error) {
	out := make([]ResolvedModelConfig, 0, len(a.Models))
	presets := a.ProviderPresets
	for _, raw := range a.Models {
		presetName := strings.TrimSpace(raw.Preset)
		var preset ModelPreset
		if presetName != "" {
			var ok bool
			if presets != nil {
				preset, ok = presets[presetName]
			}
			if !ok {
				return nil, fmt.Errorf("找不到 ai.provider_presets.%s", presetName)
			}
		}
		apiURL := strings.TrimSpace(raw.APIURL)
		if apiURL == "" {
			apiURL = strings.TrimSpace(preset.APIURL)
		}
		apiKey := strings.TrimSpace(raw.APIKey)
		if apiKey == "" {
			apiKey = strings.TrimSpace(preset.APIKey)
		}
		headers := make(map[string]string, len(preset.Headers)+len(raw.Headers))
		for k, v := range preset.Headers {
			headers[k] = v
		}
		for k, v := range raw.Headers {
			headers[k] = v
		}
		supportsVision := preset.SupportsVision
		if raw.SupportsVision != nil {
			supportsVision = *raw.SupportsVision
		}
		expectJSON := preset.ExpectJSON
		if raw.ExpectJSON != nil {
			expectJSON = *raw.ExpectJSON
		}
		out = append(out, ResolvedModelConfig{
			ID:             strings.TrimSpace(raw.ID),
			Provider:       strings.TrimSpace(raw.Provider),
			Enabled:        raw.Enabled,
			APIURL:         apiURL,
			APIKey:         apiKey,
			Model:          strings.TrimSpace(raw.Model),
			Headers:        headers,
			SupportsVision: supportsVision,
			ExpectJSON:     expectJSON,
		})
	}
	return out, nil
}

// MustResolveModelConfigs 在配置已通过校验时使用，忽略错误。
func (a AIConfig) MustResolveModelConfigs() []ResolvedModelConfig {
	out, err := a.ResolveModelConfigs()
	if err != nil {
		return nil
	}
	return out
}

// HorizonIndicators 组合该周期要使用的关键指标参数。
type HorizonIndicators struct {
	EMA EMAConfig `toml:"ema"`
	RSI RSIConfig `toml:"rsi"`
}

// LookbackBars 返回指标所需的最小历史条数。
func (h HorizonIndicators) LookbackBars() int {
	max := h.EMA.Slow
	if h.EMA.Mid > max {
		max = h.EMA.Mid
	}
	if h.EMA.Fast > max {
		max = h.EMA.Fast
	}
	if p := h.RSI.Period + 1; p > max {
		max = p
	}
	// MACD 需要至少 26
	if max < 26 {
		max = 26
	}
	return max
}

type EMAConfig struct {
	Fast int `toml:"fast"`
	Mid  int `toml:"mid"`
	Slow int `toml:"slow"`
}

type RSIConfig struct {
	Period     int     `toml:"period"`
	Oversold   float64 `toml:"oversold"`
	Overbought float64 `toml:"overbought"`
}

func (m MarketConfig) ResolveActiveSource() MarketSource {
	if len(m.Sources) == 0 {
		return MarketSource{
			Name:        "binance",
			Enabled:     true,
			RESTBaseURL: "https://fapi.binance.com",
		}
	}
	active := strings.ToLower(strings.TrimSpace(m.ActiveSource))
	var fallback MarketSource
	for _, src := range m.Sources {
		if fallback.Name == "" {
			fallback = src
		}
		if !src.Enabled {
			continue
		}
		if active == "" || strings.ToLower(src.Name) == active {
			return src
		}
	}
	return fallback
}

// PositionSizeUSD 返回默认仓位大小（若设置 default_position_usd 则直接返回）。
func (t TradingConfig) PositionSizeUSD() float64 {
	if t.DefaultPositionUSD > 0 {
		return t.DefaultPositionUSD
	}
	return 0
}

// AllTimeframes 返回去重后的完整时间段列表（entry → confirm → background）。
func (p HorizonProfile) AllTimeframes() []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(list []string) {
		for _, tf := range list {
			tf = strings.TrimSpace(tf)
			if tf == "" {
				continue
			}
			if _, ok := seen[tf]; ok {
				continue
			}
			seen[tf] = struct{}{}
			out = append(out, tf)
		}
	}
	add(p.EntryTimeframes)
	add(p.ConfirmTimeframes)
	add(p.BackgroundTimeframes)
	return out
}

// LookbackMap 返回每个时间段所需的最小 K 线条数。
func (p HorizonProfile) LookbackMap(buffer int) map[string]int {
	if buffer < 0 {
		buffer = 0
	}
	base := p.Indicators.LookbackBars()
	req := base + buffer
	if req <= 0 {
		req = 50
	}
	out := make(map[string]int)
	for _, tf := range p.AllTimeframes() {
		out[strings.ToLower(tf)] = req
	}
	return out
}

func normalizeHorizonProfile(p HorizonProfile, defaults HorizonProfileDefaults) HorizonProfile {
	defaults.normalize()
	if len(p.EntryTimeframes) == 0 && len(p.ConfirmTimeframes) == 0 && len(p.BackgroundTimeframes) == 0 {
		p.EntryTimeframes = []string{"15m"}
	}
	if p.Indicators.EMA.Fast <= 0 {
		p.Indicators.EMA.Fast = defaults.Indicators.EMA.Fast
	}
	if p.Indicators.EMA.Mid <= 0 {
		p.Indicators.EMA.Mid = defaults.Indicators.EMA.Mid
	}
	if p.Indicators.EMA.Slow <= 0 {
		p.Indicators.EMA.Slow = defaults.Indicators.EMA.Slow
	}
	if p.Indicators.RSI.Period <= 0 {
		p.Indicators.RSI.Period = defaults.Indicators.RSI.Period
	}
	if p.Indicators.RSI.Oversold == 0 {
		p.Indicators.RSI.Oversold = defaults.Indicators.RSI.Oversold
	}
	if p.Indicators.RSI.Overbought == 0 {
		p.Indicators.RSI.Overbought = defaults.Indicators.RSI.Overbought
	}
	if p.AnalysisSlice <= 0 {
		p.AnalysisSlice = defaults.AnalysisSlice
	}
	minSlice := p.Indicators.LookbackBars()
	if p.AnalysisSlice > 0 && p.AnalysisSlice < minSlice {
		p.AnalysisSlice = minSlice
	}
	if p.SliceDropTail < 0 {
		p.SliceDropTail = 0
	}
	if p.SliceDropTail == 0 && defaults.SliceDropTail > 0 {
		p.SliceDropTail = defaults.SliceDropTail
	}
	if p.AnalysisSlice > 0 && p.SliceDropTail >= p.AnalysisSlice {
		p.SliceDropTail = p.AnalysisSlice - 1
		if p.SliceDropTail < 0 {
			p.SliceDropTail = 0
		}
	}
	return p
}

func firstEnabledMarket(sources []MarketSource) string {
	for _, src := range sources {
		name := strings.TrimSpace(src.Name)
		if src.Enabled && name != "" {
			return name
		}
	}
	if len(sources) > 0 && sources[0].Name != "" {
		return sources[0].Name
	}
	return "binance"
}

func defaultHorizonProfiles(defaults HorizonProfileDefaults) map[string]HorizonProfile {
	return map[string]HorizonProfile{
		"one_hour": normalizeHorizonProfile(HorizonProfile{
			EntryTimeframes:      []string{"5m", "15m"},
			ConfirmTimeframes:    []string{"1h"},
			BackgroundTimeframes: []string{"4h"},
			Indicators: HorizonIndicators{
				EMA: EMAConfig{Fast: 21, Mid: 50, Slow: 200},
				RSI: RSIConfig{Period: 14, Oversold: 33, Overbought: 68},
			},
		}, defaults),
		"four_hour": normalizeHorizonProfile(HorizonProfile{
			EntryTimeframes:      []string{"15m", "30m"},
			ConfirmTimeframes:    []string{"4h"},
			BackgroundTimeframes: []string{"1d"},
			Indicators: HorizonIndicators{
				EMA: EMAConfig{Fast: 21, Mid: 50, Slow: 200},
				RSI: RSIConfig{Period: 14, Oversold: 30, Overbought: 70},
			},
		}, defaults),
		"one_day": normalizeHorizonProfile(HorizonProfile{
			EntryTimeframes:      []string{"1h"},
			ConfirmTimeframes:    []string{"4h", "1d"},
			BackgroundTimeframes: []string{"1w"},
			Indicators: HorizonIndicators{
				EMA: EMAConfig{Fast: 21, Mid: 50, Slow: 200},
				RSI: RSIConfig{Period: 14, Oversold: 35, Overbought: 65},
			},
		}, defaults),
		"quant_agent": normalizeHorizonProfile(HorizonProfile{
			EntryTimeframes:      []string{"1h"},
			ConfirmTimeframes:    []string{"4h", "1d"},
			BackgroundTimeframes: []string{"1w"},
			SliceDropTail:        1,
			Indicators: HorizonIndicators{
				EMA: EMAConfig{Fast: 21, Mid: 50, Slow: 200},
				RSI: RSIConfig{Period: 14, Oversold: 35, Overbought: 65},
			},
		}, defaults),
	}
}

// Load 读取并解析 TOML 配置文件，并设置缺省值与基本校验
func Load(path string) (*Config, error) {
	files, err := resolveConfigIncludes(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	setKeys := make(keySet)
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("读取配置文件失败(%s): %w", file, err)
		}
		if err := toml.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("解析 TOML 失败(%s): %w", file, err)
		}
		if err := collectConfigKeys(data, setKeys); err != nil {
			return nil, fmt.Errorf("收集配置键失败(%s): %w", file, err)
		}
	}
	cfg.applyDefaults(setKeys)
	if err := validate(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

type keySet map[string]struct{}

func (k keySet) mark(path string) {
	path = strings.ToLower(strings.TrimSpace(path))
	if path == "" {
		return
	}
	k[path] = struct{}{}
}

func (k keySet) isSet(path string) bool {
	if len(k) == 0 {
		return false
	}
	path = strings.ToLower(strings.TrimSpace(path))
	if path == "" {
		return false
	}
	_, ok := k[path]
	return ok
}

type fieldDefault struct {
	key   string
	need  func() bool
	apply func()
}

func applyFieldDefaults(keys keySet, defs ...fieldDefault) {
	for _, def := range defs {
		if def.apply == nil {
			continue
		}
		if def.key != "" && keys.isSet(def.key) {
			continue
		}
		if def.need != nil && !def.need() {
			continue
		}
		def.apply()
	}
}

func stringFieldDefault(key string, target *string, def string) fieldDefault {
	return fieldDefault{
		key: key,
		need: func() bool {
			return target != nil && strings.TrimSpace(*target) == ""
		},
		apply: func() {
			if target != nil {
				*target = def
			}
		},
	}
}

func boolFieldDefault(key string, target *bool, def bool) fieldDefault {
	return fieldDefault{
		key:  key,
		need: func() bool { return target != nil },
		apply: func() {
			if target != nil {
				*target = def
			}
		},
	}
}

func resolveConfigIncludes(path string) ([]string, error) {
	if path == "" {
		return nil, fmt.Errorf("config path 不能为空")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	stack := make(map[string]bool)
	files, err := collectConfigFiles(abs, seen, stack)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return []string{abs}, nil
	}
	return files, nil
}

func collectConfigFiles(path string, seen, stack map[string]bool) ([]string, error) {
	path = filepath.Clean(path)
	if stack[path] {
		return nil, fmt.Errorf("检测到 include 循环: %s", path)
	}
	if seen[path] {
		return nil, nil
	}
	stack[path] = true
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置失败(%s): %w", path, err)
	}
	includes, err := parseIncludeList(data)
	if err != nil {
		return nil, fmt.Errorf("解析 include 失败(%s): %w", path, err)
	}
	dir := filepath.Dir(path)
	var ordered []string
	for _, inc := range includes {
		inc = strings.TrimSpace(inc)
		if inc == "" {
			continue
		}
		incPath := inc
		if !filepath.IsAbs(inc) {
			incPath = filepath.Join(dir, inc)
		}
		sub, err := collectConfigFiles(incPath, seen, stack)
		if err != nil {
			return nil, err
		}
		if len(sub) > 0 {
			ordered = append(ordered, sub...)
		}
	}
	delete(stack, path)
	seen[path] = true
	ordered = append(ordered, path)
	return ordered, nil
}

func normalizePreferenceList(pref []string) []string {
	if len(pref) == 0 {
		return nil
	}
	out := make([]string, 0, len(pref))
	seen := make(map[string]bool, len(pref))
	for _, id := range pref {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func collectConfigKeys(data []byte, dest keySet) error {
	if dest == nil {
		return nil
	}
	var raw map[string]any
	if err := toml.Unmarshal(data, &raw); err != nil {
		return err
	}
	flattenConfigKeys("", raw, dest)
	return nil
}

func parseIncludeList(data []byte) ([]string, error) {
	var doc map[string]any
	if err := toml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	raw, ok := doc["include"]
	if !ok {
		return nil, nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("include 需为字符串数组")
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		str, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("include 仅支持字符串")
		}
		str = strings.TrimSpace(str)
		if str != "" {
			out = append(out, str)
		}
	}
	return out, nil
}

func flattenConfigKeys(prefix string, node any, dest keySet) {
	switch val := node.(type) {
	case map[string]any:
		for k, v := range val {
			next := strings.ToLower(strings.TrimSpace(k))
			if next == "" {
				continue
			}
			if prefix != "" {
				next = prefix + "." + next
			}
			flattenConfigKeys(next, v, dest)
		}
	case []any:
		if prefix != "" {
			dest.mark(prefix)
		}
		for _, item := range val {
			flattenConfigKeys(prefix, item, dest)
		}
	default:
		if prefix != "" {
			dest.mark(prefix)
		}
	}
}

// 默认值设置
func (c *Config) applyDefaults(keys keySet) {
	c.App.applyDefaults(keys)
	c.Symbols.applyDefaults(keys)
	c.Kline.applyDefaults(keys)
	c.Prompt.applyDefaults(keys)
	c.MCP.applyDefaults(keys)
	c.Market.applyDefaults()
	c.AI.applyDefaults(keys)
	c.Freqtrade.applyDefaults(keys)
	c.Advanced.applyDefaults()
	c.Trading.applyDefaults()
	// 依赖 AI 周期推导调整 K 线缓存
	if profile, ok := c.AI.HoldingProfiles[c.AI.ActiveHorizon]; ok {
		lookbacks := profile.LookbackMap(20)
		maxLookback := 0
		for _, bars := range lookbacks {
			if bars > maxLookback {
				maxLookback = bars
			}
		}
		if c.Kline.MaxCached < maxLookback+50 {
			c.Kline.MaxCached = maxLookback + 50
		}
	}
}

// 基础校验
func validate(c *Config) error {
	if c.Symbols.Provider == "default" && len(c.Symbols.DefaultList) == 0 {
		return fmt.Errorf("symbols.default_list 不能为空（当 provider=default 时）")
	}
	if len(c.AI.HoldingProfiles) == 0 {
		return fmt.Errorf("ai.holding_horizon_profiles 至少需要一个 profile")
	}
	if _, ok := c.AI.HoldingProfiles[c.AI.ActiveHorizon]; !ok {
		return fmt.Errorf("未找到 ai.active_horizon=%s 对应的 profile", c.AI.ActiveHorizon)
	}
	for name, profile := range c.AI.HoldingProfiles {
		if err := validateHorizonProfile(name, profile); err != nil {
			return err
		}
	}
	models, err := c.AI.ResolveModelConfigs()
	if err != nil {
		return err
	}
	if len(models) == 0 {
		return fmt.Errorf("ai.models 至少需要配置一个模型")
	}
	for _, m := range models {
		if strings.TrimSpace(m.Model) == "" {
			return fmt.Errorf("ai.models 中存在未配置 model 的条目 (id=%s)", m.ID)
		}
		if strings.TrimSpace(m.APIURL) == "" {
			return fmt.Errorf("ai.models.%s 缺少 api_url（可通过 preset 继承）", m.ID)
		}
		if strings.TrimSpace(m.Provider) == "" {
			return fmt.Errorf("ai.models.%s 缺少 provider", m.ID)
		}
	}
	if len(c.AI.ProviderPreference) > 0 {
		modelSet := make(map[string]struct{}, len(models))
		for _, m := range models {
			modelSet[m.ID] = struct{}{}
		}
		for _, id := range c.AI.ProviderPreference {
			if _, ok := modelSet[id]; !ok {
				return fmt.Errorf("ai.provider_preference 包含未配置的模型 id: %s", id)
			}
		}
	}
	if c.Kline.MaxCached < 50 || c.Kline.MaxCached > 1000 {
		return fmt.Errorf("kline.max_cached 需在 [50,1000]")
	}
	if len(c.Market.Sources) == 0 {
		return fmt.Errorf("market.sources 至少需要一个数据源")
	}
	activeName := strings.ToLower(strings.TrimSpace(c.Market.ActiveSource))
	enabled := 0
	activeFound := false
	for _, src := range c.Market.Sources {
		if !src.Enabled {
			continue
		}
		enabled++
		if strings.TrimSpace(src.RESTBaseURL) == "" {
			return fmt.Errorf("market source %s 缺少 rest_base_url", src.Name)
		}
		if src.Proxy.Enabled && src.Proxy.RESTURL == "" && src.Proxy.WSURL == "" {
			return fmt.Errorf("market source %s 启用了代理但未配置 rest_url 或 ws_url", src.Name)
		}
		name := strings.ToLower(strings.TrimSpace(src.Name))
		if activeName == "" || name == activeName {
			activeFound = true
		}
	}
	if enabled == 0 {
		return fmt.Errorf("market.sources 至少启用一个数据源")
	}
	if !activeFound {
		return fmt.Errorf("未找到启用的 market.active_source=%s", c.Market.ActiveSource)
	}
	if c.Notify.Telegram.Enabled {
		if c.Notify.Telegram.BotToken == "" || c.Notify.Telegram.ChatID == "" {
			return fmt.Errorf("已启用 Telegram 通知，请提供 bot_token 与 chat_id")
		}
	}
	if c.AI.MultiAgent.Enabled {
		ma := c.AI.MultiAgent
		if strings.TrimSpace(ma.IndicatorTemplate) == "" {
			return fmt.Errorf("ai.multi_agent.indicator_template 不能为空")
		}
		if strings.TrimSpace(ma.PatternTemplate) == "" {
			return fmt.Errorf("ai.multi_agent.pattern_template 不能为空")
		}
		if strings.TrimSpace(ma.TrendTemplate) == "" {
			return fmt.Errorf("ai.multi_agent.trend_template 不能为空")
		}
		if ma.MaxBlocks <= 0 {
			return fmt.Errorf("ai.multi_agent.max_blocks 需 > 0")
		}
	}
	if c.Freqtrade.Enabled {
		if strings.TrimSpace(c.Freqtrade.APIURL) == "" {
			return fmt.Errorf("freqtrade.api_url 不能为空")
		}
		if strings.TrimSpace(c.Freqtrade.APIToken) == "" {
			if strings.TrimSpace(c.Freqtrade.Username) == "" || strings.TrimSpace(c.Freqtrade.Password) == "" {
				return fmt.Errorf("freqtrade 认证需要提供 api_token 或 username+password")
			}
		}
		if c.Freqtrade.DefaultStakeUSD < 0 {
			return fmt.Errorf("freqtrade.default_stake_usd 需 >= 0")
		}
		if c.Freqtrade.DefaultLeverage < 0 {
			return fmt.Errorf("freqtrade.default_leverage 需 >= 0")
		}
		if c.Freqtrade.MinStopDistancePct < 0 {
			return fmt.Errorf("freqtrade.min_stop_distance_pct 需 >= 0")
		}
		if c.Freqtrade.EntrySlipPct < 0 {
			return fmt.Errorf("freqtrade.entry_slip_pct 需 >= 0")
		}
	}
	if c.Trading.Mode == "" {
		c.Trading.Mode = "static"
	}
	if c.Trading.Mode != "static" {
		return fmt.Errorf("trading.mode 当前仅支持 static，收到 %s", c.Trading.Mode)
	}
	if c.Trading.MaxPositionPct <= 0 || c.Trading.MaxPositionPct > 1 {
		return fmt.Errorf("trading.max_position_pct 需在 (0, 1] 区间")
	}
	if c.Trading.DefaultLeverage <= 0 {
		return fmt.Errorf("trading.default_leverage 需 > 0")
	}
	return nil
}

func validateHorizonProfile(name string, p HorizonProfile) error {
	if len(p.AllTimeframes()) == 0 {
		return fmt.Errorf("holding profile %s 至少需要一个时间周期", name)
	}
	for _, tf := range p.AllTimeframes() {
		if !isValidInterval(tf) {
			return fmt.Errorf("holding profile %s 包含非法周期: %s", name, tf)
		}
	}
	if p.Indicators.EMA.Fast <= 0 || p.Indicators.EMA.Mid <= 0 || p.Indicators.EMA.Slow <= 0 {
		return fmt.Errorf("holding profile %s 的 EMA 参数需为正整数", name)
	}
	if p.Indicators.RSI.Period <= 0 {
		return fmt.Errorf("holding profile %s 的 RSI.period 需 > 0", name)
	}
	if p.Indicators.RSI.Overbought <= p.Indicators.RSI.Oversold {
		return fmt.Errorf("holding profile %s 的 RSI overbought 需大于 oversold", name)
	}
	if p.AnalysisSlice < 0 {
		return fmt.Errorf("holding profile %s 的 analysis_slice 需 >= 0", name)
	}
	if p.SliceDropTail < 0 {
		return fmt.Errorf("holding profile %s 的 slice_drop_tail 需 >= 0", name)
	}
	if p.AnalysisSlice > 0 && p.SliceDropTail >= p.AnalysisSlice {
		return fmt.Errorf("holding profile %s 的 analysis_slice 需大于 slice_drop_tail", name)
	}
	return nil
}

// isValidInterval 简易校验：以数字开头，以 m/h/d 结尾
func isValidInterval(s string) bool {
	if s == "" {
		return false
	}
	suf := s[len(s)-1]
	if suf != 'm' && suf != 'h' && suf != 'd' && suf != 'w' {
		return false
	}
	for i := 0; i < len(s)-1; i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
