package backtest

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"brale/internal/ai"
	brcfg "brale/internal/config"
	"brale/internal/execution"
	"brale/internal/logger"

	"github.com/google/uuid"
)

// Notifier 用于运行完成后的推送（Telegram 等）。
type Notifier interface {
	SendText(text string) error
}

type SimulatorConfig struct {
	CandleStore    *Store
	ResultStore    *ResultStore
	Fetcher        *Service
	Profiles       map[string]brcfg.HorizonProfile
	Lookbacks      map[string]int
	DefaultProfile string
	Strategy       StrategyFactory
	Notifier       Notifier
	MaxConcurrent  int
	OrderRecorder  execution.Recorder
}

// Simulator 负责将历史 K 线 + 决策策略推演为资金曲线。
type Simulator struct {
	store       *Store
	results     *ResultStore
	fetcher     *Service
	profiles    map[string]brcfg.HorizonProfile
	lookbacks   map[string]int
	defaultProf string
	factory     StrategyFactory
	notifier    Notifier
	orderRec    execution.Recorder

	sem     chan struct{}
	baseCtx context.Context

	mu sync.Mutex
}

func NewSimulator(cfg SimulatorConfig) (*Simulator, error) {
	if cfg.CandleStore == nil {
		return nil, fmt.Errorf("candle store 不能为空")
	}
	if cfg.ResultStore == nil {
		return nil, fmt.Errorf("result store 不能为空")
	}
	if cfg.Strategy == nil {
		return nil, fmt.Errorf("strategy factory 不能为空")
	}
	if len(cfg.Profiles) == 0 {
		return nil, fmt.Errorf("profiles 不能为空")
	}
	defaultProf := cfg.DefaultProfile
	if defaultProf == "" {
		for name := range cfg.Profiles {
			defaultProf = name
			break
		}
	}
	maxConcurrent := cfg.MaxConcurrent
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}
	orderRec := cfg.OrderRecorder
	if orderRec == nil {
		orderRec = NewResultRecorder(cfg.ResultStore)
	}
	return &Simulator{
		store:       cfg.CandleStore,
		results:     cfg.ResultStore,
		fetcher:     cfg.Fetcher,
		profiles:    cfg.Profiles,
		lookbacks:   cfg.Lookbacks,
		defaultProf: defaultProf,
		factory:     cfg.Strategy,
		notifier:    cfg.Notifier,
		orderRec:    orderRec,
		sem:         make(chan struct{}, maxConcurrent),
		baseCtx:     context.Background(),
	}, nil
}

func (s *Simulator) SetContext(ctx context.Context) {
	if ctx != nil {
		s.baseCtx = ctx
	}
}

func (s *Simulator) ctx() context.Context {
	if s.baseCtx != nil {
		return s.baseCtx
	}
	return context.Background()
}

// StartRun 创建回测任务并立即返回，模拟过程在后台进行。
func (s *Simulator) StartRun(req RunRequest) (Run, error) {
	if req.Symbol == "" {
		return Run{}, fmt.Errorf("symbol 不能为空")
	}
	profileName := req.Profile
	if profileName == "" {
		profileName = s.defaultProf
	}
	profile, ok := s.profiles[profileName]
	if !ok {
		return Run{}, fmt.Errorf("未知 profile: %s", profileName)
	}
	execTF := req.ExecutionTimeframe
	if execTF == "" {
		if len(profile.EntryTimeframes) > 0 {
			execTF = profile.EntryTimeframes[0]
		} else {
			all := profile.AllTimeframes()
			if len(all) == 0 {
				return Run{}, fmt.Errorf("profile %s 未配置 k 线周期", profileName)
			}
			execTF = all[0]
		}
	}
	tf, err := ParseTimeframe(execTF)
	if err != nil {
		return Run{}, fmt.Errorf("execution timeframe 无效: %w", err)
	}
	start, end := req.StartTS, req.EndTS
	start, end = tf.AlignRange(start, end)
	if start <= 0 || end <= 0 || end <= start {
		return Run{}, fmt.Errorf("start/end 非法")
	}
	initialBalance := req.InitialBalance
	if initialBalance <= 0 {
		initialBalance = 10000
	}
	feeRate := req.FeeRate
	if feeRate < 0 {
		feeRate = 0
	}
	if feeRate == 0 {
		feeRate = 0.0004 // 4bps
	}
	slippageBps := req.SlippageBps
	if slippageBps < 0 {
		slippageBps = 0
	}
	if slippageBps == 0 {
		slippageBps = 2
	}

	cfg := RunConfig{
		Profile:            profileName,
		Symbol:             strings.ToUpper(req.Symbol),
		StartTS:            start,
		EndTS:              end,
		ExecutionTimeframe: execTF,
		EntryTimeframes:    append([]string{}, profile.EntryTimeframes...),
		ConfirmTimeframes:  append([]string{}, profile.ConfirmTimeframes...),
		BackgroundTFs:      append([]string{}, profile.BackgroundTimeframes...),
		InitialBalance:     initialBalance,
		FeeRate:            feeRate,
		SlippageBps:        slippageBps,
		Leverage:           max(1, req.Leverage),
	}

	run := Run{
		ID:                 uuid.NewString(),
		Symbol:             cfg.Symbol,
		Profile:            profileName,
		Status:             RunStatusPending,
		StartTS:            start,
		EndTS:              end,
		ExecutionTimeframe: execTF,
		InitialBalance:     initialBalance,
		FinalBalance:       initialBalance,
		Config:             cfg,
		Stats: RunStats{
			FinalBalance: initialBalance,
		},
	}
	if err := s.results.InsertRun(s.ctx(), run); err != nil {
		return Run{}, err
	}
	go s.runLoop(run.ID, cfg, profile)
	return run, nil
}

