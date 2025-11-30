package market

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	brcfg "brale/internal/config"
	"brale/internal/logger"
)

// DerivativesData 包含 AI 决策需要的衍生品环境数据，如 OI 和资金费率
type DerivativesData struct {
	Symbol      string
	OI          float64            // 最新 OI (从 History[0] 取)
	OIHistory   map[string]float64 // 历史快照: key="1h", val=1小时前的OI
	FundingRate float64            // 实时资金费率
	LastUpdate  time.Time
	Error       string
}

// MetricsService 负责周期性从交易所拉取 OI/资金费率并缓存
type MetricsService struct {
	source  Source                     // 依赖抽象接口
	cache   map[string]DerivativesData // 内存缓存
	mu      sync.RWMutex               // 保护 cache
	symbols []string                   // 需要监控的币种列表

	decisionInterval       time.Duration        // 用于计算后台更新频率，以平摊 API 请求
	horizonProfile         brcfg.HorizonProfile // 持仓周期配置，用于动态计算 OI 历史周期
	baseOIHistoryPeriod    string               // OI 历史拉取的基准周期 (例如 "15m", "1h")
	oiHistoryLimit         int                  // OI 历史拉取的数量 (例如 100, 30)
	targetOIHistTimeframes []string             // 需要在 Prompt 中展示的 OI 历史周期 (例如 "1h", "4h", "1d")
}

// GetTargetTimeframes 返回 MetricsService 关注的 OI 历史时间周期列表
func (s *MetricsService) GetTargetTimeframes() []string {
	return s.targetOIHistTimeframes
}

// NewMetricsService 创建 MetricsService 实例
func NewMetricsService(
	source Source,
	symbols []string,
	decisionIntervalSeconds int,
	horizon brcfg.HorizonProfile, // 新增参数
) *MetricsService {
	// 过滤空 symbol
	validSymbols := make([]string, 0, len(symbols))
	for _, s := range symbols {
		s = strings.ToUpper(strings.TrimSpace(s))
		if s != "" {
			validSymbols = append(validSymbols, s)
		}
	}
	if len(validSymbols) == 0 {
		return nil
	}

	// 合并所有 Timeframes 并去重，用于确定 OI 历史拉取范围
	allTimeframes := horizon.AllTimeframes()
	if len(allTimeframes) == 0 {
		logger.Warnf("MetricsService: Horizon Profile 未配置任何 K 线周期，无法计算 OI 历史拉取范围。")
		// Fallback to a default if no timeframes are configured
		allTimeframes = []string{"1h", "4h", "1d"}
	}

	basePeriod, historyLimit, targetTimeframes := calculateOIHistoryParams(allTimeframes)
	if basePeriod == "" {
		// 再次fallback
		basePeriod = "1h"
		historyLimit = 30
		targetTimeframes = []string{"1h", "4h", "1d"}
		logger.Warnf("MetricsService: 无法从 Horizon Profile 计算 OI 历史参数，将使用默认值: period=%s, limit=%d", basePeriod, historyLimit)
	}

	return &MetricsService{
		source:                 source,
		cache:                  make(map[string]DerivativesData),
		symbols:                validSymbols,
		decisionInterval:       time.Duration(decisionIntervalSeconds) * time.Second,
		horizonProfile:         horizon,
		baseOIHistoryPeriod:    basePeriod,
		oiHistoryLimit:         historyLimit,
		targetOIHistTimeframes: targetTimeframes,
	}
}

// Get 从缓存中获取指定交易对的衍生品数据
func (s *MetricsService) Get(symbol string) (DerivativesData, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	data, ok := s.cache[strings.ToUpper(strings.TrimSpace(symbol))]
	return data, ok
}

// OI 从缓存获取指定交易对的未平仓量
func (s *MetricsService) OI(ctx context.Context, symbol string) (float64, error) {
	data, ok := s.Get(symbol)
	if !ok {
		return 0, fmt.Errorf("MetricsService: %s 的 OI 数据未找到或未更新", symbol)
	}
	if data.Error != "" {
		return 0, fmt.Errorf("MetricsService: %s 的 OI 数据存在错误: %s", symbol, data.Error)
	}
	return data.OI, nil
}

// Funding 从缓存获取指定交易对的资金费率
func (s *MetricsService) Funding(ctx context.Context, symbol string) (float64, error) {
	data, ok := s.Get(symbol)
	if !ok {
		return 0, fmt.Errorf("MetricsService: %s 的 Funding 数据未找到或未更新", symbol)
	}
	if data.Error != "" {
		return 0, fmt.Errorf("MetricsService: %s 的 Funding 数据存在错误: %s", symbol, data.Error)
	}
	return data.FundingRate, nil
}

