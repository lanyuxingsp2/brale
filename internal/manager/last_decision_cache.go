package manager

import (
	"strings"
	"sync"
	"time"

	"brale/internal/ai"
)

// lastDecisionCache 缓存最近一次 AI 决策，供 Prompt 注入。
type lastDecisionCache struct {
	mu   sync.RWMutex
	data map[string]ai.DecisionMemory // key: symbol upper
	ttl  time.Duration
}

func newLastDecisionCache(ttl time.Duration) *lastDecisionCache {
	if ttl <= 0 {
		ttl = time.Hour
	}
	return &lastDecisionCache{data: make(map[string]ai.DecisionMemory), ttl: ttl}
}

func (c *lastDecisionCache) Load(records []ai.LastDecisionRecord) {
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
		c.data[sym] = ai.DecisionMemory{
			Symbol:    sym,
			Horizon:   rec.Horizon,
			DecidedAt: rec.DecidedAt,
			Decisions: append([]ai.Decision(nil), rec.Decisions...),
		}
	}
}

func (c *lastDecisionCache) Set(mem ai.DecisionMemory) {
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

func (c *lastDecisionCache) Snapshot(now time.Time) []ai.DecisionMemory {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.data) == 0 {
		return nil
	}
	out := make([]ai.DecisionMemory, 0, len(c.data))
	for _, mem := range c.data {
		if c.ttl > 0 && now.Sub(mem.DecidedAt) > c.ttl {
			continue
		}
		dup := mem
		dup.Decisions = append([]ai.Decision(nil), mem.Decisions...)
		out = append(out, dup)
	}
	return out
}