func (s *Simulator) runLoop(runID string, cfg RunConfig, profile brcfg.HorizonProfile) {
	select {
	case s.sem <- struct{}{}:
	default:
		logger.Warnf("[backtest] run %s 等待可用 worker", runID)
		s.sem <- struct{}{}
	}
	defer func() { <-s.sem }()

	ctx := s.ctx()
	s.results.UpdateRunStatus(ctx, runID, RunStatusRunning, "初始化策略…")
	runner := newSimRunner(s.store, s.results, s.factory, s.fetcher, s.lookbacks, cfg, profile, s.notifier, s.orderRec)
	if err := runner.Run(ctx, runID); err != nil {
		logger.Warnf("[backtest] run %s 失败: %v", runID, err)
		_ = s.results.UpdateRunStatus(ctx, runID, RunStatusFailed, err.Error())
		return
	}
}

type simRunner struct {
	store         *Store
	results       *ResultStore
	fetcher       *Service
	lookbacks     map[string]int
	factory       StrategyFactory
	cfg           RunConfig
	profile       brcfg.HorizonProfile
	notifier      Notifier
	orderRecorder execution.Recorder
}

func newSimRunner(store *Store, results *ResultStore, factory StrategyFactory, fetcher *Service, lookbacks map[string]int, cfg RunConfig, profile brcfg.HorizonProfile, notifier Notifier, recorder execution.Recorder) *simRunner {
	return &simRunner{
		store:         store,
		results:       results,
		fetcher:       fetcher,
		lookbacks:     lookbacks,
		factory:       factory,
		cfg:           cfg,
		profile:       profile,
		notifier:      notifier,
		orderRecorder: recorder,
	}
}

func (r *simRunner) recordOrder(ctx context.Context, order execution.Order) int64 {
	if r.orderRecorder == nil {
		return 0
	}
	id, err := r.orderRecorder.RecordOrder(ctx, &order)
	if err != nil {
		logger.Warnf("[backtest] run %s 记录订单失败: %v", order.RunID, err)
		return 0
	}
	return id
}

