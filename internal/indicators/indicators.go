package indicators

// 中文说明：
// 技术指标计算：EMA、MACD、RSI。
// 输入为收盘价序列（float64 切片），返回最新值。

// EMA 计算最近值（period>0 且 len(prices)>=period）。
func EMA(prices []float64, period int) float64 {
	n := len(prices)
	if period <= 0 || n < period {
		return 0
	}
	// 初始值为SMA
	ema := 0.0
	for i := n - period; i < n; i++ {
		ema += prices[i]
	}
	ema /= float64(period)
	k := 2.0 / float64(period+1)
	// 用全部序列推进一次（保持简洁）：
	// 从窗口之后的位置继续迭代（实际只有 period 个点落在尾部时，此处不会再推进）
	for i := n - period + 1; i < n; i++ {
		ema = (prices[i]-ema)*k + ema
	}
	return ema
}

// MACD = EMA12 - EMA26（不含信号线），基于输入序列的最近值
func MACD(prices []float64) float64 {
	if len(prices) < 26 {
		return 0
	}
	return EMA(prices, 12) - EMA(prices, 26)
}

// RSI（Wilder）
func RSI(prices []float64, period int) float64 {
	n := len(prices)
	if period <= 0 || n <= period {
		return 0
	}
	gains := 0.0
	losses := 0.0
	for i := n - period; i < n; i++ {
		diff := prices[i] - prices[i-1]
		if diff > 0 {
			gains += diff
		} else {
			losses -= diff
		}
	}
	if losses == 0 {
		return 100
	}
	rs := gains / losses
	rsi := 100 - 100/(1+rs)
	return rsi
}
