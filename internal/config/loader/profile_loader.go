package loader

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"brale/internal/logger"

	"github.com/fsnotify/fsnotify"
	"github.com/spf13/viper"
)

// ProfileDefinition 描述单个分析 Profile，在 V2 中用于绑定中间件链与 Prompt。
type ProfileDefinition struct {
	Name       string   `mapstructure:"-"`
	ContextTag string   `mapstructure:"context_tag"`
	Targets    []string `mapstructure:"targets"`
	Intervals  []string `mapstructure:"intervals"`
	// DecisionIntervalMultiple 表示该 profile(币种) 决策间隔 = 最短K线周期 * 倍数。
	// 默认 1。
	DecisionIntervalMultiple int                `mapstructure:"decision_interval_multiple"`
	AnalysisSlice            int                `mapstructure:"analysis_slice"`
	SliceDropTail            int                `mapstructure:"slice_drop_tail"`
	Middlewares              []MiddlewareConfig `mapstructure:"middlewares"`
	Prompts                  PromptRefs         `mapstructure:"prompts"`
	ExitPlans                ExitPlanBinding    `mapstructure:"exit_plans"`
	Derivatives              DerivativesConfig  `mapstructure:"derivatives"`
	Default                  bool               `mapstructure:"default"`

	// 归一化后的字段（避免运行期重复处理）
	targetsUpper   []string
	intervalsLower []string
}

func (d ProfileDefinition) ExitPlanCombos() []string {
	return d.ExitPlans.ComboKeys()
}

// PromptRefs 指定 system/user 模板路径。
type PromptRefs struct {
	// SystemByModel 指定每个最终决策模型所使用的 system prompt。
	// Key 必须精确匹配 ai.models[].id。
	SystemByModel map[string]string `mapstructure:"system_by_model"`
	User          string            `mapstructure:"user"`
}

const defaultExitPlanID = "plan_combo_main"

// ExitPlanBinding 描述 Profile 使用的出场计划组合（Plan ID 固定为内置值）。
type ExitPlanBinding struct {
	Allowed []string `mapstructure:"-"` // 固定使用内置 plan，用户不可配置
	Combos  []string `mapstructure:"combos"`

	allowedNormalized []string
	combosNormalized  []string
}

func (b *ExitPlanBinding) normalize() {
	// 不暴露给用户配置，固定为内置 plan。
	b.allowedNormalized = []string{defaultExitPlanID}
	b.combosNormalized = normalizeComboKeys(b.Combos)
	b.Allowed = append([]string(nil), b.allowedNormalized...)
	b.Combos = append([]string(nil), b.combosNormalized...)
}

func (b ExitPlanBinding) ComboKeys() []string {
	if len(b.combosNormalized) == 0 {
		return nil
	}
	out := make([]string, len(b.combosNormalized))
	copy(out, b.combosNormalized)
	return out
}

// DerivativesConfig 控制是否拉取衍生品指标。
type DerivativesConfig struct {
	Enabled        bool `mapstructure:"enabled"`
	IncludeOI      bool `mapstructure:"include_oi"`
	IncludeFunding bool `mapstructure:"include_funding"`
}

func (d *DerivativesConfig) normalize() {
	if d == nil {
		return
	}
	if !d.Enabled {
		d.IncludeOI = false
		d.IncludeFunding = false
		return
	}
	// 若未显式指定，默认同时注入 OI/Funding。
	if !d.IncludeOI && !d.IncludeFunding {
		d.IncludeOI = true
		d.IncludeFunding = true
	}
}

// MiddlewareConfig 为单个中间件节点的配置。
type MiddlewareConfig struct {
	Name           string                            `mapstructure:"name"`
	Stage          int                               `mapstructure:"stage"`
	Critical       bool                              `mapstructure:"critical"`
	TimeoutSeconds int                               `mapstructure:"timeout_seconds"`
	Params         map[string]interface{}            `mapstructure:"params"`
	Configs        map[string]map[string]interface{} `mapstructure:"configs"`
}

// FileConfig 是完整的 profile 配置文件结构。
type FileConfig struct {
	Profiles map[string]ProfileDefinition `mapstructure:"profiles"`
}

