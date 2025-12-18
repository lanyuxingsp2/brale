package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"brale/internal/agent/ports"
	"brale/internal/gateway/exchange"
	"brale/internal/gateway/notifier"
	"brale/internal/logger"
	"brale/internal/market"
)

// PriceObserver defines a consumer of real-time price updates.
type PriceObserver interface {
	NotifyPrice(symbol string, price float64)
}

type MonitorParams struct {
	Updater        *market.WSUpdater
	KlineStore     market.KlineStore
	Symbols        []string
	Intervals      []string
	HorizonSummary string
	WarmupSummary  string
	Telegram       *notifier.Telegram
	ExecManager    ports.ExecutionManager
	Observer       PriceObserver
}

type PriceMonitor struct {
	updater        *market.WSUpdater
	ks             market.KlineStore
	symbols        []string
	intervals      []string
	horizonSummary string
	warmupSummary  string
	tg             *notifier.Telegram
	execManager    ports.ExecutionManager
	observer       PriceObserver

	priceCache   map[string]cachedQuote
	priceCacheMu sync.RWMutex
	lastPrice    map[string]lastPriceEntry
	lastPriceMu  sync.RWMutex

	tradeStreamMu sync.Mutex
	tradeStreamUp bool
}

type cachedQuote struct {
	quote exchange.PriceQuote
	ts    int64
}

type lastPriceEntry struct {
	price float64
	ts    int64
}

const lastPriceMaxAge = 10 * time.Second

func NewPriceMonitor(p MonitorParams) *PriceMonitor {
	if p.Updater == nil && p.KlineStore == nil {
		return nil
	}
	return &PriceMonitor{
		updater:        p.Updater,
		ks:             p.KlineStore,
		symbols:        append([]string(nil), p.Symbols...),
		intervals:      append([]string(nil), p.Intervals...),
		horizonSummary: p.HorizonSummary,
		warmupSummary:  p.WarmupSummary,
		tg:             p.Telegram,
		execManager:    p.ExecManager,
		observer:       p.Observer,
		priceCache:     make(map[string]cachedQuote),
		lastPrice:      make(map[string]lastPriceEntry),
	}
}

func (m *PriceMonitor) Start(ctx context.Context) {
	if m == nil {
		return
	}
	if m.updater != nil {
		firstWSConnected := false
		m.updater.OnEvent = m.onCandleEvent
		m.updater.OnConnected = func() {
			m.clearWSLastError()
			if m.tg == nil {
				return
			}
			if !firstWSConnected {
				firstWSConnected = true
				msg := "*Brale 启动成功* ✅\nWS 已连接并开始订阅"
				if summary := strings.TrimSpace(m.horizonSummary); summary != "" {
					msg += "\n```text\n" + summary + "\n```"
				}
				if warmup := strings.TrimSpace(m.warmupSummary); warmup != "" {
					msg += "\n" + warmup
				}
				_ = m.tg.SendText(msg)
			}
		}
		m.updater.OnDisconnected = func(err error) {
			if m.tg == nil {
				return
			}
			msg := "WS 断线"
			if err != nil {
				msg = msg + ": " + err.Error()
			}
			_ = m.tg.SendText(msg)
		}
		go func() {
			if err := m.updater.Start(ctx, m.symbols, m.intervals); err != nil {
				logger.Errorf("启动行情订阅失败: %v", err)
			}
		}()
	}
	m.startTradePriceStream(ctx)
}

func (m *PriceMonitor) Close() {
	if m == nil {
		return
	}
	if m.updater != nil {
		m.updater.Close()
	}
}

func (m *PriceMonitor) clearWSLastError() {
	if m == nil || m.updater == nil || m.updater.Source == nil {
		return
	}
	if resetter, ok := m.updater.Source.(wsErrorResetter); ok {
		resetter.ClearLastError()
	}
}

func (m *PriceMonitor) startTradePriceStream(ctx context.Context) {
	if m == nil || m.updater == nil || m.updater.Source == nil {
		logger.Warnf("实时成交价订阅跳过：缺少行情源")
		return
	}
	opts := market.SubscribeOptions{
		Buffer: 2048,
		OnConnect: func() {
			if ctx.Err() != nil {
				return
			}
			m.tradeStreamMu.Lock()
			wasUp := m.tradeStreamUp
			m.tradeStreamUp = true
			m.tradeStreamMu.Unlock()
			if m.tg != nil {
				msg := "实时成交价流已建立 ✅"
				if wasUp {
					msg = "实时成交价流已恢复 ✅"
				}
				_ = m.tg.SendText(msg)
			}
		},
		OnDisconnect: func(err error) {
			if ctx.Err() != nil {
				return
			}
			m.tradeStreamMu.Lock()
			m.tradeStreamUp = false
			m.tradeStreamMu.Unlock()
			if m.tg != nil {
				reason := "未知"
				if err != nil && err.Error() != "" {
					reason = err.Error()
				}
				_ = m.tg.SendText(fmt.Sprintf("实时成交价流断线 ⚠️\n错误: %s", reason))
			}
		},
	}
	stream, err := m.updater.Source.SubscribeTrades(ctx, m.symbols, opts)
	if err != nil {
		logger.Warnf("订阅实时成交价失败: %v", err)
		return
	}
	logger.Infof("✓ 实时成交价订阅已启动 (aggTrade)")
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-stream:
				if !ok {
					return
				}
				m.handleTradePrice(ev)
			}
		}
	}()
}

