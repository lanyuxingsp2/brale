package store

import (
	"context"
	"errors"
	"sync"

	"brale/internal/market"
)

// KlineStore 抽象：读写 symbol+interval 的序列
type KlineStore interface {
	Put(ctx context.Context, symbol, interval string, ks []market.Candle, max int) error
	Get(ctx context.Context, symbol, interval string) ([]market.Candle, error)
}

// SnapshotExporter 导出固定窗口 K 线的抽象。
type SnapshotExporter interface {
	Export(ctx context.Context, symbol, interval string, limit int) ([]market.Candle, error)
}

// MemoryKlineStore 内存实现
type MemoryKlineStore struct {
	shards []klineShard
}

type klineShard struct {
	mu   sync.RWMutex
	data map[string][]market.Candle
}

const defaultShardCount = 32

func NewMemoryKlineStore() *MemoryKlineStore {
	return newMemoryKlineStore(defaultShardCount)
}

func newMemoryKlineStore(shards int) *MemoryKlineStore {
	if shards <= 0 {
		shards = 1
	}
	out := &MemoryKlineStore{
		shards: make([]klineShard, shards),
	}
	for i := range out.shards {
		out.shards[i] = klineShard{data: make(map[string][]market.Candle)}
	}
	return out
}

func (s *MemoryKlineStore) shardFor(key string) *klineShard {
	if len(s.shards) == 0 {
		s.shards = make([]klineShard, defaultShardCount)
		for i := range s.shards {
			s.shards[i] = klineShard{data: make(map[string][]market.Candle)}
		}
	}
	idx := hashKey(key) % uint32(len(s.shards))
	return &s.shards[idx]
}

func key(symbol, interval string) string { return symbol + "@" + interval }

// Put 追加并裁剪
func (s *MemoryKlineStore) Put(ctx context.Context, symbol, interval string, ks []market.Candle, max int) error {
	if symbol == "" || interval == "" {
		return errors.New("symbol/interval 不能为空")
	}
	if len(ks) == 0 {
		return nil
	}
	if max <= 0 {
		max = 100
	}
	k := key(symbol, interval)
	sh := s.shardFor(k)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	cur := sh.data[k]
	for _, candle := range ks {
		n := len(cur)
		if n > 0 && cur[n-1].OpenTime == candle.OpenTime {
			// 同一根 K 线的增量更新，覆盖末尾而非重复追加。
			cur[n-1] = candle
			continue
		}
		cur = append(cur, candle)
	}
	if len(cur) > max {
		cur = cur[len(cur)-max:]
	}
	sh.data[k] = cur
	return nil
}

// Set 全量替换指定 symbol+interval 的序列
func (s *MemoryKlineStore) Set(ctx context.Context, symbol, interval string, ks []market.Candle) error {
	if symbol == "" || interval == "" {
		return errors.New("symbol/interval 不能为空")
	}
	k := key(symbol, interval)
	sh := s.shardFor(k)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	dst := make([]market.Candle, len(ks))
	copy(dst, ks)
	sh.data[k] = dst
	return nil
}

// Get 返回拷贝
func (s *MemoryKlineStore) Get(ctx context.Context, symbol, interval string) ([]market.Candle, error) {
	k := key(symbol, interval)
	sh := s.shardFor(k)
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	cur := sh.data[k]
	out := make([]market.Candle, len(cur))
	copy(out, cur)
	return out, nil
}

// Export 返回最近 limit 根 K 线（按时间升序）
func (s *MemoryKlineStore) Export(ctx context.Context, symbol, interval string, limit int) ([]market.Candle, error) {
	if symbol == "" || interval == "" {
		return nil, errors.New("symbol/interval 不能为空")
	}
	if limit <= 0 {
		return nil, nil
	}
	k := key(symbol, interval)
	sh := s.shardFor(k)
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	cur := sh.data[k]
	if len(cur) == 0 {
		return nil, nil
	}
	if limit > len(cur) {
		limit = len(cur)
	}
	out := make([]market.Candle, limit)
	copy(out, cur[len(cur)-limit:])
	return out, nil
}

func hashKey(s string) uint32 {
	const (
		offset32 = 2166136261
		prime32  = 16777619
	)
	var h uint32 = offset32
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= prime32
	}
	return h
}
