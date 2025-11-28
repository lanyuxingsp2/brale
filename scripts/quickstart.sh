#!/usr/bin/env bash
set -euo pipefail

# 快速启动脚本：准备配置与目录，先启动 freqtrade，再启动 brale。

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

CONFIG="configs/config.toml"
FT_CFG="configs/user_data/freqtrade-config.json"

if [ ! -f "$CONFIG" ]; then
  cp configs/config.example.toml "$CONFIG"
  echo "已从 configs/config.example.toml 生成 $CONFIG，请按需填写 API/密钥。"
fi

if [ ! -f "$FT_CFG" ]; then
  cp configs/user_data/freqtrade-config.example.json "$FT_CFG"
  echo "已从 freqtrade-config.example.json 生成 $FT_CFG，请按需填写交易所/密钥。"
fi

echo "准备运行目录与 freqtrade 文件..."
make prepare-dirs

export BRALE_DATA_ROOT="$ROOT/running_log/brale_data"
export FREQTRADE_USERDATA_ROOT="$ROOT/running_log/freqtrade_data"

echo "先启动 freqtrade..."
BRALE_DATA_ROOT="$BRALE_DATA_ROOT" FREQTRADE_USERDATA_ROOT="$FREQTRADE_USERDATA_ROOT" docker compose up -d freqtrade
echo "等待 freqtrade 初始化..."
sleep 10

echo "启动 brale..."
BRALE_DATA_ROOT="$BRALE_DATA_ROOT" FREQTRADE_USERDATA_ROOT="$FREQTRADE_USERDATA_ROOT" docker compose up -d brale

echo "完成。可用以下命令查看日志："
echo "  docker compose logs -f freqtrade"
echo "  docker compose logs -f brale"
