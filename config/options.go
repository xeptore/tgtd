package config

import "time"

var (
	AlbumInfoRequestTimeout        = 5 * time.Second
	MixInfoRequestTimeout          = 5 * time.Second
	PlaylistInfoRequestTimeout     = 5 * time.Second
	DashSegmentDownloadTimeout     = 10 * time.Second
	CoverDownloadTimeout           = 5 * time.Second
	GetPageTracksRequestTimeout    = 5 * time.Second
	GetStreamURLsRequestTimeout    = 5 * time.Second
	GetTrackFileSizeRequestTimeout = 5 * time.Second
)
