package binance

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"brale/internal/logger"
	"brale/internal/market"

	"github.com/adshao/go-binance/v2/futures"
)

const maxHistoryLimit = 1500

// Source 基于 go-binance SDK 实现 market.Source。
type Source struct {
	cfg    Config
	client *futures.Client

	mu           sync.Mutex
	candleCancel context.CancelFunc
	tradeCancel  context.CancelFunc

	statsMu sync.Mutex
	stats   market.SourceStats
}

func New(cfg Config) (*Source, error) {
	final := cfg.withDefaults()
	client := futures.NewClient("", "")
	client.BaseURL = strings.TrimSpace(final.RESTBaseURL)
	httpClient := &http.Client{Timeout: final.HTTPTimeout}
	if final.ProxyEnabled && final.RESTProxyURL != "" {
		proxyURL, err := url.Parse(final.RESTProxyURL)
		if err != nil {
			return nil, fmt.Errorf("invalid REST proxy url: %w", err)
		}
		baseTransport, ok := http.DefaultTransport.(*http.Transport)
		if !ok || baseTransport == nil {
			return nil, fmt.Errorf("http DefaultTransport is not *http.Transport")
		}
		transport := baseTransport.Clone()
		transport.Proxy = http.ProxyURL(proxyURL)
		httpClient.Transport = transport
	}
	client.HTTPClient = httpClient
	if final.ProxyEnabled {
		wsProxy := final.WSProxyURL
		if wsProxy == "" {
			wsProxy = final.RESTProxyURL
		}
		if wsProxy != "" {
			futures.SetWsProxyUrl(wsProxy)
		}
	}
	return &Source{
		cfg:    final,
		client: client,
	}, nil
}

func (s *Source) FetchHistory(ctx context.Context, symbol, interval string, limit int) ([]market.Candle, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > maxHistoryLimit {
		limit = maxHistoryLimit
	}
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	if symbol == "" {
		return nil, fmt.Errorf("symbol is required")
	}
	interval = strings.ToLower(strings.TrimSpace(interval))
	if interval == "" {
		return nil, fmt.Errorf("interval is required")
	}
	svc := s.client.NewKlinesService().Symbol(symbol).Interval(interval).Limit(limit)
	kls, err := svc.Do(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]market.Candle, 0, len(kls))
	for _, kl := range kls {
		if kl == nil {
			continue
		}
		c := market.Candle{
			OpenTime:  kl.OpenTime,
			CloseTime: kl.CloseTime,
			Open:      parseFloat(kl.Open),
			High:      parseFloat(kl.High),
			Low:       parseFloat(kl.Low),
			Close:     parseFloat(kl.Close),
			Volume:    parseFloat(kl.Volume),
			Trades:    kl.TradeNum,
		}
		out = append(out, c)
	}
	return out, nil
}

func (s *Source) Subscribe(ctx context.Context, symbols, intervals []string, opts market.SubscribeOptions) (<-chan market.CandleEvent, error) {
	mapping := buildSymbolIntervals(symbols, intervals)
	if len(mapping) == 0 {
		return nil, fmt.Errorf("no valid symbols or intervals for subscription")
	}
	buffer := opts.Buffer
	if buffer <= 0 {
		buffer = 512
	}
	out := make(chan market.CandleEvent, buffer)
	subCtx, cancel := context.WithCancel(ctx)

	s.mu.Lock()
	if s.candleCancel != nil {
		s.candleCancel()
	}
	s.candleCancel = cancel
	s.mu.Unlock()

	go func() {
		defer close(out)
		s.runKlineLoop(subCtx, mapping, out, opts)
	}()
	return out, nil
}

func (s *Source) SubscribeTrades(ctx context.Context, symbols []string, opts market.SubscribeOptions) (<-chan market.TradeEvent, error) {
	if len(symbols) == 0 {
		return nil, fmt.Errorf("symbols are required for trade subscription")
	}
	targets := make([]string, 0, len(symbols))
	for _, sym := range symbols {
		upper := strings.ToUpper(strings.TrimSpace(sym))
		if upper != "" {
			targets = append(targets, upper)
		}
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("no valid symbols for trade subscription")
	}
	buffer := opts.Buffer
	if buffer <= 0 {
		buffer = 1024
	}
	out := make(chan market.TradeEvent, buffer)
	subCtx, cancel := context.WithCancel(ctx)

	s.mu.Lock()
	if s.tradeCancel != nil {
		s.tradeCancel()
	}
	s.tradeCancel = cancel
	s.mu.Unlock()

	go func() {
		defer close(out)
		s.runTradeLoop(subCtx, targets, out, opts)
	}()
	return out, nil
}

func (s *Source) runKlineLoop(ctx context.Context, mapping map[string][]string, out chan<- market.CandleEvent, opts market.SubscribeOptions) {
	delay := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		var errMu sync.Mutex
		var lastErr error
		handler := func(event *futures.WsKlineEvent) {
			ce, ok := convertKlineEvent(event)
			if !ok {
				return
			}
			select {
			case <-ctx.Done():
				return
			case out <- ce:
			default:
				logger.Warnf("[binance] kline channel full, drop %s %s", ce.Symbol, ce.Interval)
			}
		}
		errHandler := func(err error) {
			if err == nil {
				return
			}
			errMu.Lock()
			lastErr = err
			errMu.Unlock()
		}
		doneC, stopC, err := futures.WsCombinedKlineServeMultiInterval(mapping, handler, errHandler)
		if err != nil {
			s.recordSubscribeError(err)
			if opts.OnDisconnect != nil {
				opts.OnDisconnect(err)
			}
			if !sleepWithContext(ctx, delay) {
				return
			}
			delay = nextDelay(delay)
			continue
		}
		delay = time.Second
		if opts.OnConnect != nil {
			opts.OnConnect()
		}
		select {
		case <-ctx.Done():
			close(stopC)
			<-doneC
			return
		case <-doneC:
		}
		close(stopC)
		errMu.Lock()
		errCopy := lastErr
		errMu.Unlock()
		s.recordReconnect(errCopy)
		if opts.OnDisconnect != nil {
			opts.OnDisconnect(errCopy)
		}
		if !sleepWithContext(ctx, delay) {
			return
		}
		delay = nextDelay(delay)
	}
}

