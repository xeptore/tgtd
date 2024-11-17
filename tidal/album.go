package tidal

import (
	"time"
)

type AlbumMeta struct {
	Artist      string
	Title       string
	ReleaseDate time.Time
	CoverID     string
}
