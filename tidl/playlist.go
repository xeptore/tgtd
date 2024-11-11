package tidl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"time"

	"github.com/goccy/go-json"
	"github.com/xeptore/flaw/v8"
	"golang.org/x/sync/errgroup"

	"github.com/xeptore/tgtd/errutil"
	"github.com/xeptore/tgtd/must"
	"github.com/xeptore/tgtd/ptr"
	"github.com/xeptore/tgtd/ratelimit"
)

func playlistTrackDir(playlistID string) string {
	return path.Join("playlists", playlistID)
}

func (d *Downloader) Playlist(ctx context.Context, id string) error {
	tracks, err := d.playlistTracks(ctx, id)
	if nil != err {
		return err
	}

	meta, err := d.playlistInfo(ctx, id)
	if nil != err {
		return err
	}

	playlist := Playlist{
		ID:                id,
		CreatedAtYear:     meta.CreatedAtYear,
		LastUpdatedAtYear: meta.LastUpdatedAtYear,
		Title:             meta.Title,
		Tracks:            tracks,
	}

	if err := d.preparePlaylistDir(playlist); nil != err {
		return err
	}

	wg, ctx := errgroup.WithContext(ctx)
	wg.SetLimit(ratelimit.PlaylistDownloadConcurrency)
	for _, track := range tracks {
		wg.Go(func() error { return d.download(ctx, &track) })
	}

	if err := wg.Wait(); nil != err {
		return err
	}

	return nil
}

func (d *Downloader) preparePlaylistDir(p Playlist) error {
	playlistDir := path.Join(d.basePath, playlistTrackDir(p.ID))
	flawP := flaw.P{"playlist_dir": playlistDir}
	if err := os.RemoveAll(playlistDir); nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to delete possibly existing playlist directory: %v", err)).Append(flawP)
	}
	if err := os.MkdirAll(playlistDir, 0o0755); nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to create playlist directory: %v", err)).Append(flawP)
	}

	infoFilePath := path.Join(playlistDir, "info.json")
	flawP["info_file_path"] = infoFilePath
	f, err := os.OpenFile(infoFilePath, os.O_CREATE|os.O_SYNC|os.O_TRUNC|os.O_WRONLY, 0o0644)
	if nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to create playlist info file: %v", err)).Append(flawP)
	}
	if err := json.NewEncoder(f).Encode(p); nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to encode playlist info: %v", err)).Append(flawP)
	}
	if err := f.Sync(); nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to sync playlist info file: %v", err)).Append(flawP)
	}
	return nil
}

type Playlist struct {
	ID                string          `json:"id"`
	CreatedAtYear     int             `json:"created_at_year"`
	LastUpdatedAtYear int             `json:"last_updated_at_year"`
	Title             string          `json:"title"`
	Tracks            []PlaylistTrack `json:"tracks"`
}

type PlaylistTrack struct {
	ID         string  `json:"id"`
	PlayListID string  `json:"playlist_id"`
	Duration   int     `json:"duration"`
	Title      string  `json:"title"`
	ArtistName string  `json:"artist_name"`
	Cover      string  `json:"cover"`
	Version    *string `json:"version"`
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
				"version":       ptr.ValueOr(v.Item.Version, ""),
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
		"version":       ptr.ValueOr(i.Version, ""),
	}
}

func (r *PlaylistResponseItem) FlawP() flaw.P {
	return flaw.P{
		"type": r.Type,
		"item": r.Item.FlawP(),
	}
}

