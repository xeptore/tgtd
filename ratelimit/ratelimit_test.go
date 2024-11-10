package ratelimit_test

import (
	"testing"

	"github.com/xeptore/tgtd/ratelimit"
)

func TestTrackDownloadSleepMS(t *testing.T) {
	t.Parallel()
	for range 100 {
		ms := ratelimit.TrackDownloadSleepMS().Milliseconds()
		if ms < 1000 || ms > 4000 {
			t.Errorf("expected 1000 <= ms <= 4000, got %d", ms)
		}
	}
}
