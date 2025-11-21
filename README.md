# Brale

**项目名称为:** Break a leg

Brale 是一个以 AI 决策为核心的多 Agent 量化策略工具。系统会从 Binance 等交易所拉取 K 线数据，切分到多个时间周期后交由技术指标、价格形态、趋势判断等 Agent 协同分析，再交给 provider（如 LLM 模型）生成最终决策。策略信号通过权重聚合后交由 Freqtrade 执行，从而利用其成熟的仓位管理、止盈止损与风控能力。

## 核心流程
-  使用 go-talib 计算技术指标，输出结构化 JSON。
-  生成可视化图、形态描述，作为 Agent 提示的一部分。
-  汇总多 Agent 结论，通过 provider 生成执行建议
-  再由Freqtrade 对接完成下单。

## 指标计算
- **EMA (21/50/200)**：多空趋势的基础骨架，通过最新价与 EMA 的位置给出 `bullish/bearish` 状态。
- **RSI (14, 30/70 阈值)**：衡量动能，状态为 `overbought/oversold/neutral`。
- **MACD (12,26,9)**：读取直方值和信号线斜率，区分 `bullish/bearish/flat`。
- **ROC (9)**：价格变动率，用于捕捉加速或衰减的动量。
- **Stochastic Oscillator (14,3,3)**：关心 `%K` 与 `%D` 的粘性来识别震荡区间。
- **Williams %R (14)**：与 Stoch 配合确认极值区域。
- **ATR (14)**：提供波动率估计，用来动态调整止盈止损或回测滑点。
- **OBV (On Balance Volume)**：结合 ROC 判断量价共振，推断买卖力量是否同步。
如果配置中自定义了 horizon profile，则可以覆盖上述周期参数；Indicator 会自动计算所需最小样本数并驱动 `market.Warmup` 拉取更多历史。

## 参考项目
- [nofx QuantAgent](https://github.com/nofxlabs/QuantAgent)：多 Agent 决策提示模板灵感来源，自适应 prompt 结构被 Brale 复用升级。
- [QuantAgent Prompting](https://github.com/nofxlabs/quant-agent-prompting)：多模态指标→形态→趋势聚合的提示链条。
- [freqtrade/freqtrade](https://github.com/freqtrade/freqtrade)：成熟的开源 CTA 执行引擎，Brale 通过共享策略 `configs/user_data/brale_shared_strategy.py` 与其对接。

## Docker 快速启动
1. 准备配置：
   ```bash
   cp configs/config.example.toml configs/config.toml
   # 按需填入 API Key、交易对、agent/LLM provider 配置
   mkdir -p data/freqtrade/user_data
   ```
   `configs/user_data/freqtrade-config.json` 与 `brale_shared_strategy.py` 会在 Compose 中以只读挂载方式注入，无需再复制。
2. 构建镜像并启动：
   ```bash
   docker compose up --build -d
   ```
   该命令会拉起两个服务：`brale`（Go 应用）与 `freqtrade`（执行引擎）。`BRALE_CONFIG` 默认指向 `/app/configs/config.toml`，并通过 `FREQTRADE_API_URL` 与 Freqtrade RPC 对接。
3. 查看日志与健康状态：
   ```bash
   docker compose logs -f brale
   curl http://localhost:9991/healthz
   ```
4. 首次运行会在 `data/freqtrade/user_data` 里生成日志、SQLite 交易数据库，可持久化策略表现。若需停止服务，执行 `docker compose down`。

> 若希望在本地调试单独的 Go 程序，也可以按照仓库根目录的 `Makefile`：`make tidy && make build && BRALE_CONFIG=./configs/config.toml make run`。