// Start 启动后台轮询，周期性更新缓存
func (s *MetricsService) Start(ctx context.Context) {
	count := len(s.symbols)
	if count == 0 {
		logger.Warnf("MetricsService 未配置监控币种，不启动")
		return
	}

	// 计算请求步进：在决策周期的前 90% 时间内完成所有币种的更新
	// 避免在决策前最后一刻还在请求，留出缓存更新的缓冲
	effectiveInterval := s.decisionInterval
	if s.decisionInterval > 10*time.Second { // 至少保留 10s 缓冲，或者 10%
		effectiveInterval = s.decisionInterval - (s.decisionInterval / 10)
		if effectiveInterval < 10*time.Second { // 最少有效周期不能太短
			effectiveInterval = 10 * time.Second
		}
	} else if s.decisionInterval == 0 { // 避免除零
		effectiveInterval = 60 * time.Second // 默认 1分钟
	}

	step := effectiveInterval / time.Duration(count)

	// 最小请求间隔限制（防止币种太多导致间隔过短，刷爆 API）
	// 根据 Binance 期货 API 的 /fapi/v1/openInterest 接口，单个请求是 20 weight。
	// 推荐间隔至少 1s，以适应请求权重和网络延迟。
	minStep := 1 * time.Second // 最短 1 秒钟发一个请求
	if step < minStep {
		step = minStep
		logger.Warnf("MetricsService 计算步进 %v 小于最小限制 %v，已调整为 %v。请注意：币种过多可能会导致轮询周期超过决策间隔。", effectiveInterval/time.Duration(count), minStep, step)
	}

	logger.Infof("MetricsService 启动: 监控 %d 个币种, 决策周期 %v, 有效轮询周期 %v, 每个币种步进 %v",
		count, s.decisionInterval, effectiveInterval, step)

	ticker := time.NewTicker(step)
	defer ticker.Stop()

	cursor := 0
	for {
		select {
		case <-ctx.Done():
			logger.Infof("MetricsService 收到停止信号，优雅退出")
			return
		case <-ticker.C:
			// 避免并发问题，确保在访问 s.symbols 之前加锁或保证不可变
			symbolsCopy := s.symbols // 假设 s.symbols 在初始化后不会改变

			if len(symbolsCopy) == 0 {
				continue
			}

			// 循环获取当前要更新的币种
			sym := symbolsCopy[cursor]

			// 移动游标，环形循环
			cursor = (cursor + 1) % len(symbolsCopy)

			// 异步执行更新，避免阻塞 Ticker 节奏
			// 使用独立的上下文，以便在服务停止时可以取消
			updateCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second) // 设定一个合理的请求超时
			go func(sCtx context.Context, symbol string) {
				defer cancel()
				s.updateSymbol(sCtx, symbol)
			}(updateCtx, sym)
		}
	}
}

// updateSymbol 调用 source 获取数据并更新缓存
func (s *MetricsService) updateSymbol(ctx context.Context, symbol string) {
	var (
		oiHist  []OpenInterestPoint
		errOI   error
		funding float64
		errFund error
	)

	// 并发获取 OI 历史和实时 Funding
	var wg sync.WaitGroup
	wg.Add(2)

	// 获取 OI 历史数据
	go func() {
		defer wg.Done()
		oiHist, errOI = s.source.GetOpenInterestHistory(ctx, symbol, s.baseOIHistoryPeriod, s.oiHistoryLimit)
		if errOI != nil {
			logger.Warnf("MetricsService: 获取 %s OI 历史失败: %v", symbol, errOI)
		}
	}()

	// 获取实时 Funding Rate
	go func() {
		defer wg.Done()
		funding, errFund = s.source.GetFundingRate(ctx, symbol)
		if errFund != nil {
			logger.Warnf("MetricsService: 获取 %s Funding 失败: %v", symbol, errFund)
		}
	}()

	wg.Wait() // 等待所有请求完成

	newData := DerivativesData{
		Symbol:     symbol,
		LastUpdate: time.Now(),
		OIHistory:  make(map[string]float64),
	}

	var allErrors strings.Builder

	// 处理 OI 历史数据
	if errOI != nil {
		allErrors.WriteString(fmt.Sprintf("获取OI历史失败: %v; ", errOI))
	} else if len(oiHist) == 0 {
		allErrors.WriteString("获取OI历史为空; ")
	} else {
		// 最新 OI 总是最后一个点 (假设 API 返回升序)
		if len(oiHist) > 0 {
			latestOI := oiHist[len(oiHist)-1].SumOpenInterest
			newData.OI = latestOI
		} else {
			allErrors.WriteString("OI历史数据为空; ")
		}

		// 计算并存储历史 OI
		for _, tf := range s.targetOIHistTimeframes {
			duration, err := parseTimeframe(tf)
			if err != nil {
				logger.Warnf("MetricsService: 无法解析目标周期 %s: %v", tf, err)
				continue
			}

			// targetTimestamp 应该是在当前时间基础上减去 duration
			targetTime := time.Now().Add(-duration)

			// 找到最接近 targetTime 的 OI 值
			var oiAtPastTime float64
			found := false
			// OI历史数据通常是按时间升序的，从后往前找效率更高
			for i := len(oiHist) - 1; i >= 0; i-- {
				point := oiHist[i]
				pointTime := time.UnixMilli(point.Timestamp)

				// 找到第一个 timestamp 小于等于 targetTime 的点
				if pointTime.Before(targetTime) || pointTime.Equal(targetTime) {
					oiAtPastTime = point.SumOpenInterest
					found = true
					break
				}
			}
			if found {
				newData.OIHistory[tf] = oiAtPastTime
			} else {
				// 如果没有找到足够早的历史数据，则填0或日志警告
				newData.OIHistory[tf] = 0 // 或者根据业务逻辑填-1, math.NaN等
				logger.Debugf("MetricsService: %s 无法找到 %s 周期前的 OI 数据", symbol, tf)
			}
		}
	}

	// 处理 Funding Rate
	if errFund != nil {
		allErrors.WriteString(fmt.Sprintf("获取Funding失败: %v", errFund))
	} else {
		newData.FundingRate = funding
	}

	newData.Error = allErrors.String()

	s.mu.Lock()
	s.cache[symbol] = newData
	s.mu.Unlock()

	if newData.Error == "" {
		logger.Debugf("MetricsService: 已更新 %s, OI: %.2f, Funding: %.4f%%", symbol, newData.OI, newData.FundingRate*100)
	} else {
		logger.Warnf("MetricsService: 更新 %s 部分或全部失败: %s", symbol, newData.Error)
	}
}

