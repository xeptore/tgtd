package fs

import (
	"github.com/xeptore/tgtd/tidal"
)

type StoredMix struct {
	Caption  string   `json:"caption"`
	TrackIDs []string `json:"track_ids"`
}

type TrackInfo struct {
	Artists  []tidal.TrackArtist `json:"artists"`
	Title    string              `json:"title"`
	Duration int                 `json:"duration"`
	Version  *string             `json:"version"`
	Format   tidal.TrackFormat   `json:"format"`
	CoverID  string              `json:"cover_id"`
}

type StoredSingleTrack struct {
	TrackInfo
	Caption string `json:"caption"`
}

type StoredPlaylist struct {
	Caption  string   `json:"caption"`
	TrackIDs []string `json:"track_ids"`
}

type StoredAlbum struct {
	Caption        string     `json:"caption"`
	VolumeTrackIDs [][]string `json:"volume_track_ids"`
}
