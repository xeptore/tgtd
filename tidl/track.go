package tidl

import (
	"context"
	"fmt"
	"os"

	"github.com/goccy/go-json"
	"github.com/rs/zerolog"
	"github.com/xeptore/flaw/v8"

	"github.com/xeptore/tgtd/errutil"
	"github.com/xeptore/tgtd/must"
	"github.com/xeptore/tgtd/ptr"
)

const trackTypeResponseItem = "track"

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
		"version":     ptr.ValueOr(t.Version, ""),
	}
}

func (t *TrackInfo) Log(e *zerolog.Event) {
	e.
		Int("duration", t.Duration).
		Str("title", t.Title).
		Str("artist_name", t.ArtistName).
		Str("version", ptr.ValueOr(t.Version, ""))
}

func (d *Downloader) Track(ctx context.Context, id string) error {
	track, err := d.single(ctx, id)
	if nil != err {
		return err
	}

	album, err := d.albumInfo(ctx, track.Album.ID)
	if nil != err {
		return err
	}

	if err := d.prepareTrackDir(track, *album); nil != err {
		return err
	}

	if err := d.download(ctx, track); nil != err {
		return err
	}

	return nil
}

func ReadTrackInfoFile(fileName string) (info *TrackInfo, err error) {
	file, err := os.Open(fileName + ".json")
	if nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return nil, flaw.From(fmt.Errorf("failed to open track info file: %v", err)).Append(flawP)
	}
	defer func() {
		if closeErr := file.Close(); nil != closeErr {
			flawP := flaw.P{"err_debug_tree": errutil.Tree(closeErr).FlawP()}
			closeErr = flaw.From(fmt.Errorf("failed to close track info file: %v", closeErr)).Append(flawP)
			if nil != err {
				err = must.BeFlaw(err).Join(closeErr)
			} else {
				err = closeErr
			}
		}
	}()

	var trackInfo TrackInfo
	if err := json.NewDecoder(file).Decode(&trackInfo); nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP(), "track": trackInfo.FlawP()}
		return nil, flaw.From(fmt.Errorf("failed to decode track info file: %v", err)).Append(flawP)
	}
	return &trackInfo, nil
}
