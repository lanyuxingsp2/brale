package freqtrade

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"brale/internal/config"
	"brale/internal/gateway/database"
	"brale/internal/gateway/exchange"
	"brale/internal/gateway/notifier"
	"brale/internal/logger"
	"brale/internal/store"
	"brale/internal/trader"
)

// NOTE: exchange types are used now.

// Logger abstracts decision log writes (live_decision_logs & live_orders).
type Logger interface {
	Insert(ctx context.Context, rec database.DecisionLogRecord) (int64, error)
}

// Manager provides freqtrade execution, logging, and position synchronization.
type Manager struct {
	client         *Client
	cfg            config.FreqtradeConfig
	logger         Logger
	store          store.Store
	posRepo        *PositionRepo
	posStore       database.LivePositionStore
	executor       exchange.Exchange
	balance        exchange.Balance
	planUpdateHook exchange.PlanUpdateHook
	// Trader Actor (Shadow Mode or Active)
	trader *trader.Trader

	openPlanMu    sync.Mutex
	openPlanCache map[string]cachedOpenPlan

	pendingMu sync.Mutex
	pending   map[int]*pendingState
	notifier  notifier.TextNotifier
}

const (
	pendingStageOpening = "opening"
	pendingStageClosing = "closing"
	pendingTimeout      = 11 * time.Minute
	reconcileDelay      = 5 * time.Second
)

// NewManager creates a freqtrade execution manager.
// Returns an error if required dependencies are missing.
func NewManager(client *Client, cfg config.FreqtradeConfig, logStore Logger, posStore database.LivePositionStore, newStore store.Store, textNotifier notifier.TextNotifier, executor exchange.Exchange) (*Manager, error) {
	// Validate required dependencies
	if posStore == nil {
		// Try to extract from logStore if it implements the interface
		if ps, ok := logStore.(database.LivePositionStore); ok {
			posStore = ps
		} else {
			return nil, fmt.Errorf("posStore is required but not provided")
		}
	}

	if executor == nil {
		return nil, fmt.Errorf("executor is required but not provided")
	}

	initLiveOrderPnL(posStore)

	// Initialize Trader Actor with SQLite Event Store
	eventStore := trader.NewSQLiteEventStore(posStore)

	t := trader.NewTrader(executor, eventStore, posStore)
	if err := t.Recover(); err != nil {
		return nil, fmt.Errorf("trader state recovery failed: %w", err)
	}
	t.Start()

	return &Manager{
		client:        client,
		cfg:           cfg,
		logger:        logStore,
		store:         newStore,
		posStore:      posStore,
		posRepo:       NewPositionRepo(newStore, posStore),
		executor:      executor,
		trader:        t,
		notifier:      textNotifier,
		openPlanCache: make(map[string]cachedOpenPlan),
	}, nil
}

func managerEventID(seed, prefix string) string {
	seed = strings.TrimSpace(seed)
	if seed != "" {
		return seed
	}
	if prefix == "" {
		prefix = "evt"
	}
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

func initLiveOrderPnL(store database.LivePositionStore) {
	if store == nil {
		return
	}
	if err := store.AddOrderPnLColumns(); err != nil {
		logger.Warnf("freqtrade manager: pnl storage init failed: %v", err)
	}
}
