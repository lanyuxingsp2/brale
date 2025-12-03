package decision

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"brale/internal/analysis/indicator"
	"brale/internal/analysis/pattern"
	"brale/internal/analysis/visual"
	"brale/internal/config"
	"brale/internal/logger"
	"brale/internal/market"
	"brale/internal/store"
)

// AnalysisContext 汇集单个 symbol+interval 的结构化分析结果。
type AnalysisContext struct {
	Symbol          string `json:"symbol"`
	Interval        string `json:"interval"`
	KlineJSON       string `json:"kline_json"`
	IndicatorJSON   string `json:"indicator_json"`
	PatternReport   string `json:"pattern_report"`
	TrendReport     string `json:"trend_report"`
	ImageB64        string `json:"image_base64"`
	ImageNote       string `json:"image_note"`
	ForecastHorizon string `json:"forecast_horizon"`
}

// AnalysisBuildInput 生成分析上下文所需的参数。
type AnalysisBuildInput struct {
	Context     context.Context
	Exporter    store.SnapshotExporter
	Symbols     []string
	Intervals   []string
	Limit       int
	SliceLength int
	SliceDrop   int
	HorizonName string
	Indicators  config.HorizonIndicators
	WithImages  bool
}

// BuildAnalysisContexts 批量生成 AnalysisContext。
func BuildAnalysisContexts(input AnalysisBuildInput) []AnalysisContext {
	if input.Exporter == nil {
		return nil
	}
	ctx := input.Context
	if ctx == nil {
		ctx = context.Background()
	}
	limit := input.Limit
	if limit <= 0 {
		limit = 240
	}
	sliceLen := input.SliceLength
	sliceDrop := input.SliceDrop
	intervals := input.Intervals
	if len(intervals) == 0 {
		intervals = []string{"1h"}
	}
	indicatorLookback := input.Indicators.LookbackBars()
	result := make([]AnalysisContext, 0, len(input.Symbols)*len(intervals))
	generateImages := input.WithImages
	for _, rawSym := range input.Symbols {
		sym := strings.ToUpper(strings.TrimSpace(rawSym))
		if sym == "" {
			continue
		}
		var symContexts []AnalysisContext

		for _, rawIv := range intervals {
			iv := strings.TrimSpace(rawIv)
			if iv == "" {
				continue
			}
			candles, err := input.Exporter.Export(ctx, sym, iv, limit)
			if err != nil {
				logger.Debugf("analysis/export 失败 %s %s: %v", sym, iv, err)
				continue
			}
			if len(candles) == 0 {
				continue
			}
			fullCandles := cloneRoundedCandles(candles)
			shortCandles := trimCandlesWindow(fullCandles, sliceLen, sliceDrop)
			if len(shortCandles) == 0 {
				logger.Debugf("analysis trim %s %s 结果为空，保留原始 %d 根", sym, iv, len(fullCandles))
				shortCandles = fullCandles
			} else if len(shortCandles) != len(fullCandles) {
				logger.Debugf("analysis trim %s %s: %d -> %d (slice=%d drop=%d)", sym, iv, len(fullCandles), len(shortCandles), sliceLen, sliceDrop)
			}
			candles = shortCandles

			rawJSON, err := json.Marshal(shortCandles)
			if err != nil {
				logger.Warnf("analysis/json 序列化失败 %s %s: %v", sym, iv, err)
				continue
			}
			indJSON := ""
			var (
				rep    indicator.Report
				indErr error
			)
			if len(fullCandles) >= indicatorLookback {
				rep, indErr = indicator.ComputeAll(fullCandles, indicator.Settings{
					Symbol:   sym,
					Interval: iv,
					EMA:      input.Indicators.EMA,
					RSI:      input.Indicators.RSI,
				})
			} else {
				indErr = fmt.Errorf("insufficient history: need %d got %d", indicatorLookback, len(fullCandles))
				logger.Debugf("analysis %s %s 指标历史不足，需要 %d 根，当前仅 %d 根", sym, iv, indicatorLookback, len(fullCandles))
			}
			if indErr == nil {
				if len(shortCandles) > 0 && len(shortCandles) < len(fullCandles) {
					rep = clipIndicatorReport(rep, len(shortCandles))
				}
				if b, err := json.Marshal(rep); err == nil {
					indJSON = string(b)
				}
			} else {
				logger.Warnf("indicator compute 失败 %s %s: %v", sym, iv, indErr)
			}
			pat := pattern.Analyze(candles)
			trendReport := pat.TrendSummary
			if pat.Bias != "" {
				trendReport = fmt.Sprintf("%s | bias=%s", pat.TrendSummary, pat.Bias)
			}

			ac := AnalysisContext{
				Symbol:          sym,
				Interval:        iv,
				KlineJSON:       string(rawJSON),
				IndicatorJSON:   indJSON,
				PatternReport:   pat.PatternSummary,
				TrendReport:     trendReport,
				ForecastHorizon: input.HorizonName,
			}
			if generateImages && indErr == nil {
				imgInput := visual.CompositeInput{
					Context:    ctx,
					Symbol:     sym,
					Horizon:    input.HorizonName,
					Intervals:  []string{iv},
					Candles:    map[string][]market.Candle{iv: candles},
					Indicators: map[string]indicator.Report{iv: rep},
					Patterns:   map[string]pattern.Result{iv: pat},
				}
				if img, renderErr := visual.RenderComposite(imgInput); renderErr == nil {
					ac.ImageB64 = img.DataURI()
					ac.ImageNote = img.Description
				} else {
					logger.Warnf("interval render 失败 %s %s: %v", sym, iv, renderErr)
				}
			}
			symContexts = append(symContexts, ac)
		}
		if len(symContexts) == 0 {
			continue
		}
		result = append(result, symContexts...)
	}
	return result
}

