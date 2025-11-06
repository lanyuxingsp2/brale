package ai

import "fmt"

// 中文说明：
// 基础决策校验：
// - action 合法
// - 开仓（open_*）必须给出必要参数且>0

var validActions = map[string]bool{
	"open_long": true, "open_short": true, "close_long": true, "close_short": true, "hold": true, "wait": true,
}

func ValidateAll(ds []Decision) error {
	for i := range ds {
		if err := Validate(&ds[i]); err != nil {
			return fmt.Errorf("决策#%d 无效: %w", i+1, err)
		}
	}
	return nil
}

func Validate(d *Decision) error {
	if !validActions[d.Action] {
		return fmt.Errorf("非法 action: %s", d.Action)
	}
	if d.Action == "open_long" || d.Action == "open_short" {
		if d.Leverage <= 0 {
			return fmt.Errorf("开仓需提供 leverage>0")
		}
		if d.PositionSizeUSD <= 0 {
			return fmt.Errorf("开仓需提供 position_size_usd>0")
		}
		if d.StopLoss <= 0 || d.TakeProfit <= 0 {
			return fmt.Errorf("开仓需提供有效止损/止盈")
		}
		if d.Confidence < 0 || d.Confidence > 100 {
			return fmt.Errorf("confidence 范围0-100")
		}
	}
	return nil
}

// ValidateWithPrice 带当前价格的校验，增加：
// - 多空止损/止盈相对关系
// - 风险回报比（reward/risk >= minRR）
func ValidateWithPrice(d *Decision, price float64, minRR float64) error {
	if err := Validate(d); err != nil {
		return err
	}
	if d.Action != "open_long" && d.Action != "open_short" {
		return nil
	}
	if price <= 0 {
		return fmt.Errorf("缺少用于校验的当前价格")
	}
	if minRR <= 0 {
		minRR = 1.0
	}

	var risk, reward float64
	switch d.Action {
	case "open_long":
		if !(d.StopLoss < price && price < d.TakeProfit) {
			return fmt.Errorf("做多要求: 止损 < 价格 < 止盈")
		}
		risk = price - d.StopLoss
		reward = d.TakeProfit - price
	case "open_short":
		if !(d.TakeProfit < price && price < d.StopLoss) {
			return fmt.Errorf("做空要求: 止盈 < 价格 < 止损")
		}
		risk = d.StopLoss - price
		reward = price - d.TakeProfit
	}
	if risk <= 0 || reward <= 0 {
		return fmt.Errorf("无效风控参数（risk/reward<=0）")
	}
	rr := reward / risk
	if rr < minRR {
		return fmt.Errorf("风险回报比过低: %.2f < %.2f", rr, minRR)
	}
	return nil
}
