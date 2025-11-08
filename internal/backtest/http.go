package backtest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"brale/internal/ai"
	"brale/internal/backtest/ui"

	"github.com/gin-gonic/gin"
)

// HTTPServer 提供 Gin 接口，供前端触发拉取/查询进度。
type HTTPServer struct {
	addr      string
	svc       *Service
	sim       *Simulator
	results   *ResultStore
	liveLogs  *ai.DecisionLogStore
	router    *gin.Engine
	indexHTML []byte
}

type HTTPConfig struct {
	Addr      string
	Svc       *Service
	Simulator *Simulator
	Results   *ResultStore
	LiveLogs  *ai.DecisionLogStore
}

func NewHTTPServer(cfg HTTPConfig) (*HTTPServer, error) {
	if cfg.Svc == nil {
		return nil, errors.New("service 不能为空")
	}
	if cfg.Addr == "" {
		cfg.Addr = ":9991"
	}
	staticFS, err := ui.StaticFS()
	if err != nil {
		return nil, fmt.Errorf("加载前端静态资源失败: %w", err)
	}
	indexHTML, err := ui.Index()
	if err != nil {
		return nil, fmt.Errorf("加载前端首页失败: %w", err)
	}

	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery())
	router.StaticFS("/static", staticFS)

	s := &HTTPServer{
		addr:      cfg.Addr,
		svc:       cfg.Svc,
		sim:       cfg.Simulator,
		results:   cfg.Results,
		liveLogs:  cfg.LiveLogs,
		router:    router,
		indexHTML: indexHTML,
	}
	s.registerRoutes()
	return s, nil
}

func (s *HTTPServer) registerRoutes() {
	s.router.GET("/", s.handleIndex)
	api := s.router.Group("/api/backtest")
	api.POST("/fetch", s.handleFetch)
	api.GET("/fetch/:id", s.handleFetchStatus)
	api.GET("/jobs", s.handleJobs)
	api.GET("/data", s.handleManifest)
	api.GET("/candles", s.handleCandles)
	api.GET("/candles/all", s.handleAllCandles)
	api.POST("/runs", s.handleRunStart)
	api.GET("/runs", s.handleRunList)
	api.GET("/runs/:id", s.handleRunDetail)
	api.GET("/runs/:id/orders", s.handleRunOrders)
	api.GET("/runs/:id/positions", s.handleRunPositions)
	api.GET("/runs/:id/snapshots", s.handleRunSnapshots)
	api.GET("/runs/:id/logs", s.handleRunLogs)

	live := s.router.Group("/api/live")
	live.GET("/decisions", s.handleLiveDecisions)
	live.GET("/orders", s.handleLiveOrders)
}

func (s *HTTPServer) handleIndex(c *gin.Context) {
	c.Data(http.StatusOK, "text/html; charset=utf-8", s.indexHTML)
}