func trimCandlesWindow(candles []market.Candle, sliceLen, dropTail int) []market.Candle {
	n := len(candles)
	if n == 0 || (sliceLen <= 0 && dropTail <= 0) {
		return candles
	}
	end := n
	if dropTail > 0 {
		if dropTail >= end {
			dropTail = end - 1
			if dropTail < 0 {
				dropTail = 0
			}
		}
		end -= dropTail
	}
	if end <= 0 {
		return nil
	}
	start := 0
	if sliceLen > 0 && sliceLen < end {
		start = end - sliceLen
	}
	return candles[start:end]
}

func clipIndicatorReport(rep indicator.Report, keep int) indicator.Report {
	if keep <= 0 || keep >= rep.Count {
		return rep
	}
	out := rep
	out.Count = keep
	values := make(map[string]indicator.IndicatorValue, len(rep.Values))
	for k, v := range rep.Values {
		v.Series = trimFloatSeries(v.Series, keep)
		values[k] = v
	}
	out.Values = values
	return out
}

func trimFloatSeries(series []float64, keep int) []float64 {
	if keep <= 0 || len(series) == 0 {
		return nil
	}
	if len(series) <= keep {
		return append([]float64(nil), series...)
	}
	start := len(series) - keep
	return append([]float64(nil), series[start:]...)
}

func cloneRoundedCandles(src []market.Candle) []market.Candle {
	if len(src) == 0 {
		return nil
	}
	out := make([]market.Candle, len(src))
	for i, c := range src {
		out[i] = roundCandleValues(c)
	}
	return out
}

func roundCandleValues(c market.Candle) market.Candle {
	c.Open = roundTo(c.Open, 4)
	c.High = roundTo(c.High, 4)
	c.Low = roundTo(c.Low, 4)
	c.Close = roundTo(c.Close, 4)
	c.Volume = roundTo(c.Volume, 2)
	return c
}

func roundTo(v float64, decimals int) float64 {
	if decimals <= 0 {
		return math.Round(v)
	}
	pow := math.Pow(10, float64(decimals))
	return math.Round(v*pow) / pow
}
