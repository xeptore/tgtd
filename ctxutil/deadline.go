package ctxutil

import (
	"context"
	"time"
)

func WithDelayedTimeout(parent context.Context, delay time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-parent.Done()
		time.AfterFunc(delay, cancel)
	}()
	return ctx, cancel
}
