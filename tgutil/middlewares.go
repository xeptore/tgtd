package tgutil

import (
	"context"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/gotd/td/telegram"
	"github.com/iyear/tdl/core/middlewares/recovery"
	"github.com/iyear/tdl/core/middlewares/retry"
)

func DefaultMiddlewares(ctx context.Context) []telegram.Middleware {
	return []telegram.Middleware{
		retry.New(4),
		recovery.New(ctx, newBackoff(5*time.Minute)),
	}
}

func newBackoff(timeout time.Duration) backoff.BackOff {
	b := backoff.NewExponentialBackOff()
	b.Multiplier = 1.1
	b.MaxElapsedTime = timeout
	b.MaxInterval = 10 * time.Second
	return b
}