func (r *simRunner) Run(ctx context.Context, runID string) error {
	timeframes := r.collectTimeframes()
	if err := r.ensureDatasets(ctx, runID, timeframes); err != nil {
		return err
	}
	baseTF, err := ParseTimeframe(r.cfg.ExecutionTimeframe)
	if err != nil {
		return err
	}
	baseLookback := r.lookbackFor(baseTF.Key)
	startWarm := r.cfg.StartTS - int64(baseLookback+5)*baseTF.durationMillis()
	if startWarm < 0 {
		startWarm = 0
	}
	baseCandlesAll, err := r.store.RangeCandles(ctx, r.cfg.Symbol, baseTF.Key, startWarm, r.cfg.EndTS)
	if err != nil {
		return err
	}
	if len(baseCandlesAll) < baseLookback {
		return fmt.Errorf("执行周期 %s warmup 数据不足: 只有 %d 条，需要 %d", baseTF.Key, len(baseCandlesAll), baseLookback)
	}
	startIdx := 0
	for startIdx < len(baseCandlesAll) && baseCandlesAll[startIdx].OpenTime < r.cfg.StartTS {
		startIdx++
	}
	if startIdx >= len(baseCandlesAll) {
		return fmt.Errorf("未找到 %s %s 的起始 K 线", r.cfg.Symbol, baseTF.Key)
	}
	baseCandles := baseCandlesAll[startIdx:]
	if len(baseCandles) < 2 {
		return fmt.Errorf("基础周期数据不足")
	}
	frameData := make(map[string][]Candle, len(timeframes))
	execKey := strings.ToLower(r.cfg.ExecutionTimeframe)
	frameData[execKey] = baseCandlesAll
	for _, tfName := range timeframes {
		tfName = strings.ToLower(tfName)
		if tfName == execKey {
			continue
		}
		tf, err := ParseTimeframe(tfName)
		if err != nil {
			return fmt.Errorf("timeframe %s 无效: %w", tfName, err)
		}
		look := r.lookbackFor(tf.Key)
		start := r.cfg.StartTS - int64(look+5)*tf.durationMillis()
		if start < 0 {
			start = 0
		}
		data, err := r.store.RangeCandles(ctx, r.cfg.Symbol, tf.Key, start, r.cfg.EndTS)
		if err != nil {
			return err
		}
		frameData[tfName] = data
	}

	strategy, err := r.factory.NewStrategy(StrategySpec{
		RunID:       runID,
		Symbol:      r.cfg.Symbol,
		ProfileName: r.cfg.Profile,
		Profile:     r.profile,
	})
	if err != nil {
		return err
	}
	defer strategy.Close()

	state := &portfolioState{
		initialBalance: r.cfg.InitialBalance,
		balance:        r.cfg.InitialBalance,
		feeRate:        r.cfg.FeeRate,
		slippageBps:    r.cfg.SlippageBps,
		execTimeframe:  strings.ToLower(r.cfg.ExecutionTimeframe),
		peakEquity:     r.cfg.InitialBalance,
		valleyEquity:   r.cfg.InitialBalance,
	}
	baseStartIdx := startIdx
	progressStep := max(10, len(baseCandles)/20)
	if progressStep == 0 {
		progressStep = 1
	}

	tfCursors := make(map[string]int)

	for idx, candle := range baseCandles {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		currentTime := candle.CloseTime
		slices := make(map[string][]Candle, len(frameData))
		globalIdx := baseStartIdx + idx
		for tfName, candles := range frameData {
			if tfName == execKey {
				end := globalIdx + 1
				if end > len(candles) {
					end = len(candles)
				}
				slices[tfName] = candles[:end]
				continue
			}
			cursor := tfCursors[tfName]
			for cursor < len(candles) && candles[cursor].CloseTime <= currentTime {
				cursor++
			}
			tfCursors[tfName] = cursor
			if cursor == 0 {
				continue
			}
			slices[tfName] = candles[:cursor]
		}

		req := StrategyRequest{
			RunID:       runID,
			Symbol:      r.cfg.Symbol,
			ProfileName: r.cfg.Profile,
			Profile:     r.profile,
			CurrentTime: currentTime,
			Timeframes:  slices,
			Positions:   r.snapshotPositions(state, candle.CloseTime, candle.Close),
		}
		decisions, err := strategy.Decide(ctx, req)
		logs := strategy.Logs()
		if err != nil {
			r.persistRunLog(ctx, runID, candle, execKey, logs)
			logger.Warnf("[backtest] run %s 决策失败: %v", runID, err)
			continue
		}
		r.persistRunLog(ctx, runID, candle, execKey, logs)
		r.applyDecisions(ctx, runID, state, candle, decisions)

		if (idx+1)%progressStep == 0 || idx == len(baseCandles)-1 {
			percent := float64(idx+1) / float64(len(baseCandles)) * 100
			msg := fmt.Sprintf("processing %d/%d (%.1f%%)", idx+1, len(baseCandles), percent)
			_ = r.results.UpdateRunStatus(ctx, runID, RunStatusRunning, msg)
		}

		r.recordSnapshot(ctx, runID, state, candle)
	}

	if state.position != nil {
		last := baseCandles[len(baseCandles)-1]
		side := state.position.side
		action := "close_" + side
		dummy := ai.Decision{Symbol: r.cfg.Symbol, Action: action}
		r.handleClose(ctx, runID, state, last, dummy, side, action, time.UnixMilli(last.CloseTime))
	}

	stats := state.statsSummary()
	stats.FinalBalance = state.balance
	if err := r.results.UpdateRunSummary(ctx, runID, RunStatusDone, stats, "完成"); err != nil {
		return err
	}
	r.notify(runID, stats)
	return nil
}

