package app

import (
	"strings"
	"sync"
	"time"

	"brale/internal/decision"
)

// lastDecisionCache 缓存最近一次 AI 决策，供 Prompt 注入。
type lastDecisionCache struct {
	mu   sync.RWMutex
	data map[string]decision.DecisionMemory // key: symbol upper
	ttl  time.Duration
}

func newLastDecisionCache(ttl time.Duration) *lastDecisionCache {
	if ttl <= 0 {
		ttl = time.Hour
	}
	return &lastDecisionCache{data: make(map[string]decision.DecisionMemory), ttl: ttl}
}

func (c *lastDecisionCache) Load(records []decision.LastDecisionRecord) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, rec := range records {
		sym := strings.ToUpper(strings.TrimSpace(rec.Symbol))
		if sym == "" {
			continue
		}
		c.data[sym] = decision.DecisionMemory{
			Symbol:    sym,
			Horizon:   rec.Horizon,
			DecidedAt: rec.DecidedAt,
			Decisions: append([]decision.Decision(nil), rec.Decisions...),
		}
	}
}

func (c *lastDecisionCache) Set(mem decision.DecisionMemory) {
	if c == nil {
		return
	}
	sym := strings.ToUpper(strings.TrimSpace(mem.Symbol))
	if sym == "" {
		return
	}
	c.mu.Lock()
	mem.Symbol = sym
	c.data[sym] = mem
	c.mu.Unlock()
}

func (c *lastDecisionCache) Delete(symbol string) {
	if c == nil {
		return
	}
	sym := strings.ToUpper(strings.TrimSpace(symbol))
	if sym == "" {
		return
	}
	c.mu.Lock()
	delete(c.data, sym)
	c.mu.Unlock()
}

func (c *lastDecisionCache) Reset() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.data = make(map[string]decision.DecisionMemory)
	c.mu.Unlock()
}

func (c *lastDecisionCache) Snapshot(now time.Time) []decision.DecisionMemory {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.data) == 0 {
		return nil
	}
	out := make([]decision.DecisionMemory, 0, len(c.data))
	for _, mem := range c.data {
		if c.ttl > 0 && now.Sub(mem.DecidedAt) > c.ttl {
			continue
		}
		dup := mem
		dup.Decisions = append([]decision.Decision(nil), mem.Decisions...)
		out = append(out, dup)
	}
	return out
}
