package config

import (
	"fmt"
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
	} `toml:"app"`

	Exchange struct {
		Name        string `toml:"name"`
		WSBatchSize int    `toml:"ws_batch_size"`
	} `toml:"exchange"`

	Symbols struct {
		Provider    string   `toml:"provider"`
		DefaultList []string `toml:"default_list"`
		APIURL      string   `toml:"api_url"` // 当 provider=http 时，从该地址拉取币种列表
	} `toml:"symbols"`

	Kline struct {
		Periods   []string `toml:"periods"`
		MaxCached int      `toml:"max_cached"`
	} `toml:"kline"`

	WS struct {
		Periods []string `toml:"periods"`
	} `toml:"ws"`

	AI struct {
		Aggregation             string                    `toml:"aggregation"`
		LogEachModel            bool                      `toml:"log_each_model"`
		Weights                 map[string]float64        `toml:"weights"`
		DecisionIntervalSeconds int                       `toml:"decision_interval_seconds"`
		DecisionLogPath         string                    `toml:"decision_log_path"`
		IncludeLastDecision     bool                      `toml:"include_last_decision"`
		LastDecisionMaxAgeSec   int                       `toml:"last_decision_max_age_seconds"`
		ActiveHorizon           string                    `toml:"active_horizon"`
		HoldingProfiles         map[string]HorizonProfile `toml:"holding_horizon_profiles"`
		// 模型配置：完全通过配置文件提供，不再使用环境变量
		Models []struct {
			ID       string            `toml:"id"`       // 唯一标识（如 openai/deepseek/qwen_自定义名）
			Provider string            `toml:"provider"` // openai | deepseek | qwen（均按 OpenAI 兼容接口调用）
			Enabled  bool              `toml:"enabled"`
			APIURL   string            `toml:"api_url"` // OpenAI 兼容 BaseURL，如 https://api.openai.com/v1
			APIKey   string            `toml:"api_key"`
			Model    string            `toml:"model"`   // 模型名，如 gpt-4o-mini / deepseek-chat / qwen3-max
			Headers  map[string]string `toml:"headers"` // 可选：自定义请求头（例如 X-API-Key、OpenAI-Organization 等）
		} `toml:"models"`
	} `toml:"ai"`

	MCP struct {
		TimeoutSeconds int `toml:"timeout_seconds"`
	} `toml:"mcp"`

	Prompt struct {
		Dir            string `toml:"dir"`
		SystemTemplate string `toml:"system_template"`
	} `toml:"prompt"`

	Backtest struct {
		Enabled         bool   `toml:"enabled"`
		DataDir         string `toml:"data_dir"`
		HTTPAddr        string `toml:"http_addr"`
		DefaultExchange string `toml:"default_exchange"`
		RateLimitPerMin int    `toml:"rate_limit_per_min"`
		MaxBatch        int    `toml:"max_batch"`
		MaxConcurrent   int    `toml:"max_concurrent"`
	} `toml:"backtest"`

	Notify struct {
		Telegram struct {
			Enabled  bool   `toml:"enabled"`
			BotToken string `toml:"bot_token"`
			ChatID   string `toml:"chat_id"`
		} `toml:"telegram"`
	} `toml:"notify"`

	Advanced struct {
		LiquidityFilterUSDM int     `toml:"liquidity_filter_usd_m"`
		MinRiskReward       float64 `toml:"min_risk_reward"`
		OpenCooldownSeconds int     `toml:"open_cooldown_seconds"`
		MaxOpensPerCycle    int     `toml:"max_opens_per_cycle"`
	} `toml:"advanced"`
}