func (r *simRunner) applyDecisions(ctx context.Context, runID string, state *portfolioState, candle Candle, res ai.DecisionResult) {
	if len(res.Decisions) == 0 {
		return
	}
	curTime := time.UnixMilli(candle.CloseTime)
	for _, d := range res.Decisions {
		if !strings.EqualFold(d.Symbol, r.cfg.Symbol) {
			continue
		}
		action := ai.NormalizeAction(d.Action)
		switch action {
		case "open_long":
			r.handleOpen(ctx, runID, state, candle, d, "long", action, curTime)
		case "open_short":
			r.handleOpen(ctx, runID, state, candle, d, "short", action, curTime)
		case "close_long":
			r.handleClose(ctx, runID, state, candle, d, "long", action, curTime)
		case "close_short":
			r.handleClose(ctx, runID, state, candle, d, "short", action, curTime)
		case "partial_close":
			r.handlePartialClose(ctx, runID, state, candle, d, curTime)
		case "adjust_stop_loss":
			r.handleAdjustStopLoss(ctx, runID, state, candle, d, curTime)
		default:
			continue
		}
	}
}

func (r *simRunner) handleOpen(ctx context.Context, runID string, state *portfolioState, candle Candle, d ai.Decision, side, action string, ts time.Time) {
	if state.position != nil && state.position.side == side {
		return
	}
	if state.position != nil && state.position.side != side {
		r.handleClose(ctx, runID, state, candle, d, state.position.side, action, ts)
	}
	price := candle.Close
	slippage := price * r.cfg.SlippageBps / 10000
	if side == "long" {
		price += slippage
	} else {
		price -= slippage
	}
	sizeUSD := d.PositionSizeUSD
	if sizeUSD <= 0 {
		sizeUSD = state.balance * 0.2
	}
	if sizeUSD > state.balance {
		sizeUSD = state.balance
	}
	if sizeUSD <= 0 || price <= 0 {
		return
	}
	qty := sizeUSD / price
	fee := sizeUSD * state.feeRate
	if fee > state.balance {
		return
	}
	state.balance -= fee

	rawDecision, _ := json.Marshal(d)
	takeProfit := d.TakeProfit
	stopLoss := d.StopLoss
	expectedRR := calcExpectedRR(side, price, takeProfit, stopLoss)
	orderID := r.recordOrder(ctx, execution.Order{
		RunID:      runID,
		Symbol:     r.cfg.Symbol,
		Action:     action,
		Side:       side,
		Type:       "market",
		Price:      price,
		Quantity:   qty,
		Notional:   sizeUSD,
		Fee:        fee,
		Timeframe:  r.cfg.ExecutionTimeframe,
		DecidedAt:  ts,
		ExecutedAt: ts,
		TakeProfit: takeProfit,
		StopLoss:   stopLoss,
		ExpectedRR: expectedRR,
		Decision:   rawDecision,
	})
	state.orders++
	state.position = &positionState{
		side:        side,
		entryPrice:  price,
		qty:         qty,
		entryTime:   candle.CloseTime,
		entryOrder:  orderID,
		entryNotion: sizeUSD,
		takeProfit:  takeProfit,
		stopLoss:    stopLoss,
		expectedRR:  expectedRR,
	}
}

func (r *simRunner) handleClose(ctx context.Context, runID string, state *portfolioState, candle Candle, d ai.Decision, side, action string, ts time.Time) {
	pos := state.position
	if pos == nil || pos.side != side {
		return
	}
	price := candle.Close
	slippage := price * r.cfg.SlippageBps / 10000
	if side == "long" {
		price -= slippage
	} else {
		price += slippage
	}
	fee := pos.qty * price * state.feeRate
	pnl := state.unrealizedPnL(price)
	state.balance += pnl - fee

	rawDecision, _ := json.Marshal(d)
	orderID := r.recordOrder(ctx, execution.Order{
		RunID:      runID,
		Symbol:     r.cfg.Symbol,
		Action:     action,
		Side:       side,
		Type:       "market",
		Price:      price,
		Quantity:   pos.qty,
		Notional:   pos.qty * price,
		Fee:        fee,
		Timeframe:  r.cfg.ExecutionTimeframe,
		DecidedAt:  ts,
		ExecutedAt: ts,
		TakeProfit: pos.takeProfit,
		StopLoss:   pos.stopLoss,
		ExpectedRR: pos.expectedRR,
		Decision:   rawDecision,
	})
	state.orders++
	position := Position{
		RunID:        runID,
		Symbol:       r.cfg.Symbol,
		Side:         side,
		EntryOrderID: pos.entryOrder,
		ExitOrderID:  orderID,
		EntryPrice:   pos.entryPrice,
		ExitPrice:    price,
		Quantity:     pos.qty,
		PnL:          pnl - fee,
		PnLPct:       (pnl - fee) / pos.entryNotion,
		HoldingMs:    candle.CloseTime - pos.entryTime,
		TakeProfit:   pos.takeProfit,
		StopLoss:     pos.stopLoss,
		ExpectedRR:   pos.expectedRR,
		OpenedAt:     time.UnixMilli(pos.entryTime),
		ClosedAt:     ts,
	}
	if position.PnL >= 0 {
		state.wins++
	} else {
		state.losses++
	}
	if _, err := r.results.InsertPosition(ctx, &position); err != nil {
		logger.Warnf("[backtest] run %s 记录持仓失败: %v", runID, err)
	}
	state.positions++
	state.position = nil
}

