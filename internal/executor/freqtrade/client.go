package freqtrade

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	brconfig "brale/internal/config"
)

// Client wraps Freqtrade REST API interactions required by Brale.
type Client struct {
	baseURL    *url.URL
	httpClient *http.Client
	username   string
	password   string
	token      string
}

// NewClient constructs a Freqtrade client from configuration.
func NewClient(cfg brconfig.FreqtradeConfig) (*Client, error) {
	if !cfg.Enabled {
		return nil, fmt.Errorf("freqtrade 未启用")
	}
	raw := strings.TrimSpace(cfg.APIURL)
	if raw == "" {
		return nil, fmt.Errorf("freqtrade.api_url 不能为空")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("解析 freqtrade.api_url 失败: %w", err)
	}
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if cfg.InsecureSkipVerify {
		if transport.TLSClientConfig == nil {
			transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // #nosec G402
		} else {
			transport.TLSClientConfig.InsecureSkipVerify = true // #nosec G402
		}
	}
	httpClient := &http.Client{
		Timeout:   timeout,
		Transport: transport,
	}
	return &Client{
		baseURL:    parsed,
		httpClient: httpClient,
		username:   strings.TrimSpace(cfg.Username),
		password:   strings.TrimSpace(cfg.Password),
		token:      strings.TrimSpace(cfg.APIToken),
	}, nil
}

// ForceEnterPayload mirrors freqtrade's /forceenter schema.
type ForceEnterPayload struct {
	Pair        string   `json:"pair"`
	Side        string   `json:"side"`
	Price       *float64 `json:"price,omitempty"`
	OrderType   string   `json:"ordertype,omitempty"`
	StakeAmount float64  `json:"stakeamount,omitempty"`
	EntryTag    string   `json:"entry_tag,omitempty"`
	Leverage    float64  `json:"leverage,omitempty"`
}

// ForceEnterResponse contains trade identifier returned by freqtrade.
type ForceEnterResponse struct {
	TradeID int `json:"trade_id"`
}

// ForceExitPayload mirrors freqtrade's /forceexit schema.
type ForceExitPayload struct {
	TradeID   string  `json:"tradeid"`
	OrderType string  `json:"ordertype,omitempty"`
	Amount    float64 `json:"amount,omitempty"`
}

// ForceEnter creates a new trade via freqtrade.
func (c *Client) ForceEnter(ctx context.Context, payload ForceEnterPayload) (*ForceEnterResponse, error) {
	var resp ForceEnterResponse
	if err := c.doRequest(ctx, http.MethodPost, "/forceenter", payload, &resp); err != nil {
		return nil, err
	}
	if resp.TradeID == 0 {
		return nil, fmt.Errorf("freqtrade 未返回 trade_id")
	}
	return &resp, nil
}

// ForceExit partially or fully closes an existing trade.
func (c *Client) ForceExit(ctx context.Context, payload ForceExitPayload) error {
	return c.doRequest(ctx, http.MethodPost, "/forceexit", payload, nil)
}

// Trade represents a subset of freqtrade trade fields.
type Trade struct {
	ID           int     `json:"trade_id"`
	Pair         string  `json:"pair"`
	Side         string  `json:"side"`
	OpenDate     string  `json:"open_date"`
	CloseDate    string  `json:"close_date"`
	OpenRate     float64 `json:"open_rate"`
	CloseRate    float64 `json:"close_rate"`
	Amount       float64 `json:"amount"`
	OpenOrderID  string  `json:"open_order_id"`
	CloseOrderID string  `json:"close_order_id"`
	IsOpen       bool    `json:"is_open"`
}

// ListTrades fetches the latest trades from freqtrade.
func (c *Client) ListTrades(ctx context.Context) ([]Trade, error) {
	var trades []Trade
	if err := c.doRequest(ctx, http.MethodGet, "/trades", nil, &trades); err != nil {
		return nil, err
	}
	return trades, nil
}

func (c *Client) doRequest(ctx context.Context, method, path string, payload any, out any) error {
	if c == nil {
		return fmt.Errorf("freqtrade client 未初始化")
	}
	rel, err := url.Parse(path)
	if err != nil {
		return fmt.Errorf("解析路径失败: %w", err)
	}
	endpoint := c.baseURL.ResolveReference(rel)

	var body io.Reader
	if payload != nil {
		buf, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("序列化请求失败: %w", err)
		}
		body = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint.String(), body)
	if err != nil {
		return fmt.Errorf("构造请求失败: %w", err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	} else if c.username != "" {
		req.SetBasicAuth(c.username, c.password)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("调用 freqtrade 失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if len(data) == 0 {
			return fmt.Errorf("freqtrade 返回错误: %s", resp.Status)
		}
		return fmt.Errorf("freqtrade 返回错误(%s): %s", resp.Status, strings.TrimSpace(string(data)))
	}
	if out == nil {
		return nil
	}
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("解析 freqtrade 响应失败: %w", err)
	}
	return nil
}