func (s *HTTPServer) handleFetch(c *gin.Context) {
	var req struct {
		Exchange  string `json:"exchange"`
		Symbol    string `json:"symbol" binding:"required"`
		Timeframe string `json:"timeframe" binding:"required"`
		StartTS   int64  `json:"start_ts" binding:"required"`
		EndTS     int64  `json:"end_ts" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	job, err := s.svc.SubmitFetch(FetchParams{
		Exchange:  req.Exchange,
		Symbol:    req.Symbol,
		Timeframe: req.Timeframe,
		Start:     req.StartTS,
		End:       req.EndTS,
	})
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"job": job})
}

func (s *HTTPServer) handleFetchStatus(c *gin.Context) {
	id := c.Param("id")
	job, ok := s.svc.JobSnapshot(id)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "job not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"job": job})
}

func (s *HTTPServer) handleJobs(c *gin.Context) {
	list := s.svc.JobsSnapshot()
	c.JSON(http.StatusOK, gin.H{"jobs": list})
}

func (s *HTTPServer) handleManifest(c *gin.Context) {
	symbol := c.Query("symbol")
	tf := c.Query("timeframe")
	if symbol == "" || tf == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "symbol/timeframe 必填"})
		return
	}
	info, err := s.svc.ManifestInfo(c.Request.Context(), symbol, tf)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"manifest": info})
}

func (s *HTTPServer) handleCandles(c *gin.Context) {
	symbol := c.Query("symbol")
	tf := c.Query("timeframe")
	if symbol == "" || tf == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "symbol/timeframe 必填"})
		return
	}
	start, _ := strconv.ParseInt(c.Query("start_ts"), 10, 64)
	end, _ := strconv.ParseInt(c.Query("end_ts"), 10, 64)
	limit, err := strconv.Atoi(c.DefaultQuery("limit", "200"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "limit 非法"})
		return
	}
	data, err := s.svc.QueryCandles(c.Request.Context(), symbol, tf, start, end, limit)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"candles": data})
}

func (s *HTTPServer) handleAllCandles(c *gin.Context) {
	symbol := c.Query("symbol")
	tf := c.Query("timeframe")
	if symbol == "" || tf == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "symbol/timeframe 必填"})
		return
	}
	data, err := s.svc.AllCandles(c.Request.Context(), symbol, tf)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"candles": data})
}

func (s *HTTPServer) handleRunStart(c *gin.Context) {
	if s.sim == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "模拟器未启用"})
		return
	}
	var req RunRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	run, err := s.sim.StartRun(req)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"run": run})
}

func (s *HTTPServer) handleRunList(c *gin.Context) {
	if s.results == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "结果存储未启用"})
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	runs, err := s.results.ListRuns(c.Request.Context(), limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"runs": runs})
}

func (s *HTTPServer) handleRunDetail(c *gin.Context) {
	if s.results == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "结果存储未启用"})
		return
	}
	run, err := s.results.GetRun(c.Request.Context(), c.Param("id"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"run": run})
}

func (s *HTTPServer) handleRunOrders(c *gin.Context) {
	if s.results == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "结果存储未启用"})
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "200"))
	orders, err := s.results.ListOrders(c.Request.Context(), c.Param("id"), limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"orders": orders})
}

func (s *HTTPServer) handleRunPositions(c *gin.Context) {
	if s.results == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "结果存储未启用"})
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "200"))
	positions, err := s.results.ListPositions(c.Request.Context(), c.Param("id"), limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"positions": positions})
}

func (s *HTTPServer) handleRunSnapshots(c *gin.Context) {
	if s.results == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "结果存储未启用"})
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "400"))
	snaps, err := s.results.ListSnapshots(c.Request.Context(), c.Param("id"), limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"snapshots": snaps})
}

func (s *HTTPServer) handleRunLogs(c *gin.Context) {
	if s.results == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "结果存储未启用"})
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "200"))
	logs, err := s.results.ListRunLogs(c.Request.Context(), c.Param("id"), limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"logs": logs})
}

func (s *HTTPServer) handleLiveDecisions(c *gin.Context) {
	if s.liveLogs == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "实时日志未启用"})
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "100"))
	query := ai.LiveDecisionQuery{
		Limit:    limit,
		Provider: c.Query("provider"),
		Stage:    c.Query("stage"),
		Symbol:   c.Query("symbol"),
	}
	logs, err := s.liveLogs.ListDecisions(c.Request.Context(), query)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"logs": logs})
}

func (s *HTTPServer) handleLiveOrders(c *gin.Context) {
	if s.liveLogs == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "实时日志未启用"})
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "100"))
	symbol := c.Query("symbol")
	orders, err := s.liveLogs.ListOrders(c.Request.Context(), symbol, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"orders": orders})
}

// Start 启动 HTTP 服务，阻塞直到 ctx 取消或出现错误。
func (s *HTTPServer) Start(ctx context.Context) error {
	srv := &http.Server{Addr: s.addr, Handler: s.router}
	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		shCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shCtx)
		return nil
	case err := <-errCh:
		return err
	}
}
