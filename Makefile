# Brale 项目 Makefile（位于 brale 子模块内）
#
# 用途：在本目录下进行构建、测试、运行等常用操作。
# 说明：本目录是一个独立的 Go 模块（包含 go.mod）。

BIN_DIR := bin
BIN := $(BIN_DIR)/brale

# 配置文件路径（可在命令行覆盖：make run BRALE_CONFIG=...）
BRALE_CONFIG ?= ./configs/config.toml

.PHONY: help tidy fmt test build run clean

help:
	@echo "可用目标："
	@echo "  make tidy   - 拉取依赖并生成 go.sum（go mod tidy）"
	@echo "  make fmt    - 格式化当前模块所有 Go 源码"
	@echo "  make test   - 运行 ./internal 下的单元测试"
	@echo "  make build  - 构建可执行文件到 $(BIN)"
	@echo "  make run    - 运行 Brale（默认使用 $(BRALE_CONFIG)）"
	@echo "  make clean  - 清理构建产物 $(BIN_DIR)"

tidy:
	go mod tidy

fmt:
	go fmt ./...

test:
	go test ./internal/... -count=1 -v

build:
	@mkdir -p $(BIN_DIR)
	go build -o $(BIN) ./cmd/brale
	@echo "已生成：$(BIN)"

run:
	BRALE_CONFIG=$(BRALE_CONFIG) go run ./cmd/brale

clean:
	rm -rf $(BIN_DIR)
	@echo "已清理：$(BIN_DIR)"