// calculateOIHistoryParams 根据 horizonTimeframes 计算最佳的 OI 历史拉取周期和数量
func calculateOIHistoryParams(horizonTimeframes []string) (basePeriod string, limit int, targetTimeframes []string) {
	if len(horizonTimeframes) == 0 {
		return "", 0, nil
	}

	// 统一解析所有 timeframes 到 duration，并找到最小和最大
	minDuration := time.Hour * 24 * 365 // 假设一年是最大值
	maxDuration := time.Duration(0)

	parsedDurations := make(map[string]time.Duration)
	var allIntervals []string

	for _, tf := range horizonTimeframes {
		d, err := parseTimeframe(tf)
		if err != nil {
			logger.Warnf("无法解析时间周期 %s: %v", tf, err)
			continue
		}
		parsedDurations[tf] = d
		allIntervals = append(allIntervals, tf)

		if d < minDuration {
			minDuration = d
			basePeriod = tf // 最小的 timeframe 作为基准拉取周期
		}
		if d > maxDuration {
			maxDuration = d
		}
	}

	if basePeriod == "" || minDuration == 0 { // 极端情况，防止除零或无有效周期
		return "", 0, nil
	}

	// Binance OI History 支持的周期
	binanceOIPeriods := map[string]time.Duration{
		"5m":  5 * time.Minute,
		"15m": 15 * time.Minute,
		"30m": 30 * time.Minute,
		"1h":  1 * time.Hour,
		"2h":  2 * time.Hour,
		"4h":  4 * time.Hour,
		"6h":  6 * time.Hour,
		"12h": 12 * time.Hour,
		"1d":  24 * time.Hour,
	}

	// 找到最接近且小于等于 minDuration 的 Binance 支持周期作为 basePeriod
	// 避免用 30m 去拉 5m 的数据，这样会少很多请求
	actualBasePeriod := "1h" // 默认 fallback
	actualBaseDuration := 1 * time.Hour
	for p, d := range binanceOIPeriods {
		if d <= minDuration && d > actualBaseDuration {
			actualBaseDuration = d
			actualBasePeriod = p
		}
	}
	basePeriod = actualBasePeriod

	// 计算所需拉取的历史数据量
	// 确保能覆盖最大周期，并留有余量
	limit = int(maxDuration/actualBaseDuration) + 5 // 加 5 个缓冲
	if limit < 10 {                                 // 最小拉取 10 条
		limit = 10
	}
	if limit > 500 { // Binance 最大 500
		limit = 500
	}

	// 最终需要展示的周期，用于 Prompt
	targetTimeframes = allIntervals

	logger.Infof("OI 历史拉取参数计算: BasePeriod=%s, Limit=%d, TargetTimeframes=%v", basePeriod, limit, targetTimeframes)
	return basePeriod, limit, targetTimeframes
}

func parseTimeframe(tf string) (time.Duration, error) {
	if strings.HasSuffix(tf, "m") {
		minutes, err := strconv.Atoi(strings.TrimSuffix(tf, "m"))
		return time.Duration(minutes) * time.Minute, err
	} else if strings.HasSuffix(tf, "h") {
		hours, err := strconv.Atoi(strings.TrimSuffix(tf, "h"))
		return time.Duration(hours) * time.Hour, err
	} else if strings.HasSuffix(tf, "d") {
		days, err := strconv.Atoi(strings.TrimSuffix(tf, "d"))
		return time.Duration(days) * 24 * time.Hour, err
	}
	return 0, fmt.Errorf("无法解析时间周期: %s", tf)
}
