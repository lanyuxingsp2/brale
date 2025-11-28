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
- [nofx ](https://github.com/NoFxAiOS/nofx)：多 Agent 决策提示模板灵感来源，自适应 prompt 结构被 Brale 复用升级。
- [QuantAgent Prompting](https://github.com/Y-Research-SBU/QuantAgent.git)：多模态指标→形态→趋势聚合的提示链条。
- [freqtrade/freqtrade](https://github.com/freqtrade/freqtrade)：成熟的开源 CTA 执行引擎，Brale 通过共享策略 `configs/user_data/brale_shared_strategy.py` 与其对接。

## 快速启动（Docker）
1. 准备配置  
   ```bash
   cp configs/config.example.toml configs/config.toml
   cp configs/user_data/freqtrade-config.example.json configs/user_data/freqtrade-config.json
   # 按需填写 API/密钥、交易对等
   ```
2. 准备运行目录并复制 freqtrade 配置/策略  
   ```bash
   make prepare-dirs
   ```
3. 启动服务（建议先 freqtrade，再 brale）  
   ```bash
   BRALE_DATA_ROOT=running_log/brale_data FREQTRADE_USERDATA_ROOT=running_log/freqtrade_data docker compose up -d freqtrade
   # 等待 freqtrade 就绪（约 10 秒）
   BRALE_DATA_ROOT=running_log/brale_data FREQTRADE_USERDATA_ROOT=running_log/freqtrade_data docker compose up -d brale
   ```
   也可一键执行 `scripts/quickstart.sh`（会自动复制配置、make prepare-dirs，并按顺序启动 freqtrade→brale）。
4. 查看日志与健康检查  
   ```bash
   docker compose logs -f freqtrade
   docker compose logs -f brale
   curl http://localhost:9991/healthz
   ```
5. 本地调试（可选）  
   ```bash
   make build
   BRALE_CONFIG=./configs/config.toml make run
   ```

