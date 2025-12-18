# Brale (Break a leg) 🎭

> **AI-Driven Multi-Agent Quantitative Strategy Engine**
> 
> *"Break a leg" in your trading journey!*

[![中文文档](https://img.shields.io/badge/lang-中文-red.svg)](doc/README_CN.md)
[![Go Version](https://img.shields.io/badge/go-1.24.0-blue.svg)](go.mod)
[![License](https://img.shields.io/badge/license-MIT-green.svg)](LICENSE)

**Brale** is a quantitative trading system that perfectly decouples **"AI Deep Thinking"** from **"Quantitative Execution"**. It leverages multi-agent collaboration (Trend, Pattern, Momentum) combined with top-tier LLMs (GPT-4o, Claude 3.5, DeepSeek) to generate high-probability decisions, executed with millisecond-level risk alignment.

## ✨ Key Features

- 🧠 **Dual-Loop Architecture**:
  - **Slow Decision Loop**: Triggered by the Aligned Scheduler, performing multi-dimensional deep reasoning using LLMs at candle boundaries.
  - **Fast Execution Loop**: Driven by the `Plan Scheduler`, providing millisecond-level price monitoring for precise Take-Profit/Stop-Loss (TP/SL) execution.
- 🤖 **Distributed Multi-Agent Reasoning**:
  - **Indicator Agent**: Focuses on momentum resonance across RSI, MACD, and ATR.
  - **Pattern Agent**: Identifies Price Action, SMC liquidity zones, and classic candlestick patterns.
  - **Trend Agent**: Filters noise to focus on broader market structures across multiple timeframes.
- ⚙️ **Highly Configurable**:
  - **Dynamic Prompt Injection**: Supports independent prompt libraries for different symbols (e.g., BTC, ETH, SOL).
  - **Flexible Strategy Hub**: Define complex exit plans via YAML (tiered TP, dynamic ATR-based trailing stop-loss).
- 🛡️ **Passive Executor Mode**: Seamlessly integrates with **Freqtrade** as an execution terminal. Brale maintains full control, using Freqtrade's stable infrastructure for order fulfillment while keeping the strategy logic centralized in Brale.
- ⚡ **High-Performance Go Core**: Concurrent handling of multi-symbol data fetching, indicator calculation, and agent orchestration.

## 🏗️ Architecture

![Architecture](doc/Reasoning-Edition.png)

```mermaid
---
config:
  layout: fixed
  theme: neo
---
flowchart TB
    %% ==================== Layer Definitions ====================
    subgraph Layer0 ["Layer 0: Scheduling & Config"]
        direction TB
        Scheduler["Aligned Scheduler"]:::scheduler
        PlanSched["Plan Scheduler"]:::scheduler
        
        subgraph ConfigHub ["Configuration Hub"]
            direction TB
            Profile["Profile Config<br>(profiles.yaml)"]:::config
            PromptLib["Prompt Library<br>(prompts/*.txt)"]:::config
            StratLib["Strategy Config<br>(exit_strategies.yaml)"]:::config
        end
    end

    subgraph Layer1 ["Layer 1: Holographic Perception"]
        direction TB
        Exchange(("Exchange")):::external
        Gateway["Gateway"]:::data
        KlineStore[("Kline Store")]:::data
    end

    subgraph Layer2 ["Layer 2: Analysis Pipeline"]
        direction TB
        CtxInit["Context Init"]:::pipe
        subgraph Middleware ["Middleware Chain"]
            Stage0["Slicer"]:::pipe
            Stage1["Indicators"]:::pipe
            Stage2["Formatter"]:::pipe
        end
        AnalysisData["Analysis Context"]:::data
    end

    subgraph Layer3 ["Layer 3: Multi-Agent Reasoning"]
        direction TB
        IndAgent["Indicator Agent"]:::agent
        PatAgent["Pattern Agent"]:::agent
        TrendAgent["Trend Agent"]:::agent
    end

    subgraph ModelArena ["LLM Model Arena"]
        direction TB
        GPT["GPT-4o"]:::external
        Claude["Claude 3.5"]:::external
        DeepSeek["DeepSeek V3"]:::external
    end

    subgraph Layer4 ["Layer 4: Decision Engine"]
        direction TB
        ContextBuilder["Context Builder"]:::brain
        Aggregator{"Meta Aggregator"}:::meta
        Policy["ExitPlan Policy"]:::brain
    end

    subgraph Layer5 ["Layer 5: Execution & Feedback"]
        direction TB
        ExecEngine["Execution Engine"]:::exec
        Freqtrade["Freqtrade Bot<br>(Passive)"]:::exec
        DB[("Global DB")]:::db
    end

    %% ==================== Core Links ====================
    Scheduler -->|Trigger| Profile
    Profile -.->|Selects| PromptLib
    Profile -->|Configures| CtxInit
    PromptLib -.->|Injects| ContextBuilder
    
    Exchange -- Stream --> Gateway --> KlineStore
    KlineStore --> Stage0 --> Stage1 --> Stage2 --> AnalysisData
    
    AnalysisData --> IndAgent & PatAgent & TrendAgent --> ContextBuilder
    ContextBuilder --> GPT & Claude & DeepSeek --> Aggregator
    
    Aggregator -- "Consensus" --> Policy -- "Valid" --> ExecEngine
    
    ExecEngine -- "Force Entry" --> Freqtrade
    ExecEngine -- "Register Plan" --> PlanSched
    
    Gateway -. "Price Tick" .-> PlanSched
    PlanSched -- "Check TP/SL" --> PlanSched
    PlanSched -- "Trigger Exit" --> Freqtrade
    
    Freqtrade -- "Order" --> Exchange
    Freqtrade -- "Report" --> DB

    %% ==================== Style Definitions ====================
    classDef scheduler fill:#f9f,stroke:#333,stroke-width:2px;
    classDef config fill:#f3e5f5,stroke:#7b1fa2,stroke-width:2px;
    classDef external fill:#f5f5f5,stroke:#9e9e9e,stroke-width:2px,stroke-dasharray:5 5;
    classDef data fill:#e3f2fd,stroke:#1565c0,stroke-width:2px;
    classDef pipe fill:#e0f2f1,stroke:#00695c,stroke-width:2px;
    classDef agent fill:#fff3e0,stroke:#e65100,stroke-width:2px;
    classDef brain fill:#fff3e0,stroke:#e65100,stroke-width:2px;
    classDef meta fill:#fff8e1,stroke:#ff8f00,stroke-width:3px;
    classDef exec fill:#e8f5e9,stroke:#2e7d32,stroke-width:2px;
    classDef db fill:#eceff1,stroke:#455a64,stroke-width:2px;
```

## ⚠️ Financial Disclaimer

**Brale is an open-source tool for algorithmic trading research and development. It is NOT financial advice. Trading cryptocurrencies is highly speculative and carries a high level of risk. You could lose some or all of your invested capital. You should not invest money that you cannot afford to lose. Past performance is not indicative of future results. Use Brale at your own risk.**

## 🚀 Quick Start (Docker)

### 1. Configuration

```bash
# Copy configuration templates
cp configs/config.example.yaml configs/config.yaml
cp configs/user_data/freqtrade-config.example.json configs/user_data/freqtrade-config.json

# Notes:
# 1. Fill in your LLM API Key in configs/config.yaml
# 2. Configure Exchange API in configs/user_data/freqtrade-config.json (or use dry-run mode)
```

### 2. Start Services

```bash
make start
```

## 🔌 Execution Layer (Pluggable)

Brale executes trades through an `Execution Engine` abstraction. The default implementation uses [Freqtrade](https://github.com/freqtrade/freqtrade), but it is decoupled:

- **Decoupled Logic**: Set `freqtrade.enabled` to `false` in `configs/config.yaml` to run only the AI analysis.
- **Passive Control**: Brale issues `Force Entry` and `Force Exit` commands via the Freqtrade API. The Freqtrade-side strategy (`BraleSharedStrategy.py`) is intentionally passive.
- **Exit Plan Synchronization**: Brale's `Plan Scheduler` calculates real-time exit points and triggers Freqtrade, allowing for AI-driven risk management far more flexible than native stop-losses.

## 🧩 Indicator System

Brale uses `go-talib` to calculate multi-dimensional technical indicators:

- **Trend**: EMA (21/50/200), MACD (bullish/bearish/flat)
- **Momentum**: RSI (overbought/oversold), ROC, Stochastic Oscillator
- **Volatility**: ATR (for dynamic stop-loss or slippage estimation)
- **Derivatives Data**: Open Interest (OI), Funding Rate (if supported by exchange).

## 🤝 Contributing

Issues and Pull Requests are welcome!
1. Fork this repository
2. Create your feature branch (`git checkout -b feature/AmazingFeature`)
3. Commit your changes (`git commit -m 'Add some AmazingFeature'`)
4. Push to the branch (`git push origin feature/AmazingFeature`)
5. Open a Pull Request

## 📄 License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.