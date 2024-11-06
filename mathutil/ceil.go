package mathutil

import (
	"golang.org/x/exp/constraints"
)

func CeilInts[T constraints.Integer](a, b T) T {
	if (a < 0) == (b < 0) {
		if a > 0 {
			return (a + b - 1) / b
		} else {
			return (a + b + 1) / b
		}
	} else {
		return a / b
	}
}
