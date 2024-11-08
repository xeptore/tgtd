package tidl

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"strconv"

	"github.com/goccy/go-json"
	"github.com/xeptore/flaw/v8"
	"golang.org/x/sync/errgroup"

	"github.com/xeptore/tgtd/errutil"
	"github.com/xeptore/tgtd/ptr"
	"github.com/xeptore/tgtd/tidl/auth"
	"github.com/xeptore/tgtd/tidl/must"
)

func playlistTrackDir(playlistID string) string {
	return path.Join("playlists", playlistID)
}

func (d *Downloader) Playlist(ctx context.Context, id string) error {
	tracks, err := d.playlistTracks(ctx, id)
	if nil != err {
		return err
	}

	wg, ctx := errgroup.WithContext(ctx)
	wg.SetLimit(playlistDownloadConcurrency)

	if err := d.preparePlaylistDir(id, tracks); nil != err {
		return err
	}

	for _, track := range tracks {
		wg.Go(func() error {
			if err := d.download(ctx, &track); nil != err {
				return err
			}
			return nil
		})
	}

	if err := wg.Wait(); nil != err {
		return err
	}

	return nil
}

func (d *Downloader) preparePlaylistDir(id string, tracks []PlaylistTrack) error {
	playlistDir := path.Join(d.basePath, playlistTrackDir(id))
	flawP := flaw.P{"playlist_dir": playlistDir}
	if err := os.RemoveAll(playlistDir); nil != err {
		return flaw.From(fmt.Errorf("failed to delete possibly existing playlist directory: %v", err)).Append(flawP)
	}
	if err := os.MkdirAll(playlistDir, 0o0755); nil != err {
		return flaw.From(fmt.Errorf("failed to create playlist directory: %v", err)).Append(flawP)
	}

	infoFilePath := path.Join(playlistDir, "info.json")
	flawP["info_file_path"] = infoFilePath
	f, err := os.OpenFile(infoFilePath, os.O_CREATE|os.O_SYNC|os.O_TRUNC|os.O_WRONLY, 0o0644)
	if nil != err {
		return flaw.From(fmt.Errorf("failed to create playlist info file: %v", err)).Append(flawP)
	}
	if err := json.NewEncoder(f).Encode(tracks); nil != err {
		return flaw.From(fmt.Errorf("failed to encode playlist info: %v", err)).Append(flawP)
	}
	if err := f.Sync(); nil != err {
		return flaw.From(fmt.Errorf("failed to sync playlist info file: %v", err)).Append(flawP)
	}
	return nil
}

type PlaylistTrack struct {
	ID         string
	PlayListID string
	Duration   int
	Title      string
	ArtistName string
	Cover      string
	Version    *string
}

func (t *PlaylistTrack) id() string {
	return t.ID
}

func (t *PlaylistTrack) FileName() string {
	var fileName string
	if nil != t.Version {
		fileName = fmt.Sprintf("%s - %s (%s).flac", t.ArtistName, t.Title, *t.Version)
	} else {
		fileName = fmt.Sprintf("%s - %s.flac", t.ArtistName, t.Title)
	}
	return path.Join(playlistTrackDir(t.PlayListID), fileName)
}

func (t *PlaylistTrack) cover() string {
	return t.Cover
}

