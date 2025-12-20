package freqtrade

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"brale/internal/gateway/database"
	"brale/internal/gateway/exchange"
)

func (m *Manager) ListFreqtradePositions(ctx context.Context, opts exchange.PositionListOptions) (exchange.PositionListResult, error) {
	if m.posRepo == nil {
		return exchange.PositionListResult{}, fmt.Errorf("posRepo not initialized")
	}

	params := normalizePositionListParams(opts)
	now := time.Now().UnixMilli()

	switch params.status {
	case "active", "open":
		if list, ok := m.listActivePositionsFromTrader(ctx, now, params.symbolFilter); ok {
			return finalizePositionList(ctx, m, list, params.page, params.limit, params.offset), nil
		}
		return m.listActivePositionsFromRepo(ctx, now, params)

	case "closed":
		return m.listRecentPositions(ctx, now, params, listRecentClosed)

	case "all":
		return m.listRecentPositions(ctx, now, params, listRecentAll)
	default:
		return exchange.PositionListResult{}, fmt.Errorf("unknown status: %s", params.status)
	}
}

type positionListParams struct {
	limit        int
	page         int
	offset       int
	status       string
	symbolFilter string
}

func normalizePositionListParams(opts exchange.PositionListOptions) positionListParams {
	limit := opts.PageSize
	if limit <= 0 {
		limit = 100
	}
	page := opts.Page
	if page < 1 {
		page = 1
	}
	status := strings.ToLower(strings.TrimSpace(opts.Status))
	if status == "" {
		status = "active"
	}
	return positionListParams{
		limit:        limit,
		page:         page,
		offset:       (page - 1) * limit,
		status:       status,
		symbolFilter: strings.ToUpper(strings.TrimSpace(opts.Symbol)),
	}
}

func finalizePositionList(ctx context.Context, m *Manager, list []exchange.APIPosition, page, limit, offset int) exchange.PositionListResult {
	total := len(list)
	start, end := paginateRange(total, offset, limit)
	pageList := list[start:end]
	if m != nil {
		m.hydrateAPIPositionExits(ctx, pageList)
	}
	return exchange.PositionListResult{
		TotalCount: total,
		Page:       page,
		PageSize:   limit,
		Positions:  pageList,
	}
}

func paginateRange(total, offset, limit int) (int, int) {
	if total <= 0 || limit <= 0 {
		return 0, 0
	}
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	return offset, end
}

