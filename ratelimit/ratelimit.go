package ratelimit

import (
	"math/rand/v2"
	"time"
)

const (
	AlbumDownloadConcurrency    = 7
	AlbumUploadConcurrency      = 10
	PlaylistDownloadConcurrency = 7
	MixDownloadConcurrency      = 7
)

func TrackDownloadSleepMS() time.Duration {
	const (
		from = 1
		to   = 4
	)
	millis := (rand.IntN(to-from)+from)*1000 + rand.N(1000) //nolint:gosec
	return time.Duration(millis) * time.Millisecond
}
