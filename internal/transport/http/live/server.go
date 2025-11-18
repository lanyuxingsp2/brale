package livehttp

import (
	"context"
	"errors"
	"net/http"
	"time"

	"brale/internal/gateway/database"

	"github.com/gin-gonic/gin"
)

// Server 提供最小化的 /api/live HTTP 服务（决策查询 + freqtrade webhook）。
type Server struct {
	addr   string
	router *gin.Engine
}

// ServerConfig 描述 live HTTP 服务依赖。
type ServerConfig struct {
	Addr             string
	Logs             *database.DecisionLogStore
	FreqtradeHandler FreqtradeWebhookHandler
}

// NewServer 构建 live HTTP server。
func NewServer(cfg ServerConfig) (*Server, error) {
	if cfg.Logs == nil && cfg.FreqtradeHandler == nil {
		return nil, errors.New("live http server requires logs or freqtrade handler")
	}
	if cfg.Addr == "" {
		cfg.Addr = ":9991"
	}
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery())
	router.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	liveRouter := NewRouter(cfg.Logs, cfg.FreqtradeHandler)
	liveRouter.Register(router.Group("/api/live"))
	return &Server{addr: cfg.Addr, router: router}, nil
}

// Addr 返回监听地址。
func (s *Server) Addr() string {
	if s == nil {
		return ""
	}
	return s.addr
}

// Start 启动 HTTP 服务，直到 ctx 取消或出现错误。
func (s *Server) Start(ctx context.Context) error {
	if s == nil {
		return nil
	}
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