func (r *simRunner) handlePartialClose(ctx context.Context, runID string, state *portfolioState, candle Candle, d ai.Decision, ts time.Time) {
	pos := state.position
	if pos == nil {
		return
	}
	price := candle.Close
	slippage := price * r.cfg.SlippageBps / 10000
	execPrice := price
	if pos.side == "long" {
		execPrice -= slippage
	} else {
		execPrice += slippage
	}
	closeQty := r.partialCloseQty(d, execPrice, pos.qty)
	if closeQty <= 0 {
		return
	}
	if closeQty >= pos.qty*0.999 {
		r.handleClose(ctx, runID, state, candle, d, pos.side, "close_"+pos.side, ts)
		return
	}
	fee := closeQty * execPrice * state.feeRate
	var pnl float64
	if pos.side == "long" {
		pnl = (execPrice - pos.entryPrice) * closeQty
	} else {
		pnl = (pos.entryPrice - execPrice) * closeQty
	}
	state.balance += pnl - fee

	rawDecision, _ := json.Marshal(d)
	orderID := r.recordOrder(ctx, execution.Order{
		RunID:      runID,
		Symbol:     r.cfg.Symbol,
		Action:     "partial_close",
		Side:       pos.side,
		Type:       "market",
		Price:      execPrice,
		Quantity:   closeQty,
		Notional:   closeQty * execPrice,
		Fee:        fee,
		Timeframe:  r.cfg.ExecutionTimeframe,
		DecidedAt:  ts,
		ExecutedAt: ts,
		TakeProfit: pos.takeProfit,
		StopLoss:   pos.stopLoss,
		ExpectedRR: pos.expectedRR,
		Decision:   rawDecision,
	})
	state.orders++

	position := Position{
		RunID:        runID,
		Symbol:       r.cfg.Symbol,
		Side:         pos.side,
		EntryOrderID: pos.entryOrder,
		ExitOrderID:  orderID,
		EntryPrice:   pos.entryPrice,
		ExitPrice:    execPrice,
		Quantity:     closeQty,
		PnL:          pnl - fee,
		PnLPct:       (pnl - fee) / (closeQty * pos.entryPrice),
		HoldingMs:    candle.CloseTime - pos.entryTime,
		TakeProfit:   pos.takeProfit,
		StopLoss:     pos.stopLoss,
		ExpectedRR:   pos.expectedRR,
		OpenedAt:     time.UnixMilli(pos.entryTime),
		ClosedAt:     ts,
	}
	if _, err := r.results.InsertPosition(ctx, &position); err != nil {
		logger.Warnf("[backtest] run %s 记录部分仓位失败: %v", runID, err)
	}
	state.positions++
	if position.PnL >= 0 {
		state.wins++
	} else {
		state.losses++
	}

	pos.qty -= closeQty
	if pos.qty <= 0 {
		state.position = nil
	} else {
		pos.entryNotion = pos.qty * pos.entryPrice
	}
}

func (r *simRunner) recordSnapshot(ctx context.Context, runID string, state *portfolioState, candle Candle) {
	price := candle.Close
	exposure := 0.0
	if state.position != nil {
		denom := state.initialBalance
		if denom <= 0 {
			denom = 1
		}
		exposure = math.Abs(state.position.entryNotion / denom)
	}
	equity := state.equity(price)
	state.peakEquity = math.Max(state.peakEquity, equity)
	if state.valleyEquity == 0 || equity < state.valleyEquity {
		state.valleyEquity = equity
	}
	if state.peakEquity > 0 {
		drawdown := (state.peakEquity - equity) / state.peakEquity
		if drawdown > state.maxDrawdown {
			state.maxDrawdown = drawdown
		}
	}
	snap := Snapshot{
		RunID:    runID,
		TS:       candle.CloseTime,
		Equity:   equity,
		Balance:  state.balance,
		Drawdown: state.maxDrawdown,
		Exposure: exposure,
	}
	if _, err := r.results.InsertSnapshot(ctx, snap); err != nil {
		logger.Warnf("[backtest] run %s 写入 snapshot 失败: %v", runID, err)
	}
	state.snapshots++
}