// ProfileSnapshot 对外暴露的只读快照。
type ProfileSnapshot struct {
	Version  int64
	LoadedAt time.Time
	Profiles map[string]ProfileDefinition
}

// ChangeListener 在配置变更时被调用。
type ChangeListener func(ProfileSnapshot)

// ProfileLoader 负责从 YAML/JSON 文件中加载 profile，并监听热更新。
type ProfileLoader struct {
	path string
	v    *viper.Viper

	mu        sync.RWMutex
	snapshot  ProfileSnapshot
	listeners []ChangeListener
}

// NewProfileLoader 读取配置文件并开始监听 FS 事件。
func NewProfileLoader(path string) (*ProfileLoader, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("profile loader requires path")
	}
	v := viper.New()
	v.SetConfigFile(path)
	if err := v.ReadInConfig(); err != nil {
		return nil, fmt.Errorf("read profile config failed: %w", err)
	}
	loader := &ProfileLoader{path: path, v: v}
	if err := loader.reload(); err != nil {
		return nil, err
	}
	v.OnConfigChange(func(evt fsnotify.Event) {
		if err := loader.reload(); err != nil {
			logger.Errorf("profile reload failed (%s): %v", evt.Name, err)
			return
		}
		loader.notify()
	})
	v.WatchConfig()
	return loader, nil
}

// Snapshot 返回当前配置快照（深拷贝）。
func (l *ProfileLoader) Snapshot() ProfileSnapshot {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return cloneSnapshot(l.snapshot)
}

// Subscribe 注册监听器，并立即收到一次完整快照。
func (l *ProfileLoader) Subscribe(fn ChangeListener) {
	if fn == nil {
		return
	}
	l.mu.Lock()
	l.listeners = append(l.listeners, fn)
	snap := cloneSnapshot(l.snapshot)
	l.mu.Unlock()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Errorf("profile listener panic: %v", r)
			}
		}()
		fn(snap)
	}()
}

func (l *ProfileLoader) notify() {
	l.mu.RLock()
	snap := cloneSnapshot(l.snapshot)
	listeners := append([]ChangeListener(nil), l.listeners...)
	l.mu.RUnlock()
	for _, fn := range listeners {
		if fn == nil {
			continue
		}
		go func(cb ChangeListener) {
			defer func() {
				if r := recover(); r != nil {
					logger.Errorf("profile listener panic: %v", r)
				}
			}()
			cb(snap)
		}(fn)
	}
}

func (l *ProfileLoader) reload() error {
	var fileCfg FileConfig
	if err := l.v.Unmarshal(&fileCfg); err != nil {
		return fmt.Errorf("parse profile config failed: %w", err)
	}
	normalized := make(map[string]ProfileDefinition)
	for name, def := range fileCfg.Profiles {
		norm := normalizeProfileDefinition(name, def)
		normalized[name] = norm
	}
	l.mu.Lock()
	l.snapshot = ProfileSnapshot{
		Version:  l.snapshot.Version + 1,
		LoadedAt: time.Now(),
		Profiles: normalized,
	}
	l.mu.Unlock()
	logger.Infof("Profile loader reloaded %d profiles from %s", len(normalized), filepath.Base(l.path))
	return nil
}

func normalizeProfileDefinition(name string, def ProfileDefinition) ProfileDefinition {
	def.Name = name
	def.ContextTag = strings.TrimSpace(def.ContextTag)
	if def.ContextTag == "" {
		def.ContextTag = name
	}
	if def.DecisionIntervalMultiple <= 0 {
		def.DecisionIntervalMultiple = 1
	}
	def.targetsUpper = normalizeSymbols(def.Targets)
	def.intervalsLower = normalizeIntervals(def.Intervals)
	if len(def.Middlewares) == 0 {
		def.Middlewares = []MiddlewareConfig{{
			Name:     "kline_fetcher",
			Critical: true,
		}}
	}
	def.Middlewares = expandMiddlewareConfigs(def.Middlewares)
	def.ExitPlans.normalize()
	def.Derivatives.normalize()
	return def
}

