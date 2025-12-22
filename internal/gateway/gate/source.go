package gate

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"brale/internal/logger"
	"brale/internal/market"
	symbolpkg "brale/internal/pkg/symbol"
	"brale/internal/scheduler"

	"github.com/antihax/optional"
	gateapi "github.com/gateio/gateapi-go/v7"
	gatews "github.com/gateio/gatews/go"
	"github.com/gorilla/websocket"
)

const (
	gateSettle           = "usdt"
	gateMaxHistoryLimit  = 2000
	defaultGateREST      = "https://api.gateio.ws/api/v4"
	defaultCandleBufSize = 512
	defaultTradeBufSize  = 1024
)

type Source struct {
	cfg  Config
	rest *gateapi.APIClient

	candleMu    sync.Mutex
	candleClose context.CancelFunc

	tradeMu    sync.Mutex
	tradeClose context.CancelFunc

	statsMu sync.Mutex
	stats   market.SourceStats

	prevWSProxy func(*http.Request) (*url.URL, error)
	wsProxySet  bool
}

func New(cfg Config) (*Source, error) {
	final := cfg.withDefaults()

	restClient, err := newRESTClient(final)
	if err != nil {
		return nil, err
	}

	return &Source{
		cfg:  final,
		rest: restClient,
	}, nil
}

func newRESTClient(cfg Config) (*gateapi.APIClient, error) {
	conf := gateapi.NewConfiguration()
	conf.BasePath = strings.TrimSpace(cfg.RESTBaseURL)
	if conf.BasePath == "" {
		conf.BasePath = defaultGateREST
	}

	httpClient := &http.Client{Timeout: cfg.HTTPTimeout}
	if cfg.ProxyEnabled && cfg.RESTProxyURL != "" {
		proxyURL, err := url.Parse(cfg.RESTProxyURL)
		if err != nil {
			return nil, fmt.Errorf("invalid gate REST proxy url: %w", err)
		}
		baseTransport, ok := http.DefaultTransport.(*http.Transport)
		if !ok || baseTransport == nil {
			return nil, fmt.Errorf("http DefaultTransport is not *http.Transport")
		}
		transport := baseTransport.Clone()
		transport.Proxy = http.ProxyURL(proxyURL)
		httpClient.Transport = transport
	}
	conf.HTTPClient = httpClient
	return gateapi.NewAPIClient(conf), nil
}

func (s *Source) FetchHistory(ctx context.Context, symbol, interval string, limit int) ([]market.Candle, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > gateMaxHistoryLimit {
		limit = gateMaxHistoryLimit
	}
	normalized := symbolpkg.Normalize(symbol)
	if normalized == "" {
		return nil, fmt.Errorf("symbol is required")
	}
	exchangeSymbol := symbolpkg.Gate.ToExchange(normalized)

	interval = strings.ToLower(strings.TrimSpace(interval))
	if interval == "" {
		return nil, fmt.Errorf("interval is required")
	}

	opts := &gateapi.ListFuturesCandlesticksOpts{
		Limit:    optional.NewInt32(int32(limit)),
		Interval: optional.NewString(interval),
	}

	kls, _, err := s.rest.FuturesApi.ListFuturesCandlesticks(ctx, gateSettle, exchangeSymbol, opts)
	if err != nil {
		logger.Errorf("[gate] fetch kline failed %s %s limit=%d: %v", symbol, interval, limit, err)
		return nil, err
	}
	// Gate klines do not include taker buy/sell volume. For historical klines we approximate
	// using contract_stats.lsr_taker (taker buy / taker sell):
	//   buy  = volume * r / (1 + r)
	//   sell = volume / (1 + r)
	// Assumptions:
	// - contract_stats interval matches the requested kline interval
	// - volume unit matches (Gate returns contract size or quote; splitting by ratio preserves sum)
	// - this is historical only; WS klines will still lack taker split
	ratioByTime := make(map[int64]float64)
	if stats, _, statErr := s.rest.FuturesApi.ListContractStats(ctx, gateSettle, exchangeSymbol, &gateapi.ListContractStatsOpts{
		Interval: optional.NewString(interval),
		Limit:    optional.NewInt32(int32(limit)),
	}); statErr == nil {
		for _, st := range stats {
			if st.LsrTaker > 0 {
				ratioByTime[st.Time*1000] = float64(st.LsrTaker)
			}
		}
	} else {
		logger.Debugf("[gate] contract_stats unavailable for %s %s: %v", symbol, interval, statErr)
	}

	out := make([]market.Candle, 0, len(kls))
	for _, kl := range kls {
		openTime := int64(kl.T * 1000)
		closeTime := openTime
		if dur, ok := scheduler.ParseIntervalDuration(interval); ok {
			closeTime = openTime + dur.Milliseconds()
		}
		volume := parseFloat(kl.Sum)
		c := market.Candle{
			OpenTime:  openTime,
			CloseTime: closeTime,
			Open:      parseFloat(kl.O),
			High:      parseFloat(kl.H),
			Low:       parseFloat(kl.L),
			Close:     parseFloat(kl.C),
			Volume:    volume,
			Trades:    0,
		}
		if r, ok := ratioByTime[openTime]; ok && r > 0 {
			den := 1 + r
			c.TakerBuyVolume = volume * r / den
			c.TakerSellVolume = volume / den
		}
		out = append(out, c)
	}

	if dur, ok := scheduler.ParseIntervalDuration(interval); ok {
		out = scheduler.DropUnclosedBinanceKline(out, dur)
	}
	return out, nil
}