func (r *simRunner) notify(runID string, stats RunStats) {
	if r.notifier == nil {
		return
	}
	msg := fmt.Sprintf("*回测完成* ✅\n```\nid      : %s\nsymbol  : %s\nprofile : %s\npnl     : %.2f (%.2f%%)\nwinrate : %.2f%% (%d/%d)\nmaxDD   : %.2f%%\nfinal   : %.2f\n```\n",
		runID, r.cfg.Symbol, r.cfg.Profile, stats.Profit, stats.ReturnPct*100,
		stats.WinRate*100, stats.Wins, stats.Positions, stats.MaxDrawdownPct*100, stats.FinalBalance)
	if err := r.notifier.SendText(msg); err != nil {
		logger.Warnf("回测通知失败: %v", err)
	}
}

type positionState struct {
	side        string
	entryPrice  float64
	qty         float64
	entryTime   int64
	entryOrder  int64
	entryNotion float64
	takeProfit  float64
	stopLoss    float64
	expectedRR  float64
}

type portfolioState struct {
	initialBalance float64
	balance        float64
	feeRate        float64
	slippageBps    float64
	execTimeframe  string
	position       *positionState
	wins           int
	losses         int
	orders         int
	positions      int
	snapshots      int
	peakEquity     float64
	valleyEquity   float64
	maxDrawdown    float64
}

func (p *portfolioState) unrealizedPnL(price float64) float64 {
	if p.position == nil {
		return 0
	}
	if p.position.side == "long" {
		return (price - p.position.entryPrice) * p.position.qty
	}
	return (p.position.entryPrice - price) * p.position.qty
}

func (p *portfolioState) equity(price float64) float64 {
	return p.balance + p.unrealizedPnL(price)
}

