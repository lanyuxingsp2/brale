package sliceutil

import "brale/internal/decision"

func DecisionSnapshots(src []decision.PositionSnapshot) []decision.PositionSnapshot {
	if len(src) == 0 {
		return nil
	}
	dst := make([]decision.PositionSnapshot, len(src))
	copy(dst, src)
	return dst
}
