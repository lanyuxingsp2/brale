package livehttp

import (
	"bufio"
	"context"
	"database/sql"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"brale/internal/gateway/database"
	"brale/internal/gateway/exchange"
	"brale/internal/gateway/freqtrade"
	"brale/internal/logger"

	"github.com/gin-gonic/gin"
)

type Router struct {
	Logs             *database.DecisionLogStore
	FreqtradeHandler FreqtradeWebhookHandler
	logPaths         map[string]string
	logNames         []string
}

func NewRouter(logs *database.DecisionLogStore, handler FreqtradeWebhookHandler, logPaths map[string]string) *Router {
	names := make([]string, 0, len(logPaths))
	for name, path := range logPaths {
		if strings.TrimSpace(path) == "" || strings.TrimSpace(name) == "" {
			continue
		}
		names = append(names, name)
	}
	return &Router{Logs: logs, FreqtradeHandler: handler, logPaths: logPaths, logNames: names}
}

func (r *Router) Register(group *gin.RouterGroup) {
	if group == nil {
		return
	}
	group.GET("/decisions", r.handleLiveDecisions)
	group.GET("/decisions/:id", r.handleDecisionByID)
	group.GET("/traces", r.handleLiveDecisions)
	group.GET("/logs", r.handleLiveLogs)
	group.GET("/plans/changes", r.handlePlanChanges)
	group.GET("/plans/instances", r.handlePlanInstances)
	if r.FreqtradeHandler != nil {
		group.POST("/freqtrade/webhook", r.handleFreqtradeWebhook)
		group.GET("/freqtrade/positions", r.handleFreqtradePositions)
		group.GET("/freqtrade/positions/:id", r.handleFreqtradePositionDetail)
		group.POST("/freqtrade/positions/:id/refresh", r.handleFreqtradePositionRefresh)
		group.POST("/freqtrade/close", r.handleFreqtradeQuickClose)

		group.POST("/freqtrade/manual-open", r.handleFreqtradeManualOpen)
		group.GET("/freqtrade/price", r.handleFreqtradePriceQuote)
		group.GET("/freqtrade/events", r.handleFreqtradeEvents)
		group.POST("/plans/adjust", r.handlePlanAdjust)
	}
}

type FreqtradeWebhookHandler interface {
	HandleFreqtradeWebhook(ctx context.Context, msg exchange.WebhookMessage) error
	ListFreqtradePositions(ctx context.Context, opts exchange.PositionListOptions) (exchange.PositionListResult, error)
	CloseFreqtradePosition(ctx context.Context, tradeID int, symbol, side string, closeRatio float64) error

	ListFreqtradeEvents(ctx context.Context, tradeID int, limit int) ([]exchange.TradeEvent, error)
	ManualOpenPosition(ctx context.Context, req exchange.ManualOpenRequest) error
	GetLatestPriceQuote(ctx context.Context, symbol string) (exchange.PriceQuote, error)
	AdjustPlan(ctx context.Context, req PlanAdjustRequest) error
}

type PlanAdjustRequest struct {
	TradeID   int                    `json:"trade_id"`
	PlanID    string                 `json:"plan_id"`
	Component string                 `json:"plan_component"`
	Params    map[string]interface{} `json:"params"`
}

