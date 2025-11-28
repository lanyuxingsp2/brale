BIN_DIR := bin
BIN := $(BIN_DIR)/brale

# 运行时数据根目录（统一放在 running_log 下）
BRALE_DATA_ROOT := $(CURDIR)/running_log/brale_data
FREQTRADE_USERDATA_ROOT := $(CURDIR)/running_log/freqtrade_data

DOCKER_COMPOSE := BRALE_DATA_ROOT=$(BRALE_DATA_ROOT) FREQTRADE_USERDATA_ROOT=$(FREQTRADE_USERDATA_ROOT) docker compose

.PHONY: help fmt test build run clean prepare-dirs up down logs

help:
	@echo "可用目标："
	@echo "  make fmt      - gofmt 当前模块"
	@echo "  make test     - 运行 ./internal/... 单测"
	@echo "  make build    - 构建 ./cmd/brale 到 $(BIN)"
	@echo "  make run      - 本地运行（配置 ./configs/config.toml）"
	@echo "  make prepare-dirs - 创建运行目录并复制 freqtrade 配置/策略"
	@echo "  make up       - docker compose up -d（挂载 running_log 数据）"
	@echo "  make down     - docker compose down"
	@echo "  make logs     - docker compose logs -f"
	@echo "  make clean    - 删除 bin/"

fmt:
	go fmt ./...

test:
	go test ./internal/... -count=1 -v

build:
	@mkdir -p $(BIN_DIR)
	go build -o $(BIN) ./cmd/brale
	@echo "已生成：$(BIN)"

run:
	BRALE_CONFIG=./configs/config.toml go run ./cmd/brale

clean:
	rm -rf $(BIN_DIR)
	@echo "已清理：$(BIN_DIR)"

prepare-dirs:
	@echo "创建运行目录：$(BRALE_DATA_ROOT) $(FREQTRADE_USERDATA_ROOT)"
	@mkdir -p $(BRALE_DATA_ROOT) $(FREQTRADE_USERDATA_ROOT)/logs $(FREQTRADE_USERDATA_ROOT)/strategies
	@cp -f ./configs/user_data/freqtrade-config.json $(FREQTRADE_USERDATA_ROOT)/config.json
	@cp -f ./configs/user_data/brale_shared_strategy.py $(FREQTRADE_USERDATA_ROOT)/strategies/brale_shared_strategy.py
	@chmod -R 777 $(FREQTRADE_USERDATA_ROOT) $(BRALE_DATA_ROOT)

up: prepare-dirs
	$(DOCKER_COMPOSE) up -d

up-build: prepare-dirs
	$(DOCKER_COMPOSE) build brale
	$(DOCKER_COMPOSE) up -d

down:
	$(DOCKER_COMPOSE) down

logs:
	$(DOCKER_COMPOSE) logs -f