func (p *portfolioState) statsSummary() RunStats {
	total := p.positions
	winRate := 0.0
	if total > 0 {
		winRate = float64(p.wins) / float64(total)
	}
	profit := p.balance - p.initialBalance
	returnPct := 0.0
	if p.initialBalance > 0 {
		returnPct = profit / p.initialBalance
	}
	return RunStats{
		Profit:         profit,
		ReturnPct:      returnPct,
		WinRate:        winRate,
		MaxDrawdownPct: p.maxDrawdown,
		Orders:         p.orders,
		Positions:      total,
		Wins:           p.wins,
		Losses:         p.losses,
		Snapshots:      p.snapshots,
		EquityPeak:     p.peakEquity,
		EquityValley:   p.valleyEquity,
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (r *simRunner) collectTimeframes() []string {
	seen := map[string]struct{}{}
	var list []string
	add := func(tf string) {
		tf = strings.ToLower(strings.TrimSpace(tf))
		if tf == "" {
			return
		}
		if _, ok := seen[tf]; ok {
			return
		}
		seen[tf] = struct{}{}
		list = append(list, tf)
	}
	add(r.cfg.ExecutionTimeframe)
	for _, tf := range r.profile.AllTimeframes() {
		add(tf)
	}
	return list
}

func (r *simRunner) snapshotPositions(state *portfolioState, currentTs int64, price float64) []StrategyPosition {
	if state == nil || state.position == nil {
		return nil
	}
	pos := state.position
	if price <= 0 {
		return nil
	}
	unrealized := state.unrealizedPnL(price)
	unrealizedPct := 0.0
	if pos.entryPrice > 0 {
		if pos.side == "long" {
			unrealizedPct = (price - pos.entryPrice) / pos.entryPrice
		} else {
			unrealizedPct = (pos.entryPrice - price) / pos.entryPrice
		}
	}
	holding := currentTs - pos.entryTime
	if holding < 0 {
		holding = 0
	}
	return []StrategyPosition{
		{
			Symbol:          r.cfg.Symbol,
			Side:            pos.side,
			EntryPrice:      pos.entryPrice,
			Quantity:        pos.qty,
			TakeProfit:      pos.takeProfit,
			StopLoss:        pos.stopLoss,
			UnrealizedPn:    unrealized,
			UnrealizedPnPct: unrealizedPct,
			RR:              pos.expectedRR,
			HoldingMs:       holding,
		},
	}
}

func (r *simRunner) lookbackFor(tf string) int {
	if r.lookbacks == nil {
		return 300
	}
	if v, ok := r.lookbacks[strings.ToLower(tf)]; ok && v > 0 {
		return v
	}
	return 300
}

func (r *simRunner) ensureDatasets(ctx context.Context, runID string, timeframes []string) error {
	if len(timeframes) == 0 {
		return fmt.Errorf("回测缺少时间周期配置")
	}
	for _, tfName := range timeframes {
		tf, err := ParseTimeframe(tfName)
		if err != nil {
			return err
		}
		look := r.lookbackFor(tf.Key)
		start := r.cfg.StartTS - int64(look+5)*tf.durationMillis()
		if start < 0 {
			start = 0
		}
		if err := r.ensureTimeframeData(ctx, runID, tf, start, r.cfg.EndTS); err != nil {
			return err
		}
	}
	if err := r.results.UpdateRunStatus(ctx, runID, RunStatusPending, "数据准备完成"); err != nil {
		logger.Debugf("update run status failed: %v", err)
	}
	return nil
}

func (r *simRunner) ensureTimeframeData(ctx context.Context, runID string, tf Timeframe, start, end int64) error {
	need := r.lookbackFor(tf.Key)
	report, err := r.store.CheckIntegrity(ctx, r.cfg.Symbol, tf.Key, tf, start, end)
	if err != nil {
		return err
	}
	msg := fmt.Sprintf("warmup %s %s: %d/%d", r.cfg.Symbol, tf.Key, report.Present, report.Expected)
	if err := r.results.UpdateRunStatus(ctx, runID, RunStatusPending, msg); err != nil {
		logger.Debugf("update run status failed: %v", err)
	}
	if report.Complete() {
		readyMsg := fmt.Sprintf("warmup %s %s 完成（%d条，需要≥%d）", r.cfg.Symbol, tf.Key, report.Present, need)
		if err := r.results.UpdateRunStatus(ctx, runID, RunStatusPending, readyMsg); err != nil {
			logger.Debugf("update run status failed: %v", err)
		}
		return nil
	}
	if r.fetcher == nil {
		return fmt.Errorf("%s %s 数据缺失（%d 段），未配置拉取服务", r.cfg.Symbol, tf.Key, len(report.Gaps))
	}
	job, err := r.fetcher.SubmitFetch(FetchParams{
		Symbol:    r.cfg.Symbol,
		Timeframe: tf.Key,
		Start:     start,
		End:       end,
	})
	if err != nil {
		return err
	}
	return r.waitFetchJob(ctx, runID, job, tf, start, end)
}

func (r *simRunner) waitFetchJob(ctx context.Context, runID string, job FetchJob, tf Timeframe, start, end int64) error {
	updateProgress := func(j FetchJob) {
		message := fmt.Sprintf("下载 %s %s: %s", j.Params.Symbol, j.Params.Timeframe, j.Status)
		if j.Total > 0 {
			percent := float64(j.Completed) / float64(j.Total) * 100
			message = fmt.Sprintf("下载 %s %s: %.1f%%", j.Params.Symbol, j.Params.Timeframe, percent)
		}
		if j.Message != "" {
			message = message + " " + j.Message
		}
		if err := r.results.UpdateRunStatus(ctx, runID, RunStatusPending, message); err != nil {
			logger.Debugf("update run status failed: %v", err)
		}
	}

	checkFinal := func() error {
		finalReport, err := r.store.CheckIntegrity(ctx, r.cfg.Symbol, tf.Key, tf, start, end)
		if err != nil {
			return err
		}
		if !finalReport.Complete() {
			return fmt.Errorf("%s %s 数据仍缺失（%d 段）", r.cfg.Symbol, tf.Key, len(finalReport.Gaps))
		}
		readyMsg := fmt.Sprintf("warmup %s %s 完成（%d条）", r.cfg.Symbol, tf.Key, finalReport.Present)
		if err := r.results.UpdateRunStatus(ctx, runID, RunStatusPending, readyMsg); err != nil {
			logger.Debugf("update run status failed: %v", err)
		}
		return nil
	}

	updateProgress(job)
	if job.Status == JobStatusDone {
		return checkFinal()
	}
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			snap, ok := r.fetcher.JobSnapshot(job.ID)
			if !ok {
				continue
			}
			updateProgress(snap)
			switch snap.Status {
			case JobStatusDone:
				return checkFinal()
			case JobStatusFailed:
				if snap.Message != "" {
					return fmt.Errorf("下载 %s %s 失败: %s", r.cfg.Symbol, tf.Key, snap.Message)
				}
				return fmt.Errorf("下载 %s %s 失败", r.cfg.Symbol, tf.Key)
			case JobStatusPartial:
				return fmt.Errorf("下载 %s %s 未完成，缺口=%d", r.cfg.Symbol, tf.Key, len(snap.Missing))
			}
		}
	}
}

