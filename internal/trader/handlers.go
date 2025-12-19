package trader

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"brale/internal/gateway/database"
	"brale/internal/gateway/exchange"
	"brale/internal/logger"
)

func (t *Trader) handleSignalEntry(payload json.RawMessage, traceID string) error {
	var sp SignalEntryPayload
	var input exchange.OpenRequest

	if err := json.Unmarshal(payload, &sp); err == nil && sp.Order.Symbol != "" {
		input = sp.Order
	} else {
		if err := json.Unmarshal(payload, &input); err != nil {
			return fmt.Errorf("invalid payload for signal_entry: %w", err)
		}
	}

	symbol := normalizeSymbol(input.Symbol)
	if symbol == "" {
		return fmt.Errorf("signal entry missing symbol")
	}
	input.Symbol = symbol

	logger.Infof("Trader handling signal entry for %s %s (async)", input.Symbol, input.Side)

	if _, exists := t.state.Positions[symbol]; exists {
		logger.Warnf("Position already exists for %s, ignoring entry signal", input.Symbol)
		return nil
	}

	go func() {

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		logger.Infof("Open request -> symbol=%s side=%s amount=%.4f leverage=%.2f price=%.4f",
			input.Symbol, input.Side, input.Amount, input.Leverage, input.Price)

		result, err := t.executor.OpenPosition(ctx, input)
		reqID := traceID
		if reqID == "" {
			reqID = newEventID("open")
		}

		res := OrderResultPayload{
			RequestID: reqID,
			Action:    OrderActionOpen,
			Reason:    "signal_entry",
			Symbol:    input.Symbol,
			Side:      input.Side,
			Original:  input,
			Timestamp: time.Now(),
		}
		if result != nil {
			res.TradeID = result.PositionID
			res.OrderID = result.OrderID
		}
		if err != nil {
			res.Error = err.Error()
		}

		payloadBytes, _ := json.Marshal(res)
		if err := t.Send(EventEnvelope{
			ID:        newEventID("order-result"),
			Type:      EvtOrderResult,
			Payload:   payloadBytes,
			CreatedAt: time.Now(),
			Symbol:    normalizeSymbol(input.Symbol),
		}); err != nil {
			logger.Warnf("Trader: send order-result failed: %v", err)
		}
	}()

	return nil
}

func (t *Trader) applyPositionOpening(payload json.RawMessage) error {
	var p PositionOpeningPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return fmt.Errorf("invalid payload for position_opening: %w", err)
	}
	symbol := normalizeSymbol(p.Symbol)
	if symbol == "" {
		return fmt.Errorf("position_opening missing symbol")
	}
	createdAt := p.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	if t.posStore != nil {
		rec := database.LiveOrderRecord{
			Symbol:    symbol,
			Side:      strings.ToLower(strings.TrimSpace(p.Side)),
			Status:    database.LiveOrderStatusOpening,
			StartTime: &createdAt,
			CreatedAt: createdAt,
			UpdatedAt: createdAt,
		}
		if p.TradeID != "" {
			if id, err := strconv.Atoi(strings.TrimSpace(p.TradeID)); err == nil {
				rec.FreqtradeID = id
			}
		}
		if p.Stake > 0 {
			stake := p.Stake
			rec.StakeAmount = &stake
			rec.PositionValue = &stake
		}
		if p.Leverage > 0 {
			lev := p.Leverage
			rec.Leverage = &lev
		}
		if p.Amount > 0 {
			amt := p.Amount
			rec.InitialAmount = &amt
			rec.Amount = &amt
		}
		if p.Price > 0 {
			price := p.Price
			rec.Price = &price
		}
		if err := t.posStore.UpsertLiveOrder(context.Background(), rec); err != nil {
			return fmt.Errorf("failed to persist opening position for %s: %w", symbol, err)
		}
	}
	if p.TradeID != "" {
		t.state.ByTradeID[p.TradeID] = symbol
		t.state.SymbolIndex[symbol] = p.TradeID
	}
	return nil
}

func (t *Trader) applyPositionOpened(payload json.RawMessage) error {
	var p PositionOpenedPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return fmt.Errorf("invalid payload for position_opened: %w", err)
	}
	symbol := normalizeSymbol(p.Symbol)
	if symbol == "" {
		return fmt.Errorf("position_opened missing symbol")
	}

	openedAt := defaultTime(p.OpenedAt)
	side := strings.ToLower(strings.TrimSpace(p.Side))
	tradeID := parseTradeIDSafe(p.TradeID)

	if t.posStore != nil && tradeID > 0 {
		if err := t.upsertOpenedPositionDB(symbol, side, openedAt, tradeID, p); err != nil {
			return err
		}
	}

	t.updateOpenedPositionState(symbol, side, openedAt, p)
	if p.TradeID != "" {
		t.state.ByTradeID[p.TradeID] = symbol
		t.state.SymbolIndex[symbol] = p.TradeID
	}

	t.refreshSnapshot(true)

	logger.Infof("State Updated: Position opened for %s (ID: %s)", symbol, p.TradeID)
	return nil
}

func defaultTime(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now()
	}
	return t
}

func parseTradeIDSafe(raw string) int {
	if strings.TrimSpace(raw) == "" {
		return 0
	}
	if id, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil {
		return id
	}
	logger.Warnf("TradeID %s is not numeric, skipping DB mapping", raw)
	return 0
}

