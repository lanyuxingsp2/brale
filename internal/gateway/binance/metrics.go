package binance

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"brale/internal/market"
)

// GetFundingRate 获取最新资金费率（例如 0.0001 即 0.01%）
func (s *Source) GetFundingRate(ctx context.Context, symbol string) (float64, error) {
	url := fmt.Sprintf("%s/fapi/v1/premiumIndex?symbol=%s", s.cfg.RESTBaseURL, symbol)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("binance funding error: %s", resp.Status)
	}

	var r struct {
		LastFundingRate string `json:"lastFundingRate"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return 0, err
	}
	var v float64
	if r.LastFundingRate != "" {
		fmt.Sscanf(r.LastFundingRate, "%f", &v)
	}
	return v, nil
}

// GetOpenInterestHistory 获取 OI 历史数据
func (s *Source) GetOpenInterestHistory(ctx context.Context, symbol, period string, limit int) ([]market.OpenInterestPoint, error) {
	if limit <= 0 {
		limit = 30 // 默认值
	}
	if limit > 500 {
		limit = 500 // 最大值
	}
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	period = strings.ToLower(strings.TrimSpace(period))

	// Binance 官方接口文档中，未平仓合约历史的路径位于 /futures/data 下
	url := fmt.Sprintf("%s/futures/data/openInterestHist?symbol=%s&period=%s&limit=%d", s.cfg.RESTBaseURL, symbol, period, limit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("binance openInterestHist error: %s", resp.Status)
	}

	var rawData []struct {
		Symbol             string `json:"symbol"`
		SumOpenInterest    string `json:"sumOpenInterest"`
		SumOpenInterestValue string `json:"sumOpenInterestValue"`
		Timestamp          int64  `json:"timestamp"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rawData); err != nil {
		return nil, err
	}

	var points []market.OpenInterestPoint
	for _, item := range rawData {
		oi, _ := strconv.ParseFloat(item.SumOpenInterest, 64)
		oiValue, _ := strconv.ParseFloat(item.SumOpenInterestValue, 64)
		points = append(points, market.OpenInterestPoint{
			Symbol:             item.Symbol,
			SumOpenInterest:    oi,
			SumOpenInterestValue: oiValue,
			Timestamp:          item.Timestamp,
		})
	}
	return points, nil
}
