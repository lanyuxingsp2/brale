package ai

import (
	"context"
	"errors"
)

// 中文说明：
// 多模型聚合器，用于整合多个模型输出。

// ModelOutput 模型执行后的统一表示
type ModelOutput struct {
	ProviderID string
	Raw        string
	Parsed     DecisionResult
	Err        error
}

// Aggregator 聚合接口
type Aggregator interface {
	Aggregate(ctx context.Context, outputs []ModelOutput) (ModelOutput, error)
	Name() string
}

// FirstWinsAggregator 取第一个成功的输出
type FirstWinsAggregator struct{}

func (a FirstWinsAggregator) Name() string { return "first-wins" }

func (a FirstWinsAggregator) Aggregate(ctx context.Context, outputs []ModelOutput) (ModelOutput, error) {
	for _, o := range outputs {
		if o.Err == nil && len(o.Parsed.Decisions) > 0 {
			return o, nil
		}
	}
	return ModelOutput{}, errors.New("无可用的模型输出")
}

// MajorityAggregator 选择“信息量最大”的输出（简单多票近似：决策数量最多即胜出）
type MajorityAggregator struct{}

func (a MajorityAggregator) Name() string { return "majority" }

func (a MajorityAggregator) Aggregate(ctx context.Context, outputs []ModelOutput) (ModelOutput, error) {
	bestIdx := -1
	bestCount := -1
	for i, o := range outputs {
		if o.Err != nil {
			continue
		}
		c := len(o.Parsed.Decisions)
		if c > bestCount {
			bestCount = c
			bestIdx = i
		}
	}
	if bestIdx == -1 {
		return ModelOutput{}, errors.New("无可用的模型输出")
	}
	return outputs[bestIdx], nil
}

// WeightedAggregator 根据 provider 权重选择输出（权重越大优先级越高；同权时回退最多决策数）
type WeightedAggregator struct{ Weights map[string]float64 }

func (a WeightedAggregator) Name() string { return "weighted" }

func (a WeightedAggregator) Aggregate(ctx context.Context, outputs []ModelOutput) (ModelOutput, error) {
	bestIdx := -1
	bestW := -1.0
	bestCount := -1
	for i, o := range outputs {
		if o.Err != nil {
			continue
		}
		w := 1.0
		if a.Weights != nil {
			if v, ok := a.Weights[o.ProviderID]; ok {
				w = v
			}
		}
		if w > bestW || (w == bestW && len(o.Parsed.Decisions) > bestCount) {
			bestW = w
			bestCount = len(o.Parsed.Decisions)
			bestIdx = i
		}
	}
	if bestIdx == -1 {
		return ModelOutput{}, errors.New("无可用的模型输出")
	}
	return outputs[bestIdx], nil
}
