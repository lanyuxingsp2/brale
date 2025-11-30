package freqtrade

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
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

const (
	tradeHistoryPageLimit = 500
	tradeHistoryMaxPages  = 3
)

var errTradeNotFound = errors.New("freqtrade trade not found")

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
	ID             int     `json:"trade_id"`
	Pair           string  `json:"pair"`
	Side           string  `json:"side"`
	IsShort        bool    `json:"is_short"`
	OpenDate       string  `json:"open_date"`
	CloseDate      string  `json:"close_date"`
	OpenRate       float64 `json:"open_rate"`
	CloseRate      float64 `json:"close_rate"`
	TakeProfit     float64 `json:"take_profit,omitempty"`
	StopLoss       float64 `json:"stop_loss,omitempty"`
	Amount         float64 `json:"amount"`
	StakeAmount    float64 `json:"stake_amount"`
	Leverage       float64 `json:"leverage"`
	OpenOrderID    string  `json:"open_order_id"`
	CloseOrderID   string  `json:"close_order_id"`
	IsOpen         bool    `json:"is_open"`
	CurrentRate    float64 `json:"current_rate"`
	CloseProfit    float64 `json:"close_profit"`
	CloseProfitAbs float64 `json:"close_profit_abs"`
	// Freqtrade /status 接口对于未平仓订单，往往使用 profit_ratio/profit_abs
	ProfitRatio float64 `json:"profit_ratio"`
	ProfitAbs   float64 `json:"profit_abs"`
}

// Balance 描述 freqtrade /balance 返回的核心字段。
type Balance struct {
	StakeCurrency string             `json:"stake_currency,omitempty"`
	Total         float64            `json:"total,omitempty"`
	Available     float64            `json:"available,omitempty"`
	Used          float64            `json:"used,omitempty"`
	Balance       float64            `json:"balance,omitempty"`
	Stake         float64            `json:"stake_balance,omitempty"`
	Wallets       map[string]float64 `json:"wallets,omitempty"`
	Raw           map[string]any     `json:"raw,omitempty"`
	UpdatedAt     time.Time          `json:"-"`
}

// ListTrades fetches currently open trades from freqtrade (uses /status endpoint).
func (c *Client) ListTrades(ctx context.Context) ([]Trade, error) {
	trades, err := c.fetchTrades(ctx, "/status")
	if err != nil {
		return nil, err
	}
	return filterOpenTrades(trades), nil
}

func (c *Client) fetchTrades(ctx context.Context, path string) ([]Trade, error) {
	var raw json.RawMessage
	if err := c.doRequest(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	if len(raw) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return nil, nil
	}
	var trades []Trade
	if err := json.Unmarshal(raw, &trades); err == nil {
		return trades, nil
	}
	type tradeEnvelope struct {
		Trades []Trade `json:"trades"`
		Data   []Trade `json:"data"`
		Result []Trade `json:"result"`
		Items  []Trade `json:"items"`
	}
	var env tradeEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("无法解析 freqtrade trades 响应: %w", err)
	}
	switch {
	case len(env.Trades) > 0:
		return env.Trades, nil
	case len(env.Data) > 0:
		return env.Data, nil
	case len(env.Result) > 0:
		return env.Result, nil
	case len(env.Items) > 0:
		return env.Items, nil
	default:
		return nil, nil
	}
}

// GetTrade 查询指定 trade_id 的最新详情（含已平仓记录）。
func (c *Client) GetTrade(ctx context.Context, tradeID int) (*Trade, error) {
	if c == nil || c.httpClient == nil {
		return nil, fmt.Errorf("freqtrade client not initialized")
	}
	if tradeID <= 0 {
		return nil, fmt.Errorf("trade_id 必填")
	}
	for page := 0; page < tradeHistoryMaxPages; page++ {
		offset := page * tradeHistoryPageLimit
		path := fmt.Sprintf("/trades?order_by_id=true&limit=%d&offset=%d", tradeHistoryPageLimit, offset)
		trades, err := c.fetchTrades(ctx, path)
		if err != nil {
			return nil, err
		}
		if len(trades) == 0 {
			break
		}
		for _, tr := range trades {
			if tr.ID == tradeID {
				// 返回拷贝，避免后续遍历修改底层 slice。
				copy := tr
				return &copy, nil
			}
		}
	}
	return nil, errTradeNotFound
}

func filterOpenTrades(trades []Trade) []Trade {
	if len(trades) == 0 {
		return nil
	}
	open := make([]Trade, 0, len(trades))
	for _, tr := range trades {
		if tr.IsOpen {
			open = append(open, tr)
		}
	}
	return open
}

// GetBalance 查询 freqtrade /balance 接口，返回账户权益与可用资金。
func (c *Client) GetBalance(ctx context.Context) (Balance, error) {
	var raw map[string]any
	if err := c.doRequest(ctx, http.MethodGet, "/balance", nil, &raw); err != nil {
		return Balance{}, err
	}
	b := Balance{Raw: raw, UpdatedAt: time.Now()}
	if raw == nil {
		return b, nil
	}
	if v, ok := raw["stake_currency"].(string); ok {
		b.StakeCurrency = strings.ToUpper(strings.TrimSpace(v))
	}
	if v, ok := raw["balance"]; ok {
		b.Balance = parseAnyFloat(v)
		if b.Balance > 0 {
			b.Total = b.Balance
		}
	}
	if b.Total == 0 {
		if v, ok := raw["total"]; ok {
			b.Total = parseAnyFloat(v)
		}
	}
	if v, ok := raw["available"]; ok {
		b.Available = parseAnyFloat(v)
	}
	if v, ok := raw["stake_balance"]; ok {
		b.Stake = parseAnyFloat(v)
	}
	if v, ok := raw["used"]; ok {
		b.Used = parseAnyFloat(v)
	}
	if wallets, ok := raw["wallets"].(map[string]any); ok {
		b.Wallets = make(map[string]float64, len(wallets))
		for k, v := range wallets {
			b.Wallets[strings.ToUpper(strings.TrimSpace(k))] = parseAnyFloat(v)
		}
	}
	return b, nil
}

func parseAnyFloat(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case float32:
		return float64(t)
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case uint64:
		return float64(t)
	case json.Number:
		if f, err := t.Float64(); err == nil {
			return f
		}
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
			return f
		}
	}
	return 0
}

func (c *Client) doRequest(ctx context.Context, method, path string, payload any, out any) error {
	if c == nil {
		return fmt.Errorf("freqtrade client 未初始化")
	}
	endpoint, err := c.resolveEndpoint(path)
	if err != nil {
		return err
	}

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

func (c *Client) resolveEndpoint(path string) (*url.URL, error) {
	if c.baseURL == nil {
		return nil, fmt.Errorf("freqtrade API 地址未设置")
	}
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		trimmed = "/"
	}
	query := ""
	if idx := strings.Index(trimmed, "?"); idx >= 0 {
		query = trimmed[idx+1:]
		trimmed = trimmed[:idx]
	}
	if trimmed == "" {
		trimmed = "/"
	}
	if !strings.HasPrefix(trimmed, "/") {
		trimmed = "/" + trimmed
	}
	base := *c.baseURL
	base.Path = strings.TrimSuffix(base.Path, "/") + trimmed
	base.RawPath = ""
	base.RawQuery = query
	base.Fragment = ""
	return &base, nil
}
