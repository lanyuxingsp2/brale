package coins

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// SymbolProvider 币种来源接口
type SymbolProvider interface {
	List(ctx context.Context) ([]string, error)
	Name() string
}

// 默认实现：静态列表
type DefaultSymbolProvider struct{ symbols []string }

func NewDefaultProvider(symbols []string) *DefaultSymbolProvider {
	return &DefaultSymbolProvider{symbols: symbols}
}
func (p *DefaultSymbolProvider) Name() string { return "default" }
func (p *DefaultSymbolProvider) List(ctx context.Context) ([]string, error) {
	if len(p.symbols) == 0 {
		return nil, errors.New("默认币种列表为空")
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(p.symbols))
	for _, s := range p.symbols {
		s = strings.ToUpper(strings.TrimSpace(s))
		if s == "" {
			continue
		}
		if !strings.HasSuffix(s, "USDT") {
			s += "USDT"
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil, errors.New("标准化后列表为空")
	}
	return out, nil
}

// HTTP 实现：从自定义 API 拉取。支持两种返回格式：
// 1) ["BTCUSDT","ETHUSDT",...]
// 2) {"symbols": ["BTCUSDT","ETHUSDT",...]}
type HTTPSymbolProvider struct {
	URL    string
	Client *http.Client
}

func NewHTTPSymbolProvider(url string) *HTTPSymbolProvider {
	return &HTTPSymbolProvider{URL: url, Client: &http.Client{Timeout: 10 * time.Second}}
}
func (p *HTTPSymbolProvider) Name() string { return "http" }
func (p *HTTPSymbolProvider) List(ctx context.Context) ([]string, error) {
	if p.URL == "" {
		return nil, errors.New("symbols.api_url 未配置")
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	resp, err := p.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, errors.New("http 状态异常")
	}
	// 尝试解析两种形式
	var arr []string
	if err := json.NewDecoder(resp.Body).Decode(&arr); err == nil {
		return NewDefaultProvider(arr).List(ctx)
	}
	// 回退解析对象包装
	resp.Body.Close()
	req2, _ := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	resp2, err := p.Client.Do(req2)
	if err != nil {
		return nil, err
	}
	defer resp2.Body.Close()
	var obj struct {
		Symbols []string `json:"symbols"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&obj); err != nil {
		return nil, err
	}
	return NewDefaultProvider(obj.Symbols).List(ctx)
}
