package market

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// 中文说明：
// 从 Binance USDT-M 合约 REST 端点拉取 OI（未平仓量）与资金费率（最新）。
// 默认使用 https://fapi.binance.com，可通过环境或外部构造时自定义 BaseURL。

type MetricsFetcher interface {
	OI(ctx context.Context, symbol string) (float64, error)
	Funding(ctx context.Context, symbol string) (float64, error)
}

type DefaultMetricsFetcher struct {
	// BaseURL 例如: https://fapi.binance.com
	BaseURL string
	Client  *http.Client
}

func NewDefaultMetricsFetcher(baseURL string) *DefaultMetricsFetcher {
	if baseURL == "" {
		baseURL = "https://fapi.binance.com"
	}
	return &DefaultMetricsFetcher{
		BaseURL: baseURL,
		Client:  &http.Client{Timeout: 10 * time.Second},
	}
}

// OI 获取当前未平仓量（张数）
func (f *DefaultMetricsFetcher) OI(ctx context.Context, symbol string) (float64, error) {
	url := fmt.Sprintf("%s/fapi/v1/openInterest?symbol=%s", f.BaseURL, symbol)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := f.Client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var r struct {
		OpenInterest string `json:"openInterest"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return 0, err
	}
	var v float64
	if r.OpenInterest != "" {
		fmt.Sscanf(r.OpenInterest, "%f", &v)
	}
	return v, nil
}

// Funding 获取最新资金费率（例如 0.0001 即 0.01%）
func (f *DefaultMetricsFetcher) Funding(ctx context.Context, symbol string) (float64, error) {
	url := fmt.Sprintf("%s/fapi/v1/premiumIndex?symbol=%s", f.BaseURL, symbol)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	resp, err := f.Client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
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
