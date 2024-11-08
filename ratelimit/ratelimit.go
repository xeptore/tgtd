package ratelimit

import (
	"math/rand/v2"
	"time"
)

const (
	AlbumDownloadConcurrency    = 5
	PlaylistDownloadConcurrency = 5
	MixDownloadConcurrency      = 5
)

func TrackDownloadSleepMS() time.Duration {
	millis := (rand.IntN(4)+2)*1000 + rand.N(1000)
	return time.Duration(millis) * time.Millisecond
}