func (s *Source) Subscribe(ctx context.Context, symbols, intervals []string, opts market.SubscribeOptions) (<-chan market.CandleEvent, error) {
	combos, symbolMap, cleanIntervals := buildGateSubscriptions(symbols, intervals)
	if len(combos) == 0 {
		return nil, fmt.Errorf("no valid symbols or intervals for subscription")
	}
	buffer := opts.Buffer
	if buffer <= 0 {
		buffer = defaultCandleBufSize
	}

	subCtx, cancel := context.WithCancel(ctx)

	s.candleMu.Lock()
	if s.candleClose != nil {
		s.candleClose()
	}
	s.candleClose = cancel
	s.candleMu.Unlock()

	out := make(chan market.CandleEvent, buffer)
	go func() {
		defer close(out)
		s.runCandleLoop(subCtx, combos, symbolMap, cleanIntervals, out, opts)
	}()
	return out, nil
}

func (s *Source) SubscribeTrades(ctx context.Context, symbols []string, opts market.SubscribeOptions) (<-chan market.TickEvent, error) {
	contracts, symbolMap := normalizeGateSymbols(symbols)
	if len(contracts) == 0 {
		return nil, fmt.Errorf("no valid symbols for trade subscription")
	}

	buffer := opts.Buffer
	if buffer <= 0 {
		buffer = defaultTradeBufSize
	}

	subCtx, cancel := context.WithCancel(ctx)

	s.tradeMu.Lock()
	if s.tradeClose != nil {
		s.tradeClose()
	}
	s.tradeClose = cancel
	s.tradeMu.Unlock()

	out := make(chan market.TickEvent, buffer)
	go func() {
		defer close(out)
		s.runTradeLoop(subCtx, contracts, symbolMap, out, opts)
	}()
	return out, nil
}

