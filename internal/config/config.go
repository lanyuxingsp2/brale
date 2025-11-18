package config

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
)

// 配置结构体（与规划一致，保留必要字段，便于后续扩展）
type Config struct {
	App struct {
		Env      string `toml:"env"`
		LogLevel string `toml:"log_level"`
		HTTPAddr string `toml:"http_addr"`
	} `toml:"app"`

	Symbols struct {
		Provider    string   `toml:"provider"`
		DefaultList []string `toml:"default_list"`
		APIURL      string   `toml:"api_url"` // 当 provider=http 时，从该地址拉取币种列表
	} `toml:"symbols"`

	Kline struct {
		MaxCached int `toml:"max_cached"`
	} `toml:"kline"`

	Market MarketConfig `toml:"market"`

	AI AIConfig `toml:"ai"`

	MCP struct {
		TimeoutSeconds int `toml:"timeout_seconds"`
	} `toml:"mcp"`

	Prompt struct {
		Dir            string `toml:"dir"`
		SystemTemplate string `toml:"system_template"`
	} `toml:"prompt"`

	Notify struct {
		Telegram struct {
			Enabled  bool   `toml:"enabled"`
			BotToken string `toml:"bot_token"`
			ChatID   string `toml:"chat_id"`
		} `toml:"telegram"`
	} `toml:"notify"`

	Freqtrade FreqtradeConfig `toml:"freqtrade"`

	Advanced struct {
		LiquidityFilterUSDM int     `toml:"liquidity_filter_usd_m"`
		MinRiskReward       float64 `toml:"min_risk_reward"`
		OpenCooldownSeconds int     `toml:"open_cooldown_seconds"`
		MaxOpensPerCycle    int     `toml:"max_opens_per_cycle"`
	} `toml:"advanced"`

	Trading TradingConfig `toml:"trading"`
}

// TradingConfig 控制模拟/实盘资金来源与默认仓位策略。
type TradingConfig struct {
	Mode               string  `toml:"mode"`                 // "static" | 后续扩展 "live"
	StaticBalance      float64 `toml:"static_balance"`       // 静态账户资金（USDT）
	MaxPositionPct     float64 `toml:"max_position_pct"`     // 单笔最大占用比例 0~1
	DefaultPositionUSD float64 `toml:"default_position_usd"` // 若>0，直接作为 fallback 仓位
	DefaultLeverage    int     `toml:"default_leverage"`     // 缺省杠杆
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
}

// AIConfig 包含与模型、持仓周期相关的所有设置。
type AIConfig struct {
	Aggregation             string                    `toml:"aggregation"`
	LogEachModel            bool                      `toml:"log_each_model"`
	Weights                 map[string]float64        `toml:"weights"`
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
	MaxCharsPerBlock  int    `toml:"max_chars_per_block"`
}

func (m *MultiAgentConfig) applyDefaults() {
	if m == nil {
		return
	}
	if m.IndicatorTemplate == "" {
		m.IndicatorTemplate = "agent_indicator"
	}
	if m.PatternTemplate == "" {
		m.PatternTemplate = "agent_pattern"
	}
	if m.TrendTemplate == "" {
		m.TrendTemplate = "agent_trend"
	}
	if m.MaxBlocks <= 0 {
		m.MaxBlocks = 4
	}
	if m.MaxCharsPerBlock <= 0 {
		m.MaxCharsPerBlock = 1600
	}
}

type MarketConfig struct {
	ActiveSource string         `toml:"active_source"`
	Sources      []MarketSource `toml:"sources"`
}

