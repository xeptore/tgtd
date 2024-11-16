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
	ext, err := InferTrackExt(f.MimeType, f.Codec)
	if nil != err {
		panic(fmt.Sprintf("unsupported mime type %q", f.MimeType))
	}
	return ext
}

func InferTrackExt(mimeType, codec string) (string, error) {
	switch mimeType {
	case "audio/mp4":
		switch strings.ToLower(codec) {
		case "eac3", "aac", "alac":
			return "m4a", nil
		case "flac":
			return "flac", nil
		default:
			return "", fmt.Errorf("unsupported codec %q for audio/mp4 mime type", codec)
		}
	case "audio/flac":
		switch strings.ToLower(codec) {
		case "flac":
			return "flac", nil
		default:
			return "", fmt.Errorf("unsupported codec %q for audio/mp4 mime type", codec)
		}
	default:
		return "", fmt.Errorf("unsupported mime type %q", mimeType)
	}
}