func (r *Router) handleLiveDecisions(c *gin.Context) {
	if r.Logs == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "实时日志未启用"})
		return
	}
	page, pageSize, offset := parsePagination(c)
	query := buildLiveDecisionQuery(c, pageSize, offset)

	reqCtx := c.Request.Context()
	logs, err := r.fetchLiveDecisions(reqCtx, query)
	if err != nil {
		logger.Errorf("[api] live decisions list failed ip=%s err=%v", c.ClientIP(), err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	includeCount := shouldIncludeCount(c, query)
	total := r.maybeCountDecisions(reqCtx, query, includeCount, c.ClientIP())

	traces := database.BuildLiveDecisionTraces(logs)
	logger.Debugf("[api] live decisions ip=%s page=%d size=%d symbol=%s provider=%s total=%d", c.ClientIP(), page, pageSize, query.Symbol, query.Provider, total)
	c.JSON(http.StatusOK, gin.H{
		"logs":        logs,
		"traces":      traces,
		"total_count": total,
		"page":        page,
		"page_size":   pageSize,
	})
}

func parsePagination(c *gin.Context) (int, int, int) {
	pageParam := strings.TrimSpace(c.Query("page"))
	page, _ := strconv.Atoi(pageParam)
	if page < 0 {
		page = 0
	}
	pageSize := parsePageSize(c)
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if offset < 0 {
		offset = 0
	}
	if page > 0 {
		offset = (page - 1) * pageSize
	} else {
		page = offset/pageSize + 1
	}
	return page, pageSize, offset
}

func parsePageSize(c *gin.Context) int {
	candidates := []string{"pageSize", "page_size", "limit"}
	for _, key := range candidates {
		if val, _ := strconv.Atoi(c.DefaultQuery(key, "0")); val > 0 {
			return clampPageSize(val)
		}
	}
	return clampPageSize(100)
}

func clampPageSize(val int) int {
	switch {
	case val <= 0:
		return 100
	case val > 500:
		return 500
	default:
		return val
	}
}

func buildLiveDecisionQuery(c *gin.Context, pageSize, offset int) database.LiveDecisionQuery {
	return database.LiveDecisionQuery{
		Limit:    pageSize,
		Offset:   offset,
		Provider: c.Query("provider"),
		Stage:    c.DefaultQuery("stage", "core"),
		Symbol:   c.Query("symbol"),
		Symbols:  c.QueryArray("symbol"),
	}
}

func (r *Router) fetchLiveDecisions(ctx context.Context, query database.LiveDecisionQuery) ([]database.DecisionLogRecord, error) {
	listCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return r.Logs.ListDecisions(listCtx, query)
}

func shouldIncludeCount(c *gin.Context, query database.LiveDecisionQuery) bool {
	includeCount := parseBoolDefaultTrue(c.DefaultQuery("include_count", "1"))
	if !includeCount {
		return false
	}
	if query.Offset != 0 || query.Limit <= 0 || query.Limit > 20 {
		return includeCount
	}
	if !strings.EqualFold(strings.TrimSpace(query.Stage), "final") {
		return includeCount
	}
	if len(query.Symbols) > 0 || strings.TrimSpace(query.Symbol) != "" {
		return false
	}
	return includeCount
}

func (r *Router) maybeCountDecisions(ctx context.Context, query database.LiveDecisionQuery, include bool, clientIP string) int {
	if !include {
		return -1
	}
	countCtx, cancel := context.WithTimeout(ctx, 800*time.Millisecond)
	defer cancel()
	count, err := r.Logs.CountDecisions(countCtx, query)
	if err != nil {
		logger.Warnf("[api] live decisions count failed ip=%s err=%v", clientIP, err)
		return -1
	}
	return count
}

func (r *Router) handleDecisionByID(c *gin.Context) {
	if r.Logs == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "实时日志未启用"})
		return
	}
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	if id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid decision id"})
		return
	}
	ctx := c.Request.Context()
	log, err := r.Logs.GetDecision(ctx, id)
	if err != nil {
		if err == sql.ErrNoRows {
			logger.Warnf("[api] live decision detail not found ip=%s id=%d", c.ClientIP(), id)
			c.JSON(http.StatusNotFound, gin.H{"error": "decision not found"})
			return
		}
		logger.Errorf("[api] live decision detail failed ip=%s id=%d err=%v", c.ClientIP(), id, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	var (
		trace *database.LiveDecisionTrace
		round *database.DecisionRoundSummary
	)
	traceID := strings.TrimSpace(log.TraceID)
	if traceID != "" {
		if steps, err := r.Logs.ListDecisionsByTraceID(ctx, traceID, 200); err == nil {
			if traces := database.BuildLiveDecisionTraces(steps); len(traces) > 0 {
				trace = &traces[0]
			}
			traceMap := map[string][]database.DecisionLogRecord{
				traceID: steps,
			}
			if summaries := database.BuildDecisionRoundSummaries([]database.DecisionLogRecord{log}, traceMap); len(summaries) > 0 {
				round = &summaries[0]
			}
		}
	}
	if trace == nil {
		if traces := database.BuildLiveDecisionTraces([]database.DecisionLogRecord{log}); len(traces) > 0 {
			trace = &traces[0]
		}
	}
	if round == nil {
		if summaries := database.BuildDecisionRoundSummaries([]database.DecisionLogRecord{log}, nil); len(summaries) > 0 {
			round = &summaries[0]
		}
	}
	logger.Infof("[api] live decision detail ip=%s id=%d trace=%s", c.ClientIP(), id, traceID)
	c.JSON(http.StatusOK, gin.H{
		"log":   log,
		"trace": trace,
		"round": round,
	})
}

func (r *Router) handleFreqtradeWebhook(c *gin.Context) {
	if r.FreqtradeHandler == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "未配置 freqtrade 处理器"})
		return
	}

	var ftPayload freqtrade.WebhookMessage
	if err := c.ShouldBindJSON(&ftPayload); err != nil {
		logger.Errorf("[api] freqtrade webhook bind failed ip=%s err=%v", c.ClientIP(), err)
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	payload := exchange.WebhookMessage{
		Type:        ftPayload.Type,
		TradeID:     int64(ftPayload.TradeID),
		Pair:        ftPayload.Pair,
		Direction:   ftPayload.Direction,
		Amount:      float64(ftPayload.Amount),
		StakeAmount: float64(ftPayload.StakeAmount),
		OpenDate:    ftPayload.OpenDate,
		CloseDate:   ftPayload.CloseDate,
		CloseRate:   float64(ftPayload.CloseRate),
		OpenRate:    float64(ftPayload.OpenRate),
		ExitReason:  ftPayload.ExitReason,
		Reason:      ftPayload.Reason,
		Leverage:    int(ftPayload.Leverage),
	}
	logger.Infof("[api] freqtrade webhook ip=%s type=%s trade_id=%d", c.ClientIP(), strings.ToLower(strings.TrimSpace(payload.Type)), int(payload.TradeID))
	if err := r.FreqtradeHandler.HandleFreqtradeWebhook(c.Request.Context(), payload); err != nil {
		logger.Errorf("[api] freqtrade webhook handler failed ip=%s trade_id=%d err=%v", c.ClientIP(), int(payload.TradeID), err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (r *Router) handleFreqtradePositions(c *gin.Context) {
	if r.FreqtradeHandler == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "未配置 freqtrade 处理器"})
		return
	}
	symbol := strings.ToUpper(strings.TrimSpace(c.Query("symbol")))
	status := strings.ToLower(strings.TrimSpace(c.DefaultQuery("status", "active")))
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	if page < 1 {
		page = 1
	}
	pageSize, _ := strconv.Atoi(c.DefaultQuery("pageSize", "0"))
	if pageSize <= 0 {
		pageSize, _ = strconv.Atoi(c.DefaultQuery("page_size", "0"))
	}
	if pageSize <= 0 {
		pageSize, _ = strconv.Atoi(c.DefaultQuery("limit", "10"))
	}
	if pageSize <= 0 {
		pageSize = 10
	}
	if pageSize > 500 {
		pageSize = 500
	}
	logsLimit, _ := strconv.Atoi(c.DefaultQuery("logs_limit", "20"))
	includeLogs := parseBoolDefaultTrue(c.DefaultQuery("include_logs", "1"))
	opts := exchange.PositionListOptions{
		Symbol:      symbol,
		Page:        page,
		PageSize:    pageSize,
		Status:      status,
		IncludeLogs: includeLogs,
		LogsLimit:   logsLimit,
	}
	reqCtx := c.Request.Context()
	callCtx, cancel := context.WithTimeout(reqCtx, 10*time.Second)
	result, err := r.FreqtradeHandler.ListFreqtradePositions(callCtx, opts)
	cancel()
	if err != nil {
		logger.Errorf("[api] freqtrade positions failed ip=%s symbol=%s err=%v", c.ClientIP(), symbol, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	logger.Debugf("[api] freqtrade positions ip=%s symbol=%s page=%d size=%d include_logs=%v total=%d", c.ClientIP(), symbol, opts.Page, opts.PageSize, opts.IncludeLogs, result.TotalCount)
	c.JSON(http.StatusOK, gin.H{
		"total_count": result.TotalCount,
		"page":        result.Page,
		"page_size":   result.PageSize,
		"positions":   result.Positions,
	})
}

func (r *Router) handleFreqtradePositionDetail(c *gin.Context) {
	if r.FreqtradeHandler == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "未配置 freqtrade 处理器"})
		return
	}
	tradeID, _ := strconv.Atoi(c.Param("id"))
	if tradeID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid trade_id"})
		return
	}
	type apiPositionWithPlans struct {
		exchange.APIPosition
		Plans []database.StrategyInstanceRecord `json:"plans,omitempty"`
	}
	type positionGetter interface {
		GetFreqtradePosition(context.Context, int) (*exchange.APIPosition, error)
	}
	if getter, ok := r.FreqtradeHandler.(positionGetter); ok {
		pos, err := getter.GetFreqtradePosition(c.Request.Context(), tradeID)
		if err != nil {
			logger.Warnf("[api] freqtrade position detail failed ip=%s trade_id=%d err=%v", c.ClientIP(), tradeID, err)
			c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
			return
		}
		var plans []database.StrategyInstanceRecord
		if r.Logs != nil {
			if recs, err := r.Logs.ListStrategyInstances(c.Request.Context(), tradeID); err == nil {
				plans = recs
			} else {
				logger.Warnf("[api] freqtrade position detail load plans failed ip=%s trade_id=%d err=%v", c.ClientIP(), tradeID, err)
			}
		}
		logger.Debugf("[api] freqtrade position detail ip=%s trade_id=%d symbol=%s side=%s",
			c.ClientIP(), tradeID, strings.ToUpper(strings.TrimSpace(pos.Symbol)), strings.ToLower(strings.TrimSpace(pos.Side)))
		c.JSON(http.StatusOK, gin.H{
			"position": apiPositionWithPlans{APIPosition: *pos, Plans: plans},
		})
		return
	}

	logsLimit, _ := strconv.Atoi(c.DefaultQuery("logs_limit", "80"))
	opts := exchange.PositionListOptions{
		Page:        1,
		PageSize:    1000,
		Status:      "all",
		IncludeLogs: true,
		LogsLimit:   logsLimit,
	}
	result, err := r.FreqtradeHandler.ListFreqtradePositions(c.Request.Context(), opts)
	if err != nil {
		logger.Errorf("[api] freqtrade position detail list failed ip=%s trade_id=%d err=%v", c.ClientIP(), tradeID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	var target *exchange.APIPosition
	for idx, p := range result.Positions {
		if p.TradeID == tradeID {
			target = &result.Positions[idx]
			break
		}
	}
	if target == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "position not found (maybe too old)"})
		return
	}
	var plans []database.StrategyInstanceRecord
	if r.Logs != nil {
		if recs, err := r.Logs.ListStrategyInstances(c.Request.Context(), tradeID); err == nil {
			plans = recs
		} else {
			logger.Warnf("[api] freqtrade position detail load plans failed ip=%s trade_id=%d err=%v", c.ClientIP(), tradeID, err)
		}
	}
	logger.Debugf("[api] freqtrade position detail ip=%s trade_id=%d symbol=%s side=%s",
		c.ClientIP(), tradeID, strings.ToUpper(strings.TrimSpace(target.Symbol)), strings.ToLower(strings.TrimSpace(target.Side)))
	c.JSON(http.StatusOK, gin.H{
		"position": apiPositionWithPlans{APIPosition: *target, Plans: plans},
	})
}

