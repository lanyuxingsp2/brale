package market

import (
	"context"
	"encoding/json"
	"fmt"
	_ "log"
	"strconv"
	"strings"
	"sync"
	"time"

	"brale/internal/logger"
	"brale/internal/store"
	"github.com/gorilla/websocket"
)

// 中文说明：
// WSUpdater 负责将订阅到的 K 线数据写入抽象的 KlineStore。
// 首版提供一个简单的 Update 方法，便于单元测试离线验证；
// 实际的 WS 接入将复用旧版 market.NewWSMonitor 并在收到事件时调用 Update。

type WSUpdater struct {
    Store  *store.MemoryKlineStore
    Max    int // 每个周期最多缓存条数
    Client *CombinedStreamsClient
    // 可选：连接成功/断线回调，由上层（main）注入以实现通知
    OnConnected    func()
    OnDisconnected func(err error)
}

func NewWSUpdater(s *store.MemoryKlineStore, max int) *WSUpdater {
	return &WSUpdater{Store: s, Max: max}
}

// Update 将单根 K 线写入存储（附带裁剪）
func (u *WSUpdater) Update(ctx context.Context, symbol, interval string, k store.Kline) error {
	// 为简单起见，这里单条写入时复用 Put 批量接口
	return u.Store.Put(ctx, symbol, interval, []store.Kline{k}, u.Max)
}

// StartRealWS 预留：对接旧版 WS 监控器，订阅多个周期与符号
// 注意：该方法涉及真实网络，建议集成测试或手动运行时使用。
func (u *WSUpdater) StartRealWS(symbols []string, intervals []string, batchSize int) {
    client := NewCombinedStreamsClient(batchSize)
    // 将回调透传给底层 client
    client.SetOnConnect(func() {
        logger.Infof("[WS] 连接成功")
        if u.OnConnected != nil { u.OnConnected() }
    })
    client.SetOnDisconnect(func(err error) {
        if err != nil { logger.Warnf("[WS] 断线: %v", err) } else { logger.Warnf("[WS] 断线") }
        if u.OnDisconnected != nil { u.OnDisconnected(err) }
    })
    if err := client.Connect(); err != nil {
        logger.Warnf("[WS] 连接失败: %v", err)
        return
    }
	u.Client = client
	for _, sym := range symbols {
		for _, iv := range intervals {
			stream := strings.ToLower(sym) + "@kline_" + iv
			ch := client.AddSubscriber(stream, 100)
			go func(symbol, interval string, c <-chan []byte) {
				for msg := range c {
					var data KlineWSData
					if err := json.Unmarshal(msg, &data); err != nil {
						logger.Warnf("[WS] 解析失败: %v", err)
						continue
					}
					u.handleWSKline(context.Background(), symbol, interval, data)
				}
			}(sym, iv, ch)
		}
	}
	for _, iv := range intervals {
		if err := client.BatchSubscribeKlines(symbols, iv); err != nil {
			logger.Warnf("[WS] 订阅 %s 失败: %v", iv, err)
		}
	}
	logger.Infof("[WS] 已启动订阅：symbols=%v intervals=%v batch=%d", symbols, intervals, batchSize)
}

// handleWSKline 将 WS K 线事件写入存储
func (u *WSUpdater) handleWSKline(ctx context.Context, symbol, interval string, d KlineWSData) {
	// 解析字符串为浮点
	pf := func(s string) float64 {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0
		}
		return v
	}
    k := store.Kline{
        OpenTime:  d.Kline.StartTime,
        CloseTime: d.Kline.CloseTime,
        Open:      pf(string(d.Kline.OpenPrice)),
        High:      pf(string(d.Kline.HighPrice)),
        Low:       pf(string(d.Kline.LowPrice)),
        Close:     pf(string(d.Kline.ClosePrice)),
        Volume:    pf(string(d.Kline.Volume)),
    }
    // Debug：打印收到的 WS K 线要点
    // 附加人类可读时间（UTC）
    tOpen := time.UnixMilli(k.OpenTime).UTC().Format(time.RFC3339)
    tClose := time.UnixMilli(k.CloseTime).UTC().Format(time.RFC3339)
    logger.Debugf("[WS] kline %s %s t=%d(%s)->%d(%s) close=%.6f vol=%.3f final=%v trades=%d",
        strings.ToUpper(symbol), interval, k.OpenTime, tOpen, k.CloseTime, tClose, k.Close, k.Volume, d.Kline.IsFinal, d.Kline.NumberOfTrades)
    if err := u.Update(ctx, strings.ToUpper(symbol), interval, k); err != nil {
        logger.Warnf("[WS] 更新存储失败: %v", err)
        return
    }
    if u.Store != nil {
        if cur, err := u.Store.Get(ctx, strings.ToUpper(symbol), interval); err == nil && len(cur) > 0 {
            last := cur[len(cur)-1]
            logger.Debugf("[WS] 缓存 %s %s 共=%d 尾收=%.6f@%d", strings.ToUpper(symbol), interval, len(cur), last.Close, last.CloseTime)
        }
    }
}