// upsertOpenedPositionDB ensures the DB row exists and is populated.
// This keeps historical fills coherent even if websockets were missed earlier.
func (t *Trader) upsertOpenedPositionDB(symbol, side string, openedAt time.Time, tradeID int, p PositionOpenedPayload) error {
	ctx := context.Background()
	var rec database.LiveOrderRecord
	if prev, ok, err := t.posStore.GetLivePosition(ctx, tradeID); err != nil {
		logger.Warnf("failed to load previous position record trade=%d symbol=%s: %v", tradeID, symbol, err)
	} else if ok {
		rec = prev
	}

	rec.FreqtradeID = tradeID
	rec.Symbol = symbol
	rec.Side = side
	rec.Status = database.LiveOrderStatusOpen
	rec.StartTime = &openedAt

	if p.Amount > 0 {
		rec.Amount = ptrFloat(p.Amount)
		if rec.InitialAmount == nil || *rec.InitialAmount <= 0 {
			rec.InitialAmount = ptrFloat(p.Amount)
		}
	}
	if p.Price > 0 {
		rec.Price = ptrFloat(p.Price)
	}
	if p.Stake > 0 {
		rec.StakeAmount = ptrFloat(p.Stake)
		if rec.PositionValue == nil || *rec.PositionValue <= 0 {
			rec.PositionValue = ptrFloat(p.Stake)
		}
	}
	if p.Leverage > 0 {
		rec.Leverage = ptrFloat(p.Leverage)
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = openedAt
	}
	rec.UpdatedAt = openedAt

	if err := t.posStore.UpsertLiveOrder(ctx, rec); err != nil {
		return fmt.Errorf("failed to persist position to DB for %s: %w", symbol, err)
	}
	return nil
}

func (t *Trader) updateOpenedPositionState(symbol, side string, openedAt time.Time, p PositionOpenedPayload) {
	pos := &exchange.Position{
		ID:            strings.TrimSpace(p.TradeID),
		Symbol:        symbol,
		Side:          side,
		Amount:        p.Amount,
		InitialAmount: p.Amount,
		EntryPrice:    p.Price,
		Leverage:      p.Leverage,
		StakeAmount:   p.Stake,
		OpenedAt:      openedAt,
		UpdatedAt:     openedAt,
		IsOpen:        true,
	}
	t.state.Positions[symbol] = pos
}

func ptrFloat(v float64) *float64 { return &v }

func (t *Trader) applyPositionClosing(payload json.RawMessage) error {
	var p PositionClosingPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return fmt.Errorf("invalid payload for position_closing: %w", err)
	}
	if t.posStore != nil {
		if id, err := strconv.Atoi(strings.TrimSpace(p.TradeID)); err == nil && id > 0 {
			if err := t.posStore.UpdateOrderStatus(context.Background(), id, database.LiveOrderStatusClosingFull); err != nil {
				return fmt.Errorf("failed to mark trade %d closing: %w", id, err)
			}
		}
	}
	return nil
}

func (t *Trader) applyPositionClosed(payload json.RawMessage) error {
	var p PositionClosedPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return fmt.Errorf("invalid payload for position_closed: %w", err)
	}

	symbol, err := t.resolveClosedSymbol(p)
	if err != nil {
		return err
	}

	if !isFullClose(p.RemainingAmount) {
		return t.handlePartialClose(symbol, p)
	}
	return t.handleFullClose(symbol, p)
}

func (t *Trader) resolveClosedSymbol(p PositionClosedPayload) (string, error) {
	symbol := normalizeSymbol(p.Symbol)
	if symbol == "" && p.TradeID != "" {
		if sym, ok := t.state.ByTradeID[p.TradeID]; ok {
			symbol = sym
		}
	}
	if symbol == "" {
		return "", fmt.Errorf("position_closed missing symbol and trade id")
	}
	return symbol, nil
}

func isFullClose(remaining float64) bool {
	const eps = 1e-8
	return remaining <= eps
}

func (t *Trader) handlePartialClose(symbol string, p PositionClosedPayload) error {
	pos, exists := t.state.Positions[symbol]
	if exists && pos != nil {
		pos.Amount = p.RemainingAmount
		pos.UpdatedAt = time.Now()
	}
	if err := t.markOrderStatus(p.TradeID, database.LiveOrderStatusClosingPartial); err != nil {
		return err
	}
	t.refreshSnapshot(true)
	logger.Infof("State Updated: Position partially closed for %s, 剩余 %.4f", symbol, p.RemainingAmount)
	return nil
}

func (t *Trader) handleFullClose(symbol string, p PositionClosedPayload) error {
	if _, exists := t.state.Positions[symbol]; exists {
		delete(t.state.Positions, symbol)
	}
	if p.TradeID != "" {
		delete(t.state.ByTradeID, p.TradeID)
		delete(t.state.Plans, p.TradeID)
	}
	delete(t.state.SymbolIndex, symbol)

	if err := t.markOrderStatus(p.TradeID, database.LiveOrderStatusClosed); err != nil {
		return err
	}
	t.refreshSnapshot(true)
	logger.Infof("State Updated: Position closed for %s (ID: %s)", symbol, p.TradeID)
	return nil
}

func (t *Trader) markOrderStatus(tradeID string, status database.LiveOrderStatus) error {
	if t.posStore == nil {
		return nil
	}
	if id, err := strconv.Atoi(strings.TrimSpace(tradeID)); err == nil && id > 0 {
		if err := t.posStore.UpdateOrderStatus(context.Background(), id, status); err != nil {
			return fmt.Errorf("failed to mark trade %d %v: %w", id, status, err)
		}
	}
	return nil
}