func (m *Manager) listActivePositionsFromTrader(ctx context.Context, now int64, symbolFilter string) ([]exchange.APIPosition, bool) {
	if m == nil || m.trader == nil {
		return nil, false
	}
	snap := m.trader.Snapshot()
	if snap == nil || len(snap.Positions) == 0 {
		return nil, false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	realizedByTrade := map[int]database.LiveOrderRecord{}
	if m.posRepo != nil {
		if recs, err := m.posRepo.ListActivePositions(ctx, 500); err == nil {
			for _, rec := range recs {
				if symbolFilter != "" && !strings.EqualFold(rec.Symbol, symbolFilter) {
					continue
				}
				if rec.FreqtradeID > 0 {
					realizedByTrade[rec.FreqtradeID] = rec
				}
			}
		}
	}
	list := make([]exchange.APIPosition, 0, len(snap.Positions))
	for _, p := range snap.Positions {
		if p == nil || !p.IsOpen {
			continue
		}
		if symbolFilter != "" && !strings.EqualFold(p.Symbol, symbolFilter) {
			continue
		}
		pos := exchangePositionToAPIPosition(*p, now)
		if rec, ok := realizedByTrade[pos.TradeID]; ok {
			realizedUSD := valOrZero(rec.RealizedPnLUSD)
			if realizedUSD != 0 {
				baseStake := deriveBaseStake(pos)
				totalUSD := realizedUSD + pos.UnrealizedPnLUSD
				pos.PnLUSD = totalUSD
				if baseStake > 0 {
					pos.PnLRatio = totalUSD / baseStake
				} else if pos.PnLRatio == 0 {
					pos.PnLRatio = pos.UnrealizedPnLRatio + valOrZero(rec.RealizedPnLRatio)
				}
			}
		}
		list = append(list, pos)
	}
	if len(list) == 0 {
		return nil, false
	}
	sort.Slice(list, func(i, j int) bool { return list[i].OpenedAt > list[j].OpenedAt })
	return list, true
}

func (m *Manager) listActivePositionsFromRepo(ctx context.Context, now int64, params positionListParams) (exchange.PositionListResult, error) {
	activeOrders, err := m.posRepo.ListActivePositions(ctx, 500)
	if err != nil {
		return exchange.PositionListResult{}, err
	}
	activeOrders = filterOrdersBySymbol(activeOrders, params.symbolFilter)
	positions := make([]exchange.APIPosition, 0, len(activeOrders))
	for _, o := range activeOrders {
		positions = append(positions, liveOrderToAPIPosition(o, now))
	}
	return finalizePositionList(ctx, m, positions, params.page, params.limit, params.offset), nil
}

func filterOrdersBySymbol(recs []database.LiveOrderRecord, symbolFilter string) []database.LiveOrderRecord {
	if symbolFilter == "" || len(recs) == 0 {
		return recs
	}
	filtered := recs[:0]
	for _, rec := range recs {
		if strings.EqualFold(rec.Symbol, symbolFilter) {
			filtered = append(filtered, rec)
		}
	}
	return filtered
}

type listRecentMode int

const (
	listRecentAll listRecentMode = iota
	listRecentClosed
)

func (m *Manager) listRecentPositions(ctx context.Context, now int64, params positionListParams, mode listRecentMode) (exchange.PositionListResult, error) {
	fetch := params.offset + params.limit
	switch mode {
	case listRecentClosed:
		fetch = clampFetch(fetch, 50, 1000)
	default:
		fetch = clampFetch(fetch, 100, 2000)
	}

	recs, err := m.posRepo.ListRecentPositionsPaged(ctx, params.symbolFilter, fetch, 0)
	if err != nil {
		return exchange.PositionListResult{}, err
	}
	if mode == listRecentClosed {
		recs = filterClosedOrders(recs)
	}
	positions := make([]exchange.APIPosition, 0, len(recs))
	for _, o := range recs {
		positions = append(positions, liveOrderToAPIPosition(o, now))
	}
	return finalizePositionList(ctx, m, positions, params.page, params.limit, params.offset), nil
}

func clampFetch(fetch, min, max int) int {
	if fetch < min {
		return min
	}
	if fetch > max {
		return max
	}
	return fetch
}

func filterClosedOrders(recs []database.LiveOrderRecord) []database.LiveOrderRecord {
	if len(recs) == 0 {
		return recs
	}
	closed := recs[:0]
	for _, rec := range recs {
		if rec.Status == database.LiveOrderStatusClosed {
			closed = append(closed, rec)
		}
	}
	return closed
}

func (m *Manager) APIPositionByID(ctx context.Context, tradeID int) (*exchange.APIPosition, error) {
	if tradeID <= 0 {
		return nil, fmt.Errorf("invalid trade_id")
	}
	if m == nil || m.posRepo == nil {
		return nil, fmt.Errorf("posRepo not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	rec, ok, err := m.posRepo.GetPosition(ctx, tradeID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("position not found")
	}
	now := time.Now().UnixMilli()
	pos := liveOrderToAPIPosition(rec, now)
	m.hydrateAPIPositionExit(ctx, &pos)
	attachCloseHistory(&pos, rec)
	return &pos, nil
}

func (m *Manager) GetFreqtradePosition(ctx context.Context, tradeID int) (*exchange.APIPosition, error) {
	return m.APIPositionByID(ctx, tradeID)
}

func (m *Manager) RefreshAPIPosition(ctx context.Context, tradeID int) (*exchange.APIPosition, error) {
	if tradeID <= 0 {
		return nil, fmt.Errorf("invalid trade_id")
	}
	if m == nil || m.posRepo == nil {
		return nil, fmt.Errorf("posRepo not initialized")
	}
	if m.client == nil {
		return nil, fmt.Errorf("freqtrade client not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := m.reconcileTrade(ctx, tradeID); err != nil && !errors.Is(err, errTradeNotFound) {
		return nil, err
	}
	return m.APIPositionByID(ctx, tradeID)
}

func (m *Manager) ListOpenPositions(ctx context.Context) ([]exchange.Position, error) {
	if m.posRepo == nil {
		return nil, nil
	}
	recs, err := m.posRepo.ListActivePositions(ctx, 100)
	if err != nil {
		return nil, err
	}
	var out []exchange.Position
	for _, r := range recs {
		pos := exchange.Position{
			ID:                 strconv.Itoa(r.FreqtradeID),
			Symbol:             r.Symbol,
			Side:               r.Side,
			Amount:             valOrZero(r.Amount),
			InitialAmount:      valOrZero(r.InitialAmount),
			EntryPrice:         valOrZero(r.Price),
			Leverage:           valOrZero(r.Leverage),
			StakeAmount:        valOrZero(r.StakeAmount),
			IsOpen:             r.Status == 1,
			UnrealizedPnL:      valOrZero(r.UnrealizedPnLUSD),
			UnrealizedPnLRatio: valOrZero(r.UnrealizedPnLRatio),
			CurrentPrice:       valOrZero(r.CurrentPrice),
		}
		if r.StartTime != nil {
			pos.OpenedAt = *r.StartTime
		}
		out = append(out, pos)
	}
	return out, nil
}

func (m *Manager) PositionsForAPI(ctx context.Context, opts exchange.PositionListOptions) (exchange.PositionListResult, error) {
	return m.ListFreqtradePositions(ctx, opts)
}

func (m *Manager) TradeIDBySymbol(symbol string) (int, bool) {
	if m == nil || m.trader == nil {
		return 0, false
	}
	sym := strings.ToUpper(strings.TrimSpace(symbol))
	if sym == "" {
		return 0, false
	}
	snap := m.trader.Snapshot()
	if snap == nil {
		return 0, false
	}
	idStr := strings.TrimSpace(snap.TradeIDBySymbol(sym))
	if idStr == "" {
		return 0, false
	}
	id, err := strconv.Atoi(idStr)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

func (m *Manager) EntryPriceBySymbol(symbol string) float64 {
	if m == nil {
		return 0
	}
	return m.lookupEntryPrice(context.Background(), 0, symbol)
}

func (m *Manager) ListFreqtradeEvents(ctx context.Context, tradeID int, limit int) ([]exchange.TradeEvent, error) {
	if m.trader != nil {
		if events := snapshotTradeEvents(m.trader.Snapshot(), tradeID, limit); events != nil {
			return events, nil
		}
	}
	if m.posRepo == nil {
		return nil, fmt.Errorf("posRepo not initialized")
	}
	recs, err := m.posRepo.TradeEvents(ctx, tradeID, limit)
	if err != nil {
		return nil, err
	}
	return convertToExchangeTradeEvents(recs), nil
}