// 组合流客户端（最小实现）
type CombinedStreamsClient struct {
    conn        *websocket.Conn
    mu          sync.RWMutex
    subscribers map[string]chan []byte
    reconnect   bool
    done        chan struct{}
    batchSize   int
    subscribed  map[string]bool    // 已订阅流集合，用于重连后重放
    pending     map[int64][]string // 等待 ACK 的订阅 id -> params

	// 运行时统计
    stats struct {
        reconnects      int
        subscribeErrors int
        lastErr         string
    }
    // 事件回调
    onConnect    func()
    onDisconnect func(error)
}

func NewCombinedStreamsClient(batchSize int) *CombinedStreamsClient {
    return &CombinedStreamsClient{batchSize: batchSize, subscribers: map[string]chan []byte{}, subscribed: map[string]bool{}, pending: map[int64][]string{}, reconnect: true, done: make(chan struct{})}
}

func (c *CombinedStreamsClient) Connect() error {
    d := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
    conn, _, err := d.Dial("wss://fstream.binance.com/stream", nil)
    if err != nil {
        return err
    }
    c.mu.Lock()
    c.conn = conn
    c.mu.Unlock()
    go c.read()
    // 连接成功回调
    if c.onConnect != nil { c.onConnect() }
    return nil
}

// 事件回调设置
func (c *CombinedStreamsClient) SetOnConnect(fn func()) { c.onConnect = fn }
func (c *CombinedStreamsClient) SetOnDisconnect(fn func(error)) { c.onDisconnect = fn }

func (c *CombinedStreamsClient) AddSubscriber(stream string, buf int) <-chan []byte {
	ch := make(chan []byte, buf)
	c.mu.Lock()
	c.subscribers[stream] = ch
	c.mu.Unlock()
	return ch
}

