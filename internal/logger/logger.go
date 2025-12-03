package logger

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"log/slog"
)

// 中文说明：
// 轻量日志封装：使用 slog 实现结构化日志，同时保留 printf 风格接口方便过渡。

var (
	levelVar   slog.LevelVar
	loggerMu   sync.RWMutex
	baseLogger *slog.Logger
)

func init() {
	levelVar.Set(slog.LevelInfo)
	baseLogger = newLogger(os.Stdout)
}

func newLogger(w io.Writer) *slog.Logger {
	if w == nil {
		w = os.Stdout
	}
	handler := slog.NewTextHandler(w, &slog.HandlerOptions{Level: &levelVar})
	return slog.New(handler)
}

// SetOutput 替换日志输出目标（默认 stdout）。可传入 io.MultiWriter 实现文件+控制台。
func SetOutput(w io.Writer) {
	loggerMu.Lock()
	baseLogger = newLogger(w)
	loggerMu.Unlock()
}

// SetLevel 设置全局日志级别。
func SetLevel(level string) {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		levelVar.Set(slog.LevelDebug)
	case "info":
		levelVar.Set(slog.LevelInfo)
	case "warn", "warning":
		levelVar.Set(slog.LevelWarn)
	case "error":
		levelVar.Set(slog.LevelError)
	default:
		levelVar.Set(slog.LevelInfo)
	}
}

func activeLogger() *slog.Logger {
	loggerMu.RLock()
	l := baseLogger
	loggerMu.RUnlock()
	if l != nil {
		return l
	}
	loggerMu.Lock()
	defer loggerMu.Unlock()
	if baseLogger == nil {
		baseLogger = newLogger(os.Stdout)
	}
	return baseLogger
}

func Debugf(format string, v ...any) {
	activeLogger().Debug(fmt.Sprintf(format, v...))
}

func Infof(format string, v ...any) {
	activeLogger().Info(fmt.Sprintf(format, v...))
}

func Warnf(format string, v ...any) {
	activeLogger().Warn(fmt.Sprintf(format, v...))
}

func Errorf(format string, v ...any) {
	activeLogger().Error(fmt.Sprintf(format, v...))
}

// InfoBlock 将包含换行的文本逐行输出，避免日志后端对 \n 进行转义。
func InfoBlock(block string) {
	block = strings.TrimSpace(block)
	if block == "" {
		return
	}
	lines := strings.Split(block, "\n")
	for _, line := range lines {
		Infof("%s", line)
	}
}