func (d *Downloader) playlistInfo(ctx context.Context, id string) (p *Playlist, err error) {
	playlistURL, err := url.JoinPath(fmt.Sprintf(playlistAPIFormat, id))
	if nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return nil, flaw.From(fmt.Errorf("failed to join playlist base URL with playlist id: %v", err)).Append(flawP)
	}
	flawP := flaw.P{"url": playlistURL}

	reqURL, err := url.Parse(playlistURL)
	if nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, flaw.From(fmt.Errorf("failed to parse playlist URL: %v", err)).Append(flawP)
	}

	params := make(url.Values, 1)
	params.Add("countryCode", "US")
	reqURL.RawQuery = params.Encode()
	flawP["encoded_query_params"] = reqURL.RawQuery
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL.String(), nil)
	if nil != err {
		if errutil.IsContext(ctx) {
			return nil, ctx.Err()
		}
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, flaw.From(fmt.Errorf("failed to create get playlist info request: %v", err)).Append(flawP)
	}
	req.Header.Add("Authorization", "Bearer "+d.auth.Creds.AccessToken)

	client := http.Client{Timeout: 5 * time.Minute} // TODO: set it to a reasonable value
	resp, err := client.Do(req)
	if nil != err {
		switch {
		case errutil.IsContext(ctx):
			return nil, ctx.Err()
		case errors.Is(err, context.DeadlineExceeded):
			return nil, context.DeadlineExceeded
		default:
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return nil, flaw.From(fmt.Errorf("failed to send get playlist info request: %v", err)).Append(flawP)
		}
	}
	flawP["response"] = errutil.HTTPResponseFlawPayload(resp)
	defer func() {
		if closeErr := resp.Body.Close(); nil != closeErr {
			flawP["err_debug_tree"] = errutil.Tree(closeErr).FlawP()
			closeErr = flaw.From(fmt.Errorf("failed to close get playlist info response body: %v", closeErr)).Append(flawP)
			switch {
			case nil == err:
				err = closeErr
			case errutil.IsContext(ctx):
				err = flaw.From(errors.New("context was ended")).Join(closeErr)
			case errors.Is(err, context.DeadlineExceeded):
				err = flaw.From(errors.New("timeout has reached")).Join(closeErr)
			case errors.Is(err, ErrTooManyRequests):
				err = flaw.From(errors.New("too many requests")).Join(closeErr)
			case errutil.IsFlaw(err):
				err = must.BeFlaw(err).Join(closeErr)
			default:
				panic(errutil.UnknownError(err))
			}
		}
	}()

	respBytes, err := io.ReadAll(resp.Body)
	if nil != err {
		switch {
		case errutil.IsContext(ctx):
			return nil, ctx.Err()
		case errors.Is(err, context.DeadlineExceeded):
			return nil, context.DeadlineExceeded
		default:
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return nil, flaw.From(fmt.Errorf("failed to read playlist info response body: %v", err)).Append(flawP)
		}
	}

	switch code := resp.StatusCode; code {
	case http.StatusOK:
	case http.StatusTooManyRequests:
		return nil, ErrTooManyRequests
	case http.StatusForbidden:
		if ok, err := errutil.IsTooManyErrorResponse(resp, respBytes); nil != err {
			flawP["response_body"] = string(respBytes)
			return nil, must.BeFlaw(err).Append(flawP)
		} else if ok {
			return nil, ErrTooManyRequests
		}

		flawP["response_body"] = string(respBytes)
		return nil, flaw.From(errors.New("received 403 response")).Append(flawP)
	default:
		flawP["response_body"] = string(respBytes)
		return nil, flaw.From(fmt.Errorf("unexpected status code: %d", code)).Append(flawP)
	}

	var respBody struct {
		Title       string `json:"title"`
		Created     string `json:"created"`
		LastUpdated string `json:"lastUpdated"`
	}
	if err := json.Unmarshal(respBytes, &respBody); nil != err {
		flawP["response_body"] = string(respBytes)
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, flaw.From(fmt.Errorf("failed to decode playlist response: %v", err)).Append(flawP)
	}

	const dateLayout = "2006-01-02T15:04:05.000-0700"
	createdAt, err := time.Parse(dateLayout, respBody.Created)
	if nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, flaw.From(fmt.Errorf("failed to parse playlist created date: %v", err)).Append(flawP)
	}

	lastUpdatedAt, err := time.Parse(dateLayout, respBody.LastUpdated)
	if nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, flaw.From(fmt.Errorf("failed to parse playlist last updated date: %v", err)).Append(flawP)
	}

	return &Playlist{
		ID:                id,
		CreatedAtYear:     createdAt.Year(),
		LastUpdatedAtYear: lastUpdatedAt.Year(),
		Title:             respBody.Title,
		Tracks:            nil,
	}, nil
}

func (d *Downloader) playlistTracksPage(ctx context.Context, id string, page int) (tracks []PlaylistTrack, remaining int, err error) {
	playlistURL, err := url.JoinPath(fmt.Sprintf(playlistItemsAPIFormat, id))
	if nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return nil, 0, flaw.From(fmt.Errorf("failed to create playlist URL: %v", err)).Append(flawP)
	}
	flawP := flaw.P{"url": playlistURL}

	respBytes, err := d.getPagedItems(ctx, playlistURL, page)
	if nil != err {
		switch {
		case errutil.IsContext(ctx):
			return nil, 0, ctx.Err()
		case errors.Is(err, context.DeadlineExceeded):
			return nil, 0, context.DeadlineExceeded
		case errors.Is(err, ErrTooManyRequests):
			return nil, 0, ErrTooManyRequests
		case errutil.IsFlaw(err):
			return nil, 0, must.BeFlaw(err).Append(flawP)
		default:
			panic(errutil.UnknownError(err))
		}
	}

	var respBody PlaylistResponse
	if err := json.Unmarshal(respBytes, &respBody); nil != err {
		flawP["response_body"] = string(respBytes)
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, 0, flaw.From(fmt.Errorf("failed to decode mix response: %v", err)).Append(flawP)
	}

	thisPageItemsCount := len(respBody.Items)
	if thisPageItemsCount == 0 {
		return nil, 0, os.ErrNotExist
	}
	flawP["response_body"] = respBody.FlawP()

	for _, v := range respBody.Items {
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

	return tracks, respBody.TotalNumberOfItems - (thisPageItemsCount + page*pageSize), nil
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
			switch {
			case errutil.IsContext(ctx):
				return nil, ctx.Err()
			case errors.Is(err, os.ErrNotExist):
				break
			case errors.Is(err, context.DeadlineExceeded):
				return nil, context.DeadlineExceeded
			case errors.Is(err, ErrTooManyRequests):
				return nil, ErrTooManyRequests
			case errutil.IsFlaw(err):
				return nil, must.BeFlaw(err).Append(flawP)
			default:
				panic(errutil.UnknownError(err))
			}
		}
		flawP["remaining"] = rem

		tracks = append(tracks, pageTracks...)

		if rem == 0 {
			break
		}
	}

	return tracks, nil
}