func (c *CombinedStreamsClient) BatchSubscribeKlines(symbols []string, interval string) error {
	// 分批
	for i := 0; i < len(symbols); i += c.batchSize {
		end := i + c.batchSize
		if end > len(symbols) {
			end = len(symbols)
		}
		var params []string
		for _, s := range symbols[i:end] {
			params = append(params, strings.ToLower(s)+"@kline_"+interval)
		}
		if err := c.subscribe(params); err != nil {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil
}

func (c *CombinedStreamsClient) subscribe(params []string) error {
    id := time.Now().UnixNano()
    msg := map[string]any{"method": "SUBSCRIBE", "params": params, "id": id}
    if len(params) > 0 {
        sample := params[0]
        if len(params) > 1 { sample += ",..." }
        logger.Debugf("[WS] 发送订阅 id=%d 数量=%d 示例=%s", id, len(params), sample)
    }
    // 最多重试 3 次
    for attempt := 1; attempt <= 3; attempt++ {
		c.mu.RLock()
		conn := c.conn
		c.mu.RUnlock()
		if conn == nil {
			return fmt.Errorf("ws not connected")
		}
		if err := conn.WriteJSON(msg); err != nil {
			if attempt == 3 {
				return err
			}
			time.Sleep(200 * time.Millisecond)
			continue
		}
		// 写成功：记录订阅集合和 pending ack
		c.mu.Lock()
		for _, p := range params {
			c.subscribed[p] = true
		}
		c.pending[id] = params
		c.mu.Unlock()
		return nil
	}
	return fmt.Errorf("subscribe failed after retries")
}

func (c *CombinedStreamsClient) read() {
	for {
		select {
		case <-c.done:
			return
		default:
		}
		c.mu.RLock()
		conn := c.conn
		c.mu.RUnlock()
		if conn == nil {
			time.Sleep(time.Second)
			continue
		}
        _, b, err := conn.ReadMessage()
        if err != nil {
            logger.Warnf("[WS] 读失败: %v，尝试重连...", err)
            if c.onDisconnect != nil { c.onDisconnect(err) }
            c.mu.Lock()
            c.stats.reconnects++
            c.stats.lastErr = err.Error()
            c.mu.Unlock()
            time.Sleep(2 * time.Second)
			// 重连
			if err := c.Connect(); err != nil {
				logger.Warnf("[WS] 重连失败: %v", err)
				continue
			}
			// 重放订阅
			c.mu.RLock()
			streams := make([]string, 0, len(c.subscribed))
			for s := range c.subscribed {
				streams = append(streams, s)
			}
			c.mu.RUnlock()
			// 分批按原样订阅
			for i := 0; i < len(streams); i += c.batchSize {
				end := i + c.batchSize
				if end > len(streams) {
					end = len(streams)
				}
				if err := c.subscribe(streams[i:end]); err != nil {
					logger.Warnf("[WS] 重放订阅失败: %v", err)
				}
				time.Sleep(100 * time.Millisecond)
			}
			continue
		}
		// 解析两类消息：数据帧 或 订阅 ACK 帧
		var frame struct {
			Stream string          `json:"stream"`
			Data   json.RawMessage `json:"data"`
		}
        if err := json.Unmarshal(b, &frame); err == nil && frame.Stream != "" {
            c.mu.RLock()
            ch := c.subscribers[frame.Stream]
            c.mu.RUnlock()
            logger.Debugf("[WS] 数据帧 stream=%s bytes=%d", frame.Stream, len(frame.Data))
            if ch != nil {
                select {
                case ch <- frame.Data:
                default:
                }
            }
            continue
        }
		var ack struct {
			Result any   `json:"result"`
			ID     int64 `json:"id"`
		}
		if err := json.Unmarshal(b, &ack); err == nil && ack.ID != 0 {
			c.mu.Lock()
			delete(c.pending, ack.ID)
			c.mu.Unlock()
			logger.Debugf("[WS] 订阅 ACK: id=%v result=%v", ack.ID, ack.Result)
			continue
		}

		// 错误帧，如 {"code":-1102, "msg":"..."}
		var eframe struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
			ID   int64  `json:"id"`
		}
		if err := json.Unmarshal(b, &eframe); err == nil && eframe.Code != 0 {
			logger.Warnf("[WS] 订阅错误: code=%d msg=%s id=%d", eframe.Code, eframe.Msg, eframe.ID)
			c.mu.Lock()
			c.stats.subscribeErrors++
			c.stats.lastErr = eframe.Msg
			params := c.pending[eframe.ID]
			delete(c.pending, eframe.ID)
			c.mu.Unlock()
			// 失败重试（单次重放）
			if len(params) > 0 {
				if err := c.subscribe(params); err != nil {
					logger.Warnf("[WS] 订阅错误后重试失败: %v", err)
				}
			}
			continue
		}
	}
}

// Stats 返回当前运行统计的一份拷贝
func (c *CombinedStreamsClient) Stats() (reconnects, subscribeErrors int, lastErr string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.stats.reconnects, c.stats.subscribeErrors, c.stats.lastErr
}

// Binance K 线事件体（最小字段）
type KlineWSData struct {
    EventType string `json:"e"`
    EventTime int64  `json:"E"`
    Symbol    string `json:"s"`
    Kline     struct {
        StartTime           int64  `json:"t"`
        CloseTime           int64  `json:"T"`
        Symbol              string `json:"s"`
        Interval            string `json:"i"`
        OpenPrice           StrOrNum `json:"o"`
        ClosePrice          StrOrNum `json:"c"`
        HighPrice           StrOrNum `json:"h"`
        LowPrice            StrOrNum `json:"l"`
        Volume              StrOrNum `json:"v"`
        NumberOfTrades      int    `json:"n"`
        IsFinal             bool   `json:"x"`
        QuoteVolume         StrOrNum `json:"q"`
        TakerBuyBaseVolume  StrOrNum `json:"V"`
        TakerBuyQuoteVolume StrOrNum `json:"Q"`
    } `json:"k"`
}

// StrOrNum 接受字符串或数字，并以字符串形式保存原始值
type StrOrNum string

func (sn *StrOrNum) UnmarshalJSON(b []byte) error {
    // 如果是字符串，直接去引号
    if len(b) > 0 && b[0] == '"' {
        var s string
        if err := json.Unmarshal(b, &s); err != nil {
            return err
        }
        *sn = StrOrNum(s)
        return nil
    }
    // 否则视为数字字面量，按原样记录
    *sn = StrOrNum(string(b))
    return nil
}
