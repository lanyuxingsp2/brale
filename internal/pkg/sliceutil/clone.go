package sliceutil

// Strings returns a copy of the string slice.
func Strings(src []string) []string {
	if len(src) == 0 {
		return nil
	}
	dst := make([]string, len(src))
	copy(dst, src)
	return dst
}

// DecisionSnapshots copies a slice of decision.PositionSnapshot.
// Defined in a separate file to avoid import cycle.
