package main

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"

	"brale/internal/decision"
	"brale/internal/gateway/database"
)

// Seed a SQLite database with mock decision and position data for the admin UI.
// Usage: go run scripts/seed_mock_data.go [db_path]
// Default db_path: data/live/decisions.db
func main() {
	dbPath := "data/live/decisions.db"
	if len(os.Args) > 1 && strings.TrimSpace(os.Args[1]) != "" {
		dbPath = strings.TrimSpace(os.Args[1])
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		panic(err)
	}

	store, err := database.NewDecisionLogStore(dbPath)
	if err != nil {
		panic(err)
	}
	defer store.Close()

	ctx := context.Background()
	if err := seedDecisions(ctx, store); err != nil {
		panic(err)
	}
	if err := seedPositions(ctx, store); err != nil {
		panic(err)
	}

	fmt.Printf("✓ mock data seeded into %s\n", dbPath)
}

func seedDecisions(ctx context.Context, store *database.DecisionLogStore) error {
	now := time.Now()
	samples := []database.DecisionLogRecord{
		{
			TraceID:    "mock-trace-btc",
			Timestamp:  now.Add(-30 * time.Minute).UnixMilli(),
			Candidates: []string{"BTC/USDT", "ETH/USDT"},
			Timeframes: []string{"1h", "15m"},
			Horizon:    "intraday",
			ProviderID: "gpt-4o",
			Stage:      "final",
			Decisions: []decision.Decision{
				{
					Symbol:     "BTC/USDT",
					Action:     "open_long",
					Leverage:   3,
					StopLoss:   64000,
					TakeProfit: 70500,
					Confidence: 82,
					Reasoning:  "多头趋势延续，量能放大，上破 1h 区间。",
					Tiers: &decision.DecisionTiers{
						Tier1Target: 67500, Tier1Ratio: 0.33,
						Tier2Target: 69000, Tier2Ratio: 0.33,
						Tier3Target: 70500, Tier3Ratio: 0.34,
					},
				},
			},
			Symbols:    []string{"BTC/USDT"},
			RawJSON:    `{"decision":"open_long","symbol":"BTC/USDT"}`,
			Meta:       "模型一致看多，择时介入。",
			RawOutput:  "Long bias confirmed",
			Note:       "mock data",
			ImageCount: 0,
		},
		{
			TraceID:    "mock-trace-sol",
			Timestamp:  now.Add(-2 * time.Hour).UnixMilli(),
			Candidates: []string{"SOL/USDT"},
			Timeframes: []string{"4h", "1h"},
			Horizon:    "swing",
			ProviderID: "gpt-4o-mini",
			Stage:      "final",
			Decisions: []decision.Decision{
				{
					Symbol:     "SOL/USDT",
					Action:     "open_short",
					Leverage:   2,
					StopLoss:   154,
					TakeProfit: 138,
					Confidence: 68,
					Reasoning:  "4h 结构破位，成交量下滑；RSI 顶背离。",
					Tiers: &decision.DecisionTiers{
						Tier1Target: 145, Tier1Ratio: 0.4,
						Tier2Target: 141, Tier2Ratio: 0.3,
						Tier3Target: 138, Tier3Ratio: 0.3,
					},
				},
			},
			Symbols:   []string{"SOL/USDT"},
			RawJSON:   `{"decision":"open_short","symbol":"SOL/USDT"}`,
			Meta:      "趋势转弱，轻仓尝试。",
			RawOutput: "Short setup",
			Note:      "mock data",
		},
		{
			TraceID:    "mock-trace-eth-close",
			Timestamp:  now.Add(-6 * time.Hour).UnixMilli(),
			Candidates: []string{"ETH/USDT"},
			Timeframes: []string{"1h", "15m"},
			Horizon:    "intraday",
			ProviderID: "claude-3",
			Stage:      "final",
			Decisions: []decision.Decision{
				{
					Symbol:     "ETH/USDT",
					Action:     "close_long",
					CloseRatio: 1.0,
					Reasoning:  "触及目标区间，锁定收益。",
				},
			},
			Symbols:   []string{"ETH/USDT"},
			RawJSON:   `{"decision":"close_long","symbol":"ETH/USDT"}`,
			Meta:      "目标完成，建议离场。",
			RawOutput: "Close position",
			Note:      "mock data",
		},
	}

	for _, rec := range samples {
		if _, err := store.Insert(ctx, rec); err != nil {
			return err
		}
	}
	return nil
}