func (s *Source) runTradeLoop(ctx context.Context, symbols []string, out chan<- market.TradeEvent, opts market.SubscribeOptions) {
	delay := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		var errMu sync.Mutex
		var lastErr error
		handler := func(event *futures.WsAggTradeEvent) {
			te, ok := convertAggTradeEvent(event)
			if !ok {
				return
			}
			select {
			case <-ctx.Done():
				return
			case out <- te:
			default:
				logger.Warnf("[binance] aggTrade channel full, drop %s", te.Symbol)
			}
		}
		errHandler := func(err error) {
			if err == nil {
				return
			}
			errMu.Lock()
			lastErr = err
			errMu.Unlock()
		}
		doneC, stopC, err := futures.WsCombinedAggTradeServe(symbols, handler, errHandler)
		if err != nil {
			s.recordSubscribeError(err)
			if opts.OnDisconnect != nil {
				opts.OnDisconnect(err)
			}
			if !sleepWithContext(ctx, delay) {
				return
			}
			delay = nextDelay(delay)
			continue
		}
		delay = time.Second
		if opts.OnConnect != nil {
			opts.OnConnect()
		}
		select {
		case <-ctx.Done():
			close(stopC)
			<-doneC
			return
		case <-doneC:
		}
		close(stopC)
		errMu.Lock()
		errCopy := lastErr
		errMu.Unlock()
		s.recordReconnect(errCopy)
		if opts.OnDisconnect != nil {
			opts.OnDisconnect(errCopy)
		}
		if !sleepWithContext(ctx, delay) {
			return
		}
		delay = nextDelay(delay)
	}
}

func (s *Source) Stats() market.SourceStats {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	return s.stats
}

// ClearLastError resets the cached websocket error so downstream stats don't
// keep reporting older failures after a successful reconnect.
func (s *Source) ClearLastError() {
	s.statsMu.Lock()
	s.stats.LastError = ""
	s.statsMu.Unlock()
}

func (s *Source) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.candleCancel != nil {
		s.candleCancel()
		s.candleCancel = nil
	}
	if s.tradeCancel != nil {
		s.tradeCancel()
		s.tradeCancel = nil
	}
	return nil
}

func buildSymbolIntervals(symbols, intervals []string) map[string][]string {
	out := make(map[string][]string)
	for _, sym := range symbols {
		upper := strings.ToUpper(strings.TrimSpace(sym))
		if upper == "" {
			continue
		}
		for _, iv := range intervals {
			interval := strings.ToLower(strings.TrimSpace(iv))
			if interval == "" {
				continue
			}
			out[upper] = appendUnique(out[upper], interval)
		}
	}
	return out
}

func appendUnique(target []string, val string) []string {
	for _, existing := range target {
		if existing == val {
			return target
		}
	}
	return append(target, val)
}

func parseFloat(v string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(v), 64)
	return f
}

func convertKlineEvent(ev *futures.WsKlineEvent) (market.CandleEvent, bool) {
	if ev == nil {
		return market.CandleEvent{}, false
	}
	c := market.Candle{
		OpenTime:  ev.Kline.StartTime,
		CloseTime: ev.Kline.EndTime,
		Open:      parseFloat(ev.Kline.Open),
		High:      parseFloat(ev.Kline.High),
		Low:       parseFloat(ev.Kline.Low),
		Close:     parseFloat(ev.Kline.Close),
		Volume:    parseFloat(ev.Kline.Volume),
		Trades:    ev.Kline.TradeNum,
	}
	symbol := strings.ToUpper(strings.TrimSpace(ev.Symbol))
	interval := strings.ToLower(strings.TrimSpace(ev.Kline.Interval))
	if symbol == "" || interval == "" {
		return market.CandleEvent{}, false
	}
	return market.CandleEvent{Symbol: symbol, Interval: interval, Candle: c}, true
}

func convertAggTradeEvent(ev *futures.WsAggTradeEvent) (market.TradeEvent, bool) {
	if ev == nil {
		return market.TradeEvent{}, false
	}
	price := parseFloat(ev.Price)
	if price <= 0 {
		return market.TradeEvent{}, false
	}
	quantity := parseFloat(ev.Quantity)
	symbol := strings.ToUpper(strings.TrimSpace(ev.Symbol))
	if symbol == "" {
		return market.TradeEvent{}, false
	}
	return market.TradeEvent{
		Symbol:    symbol,
		Price:     price,
		Quantity:  quantity,
		EventTime: ev.Time,
		TradeTime: ev.TradeTime,
	}, true
}

func sleepWithContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		d = time.Second
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func nextDelay(current time.Duration) time.Duration {
	if current <= 0 {
		return time.Second
	}
	next := current * 2
	if next > 30*time.Second {
		next = 30 * time.Second
	}
	return next
}

func (s *Source) recordSubscribeError(err error) {
	if err == nil {
		return
	}
	s.statsMu.Lock()
	s.stats.SubscribeErrors++
	s.stats.LastError = err.Error()
	s.statsMu.Unlock()
}

func (s *Source) recordReconnect(err error) {
	s.statsMu.Lock()
	s.stats.Reconnects++
	if err != nil && err.Error() != "" {
		s.stats.LastError = err.Error()
	}
	s.statsMu.Unlock()
}
