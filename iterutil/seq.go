package iterutil

import (
	"iter"
)

func WithIndex[Slice ~[]E, E any](s iter.Seq[Slice]) iter.Seq2[int, Slice] {
	return func(yield func(int, Slice) bool) {
		index := 0
		for v := range s {
			if !yield(index, v) {
				return
			}
			index++
		}
	}
}
