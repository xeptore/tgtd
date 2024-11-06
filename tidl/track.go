package tidl

import (
	"context"
	"fmt"
	"os"

	"github.com/goccy/go-json"
	"github.com/xeptore/flaw/v8"

	"github.com/xeptore/tgtd/ptr"
	"github.com/xeptore/tgtd/tidl/must"
)

type Track interface {
	id() string
	FileName() string
	cover() string
	info() TrackInfo
}

type TrackInfo struct {
	Duration   int     `json:"duration"`
	Title      string  `json:"title"`
	ArtistName string  `json:"artistName"`
	Version    *string `json:"version"`
}

func (t *TrackInfo) FlawP() flaw.P {
	return flaw.P{
		"duration":    t.Duration,
		"title":       t.Title,
		"artist_name": t.ArtistName,
		"version":     ptr.ValueOr(t.Version, "<nil>"),
	}
}

func (d *Downloader) Track(ctx context.Context, id string) error {
	track, err := d.single(ctx, id)
	if nil != err {
		return fmt.Errorf("failed to get track info: %v", err)
	}

	if err := track.createDir(d.basePath); nil != err {
		return fmt.Errorf("failed to create track directory: %v", err)
	}

	if err := d.download(ctx, track); nil != err {
		return fmt.Errorf("failed to download track: %v", err)
	}

	return nil
}

func ReadTrackInfoFile(ctx context.Context, fileName string) (info *TrackInfo, err error) {
	file, err := os.Open(fileName + ".json")
	if nil != err {
		return nil, fmt.Errorf("failed to open track info file: %v", err)
	}
	defer func() {
		if closeErr := file.Close(); nil != closeErr {
			closeErr = flaw.From(fmt.Errorf("failed to close track info file: %v", closeErr))
			if nil != err {
				err = must.BeFlaw(err).Join(closeErr)
			} else {
				err = closeErr
			}
		}
	}()

	var trackInfo TrackInfo
	if err := json.NewDecoder(file).DecodeContext(ctx, &trackInfo); nil != err {
		return nil, flaw.From(fmt.Errorf("failed to decode track info file: %v", err))
	}
	return &trackInfo, nil
}
