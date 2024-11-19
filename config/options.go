package config

import "time"

var (
	AlbumMetaRequestTimeout        = 5 * time.Second
	MixMetaRequestTimeout          = 5 * time.Second
	PlaylistMetaRequestTimeout     = 5 * time.Second
	DashSegmentDownloadTimeout     = 10 * time.Second
	VNDSegmentDownloadTimeout      = 10 * time.Second
	CoverDownloadTimeout           = 5 * time.Second
	GetPageTracksRequestTimeout    = 5 * time.Second
	GetStreamURLsRequestTimeout    = 5 * time.Second
	GetTrackFileSizeRequestTimeout = 5 * time.Second
	GetTrackCreditsRequestTimeout  = 2 * time.Second
	GetTrackLyricsRequestTimeout   = 2 * time.Second
)