func (m *PriceMonitor) handleTradePrice(ev market.TickEvent) {
	if m == nil {
		return
	}
	price := ev.Price
	if price <= 0 {
		return
	}
	symbol := strings.ToUpper(strings.TrimSpace(ev.Symbol))
	if symbol == "" {
		return
	}
	ts := ev.EventTime
	if ts == 0 {
		ts = ev.TradeTime
	}
	if ts == 0 {
		ts = time.Now().UnixMilli()
	}
	m.lastPriceMu.Lock()
	m.lastPrice[symbol] = lastPriceEntry{price: price, ts: ts}
	m.lastPriceMu.Unlock()
	m.priceCacheMu.Lock()
	cq := m.priceCache[symbol]
	cq.quote.Last = price
	cq.ts = ts
	m.priceCache[symbol] = cq
	m.priceCacheMu.Unlock()

	if m.observer != nil {
		m.observer.NotifyPrice(symbol, price)
	}
}

func (m *PriceMonitor) freshLastPrice(symbol string) (float64, bool) {
	if m == nil {
		return 0, false
	}
	m.lastPriceMu.RLock()
	entry, ok := m.lastPrice[symbol]
	m.lastPriceMu.RUnlock()
	if !ok || entry.price <= 0 {
		return 0, false
	}
	if entry.ts <= 0 {
		return entry.price, true
	}
	if time.Since(time.UnixMilli(entry.ts)) > lastPriceMaxAge {
		return 0, false
	}
	return entry.price, true
}

func (m *PriceMonitor) LatestPrice(ctx context.Context, symbol string) float64 {
	if m == nil {
		return 0
	}
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	if lp, ok := m.freshLastPrice(symbol); ok {
		return lp
	}
	quote := m.LatestPriceQuote(ctx, symbol)
	return quote.Last
}

func (m *PriceMonitor) LatestPriceQuote(ctx context.Context, symbol string) exchange.PriceQuote {
	var quote exchange.PriceQuote
	if m == nil || m.ks == nil {
		return quote
	}
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	lp, lastPriceFresh := m.freshLastPrice(symbol)
	if cached, ok := m.cachedQuote(symbol); ok {
		quote = cached
		if lastPriceFresh {
			quote.Last = lp
		}
		return quote
	}
	interval := "1m"
	if len(m.intervals) > 0 {
		interval = m.intervals[0]
	}
	klines, err := m.ks.Get(ctx, symbol, interval)
	if err != nil || len(klines) == 0 {
		return quote
	}
	last := klines[len(klines)-1]
	ts := last.CloseTime
	if ts == 0 {
		ts = last.OpenTime
	}
	if ts > 0 {
		const maxAge = 30 * time.Second
		age := time.Since(time.UnixMilli(ts))
		if age > maxAge {
			logger.Warnf("价格回退数据过期，跳过自动触发: %s %s age=%s", symbol, interval, age.Truncate(time.Second))
			return quote
		}
	}
	quote.Last = last.Close
	quote.High = last.High
	quote.Low = last.Low
	if lastPriceFresh {
		quote.Last = lp
	}
	return quote
}

func (m *PriceMonitor) cachedQuote(symbol string) (exchange.PriceQuote, bool) {
	if m == nil {
		return exchange.PriceQuote{}, false
	}
	m.priceCacheMu.RLock()
	cq, ok := m.priceCache[symbol]
	m.priceCacheMu.RUnlock()
	if !ok || (cq.quote.Last <= 0 && cq.quote.High <= 0 && cq.quote.Low <= 0) {
		return exchange.PriceQuote{}, false
	}
	return cq.quote, true
}

func (m *PriceMonitor) onCandleEvent(evt market.CandleEvent) {
	if m == nil {
		return
	}
	symbol := strings.ToUpper(strings.TrimSpace(evt.Symbol))
	if symbol == "" {
		return
	}
	c := evt.Candle
	if c.Close <= 0 && c.High <= 0 && c.Low <= 0 {
		return
	}
	ts := c.CloseTime
	if ts == 0 {
		ts = c.OpenTime
	}

	q := exchange.PriceQuote{Symbol: symbol, Last: c.Close, High: c.High, Low: c.Low, UpdatedAt: time.UnixMilli(ts)}
	m.priceCacheMu.Lock()
	m.priceCache[symbol] = cachedQuote{quote: q, ts: ts}
	m.priceCacheMu.Unlock()
}

func (m *PriceMonitor) GetLatestPriceQuote(ctx context.Context, symbol string) (exchange.PriceQuote, error) {
	var empty exchange.PriceQuote
	if m == nil {
		return empty, fmt.Errorf("price monitor 未初始化")
	}
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	if symbol == "" {
		return empty, fmt.Errorf("symbol 不能为空")
	}
	quote := m.LatestPriceQuote(ctx, symbol)
	if quote.Last <= 0 {
		return quote, fmt.Errorf("未获取到 %s 的最新价格", symbol)
	}
	return quote, nil
}

func (m *PriceMonitor) Stats() market.SourceStats {
	if m == nil || m.updater == nil {
		return market.SourceStats{}
	}
	return m.updater.Stats()
}

type wsErrorResetter interface {
	ClearLastError()
}
