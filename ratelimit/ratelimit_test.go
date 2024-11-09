package ratelimit_test

import (
	"testing"

	"github.com/xeptore/tgtd/ratelimit"
)

func Test_TrackDownloadSleepMS(t *testing.T) {
	for range 100 {
		ms := ratelimit.TrackDownloadSleepMS().Milliseconds()
		if ms < 1000 || ms > 5000 {
			t.Errorf("expected 1000 <= ms <= 5000, got %d", ms)
		}
	}
}