func seedPositions(ctx context.Context, store *database.DecisionLogStore) error {
	now := time.Now()
	type pos struct {
		id     int
		symbol string
		side   string
		entry  float64
		stop   float64
		tp     float64
		t1     float64
		t1r    float64
		t2     float64
		t2r    float64
		t3     float64
		t3r    float64
		status database.LiveOrderStatus
		exit   float64
	}

	list := []pos{
		{
			id:     1001,
			symbol: "BTC/USDT",
			side:   "long",
			entry:  66200,
			stop:   64000,
			tp:     70500,
			t1:     67500, t1r: 0.33,
			t2: 69000, t2r: 0.33,
			t3: 70500, t3r: 0.34,
			status: database.LiveOrderStatusOpen,
		},
		{
			id:     1002,
			symbol: "SOL/USDT",
			side:   "short",
			entry:  149.5,
			stop:   154,
			tp:     138,
			t1:     145, t1r: 0.4,
			t2: 141, t2r: 0.3,
			t3: 138, t3r: 0.3,
			status: database.LiveOrderStatusOpen,
		},
		{
			id:     1003,
			symbol: "ETH/USDT",
			side:   "long",
			entry:  3450,
			stop:   3320,
			tp:     3620,
			t1:     3520, t1r: 0.4,
			t2: 3580, t2r: 0.3,
			t3: 3620, t3r: 0.3,
			status: database.LiveOrderStatusClosed,
			exit:   3570,
		},
	}

	for _, p := range list {
		order := database.LiveOrderRecord{
			FreqtradeID:   p.id,
			Symbol:        p.symbol,
			Side:          p.side,
			Amount:        ptrFloat(0.5),
			InitialAmount: ptrFloat(0.5),
			StakeAmount:   ptrFloat(2000),
			Leverage:      ptrFloat(3),
			PositionValue: ptrFloat(p.entry * 0.5 * 3),
			Price:         ptrFloat(p.entry),
			ClosedAmount:  ptrFloat(0),
			IsSimulated:   ptrBool(false),
			Status:        p.status,
			StartTime:     ptrTime(now.Add(-time.Duration(rand.Intn(180)) * time.Minute)),
		}
		if p.status == database.LiveOrderStatusClosed {
			order.EndTime = ptrTime(now.Add(-30 * time.Minute))
			order.ClosedAmount = ptrFloat(0.5)
		}

		tier := database.LiveTierRecord{
			FreqtradeID:    p.id,
			Symbol:         p.symbol,
			TakeProfit:     p.tp,
			StopLoss:       p.stop,
			Tier1:          p.t1,
			Tier1Ratio:     p.t1r,
			Tier1Done:      false,
			Tier2:          p.t2,
			Tier2Ratio:     p.t2r,
			Tier2Done:      false,
			Tier3:          p.t3,
			Tier3Ratio:     p.t3r,
			Tier3Done:      p.status == database.LiveOrderStatusClosed,
			RemainingRatio: 1.0,
			Status:         0,
			Source:         "mock",
			Reason:         "seed",
			TierNotes:      "mock data",
			Timestamp:      now,
			CreatedAt:      now,
			UpdatedAt:      now,
		}
		if err := store.SavePosition(ctx, order, tier); err != nil {
			return err
		}

		// Tier modifications
		if err := store.InsertTierModification(ctx, database.TierModificationLog{
			FreqtradeID: p.id,
			Field:       database.TierFieldTakeProfit,
			OldValue:    "",
			NewValue:    fmt.Sprintf("%.2f", p.tp),
			Source:      1,
			Reason:      "mock seed",
			Timestamp:   now.Add(-20 * time.Minute),
		}); err != nil {
			return err
		}

		// Operation records
		if err := store.AppendTradeOperation(ctx, database.TradeOperationRecord{
			FreqtradeID: p.id,
			Symbol:      p.symbol,
			Operation:   database.OperationOpen,
			Details: map[string]any{
				"price": p.entry,
				"side":  p.side,
			},
			Timestamp: now.Add(-15 * time.Minute),
		}); err != nil {
			return err
		}
		if p.status == database.LiveOrderStatusClosed {
			if err := store.AppendTradeOperation(ctx, database.TradeOperationRecord{
				FreqtradeID: p.id,
				Symbol:      p.symbol,
				Operation:   database.OperationTakeProfit,
				Details: map[string]any{
					"price": p.exit,
					"side":  p.side,
				},
				Timestamp: now.Add(-10 * time.Minute),
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

func ptrFloat(v float64) *float64    { return &v }
func ptrTime(t time.Time) *time.Time { return &t }
func ptrBool(v bool) *bool           { return &v }
