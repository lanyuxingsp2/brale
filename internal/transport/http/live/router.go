package livehttp

import (
	"bufio"
	"context"
	"database/sql"
	"net/http"
	"os"
	"strconv"
	"strings"

	"brale/internal/executor/freqtrade"
	"brale/internal/gateway/database"
	"brale/internal/logger"

	"github.com/gin-gonic/gin"
)

// Router 暴露实盘相关的查询接口（决策/订单）。
type Router struct {
	Logs             *database.DecisionLogStore
	FreqtradeHandler FreqtradeWebhookHandler
	logPaths         map[string]string
	logNames         []string
}

// NewRouter 构造 live HTTP router。
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

// Register 将 /api/live 路由挂载到给定分组下。
func (r *Router) Register(group *gin.RouterGroup) {
	if group == nil {
		return
	}
	group.GET("/decisions", r.handleLiveDecisions)
	group.GET("/decisions/:id", r.handleDecisionByID)
	group.GET("/traces", r.handleLiveDecisions)
	group.GET("/orders", r.handleLiveOrders)
	group.GET("/logs", r.handleLiveLogs)
	if r.FreqtradeHandler != nil {
		group.POST("/freqtrade/webhook", r.handleFreqtradeWebhook)
		group.GET("/freqtrade/positions", r.handleFreqtradePositions)
		group.GET("/freqtrade/positions/:id", r.handleFreqtradePositionDetail)
		group.POST("/freqtrade/close", r.handleFreqtradeQuickClose)
		group.POST("/freqtrade/tiers", r.handleFreqtradeUpdateTiers)
		group.POST("/freqtrade/manual-open", r.handleFreqtradeManualOpen)
		group.GET("/freqtrade/price", r.handleFreqtradePriceQuote)
		group.GET("/freqtrade/tier-logs", r.handleFreqtradeTierLogs)
		group.GET("/freqtrade/events", r.handleFreqtradeEvents)
	}
}

// FreqtradeWebhookHandler 供 LiveService 实现，以处理 freqtrade 推送。
type FreqtradeWebhookHandler interface {
	HandleFreqtradeWebhook(ctx context.Context, msg freqtrade.WebhookMessage) error
	ListFreqtradePositions(ctx context.Context, opts freqtrade.PositionListOptions) (freqtrade.PositionListResult, error)
	CloseFreqtradePosition(ctx context.Context, symbol, side string, closeRatio float64) error
	UpdateFreqtradeTiers(ctx context.Context, req freqtrade.TierUpdateRequest) error
	ListFreqtradeTierLogs(ctx context.Context, tradeID int, limit int) ([]freqtrade.TierLog, error)
	ListFreqtradeEvents(ctx context.Context, tradeID int, limit int) ([]freqtrade.TradeEvent, error)
	ManualOpenPosition(ctx context.Context, req freqtrade.ManualOpenRequest) error
	GetLatestPriceQuote(ctx context.Context, symbol string) (freqtrade.TierPriceQuote, error)
}

