package errutil

import (
	"context"
	"errors"
)

func IsAny(err error, target error, targets ...error) (error, bool) {
	if errors.Is(err, target) {
		return target, true
	}
	for _, t := range targets {
		if errors.Is(err, t) {
			return t, true
		}
	}
	return nil, false
}

func IsContext(ctx context.Context) bool {
	err := ctx.Err()
	return nil != err && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded))
}
