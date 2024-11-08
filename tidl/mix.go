package tidl

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path"
	"strconv"

	"github.com/goccy/go-json"
	"github.com/xeptore/flaw/v8"
	"golang.org/x/sync/errgroup"

	"github.com/xeptore/tgtd/errutil"
	"github.com/xeptore/tgtd/ratelimit"
	"github.com/xeptore/tgtd/tidl/auth"
	"github.com/xeptore/tgtd/tidl/must"
)

func mixTrackDir(mixID string) string {
	return path.Join("mixes", mixID)
}

func (d *Downloader) Mix(ctx context.Context, id string) error {
	tracks, err := d.mixTracks(ctx, id)
	if nil != err {
		return err
	}

	if err := d.prepareMixDir(ctx, id, tracks); nil != err {
		return err
	}

	wg, ctx := errgroup.WithContext(ctx)
	wg.SetLimit(ratelimit.MixDownloadConcurrency)
	for _, track := range tracks {
		wg.Go(func() error { return d.download(ctx, &track) })
	}

	if err := wg.Wait(); nil != err {
		return err
	}

	return nil
}

func (d *Downloader) prepareMixDir(ctx context.Context, id string, tracks []MixTrack) error {
	mixDir := path.Join(d.basePath, mixTrackDir(id))
	if err := os.RemoveAll(mixDir); nil != err {
		return flaw.From(fmt.Errorf("failed to delete possibly existing mix directory: %v", err))
	}
	flawP := flaw.P{"mix_dir": mixDir}
	if err := os.MkdirAll(mixDir, 0o0755); nil != err {
		return flaw.From(fmt.Errorf("failed to create mix directory: %v", err)).Append(flawP)
	}

	f, err := os.OpenFile(path.Join(mixDir, "info.json"), os.O_CREATE|os.O_SYNC|os.O_TRUNC|os.O_WRONLY, 0o0644)
	if nil != err {
		return flaw.From(fmt.Errorf("failed to create mix info file: %v", err)).Append(flawP)
	}
	if err := json.NewEncoder(f).EncodeContext(ctx, tracks); nil != err {
		if err, ok := errutil.IsAny(err, context.Canceled); ok {
			return err
		}
		return flaw.From(fmt.Errorf("failed to encode mix info: %v", err)).Append(flawP)
	}
	if err := f.Close(); nil != err {
		return flaw.From(fmt.Errorf("failed to close mix info file: %v", err)).Append(flawP)
	}
	return nil
}

type MixTrack struct {
	ID         string
	MixID      string
	Duration   int
	Title      string
	ArtistName string
	Cover      string
	Version    *string
}

func (t *MixTrack) id() string {
	return t.ID
}

func (t *MixTrack) FileName() string {
	var fileName string
	if nil != t.Version {
		fileName = fmt.Sprintf("%s - %s (%s).flac", t.ArtistName, t.Title, *t.Version)
	} else {
		fileName = fmt.Sprintf("%s - %s.flac", t.ArtistName, t.Title)
	}
	return path.Join(mixTrackDir(t.MixID), fileName)
}

func (t *MixTrack) cover() string {
	return t.Cover
}

func (t *MixTrack) info() TrackInfo {
	var title string
	if nil != t.Version {
		title = fmt.Sprintf("%s (%s)", t.Title, *t.Version)
	} else {
		title = t.Title
	}
	return TrackInfo{
		Duration:   t.Duration,
		Title:      title,
		ArtistName: t.ArtistName,
		Version:    t.Version,
	}
}

func (d *Downloader) mixTracksPage(ctx context.Context, id string, page int) (tracks []MixTrack, remaining int, err error) {
	mixURL, err := url.JoinPath(fmt.Sprintf(mixItemsAPIFormat, id))
	if nil != err {
		return nil, 0, flaw.From(fmt.Errorf("failed to create mix URL: %v", err))
	}
	flawP := flaw.P{"mix_url": mixURL}

	response, err := d.getPagedItems(ctx, mixURL, page)
	if nil != err {
		if err, ok := errutil.IsAny(err, auth.ErrUnauthorized, context.DeadlineExceeded, context.Canceled); ok {
			return nil, 0, err
		}
		return nil, 0, must.BeFlaw(err).Append(flawP)
	}
	defer func() {
		if closeErr := response.Body.Close(); nil != closeErr {
			closeErr = flaw.From(fmt.Errorf("failed to close get mix page items response body: %v", closeErr)).Append(flawP)
			if nil != err {
				if _, ok := errutil.IsAny(err, auth.ErrUnauthorized, context.DeadlineExceeded, context.Canceled); !ok {
					err = must.BeFlaw(err).Join(closeErr)
					return
				}
			}
			err = closeErr
		}
	}()
	flawP["response"] = errutil.HTTPResponseFlawPayload(response)

	var responseBody struct {
		TotalNumberOfItems int `json:"totalNumberOfItems"`
		Items              []struct {
			Type string `json:"type"`
			Item struct {
				ID           int    `json:"id"`
				TrackNumber  int    `json:"trackNumber"`
				VolumeNumber int    `json:"volumeNumber"`
				Title        string `json:"title"`
				Duration     int    `json:"duration"`
				Artist       struct {
					Name string `json:"name"`
				} `json:"artist"`
				Album struct {
					Cover string `json:"cover"`
				} `json:"album"`
				Version *string `json:"version"`
			} `json:"item"`
		} `json:"items"`
	}
	if err := json.NewDecoder(response.Body).DecodeContext(ctx, &responseBody); nil != err {
		if err, ok := errutil.IsAny(err, context.DeadlineExceeded, context.Canceled); ok {
			return nil, 0, err
		}
		return nil, 0, flaw.From(fmt.Errorf("failed to decode mix response: %v", err)).Append(flawP)
	}
	thisPageItems := len(responseBody.Items)
	if thisPageItems == 0 {
		return nil, 0, nil
	}

	for _, v := range responseBody.Items {
		if v.Type != trackTypeResponseItem {
			continue
		}
		mixTrack := MixTrack{
			ID:         strconv.Itoa(v.Item.ID),
			Duration:   v.Item.Duration,
			Title:      v.Item.Title,
			ArtistName: v.Item.Artist.Name,
			Cover:      v.Item.Album.Cover,
			MixID:      id,
			Version:    v.Item.Version,
		}
		tracks = append(tracks, mixTrack)
	}

	return tracks, responseBody.TotalNumberOfItems - (thisPageItems + page*pageSize), nil
}

func (d *Downloader) mixTracks(ctx context.Context, id string) ([]MixTrack, error) {
	var tracks []MixTrack
	var loopFlawPs []flaw.P
	flawP := flaw.P{"loop_flaw_payloads": loopFlawPs}
	for i := 0; ; i++ {
		loopFlawP := flaw.P{"page": i}
		loopFlawPs = append(loopFlawPs, loopFlawP)
		pageTracks, rem, err := d.mixTracksPage(ctx, id, i)
		if nil != err {
			if err, ok := errutil.IsAny(err, auth.ErrUnauthorized, context.DeadlineExceeded, context.Canceled); ok {
				return nil, err
			}
			return nil, must.BeFlaw(err).Append(flawP)
		}
		tracks = append(tracks, pageTracks...)

		if rem == 0 {
			break
		}
	}

	return tracks, nil
}
