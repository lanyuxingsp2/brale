package config

import (
	"fmt"
	"os"

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
        Aggregation string `toml:"aggregation"`
        LogEachModel bool   `toml:"log_each_model"`
        Weights   map[string]float64 `toml:"weights"`
        DecisionIntervalSeconds int `toml:"decision_interval_seconds"`
        // 模型配置：完全通过配置文件提供，不再使用环境变量
        Models      []struct {
            ID       string `toml:"id"`         // 唯一标识（如 openai/deepseek/qwen_自定义名）
            Provider string `toml:"provider"`   // openai | deepseek | qwen（均按 OpenAI 兼容接口调用）
            Enabled  bool   `toml:"enabled"`
            APIURL   string `toml:"api_url"`    // OpenAI 兼容 BaseURL，如 https://api.openai.com/v1
            APIKey   string `toml:"api_key"`
            Model    string `toml:"model"`      // 模型名，如 gpt-4o-mini / deepseek-chat / qwen3-max
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
	if len(c.Kline.Periods) == 0 {
		c.Kline.Periods = []string{"5m"}
	}
	if c.Kline.MaxCached <= 0 {
		c.Kline.MaxCached = 100
	}
	if len(c.WS.Periods) == 0 {
		c.WS.Periods = []string{"3m", "4h"}
	}
	if c.Prompt.Dir == "" {
		c.Prompt.Dir = "prompts"
	}
	if c.Prompt.SystemTemplate == "" {
		c.Prompt.SystemTemplate = "default"
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
	return nil
}

// isValidInterval 简易校验：以数字开头，以 m/h/d 结尾
func isValidInterval(s string) bool {
	if s == "" {
		return false
	}
	suf := s[len(s)-1]
	if suf != 'm' && suf != 'h' && suf != 'd' {
		return false
	}
	for i := 0; i < len(s)-1; i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