type MarketSource struct {
	Name            string `toml:"name"`
	Enabled         bool   `toml:"enabled"`
	RESTBaseURL     string `toml:"rest_base_url"`
	WSBaseURL       string `toml:"ws_base_url"`
	RateLimitPerMin int    `toml:"rate_limit_per_min"`
	WSBatchSize     int    `toml:"ws_batch_size"`
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
	if max <= 0 {
		max = 50
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
			Name:            "binance",
			Enabled:         true,
			RESTBaseURL:     "https://fapi.binance.com",
			WSBaseURL:       "wss://fstream.binance.com/stream",
			RateLimitPerMin: 1200,
			WSBatchSize:     150,
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

// PositionSizeUSD 返回默认仓位大小（若设置 default_position_usd 则直接返回，
// 否则按 static_balance × max_position_pct 计算，自动限制不超过账户余额）。
func (t TradingConfig) PositionSizeUSD() float64 {
	if t.DefaultPositionUSD > 0 {
		return t.DefaultPositionUSD
	}
	if t.StaticBalance > 0 && t.MaxPositionPct > 0 {
		size := t.StaticBalance * t.MaxPositionPct
		if size <= 0 {
			return 0
		}
		return math.Min(size, t.StaticBalance)
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
	}
}

// Load 读取并解析 TOML 配置文件，并设置缺省值与基本校验
func Load(path string) (*Config, error) {
	files, err := resolveConfigIncludes(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("读取配置文件失败(%s): %w", file, err)
		}
		if err := toml.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("解析 TOML 失败(%s): %w", file, err)
		}
	}
	applyDefaults(&cfg)
	if err := validate(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

type includeHeader struct {
	Include []string `toml:"include"`
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
	var header includeHeader
	if err := toml.Unmarshal(data, &header); err != nil {
		return nil, fmt.Errorf("解析 include 失败(%s): %w", path, err)
	}
	dir := filepath.Dir(path)
	var ordered []string
	for _, inc := range header.Include {
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

// 默认值设置
func applyDefaults(c *Config) {
	if c.App.Env == "" {
		c.App.Env = "dev"
	}
	if c.App.LogLevel == "" {
		c.App.LogLevel = "info"
	}
	if c.Kline.MaxCached <= 0 {
		c.Kline.MaxCached = 100
	}
	if c.Prompt.Dir == "" {
		c.Prompt.Dir = "prompts"
	}
	if c.Prompt.SystemTemplate == "" {
		c.Prompt.SystemTemplate = "default"
	}
	if strings.TrimSpace(c.App.HTTPAddr) == "" {
		c.App.HTTPAddr = ":9991"
	}
	if c.AI.ProviderPresets == nil {
		c.AI.ProviderPresets = make(map[string]ModelPreset)
	}
	profileDefaults := c.AI.ProfileDefaults
	profileDefaults.normalize()
	if profileDefaults.AnalysisSlice <= 0 {
		// 再兜底一次，防止 normalize 被重置。
		profileDefaults.AnalysisSlice = 46
		if profileDefaults.SliceDropTail <= 0 {
			profileDefaults.SliceDropTail = 3
		}
	}
	c.AI.ProfileDefaults = profileDefaults

	// Horizon profiles 与周期派生
	if len(c.AI.HoldingProfiles) == 0 {
		c.AI.HoldingProfiles = defaultHorizonProfiles(profileDefaults)
	} else {
		normalized := make(map[string]HorizonProfile, len(c.AI.HoldingProfiles))
		for name, profile := range c.AI.HoldingProfiles {
			normalized[name] = normalizeHorizonProfile(profile, profileDefaults)
		}
		c.AI.HoldingProfiles = normalized
	}
	if c.AI.ActiveHorizon == "" {
		c.AI.ActiveHorizon = "one_hour"
	}
	if _, ok := c.AI.HoldingProfiles[c.AI.ActiveHorizon]; !ok {
		for name := range c.AI.HoldingProfiles {
			c.AI.ActiveHorizon = name
			break
		}
	}
	activeProfile := c.AI.HoldingProfiles[c.AI.ActiveHorizon]
	derivedIntervals := activeProfile.AllTimeframes()
	if len(derivedIntervals) == 0 {
		derivedIntervals = []string{"5m"}
	}
	lookbacks := activeProfile.LookbackMap(20)
	maxLookback := 0
	for _, bars := range lookbacks {
		if bars > maxLookback {
			maxLookback = bars
		}
	}
	if c.Kline.MaxCached < maxLookback+50 {
		c.Kline.MaxCached = maxLookback + 50
	}
	if len(c.Market.Sources) == 0 {
		c.Market.Sources = []MarketSource{{
			Name:            "binance",
			Enabled:         true,
			RESTBaseURL:     "https://fapi.binance.com",
			WSBaseURL:       "wss://fstream.binance.com/stream",
			RateLimitPerMin: 1200,
			WSBatchSize:     150,
		}}
	}
	for i := range c.Market.Sources {
		src := &c.Market.Sources[i]
		if src.RESTBaseURL == "" {
			src.RESTBaseURL = "https://fapi.binance.com"
		}
		if src.WSBaseURL == "" {
			src.WSBaseURL = "wss://fstream.binance.com/stream"
		}
		if src.RateLimitPerMin <= 0 {
			src.RateLimitPerMin = 1200
		}
		if src.WSBatchSize <= 0 {
			src.WSBatchSize = 150
		}
		if src.Name == "" {
			src.Name = fmt.Sprintf("market_%d", i)
		}
	}
	if c.Market.ActiveSource == "" {
		c.Market.ActiveSource = firstEnabledMarket(c.Market.Sources)
	}
	if c.AI.DecisionLogPath == "" {
		c.AI.DecisionLogPath = filepath.Join("data", "live", "decisions.db")
	}
	if c.AI.LastDecisionMaxAgeSec <= 0 {
		c.AI.LastDecisionMaxAgeSec = 3600
	}
	if !c.AI.IncludeLastDecision {
		c.AI.IncludeLastDecision = true
	}
	c.AI.MultiAgent.applyDefaults()

	if c.MCP.TimeoutSeconds <= 0 {
		c.MCP.TimeoutSeconds = 120
	}
	// 决策周期（秒），默认 60s
	if c.AI.DecisionIntervalSeconds <= 0 {
		c.AI.DecisionIntervalSeconds = 60
	}
	// 与旧项目保持一致：默认 RR 底线 3.0
	if c.Advanced.MinRiskReward <= 0 {
		c.Advanced.MinRiskReward = 3.0
	}
	if c.Advanced.OpenCooldownSeconds <= 0 {
		c.Advanced.OpenCooldownSeconds = 180
	} // 3分钟冷却
	if c.Advanced.MaxOpensPerCycle <= 0 {
		c.Advanced.MaxOpensPerCycle = 3
	}

	// Trading defaults
	if c.Trading.Mode == "" {
		c.Trading.Mode = "static"
	}
	if c.Trading.StaticBalance <= 0 {
		c.Trading.StaticBalance = 10000
	}
	if c.Trading.MaxPositionPct <= 0 || c.Trading.MaxPositionPct > 1 {
		c.Trading.MaxPositionPct = 0.05
	}
	if c.Trading.DefaultLeverage <= 0 {
		c.Trading.DefaultLeverage = 2
	}
	if c.Trading.DefaultPositionUSD < 0 {
		c.Trading.DefaultPositionUSD = 0
	}
	if c.Freqtrade.TimeoutSeconds <= 0 {
		c.Freqtrade.TimeoutSeconds = 15
	}
	if c.Freqtrade.DefaultLeverage < 0 {
		c.Freqtrade.DefaultLeverage = 0
	}
	if c.Freqtrade.DefaultStakeUSD < 0 {
		c.Freqtrade.DefaultStakeUSD = 0
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
		if strings.TrimSpace(src.RESTBaseURL) == "" || strings.TrimSpace(src.WSBaseURL) == "" {
			return fmt.Errorf("market source %s 需配置 rest_base_url 与 ws_base_url", src.Name)
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
		if ma.MaxCharsPerBlock <= 0 {
			return fmt.Errorf("ai.multi_agent.max_chars_per_block 需 > 0")
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
	}
	if c.Trading.Mode == "" {
		c.Trading.Mode = "static"
	}
	if c.Trading.Mode != "static" {
		return fmt.Errorf("trading.mode 当前仅支持 static，收到 %s", c.Trading.Mode)
	}
	if c.Trading.StaticBalance <= 0 {
		return fmt.Errorf("trading.static_balance 需 > 0")
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
