package main

import (
	"context"
	"log"
	"os"

	brcfg "brale/internal/config"
	"brale/internal/logger"
	"brale/internal/manager"
)

// main 保持精简：加载配置并交由 manager.App 运行
func main() {
	ctx := context.Background()
	cfgPath := os.Getenv("BRALE_CONFIG")
	if cfgPath == "" {
		cfgPath = "configs/config.toml"
	}

	cfg, err := brcfg.Load(cfgPath)
	if err != nil {
		log.Fatalf("读取配置失败: %v", err)
	}
	logger.SetLevel(cfg.App.LogLevel)
	logger.Infof("✓ 配置加载成功（环境=%s，持仓周期=%s，订阅周期=%v）", cfg.App.Env, cfg.AI.ActiveHorizon, cfg.WS.Periods)

	app, err := manager.NewApp(cfg)
	if err != nil {
		log.Fatalf("初始化应用失败: %v", err)
	}
	if err := app.Run(ctx); err != nil {
		log.Fatalf("运行失败: %v", err)
	}
}
