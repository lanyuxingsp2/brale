package store

import (
	"context"
	"errors"
	"sync"
)

// Kline 简化的 K 线结构
type Kline struct {
	OpenTime  int64
	CloseTime int64
	Open      float64
	High      float64
	Low       float64
	Close     float64
	Volume    float64
}

// KlineStore 抽象：读写 symbol+interval 的序列
type KlineStore interface {
	Put(ctx context.Context, symbol, interval string, ks []Kline, max int) error
	Get(ctx context.Context, symbol, interval string) ([]Kline, error)
}

// MemoryKlineStore 内存实现
type MemoryKlineStore struct {
	mu   sync.RWMutex
	data map[string][]Kline
}

func NewMemoryKlineStore() *MemoryKlineStore {
	return &MemoryKlineStore{data: make(map[string][]Kline)}
}
func key(symbol, interval string) string { return symbol + "@" + interval }

// Put 追加并裁剪
func (s *MemoryKlineStore) Put(ctx context.Context, symbol, interval string, ks []Kline, max int) error {
	if symbol == "" || interval == "" {
		return errors.New("symbol/interval 不能为空")
	}
	if len(ks) == 0 {
		return nil
	}
	if max <= 0 {
		max = 100
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(symbol, interval)
	cur := s.data[k]
	cur = append(cur, ks...)
	if len(cur) > max {
		cur = cur[len(cur)-max:]
	}
	s.data[k] = cur
	return nil
}

// Get 返回拷贝
func (s *MemoryKlineStore) Get(ctx context.Context, symbol, interval string) ([]Kline, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cur := s.data[key(symbol, interval)]
	out := make([]Kline, len(cur))
	copy(out, cur)
	return out, nil
}
