package livehttp

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"brale/internal/gateway/database"
	"brale/internal/logger"
	webassets "brale/internal/transport/web"

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
	DefaultSymbols   []string
	LogPaths         map[string]string
}

// NewServer 构建 live HTTP server。
func NewServer(cfg ServerConfig) (*Server, error) {
	if cfg.Logs == nil && cfg.FreqtradeHandler == nil {
		return nil, errors.New("live http server requires logs or freqtrade handler")
	}
	if cfg.Addr == "" {
		cfg.Addr = ":9991"
	}
	if cfg.LogPaths == nil {
		cfg.LogPaths = map[string]string{}
	}
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery(), requestLogger())

	// 先注册模板函数，再加载模板，否则自定义函数不可用
	registerAdminRoutes(router, cfg.Logs, cfg.FreqtradeHandler, cfg.DefaultSymbols)

	// 静态资源与模板
	if err := loadTemplates(router); err != nil {
		return nil, err
	}
	if err := serveStatic(router); err != nil {
		return nil, err
	}

	router.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	liveRouter := NewRouter(cfg.Logs, cfg.FreqtradeHandler, cfg.LogPaths)
	liveRouter.Register(router.Group("/api/live"))

	return &Server{addr: cfg.Addr, router: router}, nil
}

func loadTemplates(router *gin.Engine) error {
	dirs := []string{
		"internal/transport/web/templates",
		"/src/internal/transport/web/templates",
		"/app/internal/transport/web/templates",
		"web/templates",
		"/src/web/templates",
		"/app/web/templates",
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		dirs = append(dirs, filepath.Join(dir, "web", "templates"))
		dirs = append(dirs, filepath.Join(dir, "internal", "transport", "web", "templates"))
	}
	for _, base := range dirs {
		stat, err := os.Stat(base)
		if err != nil || !stat.IsDir() {
			continue
		}
		mainGlob := filepath.Join(base, "*.html")
		files, _ := filepath.Glob(mainGlob)
		if len(files) == 0 {
			continue
		}
		router.LoadHTMLFiles(files...)
		return nil
	}
	const embeddedTplBase = "templates"
	// fallback to embedded templates
	fsys, err := fs.Sub(webassets.Templates, embeddedTplBase)
	if err != nil {
		return err
	}
	files, err := fs.Glob(fsys, "*.html")
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no templates found in embedded FS")
	}
	tmpls := make([]string, len(files))
	for i, name := range files {
		tmpls[i] = embeddedTplBase + "/" + name
	}
	tmpl, err := template.New("admin").Funcs(adminTemplateFuncs).ParseFS(webassets.Templates, tmpls...)
	if err != nil {
		return err
	}
	router.SetHTMLTemplate(tmpl)
	return nil
}

func serveStatic(router *gin.Engine) error {
	dirs := []string{
		"internal/transport/web/static",
		"/src/internal/transport/web/static",
		"/app/internal/transport/web/static",
		"web/static",
		"/src/web/static",
		"/app/web/static",
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		dirs = append(dirs, filepath.Join(dir, "web", "static"))
		dirs = append(dirs, filepath.Join(dir, "internal", "transport", "web", "static"))
	}
	for _, base := range dirs {
		stat, err := os.Stat(base)
		if err == nil && stat.IsDir() {
			router.Static("/static", base)
			return nil
		}
	}
	// fallback to embedded static assets
	const embeddedStaticBase = "static"
	sub, err := fs.Sub(webassets.Static, embeddedStaticBase)
	if err != nil {
		return err
	}
	fileServer := http.FileServer(http.FS(sub))
	router.GET("/static/*filepath", func(c *gin.Context) {
		c.Request.URL.Path = c.Param("filepath")
		fileServer.ServeHTTP(c.Writer, c.Request)
	})
	return nil
}

// requestLogger 记录后台/接口的人工操作，便于追踪刷新与调用。
func requestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		method := c.Request.Method
		path := c.Request.URL.Path
		query := c.Request.URL.RawQuery
		client := c.ClientIP()
		c.Next()
		dur := time.Since(start)
		status := c.Writer.Status()
		fullPath := path
		if query != "" {
			fullPath = path + "?" + query
		}
		logger.Infof("HTTP %s %s status=%d ip=%s dur=%s", method, fullPath, status, client, dur)
	}
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
