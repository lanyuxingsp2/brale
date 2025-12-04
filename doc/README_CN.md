# Brale (Break a leg) 🎭

> **AI 驱动的多 Agent 量化策略引擎**
> 
> *既然是演戏（交易），那就祝你 "Break a leg"（演出成功/大赚一笔）！*

[![English Documentation](https://img.shields.io/badge/lang-English-blue.svg)](../README.md)
[![Go Version](https://img.shields.io/github/go-mod/go-version/lauk/brale)](../go.mod)
[![License](https://img.shields.io/badge/license-MIT-green.svg)](../LICENSE)

**Brale** 是一个以 AI 决策为核心的量化策略生成器。它不直接持有账户资金，而是作为 "超级大脑"，利用多 Agent（技术指标、形态识别、趋势判断）协同分析市场，最终由 LLM (Large Language Model) 生成决策信号，并通过 [Freqtrade](https://github.com/freqtrade/freqtrade) 强大的执行引擎进行安全交易。[视频介绍](https://www.bilibili.com/video/BV1Ab2aB2EUY)

## ✨ 核心特性

- 🧠 **AI 驱动决策**: 摒弃传统的硬编码逻辑，利用 LLM 综合分析多维数据，像人类交易员一样思考。
- 🤖 **多 Agent 协同**:
  - **Technical Agent**: 计算 EMA, RSI, MACD, ATR 等硬指标。
  - **Pattern Agent**: 识别 K 线形态（如头肩顶、吞没形态）。
  - **Trend Agent**: 结合多周期 (Multi-timeframe) 判断大势。
- 🛡️ **站在巨人的肩膀上**: 无缝集成 **Freqtrade**。本程序来控制下单，止盈止损点位，freqtrade负责和多交易所对接完成下单。
- ⚡ **高性能**: 核心逻辑由 Go 编写，并发处理多币种数据拉取与指标计算。
- 📊 **可视化与解释性**: 生成图表和自然语言分析报告，让你知道 AI 为什么开单。

## 🏗️ 架构流程

![架构图](Reasoning-Edition.png)

1.  **数据获取**: 从 Binance 等交易所拉取 K 线数据。
2.  **分析**: 切分到多个时间周期，交由技术指标、价格形态、趋势判断等 Agent 协同分析。
3.  **决策**: 汇总 Agent 结论，通过 Provider（如 LLM 模型）生成最终决策。
4.  **执行**: 策略信号通过权重聚合后交由 Freqtrade 执行。

## ⚠️ 风险免责声明

**Brale 是一个用于量化交易研究和开发的开源工具，它并非金融投资建议。加密货币交易具有高度投机性，并伴随着巨大的风险。您可能会损失部分或全部投资资本。请勿投入您无法承受损失的资金。过往表现不代表未来业绩。使用 Brale 存在固有风险，请自行承担。**

## 🚀 快速启动 (Docker)

### 1. 准备配置

```bash
# 复制配置文件
cp configs/config.example.toml configs/config.toml
cp configs/user_data/freqtrade-config.example.json configs/user_data/freqtrade-config.json

# 注意：
# 1. 在 configs/config.toml 中填入你的 LLM API Key
# 2. 在 configs/user_data/freqtrade-config.json 中配置交易所 API（或使用 dry-run 模式）
# 3. 根据你选择的模型修改 [ai.multi_agent] [ai.provider_preference] 以及所需要的 K 线周期，或使用默认
# 4. 修改 [freqtrade.username] [freqtrade.password] 与 freqtrade-config.json 中 [api_server.username][api_server.password] 保持一致
# 5. 如果需要开启 Telegram 推送，请填写 freqtrade-config.json 中的 [telegram.enabled] 以及 config.toml 中的 [notify.telegram.enabled] 为 true 并填写相应的 token 和 chat_id
```

#### 1.1 代理访问 (Proxy)
```bash
# 1. 如果你使用代理，请确保打开 config.toml 中的 [market.sources.proxy.enabled] 填写你的 HTTP 以及 SOCKS5 的链接。
# 2. 请打开 docker-compose.yml 中的注释（freqtrade/brale 都需要），将 HTTP_PROXY 和 HTTPS_PROXY 修改为本地的端口。
# 3. 请修改  freqtrade-config.json 中的 [exchange.ccxt_config.proxies] 和 [exchange.ccxt_async_config.aiohttp_proxy] 为本地的端口，可直接复制 config-proxy.json 中的字段，修改端口即可。
```

### 2. 启动服务

推荐使用 Make 命令一键启动，它会自动清理环境、准备数据目录并按依赖顺序启动服务：

```bash
make start
```

或者手动分步启动：

```bash
# 1. 准备数据目录和策略文件
make prepare-dirs

# 2. 启动 Freqtrade (需先行启动)
BRALE_DATA_ROOT=running_log/brale_data FREQTRADE_USERDATA_ROOT=running_log/freqtrade_data docker compose up -d freqtrade

# 3. 启动 Brale
BRALE_DATA_ROOT=running_log/brale_data FREQTRADE_USERDATA_ROOT=running_log/freqtrade_data docker compose up -d brale
```

### 3. 验证运行

```bash
# 查看实时日志
make logs

# 服务健康检查
curl http://localhost:9991/healthz
```

## 🧩 指标体系

Brale 基于 `go-talib` 计算多维技术指标，支持自动根据配置调整：

- **趋势 (Trend)**: EMA (21/50/200), MACD (bullish/bearish/flat)
- **动能 (Momentum)**: RSI (overbought/oversold), ROC, Stochastic Oscillator, Williams %R
- **波动率 (Volatility)**: ATR (用于动态止损或滑点估算)
- **量价 (Volume)**: OBV (结合 ROC 判断量价共振)

## 🤝 贡献指南

欢迎提交 Issue 或 Pull Request！
1. Fork 本仓库
2. 创建特性分支 (`git checkout -b feature/AmazingFeature`)
3. 提交更改 (`git commit -m 'Add some AmazingFeature'`)
4. 推送到分支 (`git push origin feature/AmazingFeature`)
5. 开启 Pull Request

## 📄 版权说明

本项目采用 MIT 许可证 - 详见 [LICENSE](LICENSE) 文件。

## 🙏 致谢

- [Freqtrade](https://github.com/freqtrade/freqtrade) - 优秀的加密货币交易机器人
- [NoFxAiOS/nofx](https://github.com/NoFxAiOS/nofx) - 多 Agent 决策提示灵感来源
- [adshao/go-binance](https://github.com/adshao/go-binance) - 优雅的 Go 语言 Binance SDK
- [go-talib](https://github.com/markcheno/go-talib) - Go 语言技术分析库
