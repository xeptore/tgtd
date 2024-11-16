package fs

import "github.com/xeptore/tgtd/tidal"

type StoredMix struct {
	Caption string           `json:"caption"`
	Tracks  []StoredMixTrack `json:"tracks"`
}

type StoredMixTrack TrackWithID

type TrackWithID struct {
	Track
	ID string `json:"id"`
}

type Track struct {
	Artists  []tidal.TrackArtist `json:"artists"`
	Title    string              `json:"title"`
	Duration int                 `json:"duration"`
	Version  *string             `json:"version"`
	Format   tidal.TrackFormat   `json:"format"`
	CoverID  string              `json:"cover_id"`
}

type StoredSingleTrack struct {
	Track
	Album   StoredSingleTrackAlbum `json:"album"`
	Caption string                 `json:"caption"`
}

type StoredSingleTrackAlbum struct {
	Title string `json:"title"`
	Year  int    `json:"year"`
}

type StoredPlaylist struct {
	Caption string                `json:"caption"`
	Tracks  []StoredPlaylistTrack `json:"tracks"`
}

type StoredPlaylistTrack TrackWithID

type StoredAlbum struct {
	Caption string                     `json:"caption"`
	Volumes [][]StoredAlbumVolumeTrack `json:"volumes"`
}

type StoredAlbumVolumeTrack TrackWithID
