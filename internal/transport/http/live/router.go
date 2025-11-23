package livehttp

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"brale/internal/executor/freqtrade"
	"brale/internal/gateway/database"

	"github.com/gin-gonic/gin"
)

// Router 暴露实盘相关的查询接口（决策/订单）。
type Router struct {
	Logs             *database.DecisionLogStore
	FreqtradeHandler FreqtradeWebhookHandler
}

// NewRouter 构造 live HTTP router。
func NewRouter(logs *database.DecisionLogStore, handler FreqtradeWebhookHandler) *Router {
	return &Router{Logs: logs, FreqtradeHandler: handler}
}

// Register 将 /api/live 路由挂载到给定分组下。
func (r *Router) Register(group *gin.RouterGroup) {
	if group == nil {
		return
	}
	group.GET("/decisions", r.handleLiveDecisions)
	group.GET("/orders", r.handleLiveOrders)
	if r.FreqtradeHandler != nil {
		group.POST("/freqtrade/webhook", r.handleFreqtradeWebhook)
		group.GET("/freqtrade/positions", r.handleFreqtradePositions)
		group.POST("/freqtrade/close", r.handleFreqtradeQuickClose)
		group.POST("/freqtrade/tiers", r.handleFreqtradeUpdateTiers)
		group.GET("/freqtrade/tier-logs", r.handleFreqtradeTierLogs)
		group.GET("/freqtrade/events", r.handleFreqtradeEvents)
	}
}

// FreqtradeWebhookHandler 供 LiveService 实现，以处理 freqtrade 推送。
type FreqtradeWebhookHandler interface {
	HandleFreqtradeWebhook(ctx context.Context, msg freqtrade.WebhookMessage) error
	ListFreqtradePositions(ctx context.Context, opts freqtrade.PositionListOptions) []freqtrade.APIPosition
	CloseFreqtradePosition(ctx context.Context, symbol, side string, closeRatio float64) error
	UpdateFreqtradeTiers(ctx context.Context, req freqtrade.TierUpdateRequest) error
	ListFreqtradeTierLogs(ctx context.Context, tradeID int, limit int) ([]freqtrade.TierLog, error)
	ListFreqtradeEvents(ctx context.Context, tradeID int, limit int) ([]freqtrade.TradeEvent, error)
}

func (r *Router) handleLiveDecisions(c *gin.Context) {
	if r.Logs == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "实时日志未启用"})
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "100"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))
	if limit <= 0 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	query := database.LiveDecisionQuery{
		Limit:    limit,
		Offset:   offset,
		Provider: c.Query("provider"),
		Stage:    c.DefaultQuery("stage", "core"),
		Symbol:   c.Query("symbol"),
	}
	logs, err := r.Logs.ListDecisions(c.Request.Context(), query)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	traces := database.BuildLiveDecisionTraces(logs)
	c.JSON(http.StatusOK, gin.H{
		"logs":   logs,
		"traces": traces,
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
	c.JSON(http.StatusOK, gin.H{"orders": orders})
}

func (r *Router) handleFreqtradeWebhook(c *gin.Context) {
	if r.FreqtradeHandler == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "未配置 freqtrade 处理器"})
		return
	}
	var payload freqtrade.WebhookMessage
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := r.FreqtradeHandler.HandleFreqtradeWebhook(c.Request.Context(), payload); err != nil {
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
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "100"))
	if limit <= 0 {
		limit = 100
	}
	historyLimit, _ := strconv.Atoi(c.DefaultQuery("history_limit", "5"))
	if historyLimit < 0 {
		historyLimit = 0
	}
	logsLimit, _ := strconv.Atoi(c.DefaultQuery("logs_limit", "20"))
	includeLogs := parseBoolDefaultTrue(c.DefaultQuery("include_logs", "1"))
	opts := freqtrade.PositionListOptions{
		Symbol:      symbol,
		Limit:       limit,
		ClosedLimit: historyLimit,
		IncludeLogs: includeLogs,
		LogsLimit:   logsLimit,
	}
	positions := r.FreqtradeHandler.ListFreqtradePositions(c.Request.Context(), opts)
	c.JSON(http.StatusOK, gin.H{"positions": positions})
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
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := r.FreqtradeHandler.CloseFreqtradePosition(c.Request.Context(), req.Symbol, req.Side, req.CloseRatio); err != nil {
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
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := r.FreqtradeHandler.UpdateFreqtradeTiers(c.Request.Context(), req); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"events": events})
}
