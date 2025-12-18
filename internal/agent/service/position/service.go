package position

import (
	"context"
	"strings"
	"time"

	"brale/internal/agent/interfaces"
	"brale/internal/agent/ports"
	"brale/internal/decision"
	"brale/internal/gateway/exchange"
	"brale/internal/logger"
)

// Service implements interfaces.PositionService.
type Service struct {
	manager ports.ExecutionManager
}

// NewService creates a new PositionService.
func NewService(manager ports.ExecutionManager) *Service {
	return &Service{
		manager: manager,
	}
}

// Ensure implementation
var _ interfaces.PositionService = (*Service)(nil)

func (s *Service) GetAccountSnapshot(ctx context.Context) (decision.AccountSnapshot, error) {
	if s.manager == nil {
		return decision.AccountSnapshot{Currency: "USDT"}, nil
	}

	bal, err := s.manager.RefreshBalance(ctx)
	if err != nil {
		logger.Warnf("PositionService: refresh balance failed: %v", err)
		bal = s.manager.AccountBalance()
	}

	currency := bal.StakeCurrency
	if strings.TrimSpace(currency) == "" {
		currency = "USDT"
	}
	return decision.AccountSnapshot{
		Total:     bal.Total,
		Available: bal.Available,
		Used:      bal.Used,
		Currency:  currency,
		UpdatedAt: bal.UpdatedAt,
	}, nil
}

func (s *Service) ListPositions(ctx context.Context) ([]decision.PositionSnapshot, error) {
	if s.manager == nil {
		return nil, nil
	}

	// Use ListOpenPositions from exchange manager
	// This usually returns []exchange.Position
	positions, err := s.manager.ListOpenPositions(ctx)
	if err != nil {
		return nil, err
	}
	if len(positions) == 0 {
		return nil, nil
	}

	// Need total balance to calculate AccountRatio, so we fetch it cheaply (or cached inside manager)
	// For ratio calculation, we ideally want valid Total.
	// Since GetAccountSnapshot does a Refresh, we might want to avoid double refresh.
	// We'll use cached AccountBalance from manager.
	bal := s.manager.AccountBalance()
	total := bal.Total

	out := make([]decision.PositionSnapshot, 0, len(positions))
	now := time.Now().UnixMilli()

	for _, p := range positions {
		snap := decision.PositionSnapshot{
			Symbol:          p.Symbol,
			Side:            p.Side,
			Quantity:        p.Amount,
			EntryPrice:      p.EntryPrice,
			CurrentPrice:    p.CurrentPrice,
			UnrealizedPn:    p.UnrealizedPnL,
			UnrealizedPnPct: p.UnrealizedPnLRatio * 100,
			HoldingMs:       now - p.OpenedAt.UnixMilli(),
			Leverage:        p.Leverage,
			Stake:           p.StakeAmount,
		}

		// Fallback stake calculation
		stake := snap.Stake
		if stake <= 0 && snap.Quantity > 0 && snap.EntryPrice > 0 {
			leverage := snap.Leverage
			if leverage <= 0 {
				leverage = 1
			}
			stake = snap.Quantity * snap.EntryPrice / float64(leverage)
		}

		if total > 0 && stake > 0 {
			snap.AccountRatio = stake / total
		}
		out = append(out, snap)
	}
	return out, nil
}

func (s *Service) SyncStrategies(ctx context.Context, hook exchange.PlanUpdateHook) error {
	if s.manager == nil {
		return nil
	}
	s.manager.SetPlanUpdateHook(hook)
	// We might also trigger an initial sync if the manager supports it
	// s.manager.SyncStrategyPlans(...)
	// For now, setting the hook is the primary linkage.
	return nil
}

func (s *Service) TradeIDForSymbol(symbol string) (int, bool) {
	if s.manager == nil {
		return 0, false
	}
	id, ok := s.manager.TradeIDBySymbol(symbol)
	return id, ok
}

// ExecuteDecision delegates the execution to the underlying manager.
func (s *Service) ExecuteDecision(ctx context.Context, traceID string, d decision.Decision, marketPrice float64) error {
	if s.manager == nil {
		return nil
	}

	// Logic ported from LiveService.freqtradeHandleDecision partially
	// Ideally we cache decision here too?
	// s.manager.CacheDecision(traceID, d)

	// Note: traceID handling and caching logic was in LiveService.
	// We can trust the caller provided a valid traceID.

	input := decision.DecisionInput{
		TraceID:     traceID,
		Decision:    d,
		MarketPrice: marketPrice,
	}

	// We assume CacheDecision is called by manager.Execute or we call it here?
	// Adapter ExecutionManager interface has CacheDecision.
	s.manager.CacheDecision(traceID, d)

	return s.manager.Execute(ctx, input)
}
