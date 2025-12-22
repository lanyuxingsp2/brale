# Brale (Break a leg) 🎭

> **AI 驱动的多 Agent 量化策略引擎**
> 
> *既然是演戏（交易），那就祝你 "Break a leg"（演出成功/大赚一笔）！*

[![English Documentation](https://img.shields.io/badge/lang-English-blue.svg)](../README.md)
[![新手指南](https://img.shields.io/badge/doc-新手指南-blueviolet.svg)](USER_GUIDE_CN.org)
[![Go Version](https://img.shields.io/badge/go-1.24.0-blue.svg)](../go.mod)
[![License](https://img.shields.io/badge/license-MIT-green.svg)](../LICENSE)

**Brale** 是一个将 **“AI 的深度思考”** 与 **“量化的极致执行”** 完美解耦的量化交易系统。它通过多 Agent 协同分析（趋势、形态、动能），结合 LLM（GPT, Claude, DeepSeek...）生成高胜率决策，并由高度优化的执行引擎进行毫秒级风险对齐。
[中文视频介绍](https://www.bilibili.com/video/BV1Ab2aB2EUY) 
## ✨ 核心特性

- 🧠 **双循环架构 (Dual-Loop)**:
  - **慢速决策环 (Slow Loop)**: 由 K 线对齐调度器触发，利用大模型进行多维深度推理。
  - **快速执行环 (Fast Loop)**: 由 `Plan Scheduler` 驱动，毫秒级价格监控，确保止盈止损（TP/SL）精准触发。
- 🤖 **多 Agent 分布式推理**:
  - **Indicator Agent**: 专注于 RSI, MACD, ATR 等数学指标的趋势共振。
  - **Pattern Agent**: 识别价格行为（Price Action）、SMC 流动性区域和经典 K 线形态。
  - **Trend Agent**: 屏蔽噪音，专注于多周期的大结构研判。
- ⚙️ **高度可配置化**:
  - **动态 Prompt 注入**: 支持为不同币种（如 BTC, ETH, SOL）配置独立的提示词库。
  - **灵活策略库**: 通过 YAML 定义复杂的退出计划（分批止盈、动态 ATR 追踪止损）。
- 🛡️ **被动执行模式 (Passive Executor)**: 无缝集成 **Freqtrade** 作为执行终端。Brale 掌握完全控制权，利用 Freqtrade 的稳定基础设施完成物理下单，消除策略层面的冗余。
- ⚡ **高性能 Go 内核**: 并发处理多币种行情拉取、指标计算与 Agent 调度。

## 🏗️ 架构流程

![架构图](Reasoning-Edition.png)

## ⚠️ 风险免责声明

**Brale 是一个用于量化交易研究和开发的开源工具，它并非金融投资建议。加密货币交易具有高度投机性，并伴随着巨大的风险。您可能会损失部分或全部投资资本。请勿投入您无法承受损失的资金。过往表现不代表未来业绩。使用 Brale 存在固有风险，请自行承担。**

## 🚀 快速启动 (Docker)

### 1. 准备配置

```bash
# 复制配置文件
cp configs/config.example.yaml configs/config.yaml
cp configs/user_data/freqtrade-config.example.json configs/user_data/freqtrade-config.json

# 注意：
# 1. 在 configs/config.yaml 中填入你的 LLM API Key
# 2. 在 configs/user_data/freqtrade-config.json 中配置交易所 API（或使用 dry-run 模式）
# 3. 根据你选择的模型修改 config.yaml / profiles.yaml 内的 [ai.multi_agent]、[ai.provider_preference] 和周期参数
```

### 2. 启动服务

推荐使用 Make 命令一键启动，它会自动清理环境、准备数据目录并按依赖顺序启动服务：

```bash
make start
```

## 🔌 执行层（可插拔）

Brale 通过 `Execution Engine` 抽象下发真实订单。默认实现是 [Freqtrade](https://github.com/freqtrade/freqtrade)，但核心逻辑与它解耦：

- **解耦逻辑**: 在 `configs/config.yaml` 中把 `freqtrade.enabled` 设为 `false`，即可只运行 AI 策略和指标分析。
- **被动控制**: Brale 通过 Freqtrade API 进行强制入场 (Force Entry) 和强制出场 (Force Exit)，Freqtrade 本身的策略文件 (`BraleSharedStrategy.py`) 保持为空，仅作为执行终端使用。
- **退出计划同步**: Brale 的 `Plan Scheduler` 会实时计算止盈止损点位，并在触及时向 Freqtrade 发送平仓指令，实现比 Freqtrade 原生止损更灵活的 AI 逻辑。

## 🧩 指标体系

Brale 基于 `go-talib` 计算多维技术指标，支持自动根据配置调整：

- **趋势 (Trend)**: EMA (21/50/200), MACD (bullish/bearish/flat)
- **动能 (Momentum)**: RSI (overbought/oversold), ROC, Stochastic Oscillator
- **波动率 (Volatility)**: ATR (用于动态止损或滑点估算)
- **衍生品数据**: 持仓量 (OI)、资金费率 (Funding Rate) - 需交易所支持。

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
