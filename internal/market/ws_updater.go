package market

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"brale/internal/logger"
)

// WSUpdater 接收 Source 提供的实时 K 线并写入 Store。
type WSUpdater struct {
	Store  KlineStore
	Max    int
	Source Source

	// 可选：连接成功/断线回调，由上层注入以实现通知
	OnConnected    func()
	OnDisconnected func(error)
	// 可选：每条行情事件的回调（用于价格缓存等）
	OnEvent func(CandleEvent)

	startOnce sync.Once
}

// WSUpdaterOption configures optional behavior for WSUpdater.
type WSUpdaterOption func(*WSUpdater)

// WithWSCallbacks sets connection callbacks invoked when the underlying stream connects/disconnects.
func WithWSCallbacks(onConnect func(), onDisconnect func(error)) WSUpdaterOption {
	return func(u *WSUpdater) {
		u.OnConnected = onConnect
		u.OnDisconnected = onDisconnect
	}
}

// WithWSEventHandler installs a handler that receives every candle event before it is persisted.
func WithWSEventHandler(handler func(CandleEvent)) WSUpdaterOption {
	return func(u *WSUpdater) {
		u.OnEvent = handler
	}
}

func NewWSUpdater(s KlineStore, max int, src Source, opts ...WSUpdaterOption) *WSUpdater {
	u := &WSUpdater{Store: s, Max: max, Source: src}
	for _, opt := range opts {
		if opt != nil {
			opt(u)
		}
	}
	return u
}

// Update 将单根 K 线写入存储（附带裁剪）
func (u *WSUpdater) Update(ctx context.Context, symbol, interval string, k Candle) error {
	return u.Store.Put(ctx, symbol, interval, []Candle{k}, u.Max)
}

// Start 订阅实时行情并持续写入 Store。
func (u *WSUpdater) Start(ctx context.Context, symbols []string, intervals []string) error {
	if u.Source == nil {
		return fmt.Errorf("ws updater missing source")
	}
	if len(symbols) == 0 || len(intervals) == 0 {
		return fmt.Errorf("ws updater requires symbols & intervals")
	}
	opts := SubscribeOptions{
		OnConnect:    u.OnConnected,
		OnDisconnect: u.OnDisconnected,
	}
	events, err := u.Source.Subscribe(ctx, symbols, intervals, opts)
	if err != nil {
		return err
	}
	u.startOnce.Do(func() {
		go u.consume(ctx, events)
	})
	logger.Infof("[WS] 订阅已启动 symbols=%v intervals=%v", symbols, intervals)
	return nil
}

func (u *WSUpdater) consume(ctx context.Context, events <-chan CandleEvent) {
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-events:
			if !ok {
				return
			}
			candle := evt.Candle
			if err := u.Update(ctx, strings.ToUpper(evt.Symbol), evt.Interval, candle); err != nil {
				logger.Warnf("[WS] 写入 %s %s 失败: %v", evt.Symbol, evt.Interval, err)
			}
			if u.OnEvent != nil {
				u.OnEvent(evt)
			}
		}
	}
}

// Stats 透传当前数据源的运行情况。
func (u *WSUpdater) Stats() SourceStats {
	if u.Source == nil {
		return SourceStats{}
	}
	return u.Source.Stats()
}

// Close 释放底层资源。
func (u *WSUpdater) Close() {
	if u.Source != nil {
		_ = u.Source.Close()
	}
}
