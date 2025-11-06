package logger

import (
	"log"
	"strings"
)

// 中文说明：
// 轻量日志封装：支持设置全局级别，便于减少刷屏。

type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

var current = LevelInfo

func SetLevel(s string) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		current = LevelDebug
	case "info":
		current = LevelInfo
	case "warn", "warning":
		current = LevelWarn
	case "error":
		current = LevelError
	default:
		current = LevelInfo
	}
}

func Debugf(format string, v ...any) {
	if current <= LevelDebug {
		log.Printf("[DEBUG] "+format, v...)
	}
}
func Infof(format string, v ...any) {
	if current <= LevelInfo {
		log.Printf("[INFO] "+format, v...)
	}
}
func Warnf(format string, v ...any) {
	if current <= LevelWarn {
		log.Printf("[WARN] "+format, v...)
	}
}
func Errorf(format string, v ...any) {
	if current <= LevelError {
		log.Printf("[ERROR] "+format, v...)
	}
}