func (t *PlaylistTrack) info() TrackInfo {
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

type PlaylistResponse struct {
	TotalNumberOfItems int                    `json:"totalNumberOfItems"`
	Items              []PlaylistResponseItem `json:"items"`
}

func (r *PlaylistResponse) FlawP() flaw.P {
	items := make([]flaw.P, 0, len(r.Items))
	for _, v := range r.Items {
		items = append(items, flaw.P{
			"type": v.Type,
			"item": flaw.P{
				"id":            v.Item.ID,
				"track_number":  v.Item.TrackNumber,
				"volume_number": v.Item.VolumeNumber,
				"title":         v.Item.Title,
				"duration":      v.Item.Duration,
				"artist":        flaw.P{"name": v.Item.Artist.Name},
				"album":         flaw.P{"cover": v.Item.Album.Cover},
				"version":       ptr.ValueOr(v.Item.Version, "<nil>"),
			},
		})
	}
	return flaw.P{
		"total_number_of_items": r.TotalNumberOfItems,
		"items":                 items,
	}
}

type PlaylistResponseItem struct {
	Type string       `json:"type"`
	Cut  any          `json:"any"`
	Item PlaylistItem `json:"item"`
}

type PlaylistItem struct {
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
}

func (i *PlaylistItem) FlawP() flaw.P {
	return flaw.P{
		"id":            i.ID,
		"track_number":  i.TrackNumber,
		"volume_number": i.VolumeNumber,
		"title":         i.Title,
		"duration":      i.Duration,
		"artist":        flaw.P{"name": i.Artist.Name},
		"album":         flaw.P{"cover": i.Album.Cover},
		"version":       ptr.ValueOr(i.Version, "<nil>"),
	}
}

func (r *PlaylistResponseItem) FlawP() flaw.P {
	return flaw.P{
		"type": r.Type,
		"item": r.Item.FlawP(),
	}
}

func (d *Downloader) playlistTracksPage(ctx context.Context, id string, page int) (tracks []PlaylistTrack, remaining int, err error) {
	playlistURL, err := url.JoinPath(fmt.Sprintf(playlistItemsAPIFormat, id))
	if nil != err {
		return nil, 0, flaw.From(fmt.Errorf("failed to create playlist URL: %v", err))
	}
	flawP := flaw.P{"url": playlistURL}

	response, err := d.getPagedItems(ctx, playlistURL, page)
	if nil != err {
		if err, ok := errutil.IsAny(err, auth.ErrUnauthorized, context.DeadlineExceeded, context.Canceled); ok {
			return nil, 0, err
		}
		return nil, 0, must.BeFlaw(err).Append(flawP)
	}
	defer func() {
		if closeErr := response.Body.Close(); nil != closeErr {
			closeErr = flaw.From(fmt.Errorf("failed to close get playlist page items response body: %v", closeErr)).Append(flawP)
			if nil != err {
				if _, ok := errutil.IsAny(err, context.DeadlineExceeded, context.Canceled); !ok {
					err = must.BeFlaw(err).Join(closeErr)
					return
				}
			}
			err = closeErr
		}
	}()
	flawP["response"] = errutil.HTTPResponseFlawPayload(response)

	var responseBody PlaylistResponse
	if err := json.NewDecoder(response.Body).DecodeContext(ctx, &responseBody); nil != err {
		if err, ok := errutil.IsAny(err, context.DeadlineExceeded, context.Canceled); ok {
			return nil, 0, err
		}
		return nil, 0, flaw.From(fmt.Errorf("failed to decode mix response: %v", err)).Append(flawP)
	}
	thisPageItems := len(responseBody.Items)
	if thisPageItems == 0 {
		return nil, 0, os.ErrNotExist
	}
	flawP["response_body"] = responseBody.FlawP()

	for _, v := range responseBody.Items {
		if v.Type != trackTypeResponseItem {
			continue
		}
		if v.Cut != nil {
			return nil, 0, flaw.From(errors.New("cut items are not supported")).Append(flawP, flaw.P{"failed_item": v.FlawP()})
		}

		playlistTrack := PlaylistTrack{
			ID:         strconv.Itoa(v.Item.ID),
			Duration:   v.Item.Duration,
			Title:      v.Item.Title,
			ArtistName: v.Item.Artist.Name,
			Cover:      v.Item.Album.Cover,
			PlayListID: id,
			Version:    v.Item.Version,
		}
		tracks = append(tracks, playlistTrack)
	}

	return tracks, responseBody.TotalNumberOfItems - (thisPageItems + page*pageSize), nil
}

func (d *Downloader) playlistTracks(ctx context.Context, id string) ([]PlaylistTrack, error) {
	var tracks []PlaylistTrack
	var loopFlawPs []flaw.P
	flawP := flaw.P{"loop_flaw_payloads": loopFlawPs}
	for i := 0; ; i++ {
		loopFlawP := flaw.P{"page": i}
		loopFlawPs = append(loopFlawPs, loopFlawP)
		flawP["loop_flaw_payloads"] = loopFlawPs
		pageTracks, rem, err := d.playlistTracksPage(ctx, id, i)
		if nil != err {
			if errors.Is(err, os.ErrNotExist) {
				break
			}
			if err, ok := errutil.IsAny(err, auth.ErrUnauthorized, context.DeadlineExceeded, context.Canceled); ok {
				return nil, err
			}
			return nil, must.BeFlaw(err).Append(flawP)
		}
		flawP["remaining"] = rem

		tracks = append(tracks, pageTracks...)

		if rem == 0 {
			break
		}
	}

	return tracks, nil
}
