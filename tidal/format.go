package tidal

import (
	"fmt"
	"strings"
)

type TrackFormat struct {
	MimeType string `json:"mime_type"`
	Codec    string `json:"codec"`
}

func (f TrackFormat) InferTrackExt() string {
	switch f.MimeType {
	case "audio/mp4":
		switch strings.ToLower(f.Codec) {
		case "eac3", "aac", "alac":
			return "m4a"
		case "flac":
			return "flac"
		default:
			panic(fmt.Sprintf("unsupported codec %q for audio/mp4 mime type", f.Codec))
		}
	default:
		panic(fmt.Sprintf("unsupported mime type %q", f.MimeType))
	}
}
