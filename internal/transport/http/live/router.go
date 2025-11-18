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
	}
}

// FreqtradeWebhookHandler 供 LiveService 实现，以处理 freqtrade 推送。
type FreqtradeWebhookHandler interface {
	HandleFreqtradeWebhook(ctx context.Context, msg freqtrade.WebhookMessage) error
	ListFreqtradePositions(ctx context.Context, symbol string, limit int) []freqtrade.APIPosition
}

func (r *Router) handleLiveDecisions(c *gin.Context) {
	if r.Logs == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "实时日志未启用"})
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "100"))
	query := database.LiveDecisionQuery{
		Limit:    limit,
		Provider: c.Query("provider"),
		Stage:    c.Query("stage"),
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
	positions := r.FreqtradeHandler.ListFreqtradePositions(c.Request.Context(), symbol, limit)
	c.JSON(http.StatusOK, gin.H{"positions": positions})
}