func (r *Router) handleFreqtradePositionRefresh(c *gin.Context) {
	if r.FreqtradeHandler == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "未配置 freqtrade 处理器"})
		return
	}
	tradeID, _ := strconv.Atoi(c.Param("id"))
	if tradeID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid trade_id"})
		return
	}
	type refresher interface {
		RefreshFreqtradePosition(context.Context, int) (*exchange.APIPosition, error)
	}
	handler, ok := r.FreqtradeHandler.(refresher)
	if !ok {
		c.JSON(http.StatusNotImplemented, gin.H{"error": "refresh not supported"})
		return
	}
	pos, err := handler.RefreshFreqtradePosition(c.Request.Context(), tradeID)
	if err != nil {
		logger.Warnf("[api] freqtrade position refresh failed ip=%s trade_id=%d err=%v", c.ClientIP(), tradeID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	logger.Infof("[api] freqtrade position refresh ip=%s trade_id=%d", c.ClientIP(), tradeID)
	c.JSON(http.StatusOK, gin.H{"position": pos})
}

func (r *Router) handleLiveLogs(c *gin.Context) {
	if len(r.logPaths) == 0 {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "未配置日志文件"})
		return
	}
	name := strings.TrimSpace(c.DefaultQuery("name", ""))
	path := ""
	if name != "" {
		path = strings.TrimSpace(r.logPaths[name])
	}
	if path == "" {
		for k, v := range r.logPaths {
			name = k
			path = v
			break
		}
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "200"))
	if limit <= 0 {
		limit = 200
	}
	lines, err := readLastLines(path, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error(), "path": path})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"name":      name,
		"path":      path,
		"lines":     lines,
		"available": r.logNames,
	})
}