func (r *Router) handleLiveDecisions(c *gin.Context) {
	if r.Logs == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "实时日志未启用"})
		return
	}
	pageParam := strings.TrimSpace(c.Query("page"))
	page, _ := strconv.Atoi(pageParam)
	if page < 0 {
		page = 0
	}
	pageSize, _ := strconv.Atoi(c.DefaultQuery("pageSize", "0"))
	if pageSize <= 0 {
		pageSize, _ = strconv.Atoi(c.DefaultQuery("page_size", "0"))
	}
	if pageSize <= 0 {
		pageSize, _ = strconv.Atoi(c.DefaultQuery("limit", "100"))
	}
	if pageSize <= 0 {
		pageSize = 100
	}
	if pageSize > 500 {
		pageSize = 500
	}
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if offset < 0 {
		offset = 0
	}
	if page > 0 {
		offset = (page - 1) * pageSize
	} else {
		page = offset/pageSize + 1
	}
	query := database.LiveDecisionQuery{
		Limit:    pageSize,
		Offset:   offset,
		Provider: c.Query("provider"),
		Stage:    c.DefaultQuery("stage", "core"),
		Symbol:   c.Query("symbol"),
		Symbols:  c.QueryArray("symbol"),
	}
	total, err := r.Logs.CountDecisions(c.Request.Context(), query)
	if err != nil {
		logger.Errorf("[api] live decisions count failed ip=%s err=%v", c.ClientIP(), err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	logs, err := r.Logs.ListDecisions(c.Request.Context(), query)
	if err != nil {
		logger.Errorf("[api] live decisions list failed ip=%s err=%v", c.ClientIP(), err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
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

func (r *Router) handleLiveOrders(c *gin.Context) {
	if r.Logs == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "实时日志未启用"})
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "100"))
	symbol := c.Query("symbol")
	orders, err := r.Logs.ListOrders(c.Request.Context(), symbol, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	logger.Infof("[api] live orders ip=%s symbol=%s limit=%d returned=%d", c.ClientIP(), symbol, limit, len(orders))
	c.JSON(http.StatusOK, gin.H{"orders": orders})
}

func (r *Router) handleFreqtradeWebhook(c *gin.Context) {
	if r.FreqtradeHandler == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "未配置 freqtrade 处理器"})
		return
	}
	var payload freqtrade.WebhookMessage
	if err := c.ShouldBindJSON(&payload); err != nil {
		logger.Errorf("[api] freqtrade webhook bind failed ip=%s err=%v", c.ClientIP(), err)
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
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
	opts := freqtrade.PositionListOptions{
		Symbol:      symbol,
		Page:        page,
		PageSize:    pageSize,
		IncludeLogs: includeLogs,
		LogsLimit:   logsLimit,
	}
	result, err := r.FreqtradeHandler.ListFreqtradePositions(c.Request.Context(), opts)
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
	logsLimit, _ := strconv.Atoi(c.DefaultQuery("logs_limit", "80"))
	opts := freqtrade.PositionListOptions{
		Page:        1,
		PageSize:    300,
		IncludeLogs: true,
		LogsLimit:   logsLimit,
	}
	result, err := r.FreqtradeHandler.ListFreqtradePositions(c.Request.Context(), opts)
	if err != nil {
		logger.Errorf("[api] freqtrade position detail list failed ip=%s trade_id=%d err=%v", c.ClientIP(), tradeID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	var target *freqtrade.APIPosition
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
	logger.Debugf("[api] freqtrade position detail ip=%s trade_id=%d symbol=%s side=%s",
		c.ClientIP(), tradeID, strings.ToUpper(strings.TrimSpace(target.Symbol)), strings.ToLower(strings.TrimSpace(target.Side)))
	c.JSON(http.StatusOK, gin.H{
		"position": target,
	})
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

const maxLogLineSize = 4 * 1024 * 1024 // 4MB per line for payload-heavy logs

func readLastLines(path string, limit int) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
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
	logger.Infof("[api] freqtrade quick close ip=%s symbol=%s side=%s ratio=%.4f", c.ClientIP(), strings.ToUpper(strings.TrimSpace(req.Symbol)), strings.ToLower(strings.TrimSpace(req.Side)), req.CloseRatio)
	if err := r.FreqtradeHandler.CloseFreqtradePosition(c.Request.Context(), req.Symbol, req.Side, req.CloseRatio); err != nil {
		logger.Errorf("[api] freqtrade quick close failed ip=%s symbol=%s side=%s err=%v", c.ClientIP(), strings.ToUpper(strings.TrimSpace(req.Symbol)), strings.ToLower(strings.TrimSpace(req.Side)), err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (r *Router) handleFreqtradeUpdateTiers(c *gin.Context) {
	if r.FreqtradeHandler == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "未配置 freqtrade 处理器"})
		return
	}
	var req freqtrade.TierUpdateRequest
	if err := c.ShouldBind(&req); err != nil {
		logger.Errorf("[api] freqtrade tier update bind failed ip=%s err=%v", c.ClientIP(), err)
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	logger.Infof("[api] freqtrade tier update ip=%s trade_id=%d symbol=%s reason=%s", c.ClientIP(), req.TradeID, strings.ToUpper(strings.TrimSpace(req.Symbol)), strings.TrimSpace(req.Reason))
	if err := r.FreqtradeHandler.UpdateFreqtradeTiers(c.Request.Context(), req); err != nil {
		logger.Errorf("[api] freqtrade tier update failed ip=%s trade_id=%d err=%v", c.ClientIP(), req.TradeID, err)
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
	var req freqtrade.ManualOpenRequest
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

func (r *Router) handleFreqtradeTierLogs(c *gin.Context) {
	if r.FreqtradeHandler == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "未配置 freqtrade 处理器"})
		return
	}
	tradeID, _ := strconv.Atoi(c.DefaultQuery("trade_id", "0"))
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	if tradeID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "trade_id 必填"})
		return
	}
	logs, err := r.FreqtradeHandler.ListFreqtradeTierLogs(c.Request.Context(), tradeID, limit)
	if err != nil {
		logger.Errorf("[api] freqtrade tier logs failed ip=%s trade_id=%d err=%v", c.ClientIP(), tradeID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	logger.Infof("[api] freqtrade tier logs ip=%s trade_id=%d limit=%d returned=%d", c.ClientIP(), tradeID, limit, len(logs))
	c.JSON(http.StatusOK, gin.H{"logs": logs})
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
