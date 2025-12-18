package agent

import (
	"brale/internal/pkg/circuit"
	"context"
	"fmt"
	"time"

	"brale/internal/agent/engine"
	"brale/internal/agent/ports"
	mktsvc "brale/internal/agent/service/market"
	"brale/internal/agent/service/position"
	brcfg "brale/internal/config"
	"brale/internal/decision"
	"brale/internal/exitplan"
	"brale/internal/gateway/database"
	"brale/internal/gateway/notifier" // Used if we add logging
	"brale/internal/market"
	"brale/internal/profile"
	promptkit "brale/internal/prompt"
	"brale/internal/strategy/exit"
)

type LiveServiceParams struct {
	Config          *brcfg.Config
	KlineStore      market.KlineStore
	Updater         *market.WSUpdater
	Engine          decision.Decider
	Telegram        *notifier.Telegram
	DecisionLogs    *database.DecisionLogStore
	Symbols         []string
	Intervals       []string
	HorizonName     string
	HorizonSummary  string
	WarmupSummary   string
	ExecManager     ports.ExecutionManager
	VisionReady     bool
	ProfileManager  *profile.Manager
	ExitPlans       *exitplan.Registry
	PlanHandlers    *exit.HandlerRegistry
	StrategyStore   exit.StrategyStore
	ExitPlanPrompts map[string]promptkit.ExitPlanPrompt
}

type LiveService struct {
	cfg        *brcfg.Config
	monitor    *PriceMonitor
	liveEngine *engine.LiveEngine
	tg         *notifier.Telegram
	decLogs    *database.DecisionLogStore

	symbols       []string
	hIntervals    []string
	horizonName   string
	hSummary      string
	warmupSummary string

	execManager ports.ExecutionManager

	profileMgr     *profile.Manager
	exitPlans      *exitplan.Registry
	planHandlers   *exit.HandlerRegistry
	planScheduler  *PlanScheduler
	strategyStore  exit.StrategyStore
	strategyCloser interface {
		Close() error
	}

	circuitBreaker *circuit.CircuitBreaker
}

func NewLiveService(p LiveServiceParams) *LiveService {

	var planScheduler *PlanScheduler
	if p.StrategyStore != nil && p.PlanHandlers != nil && p.ExitPlans != nil {
		planScheduler = NewPlanScheduler(PlanSchedulerParams{
			Store:       p.StrategyStore,
			Plans:       p.ExitPlans,
			Handlers:    p.PlanHandlers,
			ExecManager: p.ExecManager,
			Notifier:    p.Telegram,
		})
	}

	var monitor *PriceMonitor
	var symbols []string
	if len(p.Symbols) > 0 {
		symbols = append([]string(nil), p.Symbols...)
	}
	var intervals []string
	if len(p.Intervals) > 0 {
		intervals = append([]string(nil), p.Intervals...)
	}

	if p.Updater != nil || p.KlineStore != nil {
		monitor = NewPriceMonitor(MonitorParams{
			Updater:        p.Updater,
			KlineStore:     p.KlineStore,
			Symbols:        symbols,
			Intervals:      intervals,
			HorizonSummary: p.HorizonSummary,
			WarmupSummary:  p.WarmupSummary,
			Telegram:       p.Telegram,
			ExecManager:    p.ExecManager,
			Observer:       planScheduler,
		})
	}

	// --- New Architecture Initialization ---

	// 1. Position Service
	posSvc := position.NewService(p.ExecManager)

	// 2. Market Service
	// We need to ensure we pass the correct interfaces/structs
	mktParams := mktsvc.ServiceParams{
		Config:      p.Config,
		KlineStore:  p.KlineStore,
		ProfileMgr:  p.ProfileManager,
		Monitor:     monitor, // monitor implements PriceSource ideally
		Intervals:   intervals,
		HorizonName: p.HorizonName,
		VisionReady: p.VisionReady,
	}
	mktSvc := mktsvc.NewService(mktParams)

	// 3. Live Engine
	engParams := engine.EngineParams{
		Config:          p.Config,
		PosService:      posSvc,
		MktService:      mktSvc,
		Decider:         p.Engine,
		ProfileMgr:      p.ProfileManager,
		Candidates:      p.Symbols,
		ExitPlans:       p.ExitPlans,
		PlanHandlers:    p.PlanHandlers,
		PlanScheduler:   planScheduler,
		ExitPlanPrompts: p.ExitPlanPrompts,
		Notifier:        p.Telegram,
	}
	liveEngine := engine.NewLiveEngine(engParams)

	svc := &LiveService{
		cfg:            p.Config,
		liveEngine:     liveEngine, // internal/agent/engine
		tg:             p.Telegram,
		decLogs:        p.DecisionLogs,
		horizonName:    p.HorizonName,
		hSummary:       p.HorizonSummary,
		warmupSummary:  p.WarmupSummary,
		execManager:    p.ExecManager,
		profileMgr:     p.ProfileManager,
		exitPlans:      p.ExitPlans,
		planHandlers:   p.PlanHandlers,
		strategyStore:  p.StrategyStore,
		circuitBreaker: circuit.NewCircuitBreaker("LiveService", 5, 2*time.Minute),
		symbols:        symbols,
		hIntervals:     intervals,
		planScheduler:  planScheduler,
		monitor:        monitor,
	}

	if planStore := p.StrategyStore; planStore != nil {
		if closable, ok := planStore.(interface{ Close() error }); ok {
			svc.strategyCloser = closable
		}
	}
	if svc.planScheduler != nil && svc.execManager != nil {
		svc.execManager.SetPlanUpdateHook(svc.planScheduler)
		// Sync strategies using new PositionService as well if needed,
		// but Scheduler usually handles this.
		// posSvc.SyncStrategies(context.Background(), svc.planScheduler)
	}
	return svc
}

func (s *LiveService) Run(ctx context.Context) error {
	if s.planScheduler != nil {
		s.planScheduler.Start(ctx)
	}
	if s.monitor != nil {
		s.monitor.Start(ctx)
	}

	if s.liveEngine != nil {
		return s.liveEngine.Run(ctx)
	}

	return fmt.Errorf("live engine not initialized")
}