func normalizeSymbols(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, sym := range in {
		s := strings.ToUpper(strings.TrimSpace(sym))
		if s == "" {
			continue
		}
		out = append(out, s)
	}
	return out
}

func normalizePlanIDs(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{})
	out := make([]string, 0, len(in))
	for _, id := range in {
		norm := strings.TrimSpace(id)
		if norm == "" {
			continue
		}
		key := strings.ToLower(norm)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, norm)
	}
	return out
}

func normalizeComboKeys(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{})
	out := make([]string, 0, len(in))
	for _, key := range in {
		norm := strings.ToLower(strings.TrimSpace(key))
		if norm == "" {
			continue
		}
		if _, ok := seen[norm]; ok {
			continue
		}
		seen[norm] = struct{}{}
		out = append(out, norm)
	}
	return out
}

func normalizeIntervals(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, iv := range in {
		s := strings.TrimSpace(iv)
		if s == "" {
			continue
		}
		out = append(out, strings.ToLower(s))
	}
	return out
}

func expandMiddlewareConfigs(list []MiddlewareConfig) []MiddlewareConfig {
	if len(list) == 0 {
		return list
	}
	expanded := make([]MiddlewareConfig, 0, len(list))
	for _, cfg := range list {
		children := expandSingleMiddlewareConfig(cfg)
		if len(children) == 0 {
			continue
		}
		expanded = append(expanded, children...)
	}
	return expanded
}

func expandSingleMiddlewareConfig(cfg MiddlewareConfig) []MiddlewareConfig {
	if len(cfg.Configs) == 0 {
		cfg.Params = cloneParams(cfg.Params)
		return []MiddlewareConfig{cfg}
	}
	keys := make([]string, 0, len(cfg.Configs))
	for k := range cfg.Configs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		cfg.Configs = nil
		cfg.Params = cloneParams(cfg.Params)
		return []MiddlewareConfig{cfg}
	}

	out := make([]MiddlewareConfig, 0, len(keys))
	for _, k := range keys {
		spec := cfg.Configs[k]
		// Start with parent params
		params := cloneParams(cfg.Params)
		if params == nil {
			params = make(map[string]interface{})
		}
		// Merge specific params, child params override parent params
		for pk, pv := range spec {
			params[pk] = pv
		}

		if _, ok := params["interval"]; !ok {
			if trimmed := strings.TrimSpace(k); trimmed != "" {
				params["interval"] = trimmed
			}
		}
		child := cfg
		child.Params = params
		child.Configs = nil
		out = append(out, child)
	}
	return out
}

func cloneParams(src map[string]interface{}) map[string]interface{} {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]interface{}, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

// TargetsUpper 返回标准化后的交易对列表。
func (p ProfileDefinition) TargetsUpper() []string {
	out := make([]string, len(p.targetsUpper))
	copy(out, p.targetsUpper)
	return out
}

// IntervalsLower 返回标准化后的周期列表。
func (p ProfileDefinition) IntervalsLower() []string {
	out := make([]string, len(p.intervalsLower))
	copy(out, p.intervalsLower)
	return out
}

// AgentEnabled 当 profile 配置了 EMA/RSI/MACD 中间件中任意一个时，视为启用多 Agent。
func (p ProfileDefinition) AgentEnabled() bool {
	for _, mw := range p.Middlewares {
		if isAgentMiddleware(mw.Name) {
			return true
		}
	}
	return false
}

// DerivativesEnabled 返回该 profile 是否允许注入衍生品数据。
func (p ProfileDefinition) DerivativesEnabled() bool {
	return p.Derivatives.Enabled && (p.Derivatives.IncludeOI || p.Derivatives.IncludeFunding)
}

func cloneSnapshot(src ProfileSnapshot) ProfileSnapshot {
	dst := ProfileSnapshot{
		Version:  src.Version,
		LoadedAt: src.LoadedAt,
		Profiles: make(map[string]ProfileDefinition, len(src.Profiles)),
	}
	for name, def := range src.Profiles {
		dst.Profiles[name] = def
	}
	return dst
}

func isAgentMiddleware(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "ema_trend", "rsi_extreme", "macd_trend":
		return true
	default:
		return false
	}
}