func (s *Source) runCandleLoop(ctx context.Context, combos []gateSubscription, symbolMap map[string]string, cleanIntervals []string, out chan<- market.CandleEvent, opts market.SubscribeOptions) {
	delay := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		subCtx, cancel := context.WithCancel(ctx)
		ws, err := s.newWsService(subCtx)
		if err != nil {
			s.recordSubscribeError(err)
			if opts.OnDisconnect != nil {
				opts.OnDisconnect(err)
			}
			cancel()
			if !sleepWithContext(ctx, delay) {
				return
			}
			delay = nextDelay(delay)
			continue
		}

		ws.SetCallBack(gatews.ChannelFutureCandleStick, gatews.NewCallBack(func(msg *gatews.UpdateMsg) {
			evt, ok := convertCandleUpdate(msg, symbolMap, cleanIntervals)
			if !ok {
				return
			}
			select {
			case <-subCtx.Done():
				return
			case out <- evt:
			default:
				logger.Warnf("[gate] kline channel full, drop %s %s", evt.Symbol, evt.Interval)
			}
		}))

		var firstErr error
		for _, combo := range combos {
			if err := ws.Subscribe(gatews.ChannelFutureCandleStick, []string{combo.interval, combo.contract}); err != nil {
				firstErr = err
				s.recordSubscribeError(err)
			}
		}

		if firstErr != nil {
			if opts.OnDisconnect != nil {
				opts.OnDisconnect(firstErr)
			}
			cancel()
			if conn := ws.GetConnection(); conn != nil {
				_ = conn.Close()
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

		if err := s.monitorGateWS(subCtx, ws, opts); err != nil {
			s.recordReconnect(err)
			if opts.OnDisconnect != nil {
				opts.OnDisconnect(err)
			}
		}
		cancel()
		if conn := ws.GetConnection(); conn != nil {
			_ = conn.Close()
		}
		if !sleepWithContext(ctx, delay) {
			return
		}
		delay = nextDelay(delay)
	}
}

func (s *Source) runTradeLoop(ctx context.Context, contracts []string, symbolMap map[string]string, out chan<- market.TickEvent, opts market.SubscribeOptions) {
	delay := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		subCtx, cancel := context.WithCancel(ctx)
		ws, err := s.newWsService(subCtx)
		if err != nil {
			s.recordSubscribeError(err)
			if opts.OnDisconnect != nil {
				opts.OnDisconnect(err)
			}
			cancel()
			if !sleepWithContext(ctx, delay) {
				return
			}
			delay = nextDelay(delay)
			continue
		}

		ws.SetCallBack(gatews.ChannelFutureTrade, gatews.NewCallBack(func(msg *gatews.UpdateMsg) {
			evt, ok := convertTradeUpdate(msg, symbolMap)
			if !ok {
				return
			}
			select {
			case <-subCtx.Done():
				return
			case out <- evt:
			default:
				logger.Warnf("[gate] trade channel full, drop %s", evt.Symbol)
			}
		}))

		var firstErr error
		for _, contract := range contracts {
			if err := ws.Subscribe(gatews.ChannelFutureTrade, []string{contract}); err != nil {
				firstErr = err
				s.recordSubscribeError(err)
			}
		}

		if firstErr != nil {
			if opts.OnDisconnect != nil {
				opts.OnDisconnect(firstErr)
			}
			cancel()
			if conn := ws.GetConnection(); conn != nil {
				_ = conn.Close()
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

		if err := s.monitorGateWS(subCtx, ws, opts); err != nil {
			s.recordReconnect(err)
			if opts.OnDisconnect != nil {
				opts.OnDisconnect(err)
			}
		}
		cancel()
		if conn := ws.GetConnection(); conn != nil {
			_ = conn.Close()
		}
		if !sleepWithContext(ctx, delay) {
			return
		}
		delay = nextDelay(delay)
	}
}

func (s *Source) monitorGateWS(ctx context.Context, ws *gatews.WsService, opts market.SubscribeOptions) error {
	const (
		checkInterval = 5 * time.Second
		maxReconnect  = 30 * time.Second
	)
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	lastStatus := ""
	reconnectSince := time.Time{}
	if ws != nil {
		lastStatus = ws.Status()
		if lastStatus != "connected" {
			reconnectSince = time.Now()
		}
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if ws == nil {
				return fmt.Errorf("gate ws unavailable")
			}
			status := ws.Status()
			if status != lastStatus {
				if status == "connected" {
					reconnectSince = time.Time{}
					if opts.OnConnect != nil {
						opts.OnConnect()
					}
				} else {
					if lastStatus == "connected" {
						s.recordReconnect(nil)
					}
					if reconnectSince.IsZero() {
						reconnectSince = time.Now()
					}
					if opts.OnDisconnect != nil {
						opts.OnDisconnect(fmt.Errorf("gate ws status=%s", status))
					}
				}
				lastStatus = status
			}
			if status != "connected" {
				if reconnectSince.IsZero() {
					reconnectSince = time.Now()
				}
				if time.Since(reconnectSince) > maxReconnect {
					return fmt.Errorf("gate ws reconnect timeout (%s)", status)
				}
			} else {
				reconnectSince = time.Time{}
			}
		}
	}
}

func (s *Source) newWsService(ctx context.Context) (*gatews.WsService, error) {
	if err := s.ensureWSProxy(); err != nil {
		return nil, err
	}
	conf := gatews.NewConnConfFromOption(&gatews.ConfOptions{
		App: "futures",
		URL: gatews.FuturesUsdtUrl,
	})
	return gatews.NewWsService(ctx, nil, conf)
}

func (s *Source) ensureWSProxy() error {
	if s.wsProxySet || !s.cfg.ProxyEnabled {
		return nil
	}
	wsProxy := s.cfg.WSProxyURL
	if wsProxy == "" {
		wsProxy = s.cfg.RESTProxyURL
	}
	if strings.TrimSpace(wsProxy) == "" {
		return nil
	}
	proxyURL, err := url.Parse(wsProxy)
	if err != nil {
		return fmt.Errorf("invalid gate WS proxy url: %w", err)
	}
	s.prevWSProxy = websocket.DefaultDialer.Proxy
	websocket.DefaultDialer.Proxy = http.ProxyURL(proxyURL)
	s.wsProxySet = true
	return nil
}

func (s *Source) Stats() market.SourceStats {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	return s.stats
}

func (s *Source) ClearLastError() {
	s.statsMu.Lock()
	s.stats.LastError = ""
	s.statsMu.Unlock()
}

func (s *Source) Close() error {
	s.candleMu.Lock()
	if s.candleClose != nil {
		s.candleClose()
		s.candleClose = nil
	}
	s.candleMu.Unlock()

	s.tradeMu.Lock()
	if s.tradeClose != nil {
		s.tradeClose()
		s.tradeClose = nil
	}
	s.tradeMu.Unlock()

	if s.wsProxySet {
		websocket.DefaultDialer.Proxy = s.prevWSProxy
		s.wsProxySet = false
	}
	return nil
}

type gateSubscription struct {
	contract string
	interval string
}

func buildGateSubscriptions(symbols, intervals []string) ([]gateSubscription, map[string]string, []string) {
	contracts, symbolMap := normalizeGateSymbols(symbols)
	cleanIntervals := normalizeGateIntervals(intervals)

	var combos []gateSubscription
	for _, c := range contracts {
		for _, iv := range cleanIntervals {
			combos = append(combos, gateSubscription{contract: c, interval: iv})
		}
	}
	return combos, symbolMap, cleanIntervals
}

func normalizeGateIntervals(intervals []string) []string {
	if len(intervals) == 0 {
		return nil
	}
	out := make([]string, 0, len(intervals))
	seen := make(map[string]struct{}, len(intervals))
	for _, iv := range intervals {
		norm := strings.ToLower(strings.TrimSpace(iv))
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

func normalizeGateSymbols(symbols []string) ([]string, map[string]string) {
	symbolMap := make(map[string]string)
	out := make([]string, 0, len(symbols))
	seen := make(map[string]struct{}, len(symbols))
	for _, sym := range symbols {
		norm := symbolpkg.Normalize(sym)
		if norm == "" {
			continue
		}
		contract := symbolpkg.Gate.ToExchange(norm)
		if contract == "" {
			continue
		}
		contract = strings.ToUpper(contract)
		if _, ok := seen[contract]; ok {
			continue
		}
		seen[contract] = struct{}{}
		symbolMap[contract] = norm
		out = append(out, contract)
	}
	return out, symbolMap
}

func convertCandleUpdate(msg *gatews.UpdateMsg, symbolMap map[string]string, intervals []string) (market.CandleEvent, bool) {
	if msg == nil {
		return market.CandleEvent{}, false
	}
	interval, contract, body, ok := parseGateCandlePayload(msg.Result)
	if !ok {
		return market.CandleEvent{}, false
	}

	if interval == "" && len(intervals) == 1 {
		interval = intervals[0]
	}
	interval = strings.ToLower(strings.TrimSpace(interval))
	if interval == "" {
		return market.CandleEvent{}, false
	}

	contract = strings.ToUpper(strings.TrimSpace(contract))
	if contract == "" {
		contract = strings.ToUpper(strings.TrimSpace(body.N))
	}
	if contract == "" {
		return market.CandleEvent{}, false
	}

	symbol := symbolpkg.Gate.FromExchange(contract)
	if original, ok := symbolMap[contract]; ok && original != "" {
		symbol = original
	}
	if symbol == "" {
		return market.CandleEvent{}, false
	}

	openTime := body.T * 1000
	closeTime := openTime
	if dur, ok := scheduler.ParseIntervalDuration(interval); ok {
		closeTime = openTime + dur.Milliseconds()
	}
	volume := parseFloat(body.Amount)
	if volume == 0 && body.V > 0 {
		volume = float64(body.V)
	}

	c := market.Candle{
		OpenTime:  openTime,
		CloseTime: closeTime,
		Open:      parseFloat(body.O),
		High:      parseFloat(body.H),
		Low:       parseFloat(body.L),
		Close:     parseFloat(body.C),
		Volume:    volume,
		Trades:    0,
	}

	return market.CandleEvent{
		Symbol:   symbol,
		Interval: interval,
		Candle:   c,
	}, true
}

func parseGateCandlePayload(raw json.RawMessage) (string, string, gatews.FuturesCandlestick, bool) {
	var body gatews.FuturesCandlestick

	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err == nil && len(arr) > 0 {
		var interval, contract string
		_ = json.Unmarshal(arr[0], &interval)
		if len(arr) > 1 {
			_ = json.Unmarshal(arr[1], &contract)
		}
		_ = json.Unmarshal(arr[len(arr)-1], &body)
		return interval, contract, body, true
	}

	if err := json.Unmarshal(raw, &body); err == nil {
		return "", "", body, true
	}
	return "", "", gatews.FuturesCandlestick{}, false
}

func convertTradeUpdate(msg *gatews.UpdateMsg, symbolMap map[string]string) (market.TickEvent, bool) {
	if msg == nil {
		return market.TickEvent{}, false
	}

	var trade gatews.FuturesTrade

	var arr []json.RawMessage
	if err := json.Unmarshal(msg.Result, &arr); err == nil && len(arr) > 0 {
		_ = json.Unmarshal(arr[len(arr)-1], &trade)
		if trade.Contract == "" {
			var contract string
			_ = json.Unmarshal(arr[0], &contract)
			trade.Contract = contract
		}
	} else if err := json.Unmarshal(msg.Result, &trade); err != nil {
		return market.TickEvent{}, false
	}

	contract := strings.ToUpper(strings.TrimSpace(trade.Contract))
	if contract == "" {
		return market.TickEvent{}, false
	}

	symbol := symbolpkg.Gate.FromExchange(contract)
	if original, ok := symbolMap[contract]; ok && original != "" {
		symbol = original
	}
	if symbol == "" {
		return market.TickEvent{}, false
	}

	price := parseFloat(trade.Price)
	if price <= 0 {
		return market.TickEvent{}, false
	}

	quantity := math.Abs(float64(trade.Size))
	eventTime := trade.CreateTimeMs
	if eventTime == 0 && trade.CreateTime != 0 {
		eventTime = trade.CreateTime * 1000
	}

	return market.TickEvent{
		Symbol:    symbol,
		Price:     price,
		Quantity:  quantity,
		EventTime: eventTime,
		TradeTime: eventTime,
	}, true
}

func parseFloat(v string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(v), 64)
	return f
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

func sleepWithContext(ctx context.Context, d time.Duration) bool {
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