func (r *Router) handlePlanChanges(c *gin.Context) {
	if r.Logs == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "strategy log store unavailable"})
		return
	}
	tradeID, _ := strconv.Atoi(strings.TrimSpace(c.Query("trade_id")))
	if tradeID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "trade_id 必填"})
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "100"))
	ctx := c.Request.Context()
	logs, err := r.Logs.ListStrategyChangeLogs(ctx, tradeID, limit)
	if err != nil {
		logger.Errorf("[api] plan changes failed trade=%d err=%v", tradeID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"changes": logs})
}

func (r *Router) handlePlanInstances(c *gin.Context) {
	if r.Logs == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "strategy log store unavailable"})
		return
	}
	tradeID, _ := strconv.Atoi(strings.TrimSpace(c.Query("trade_id")))
	if tradeID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "trade_id 必填"})
		return
	}
	ctx := c.Request.Context()
	recs, err := r.Logs.ListStrategyInstances(ctx, tradeID)
	if err != nil {
		logger.Errorf("[api] plan instances failed trade=%d err=%v", tradeID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"instances": recs})
}

const maxLogLineSize = 4 * 1024 * 1024

func readLastLines(path string, limit int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, maxLogLineSize)
	lines := make([]string, 0, limit)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
		if len(lines) > limit {
			lines = lines[1:]
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return lines, nil
}

