package ctxutil_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/xeptore/tgtd/ctxutil"
)

func TestWithDelayedTimeout(t *testing.T) {
	t.Parallel()

	t.Run("initially_active", func(t *testing.T) {
		t.Parallel()

		parentCtx, parentCancel := context.WithCancel(t.Context())
		defer parentCancel()

		waitDur := 2 * time.Second

		ctx, cancel := ctxutil.WithDelayedTimeout(parentCtx, waitDur)
		defer cancel()

		select {
		case <-ctx.Done():
			assert.Fail(t, "expected returned context to be active initially")
		default:
		}
	})

	t.Run("cancels_after_delay", func(t *testing.T) {
		t.Parallel()

		parentCtx, parentCancel := context.WithCancel(t.Context())
		defer parentCancel()

		waitDur := 2 * time.Second

		ctx, cancel := ctxutil.WithDelayedTimeout(parentCtx, waitDur)
		defer cancel()

		parentCancel()

		select {
		case <-ctx.Done():
			assert.Fail(t, "expected returned context to remain active immediately after parent cancellation")
		default:
		}

		time.Sleep(waitDur)

		select {
		case <-ctx.Done():
			elapsed := time.Since(time.Now().Add(-waitDur))
			if elapsed < 2*time.Second {
				assert.Fail(t, "unexpected delay", "expected delay of approximately 2 seconds, but was %v", elapsed)
			}
		case <-time.After(waitDur + 500*time.Millisecond):
			assert.Fail(t, "expected returned context to be canceled after approximately 2 seconds")
		}
	})
}