func (r *simRunner) persistRunLog(ctx context.Context, runID string, candle Candle, timeframe string, logs []AILogRecord) {
	if r.results == nil || len(logs) == 0 {
		return
	}
	for _, rec := range logs {
		log := RunLog{
			RunID:        runID,
			CandleTS:     candle.CloseTime,
			Timeframe:    timeframe,
			ProviderID:   rec.ProviderID,
			Stage:        rec.Stage,
			SystemPrompt: rec.SystemPrompt,
			UserPrompt:   rec.UserPrompt,
			RawOutput:    rec.RawOutput,
			RawJSON:      rec.RawJSON,
			MetaSummary:  rec.MetaSummary,
			Decisions:    append([]ai.Decision(nil), rec.Decisions...),
			Error:        rec.Error,
			Note:         rec.Note,
		}
		if _, err := r.results.InsertRunLog(ctx, log); err != nil {
			logger.Debugf("run log insert failed: %v", err)
		}
	}
}

func calcExpectedRR(side string, entry, tp, sl float64) float64 {
	if entry <= 0 || tp <= 0 || sl <= 0 {
		return 0
	}
	switch side {
	case "long":
		if tp <= entry || sl >= entry {
			return 0
		}
		return (tp - entry) / (entry - sl)
	case "short":
		if tp >= entry || sl <= entry {
			return 0
		}
		return (entry - tp) / (sl - entry)
	default:
		return 0
	}
}

func (r *simRunner) partialCloseQty(d ai.Decision, price, total float64) float64 {
	if price <= 0 || total <= 0 {
		return 0
	}
	if ratio := d.CloseRatio; ratio > 0 {
		if ratio > 1 {
			ratio = 1
		}
		qty := total * ratio
		if qty > 0 {
			return qty
		}
	}
	if d.PositionSizeUSD > 0 {
		qty := d.PositionSizeUSD / price
		if qty > total {
			qty = total
		}
		if qty > 0 {
			return qty
		}
	}
	qty := total / 2
	if qty <= 0 {
		return total
	}
	return qty
}

func (r *simRunner) handleAdjustStopLoss(ctx context.Context, runID string, state *portfolioState, candle Candle, d ai.Decision, ts time.Time) {
	pos := state.position
	if pos == nil {
		return
	}
	newSL := d.StopLoss
	if newSL <= 0 {
		return
	}
	price := candle.Close
	if price <= 0 {
		return
	}
	switch pos.side {
	case "long":
		if newSL >= price {
			return
		}
	case "short":
		if newSL <= price {
			return
		}
	default:
		return
	}
	if math.Abs(pos.stopLoss-newSL) < 1e-8 {
		return
	}
	oldSL := pos.stopLoss
	pos.stopLoss = newSL
	pos.expectedRR = calcExpectedRR(pos.side, pos.entryPrice, pos.takeProfit, pos.stopLoss)

	rawDecision, _ := json.Marshal(d)
	meta := map[string]float64{
		"old_stop_loss": oldSL,
		"new_stop_loss": newSL,
	}
	metaJSON, _ := json.Marshal(meta)
	if _, err := r.orderRecorder.RecordOrder(ctx, &execution.Order{
		RunID:      runID,
		Symbol:     r.cfg.Symbol,
		Action:     "adjust_stop_loss",
		Side:       pos.side,
		Type:       "adjustment",
		Price:      newSL,
		Quantity:   pos.qty,
		Notional:   pos.qty * newSL,
		Fee:        0,
		Timeframe:  r.cfg.ExecutionTimeframe,
		DecidedAt:  ts,
		ExecutedAt: ts,
		TakeProfit: pos.takeProfit,
		StopLoss:   pos.stopLoss,
		ExpectedRR: pos.expectedRR,
		Decision:   rawDecision,
		Meta:       metaJSON,
	}); err != nil {
		logger.Warnf("[backtest] run %s 记录调整止损失败: %v", runID, err)
	}
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
