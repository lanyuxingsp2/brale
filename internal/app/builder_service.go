package app

import (
	"fmt"
	"strings"

	brcfg "brale/internal/config"
	"brale/internal/gateway/database"
	freqexec "brale/internal/gateway/freqtrade"
	"brale/internal/gateway/notifier"
	"brale/internal/logger"
	"brale/internal/store"
	livehttp "brale/internal/transport/http/live"
)

func buildFreqManager(cfg brcfg.FreqtradeConfig, horizon string, logStore *database.DecisionLogStore, liveStore database.LivePositionStore, newStore store.Store, textNotifier notifier.TextNotifier) (*freqexec.Manager, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	client, err := freqexec.NewClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to init freqtrade client: %w", err)
	}
	logger.Infof("Freqtrade executor enabled: %s", cfg.APIURL)

	// Adapter now resides in gateway/freqtrade
	adapter := freqexec.NewAdapter(client, &cfg)
	manager, err := freqexec.NewManager(client, cfg, logStore, liveStore, newStore, textNotifier, adapter)
	if err != nil {
		return nil, fmt.Errorf("failed to init freqtrade manager: %w", err)
	}
	return manager, nil
}

func buildLiveHTTPServer(cfg brcfg.AppConfig, logs *database.DecisionLogStore, freqHandler livehttp.FreqtradeWebhookHandler, defaultSymbols []string, symbolDetails map[string]livehttp.SymbolDetail) (*livehttp.Server, error) {
	if logs == nil && freqHandler == nil {
		return nil, nil
	}
	logPaths := map[string]string{}
	if path := strings.TrimSpace(cfg.LogPath); path != "" {
		logPaths["app"] = path
	}
	if path := strings.TrimSpace(cfg.LLMLog); path != "" {
		logPaths["llm"] = path
	}
	server, err := livehttp.NewServer(livehttp.ServerConfig{
		Addr:             cfg.HTTPAddr,
		Logs:             logs,
		FreqtradeHandler: freqHandler,
		DefaultSymbols:   defaultSymbols,
		SymbolDetails:    symbolDetails,
		LogPaths:         logPaths,
	})
	if err != nil {
		return nil, fmt.Errorf("初始化 live HTTP 失败: %w", err)
	}
	logger.Infof("✓ Live HTTP 接口监听 %s", server.Addr())
	return server, nil
}

func newTelegram(cfg brcfg.NotifyConfig) *notifier.Telegram {
	if !cfg.Telegram.Enabled {
		return nil
	}
	return notifier.NewTelegram(cfg.Telegram.BotToken, cfg.Telegram.ChatID)
}