// HorizonProfile 描述特定持仓周期所需的时间段与指标参数。
type HorizonProfile struct {
	EntryTimeframes      []string          `toml:"entry_timeframes"`
	ConfirmTimeframes    []string          `toml:"confirm_timeframes"`
	BackgroundTimeframes []string          `toml:"background_timeframes"`
	Indicators           HorizonIndicators `toml:"indicators"`
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

func normalizeHorizonProfile(p HorizonProfile) HorizonProfile {
	if len(p.EntryTimeframes) == 0 && len(p.ConfirmTimeframes) == 0 && len(p.BackgroundTimeframes) == 0 {
		p.EntryTimeframes = []string{"15m"}
	}
	if p.Indicators.EMA.Fast <= 0 {
		p.Indicators.EMA.Fast = 21
	}
	if p.Indicators.EMA.Mid <= 0 {
		p.Indicators.EMA.Mid = 50
	}
	if p.Indicators.EMA.Slow <= 0 {
		p.Indicators.EMA.Slow = 200
	}
	if p.Indicators.RSI.Period <= 0 {
		p.Indicators.RSI.Period = 14
	}
	if p.Indicators.RSI.Oversold == 0 {
		p.Indicators.RSI.Oversold = 30
	}
	if p.Indicators.RSI.Overbought == 0 {
		p.Indicators.RSI.Overbought = 70
	}
	return p
}

func defaultHorizonProfiles() map[string]HorizonProfile {
	return map[string]HorizonProfile{
		"one_hour": normalizeHorizonProfile(HorizonProfile{
			EntryTimeframes:      []string{"5m", "15m"},
			ConfirmTimeframes:    []string{"1h"},
			BackgroundTimeframes: []string{"4h"},
			Indicators: HorizonIndicators{
				EMA: EMAConfig{Fast: 21, Mid: 50, Slow: 200},
				RSI: RSIConfig{Period: 14, Oversold: 33, Overbought: 68},
			},
		}),
		"four_hour": normalizeHorizonProfile(HorizonProfile{
			EntryTimeframes:      []string{"15m", "30m"},
			ConfirmTimeframes:    []string{"4h"},
			BackgroundTimeframes: []string{"1d"},
			Indicators: HorizonIndicators{
				EMA: EMAConfig{Fast: 21, Mid: 50, Slow: 200},
				RSI: RSIConfig{Period: 14, Oversold: 30, Overbought: 70},
			},
		}),
		"one_day": normalizeHorizonProfile(HorizonProfile{
			EntryTimeframes:      []string{"1h"},
			ConfirmTimeframes:    []string{"4h", "1d"},
			BackgroundTimeframes: []string{"1w"},
			Indicators: HorizonIndicators{
				EMA: EMAConfig{Fast: 21, Mid: 50, Slow: 200},
				RSI: RSIConfig{Period: 14, Oversold: 35, Overbought: 65},
			},
		}),
	}
}

// Load 读取并解析 TOML 配置文件，并设置缺省值与基本校验
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}
	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析 TOML 失败: %w", err)
	}
	applyDefaults(&cfg)
	if err := validate(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// 默认值设置
func applyDefaults(c *Config) {
	if c.App.Env == "" {
		c.App.Env = "dev"
	}
	if c.App.LogLevel == "" {
		c.App.LogLevel = "info"
	}
	if c.Exchange.WSBatchSize <= 0 {
		c.Exchange.WSBatchSize = 150
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

	// Horizon profiles 与周期派生
	if len(c.AI.HoldingProfiles) == 0 {
		c.AI.HoldingProfiles = defaultHorizonProfiles()
	} else {
		normalized := make(map[string]HorizonProfile, len(c.AI.HoldingProfiles))
		for name, profile := range c.AI.HoldingProfiles {
			normalized[name] = normalizeHorizonProfile(profile)
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
	if len(c.Kline.Periods) == 0 {
		c.Kline.Periods = append([]string(nil), derivedIntervals...)
	}
	if len(c.WS.Periods) == 0 {
		c.WS.Periods = append([]string(nil), derivedIntervals...)
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

	if c.Backtest.DataDir == "" {
		c.Backtest.DataDir = "data/backtest"
	}
	if c.Backtest.HTTPAddr == "" {
		c.Backtest.HTTPAddr = ":9991"
	}
	if c.Backtest.DefaultExchange == "" {
		c.Backtest.DefaultExchange = "binance"
	}
	if c.Backtest.RateLimitPerMin <= 0 {
		c.Backtest.RateLimitPerMin = 600
	}
	if c.Backtest.MaxBatch <= 0 {
		c.Backtest.MaxBatch = 1000
	}
	if c.Backtest.MaxConcurrent <= 0 {
		c.Backtest.MaxConcurrent = 2
	}
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
	if len(c.Kline.Periods) == 0 {
		return fmt.Errorf("kline.periods 至少需要一个周期")
	}
	if len(c.WS.Periods) == 0 {
		return fmt.Errorf("ws.periods 至少需要一个周期")
	}
	if c.Kline.MaxCached < 50 || c.Kline.MaxCached > 1000 {
		return fmt.Errorf("kline.max_cached 需在 [50,1000]")
	}
	for _, p := range c.Kline.Periods {
		if !isValidInterval(p) {
			return fmt.Errorf("非法 kline 周期: %s", p)
		}
	}
	for _, p := range c.WS.Periods {
		if !isValidInterval(p) {
			return fmt.Errorf("非法 ws 周期: %s", p)
		}
	}
	if c.Notify.Telegram.Enabled {
		if c.Notify.Telegram.BotToken == "" || c.Notify.Telegram.ChatID == "" {
			return fmt.Errorf("已启用 Telegram 通知，请提供 bot_token 与 chat_id")
		}
	}
	if c.Backtest.Enabled {
		if c.Backtest.DataDir == "" {
			return fmt.Errorf("backtest.data_dir 不能为空")
		}
		if c.Backtest.HTTPAddr == "" {
			return fmt.Errorf("backtest.http_addr 不能为空")
		}
		if c.Backtest.RateLimitPerMin <= 0 {
			return fmt.Errorf("backtest.rate_limit_per_min 需 > 0")
		}
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