func parseBoolDefaultTrue(val string) bool {
	s := strings.TrimSpace(strings.ToLower(val))
	if s == "" {
		return true
	}
	if s == "0" || s == "false" {
		return false
	}
	return true
}

type freqtradeCloseRequest struct {
	TradeID    int     `json:"trade_id"`
	Symbol     string  `json:"symbol"`
	Side       string  `json:"side"`
	CloseRatio float64 `json:"close_ratio"`
}

func (r *Router) handleFreqtradeQuickClose(c *gin.Context) {
	if r.FreqtradeHandler == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "未配置 freqtrade 处理器"})
		return
	}
	var req freqtradeCloseRequest
	if err := c.ShouldBind(&req); err != nil {
		logger.Errorf("[api] freqtrade quick close bind failed ip=%s err=%v", c.ClientIP(), err)
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	logger.Infof("[api] freqtrade quick close ip=%s trade_id=%d symbol=%s side=%s ratio=%.4f", c.ClientIP(), req.TradeID, strings.ToUpper(strings.TrimSpace(req.Symbol)), strings.ToLower(strings.TrimSpace(req.Side)), req.CloseRatio)
	if err := r.FreqtradeHandler.CloseFreqtradePosition(c.Request.Context(), req.TradeID, req.Symbol, req.Side, req.CloseRatio); err != nil {
		logger.Errorf("[api] freqtrade quick close failed ip=%s symbol=%s side=%s err=%v", c.ClientIP(), strings.ToUpper(strings.TrimSpace(req.Symbol)), strings.ToLower(strings.TrimSpace(req.Side)), err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (r *Router) handleFreqtradeManualOpen(c *gin.Context) {
	if r.FreqtradeHandler == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "未配置 freqtrade 处理器"})
		return
	}
	var req exchange.ManualOpenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		logger.Errorf("[api] freqtrade manual open bind failed ip=%s err=%v", c.ClientIP(), err)
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := r.FreqtradeHandler.ManualOpenPosition(c.Request.Context(), req); err != nil {
		logger.Errorf("[api] freqtrade manual open failed ip=%s symbol=%s err=%v", c.ClientIP(), strings.ToUpper(strings.TrimSpace(req.Symbol)), err)
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	logger.Infof("[api] freqtrade manual open ip=%s symbol=%s side=%s size=%.2f", c.ClientIP(), strings.ToUpper(strings.TrimSpace(req.Symbol)), strings.ToLower(strings.TrimSpace(req.Side)), req.PositionSizeUSD)
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (r *Router) handleFreqtradeEvents(c *gin.Context) {
	if r.FreqtradeHandler == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "未配置 freqtrade 处理器"})
		return
	}
	tradeID, _ := strconv.Atoi(c.DefaultQuery("trade_id", "0"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "100"))
	if tradeID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "trade_id 必填"})
		return
	}
	events, err := r.FreqtradeHandler.ListFreqtradeEvents(c.Request.Context(), tradeID, limit)
	if err != nil {
		logger.Errorf("[api] freqtrade events failed ip=%s trade_id=%d err=%v", c.ClientIP(), tradeID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	logger.Infof("[api] freqtrade events ip=%s trade_id=%d limit=%d returned=%d", c.ClientIP(), tradeID, limit, len(events))
	c.JSON(http.StatusOK, gin.H{"events": events})
}

func (r *Router) handlePlanAdjust(c *gin.Context) {
	if r.FreqtradeHandler == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "plan scheduler 未启用"})
		return
	}
	req := PlanAdjustRequest{}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request", "detail": err.Error()})
		return
	}
	if req.TradeID <= 0 || strings.TrimSpace(req.PlanID) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "trade_id 与 plan_id 必填"})
		return
	}
	ctx := c.Request.Context()
	if err := r.FreqtradeHandler.AdjustPlan(ctx, req); err != nil {
		logger.Warnf("[api] plan adjust failed trade=%d plan=%s comp=%s err=%v", req.TradeID, req.PlanID, req.Component, err)
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (r *Router) handleFreqtradePriceQuote(c *gin.Context) {
	if r.FreqtradeHandler == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "未配置 freqtrade 处理器"})
		return
	}
	symbol := strings.ToUpper(strings.TrimSpace(c.Query("symbol")))
	if symbol == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "symbol 不能为空"})
		return
	}
	quote, err := r.FreqtradeHandler.GetLatestPriceQuote(c.Request.Context(), symbol)
	if err != nil {
		logger.Errorf("[api] freqtrade price quote failed ip=%s symbol=%s err=%v", c.ClientIP(), symbol, err)
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"symbol": symbol,
		"last":   quote.Last,
		"high":   quote.High,
		"low":    quote.Low,
	})
}
