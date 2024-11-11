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
	"github.com/xeptore/tgtd/ratelimit"
)

func albumTrackDir(albumID string, volumeNumber int) string {
	return path.Join("albums", albumID, strconv.Itoa(volumeNumber))
}

func (d *Downloader) Album(ctx context.Context, id string) error {
	volumeTracks, err := d.albumVolumeTracks(ctx, id)
	if nil != err {
		return err
	}

	album, err := d.albumInfo(ctx, id)
	if nil != err {
		return err
	}

	vols := make([]Volume, len(volumeTracks))
	for i, v := range volumeTracks {
		vols[i] = Volume{
			Number: i + 1,
			Album:  *album,
			Tracks: v,
		}
	}

	for _, vol := range vols {
		if err := d.prepareAlbumVolumeDir(vol); nil != err {
			return must.BeFlaw(err)
		}
	}

	wg, wgCtx := errgroup.WithContext(ctx)
	wg.SetLimit(ratelimit.AlbumDownloadConcurrency)
	for _, vol := range vols {
		for _, track := range vol.Tracks {
			wg.Go(func() error { return d.download(wgCtx, &track) })
		}
	}

	if err := wg.Wait(); nil != err {
		return err
	}

	return nil
}

type Volume struct {
	Number int          `json:"number"`
	Album  Album        `json:"album"`
	Tracks []AlbumTrack `json:"tracks"`
}

func (d *Downloader) prepareAlbumVolumeDir(vol Volume) (err error) {
	volDir := path.Join(d.basePath, albumTrackDir(vol.Album.ID, vol.Number))
	flawP := flaw.P{"volume_dir": volDir}
	if err := os.RemoveAll(volDir); nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to delete possibly existing album volume directory: %v", err)).Append(flawP)
	}
	if err := os.MkdirAll(volDir, 0o0755); nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to create album volume directory: %v", err)).Append(flawP)
	}

	volumeInfoFilePath := path.Join(volDir, "volume.json")
	flawP["volume_info_file"] = volumeInfoFilePath
	f, err := os.OpenFile(volumeInfoFilePath, os.O_CREATE|os.O_SYNC|os.O_TRUNC|os.O_WRONLY, 0o0644)
	if nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to create volume info file: %v", err)).Append(flawP)
	}
	defer func() {
		if closeErr := f.Close(); nil != closeErr {
			flawP["err_debug_tree"] = errutil.Tree(closeErr).FlawP()
			closeErr = flaw.From(fmt.Errorf("failed to close volume info file properly: %v", err)).Append(flawP)
			if nil != err {
				err = must.BeFlaw(err).Join(closeErr)
			} else {
				err = closeErr
			}
		}
	}()

	if err := json.NewEncoder(f).Encode(vol); nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to encode and write volume info json file content: %v", err)).Append(flawP)
	}

	if err := f.Sync(); nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to sync volume info file: %v", err)).Append(flawP)
	}

	return nil
}

func (d *Downloader) albumInfo(ctx context.Context, id string) (a *Album, err error) {
	albumURL, err := url.JoinPath(fmt.Sprintf(albumAPIFormat, id))
	if nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return nil, flaw.From(fmt.Errorf("failed to join album base URL with album id: %v", err)).Append(flawP)
	}
	flawP := flaw.P{"url": albumURL}

	reqURL, err := url.Parse(albumURL)
	if nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, flaw.From(fmt.Errorf("failed to parse album URL: %v", err)).Append(flawP)
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
		return nil, flaw.From(fmt.Errorf("failed to create get album info request: %v", err)).Append(flawP)
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
			return nil, flaw.From(fmt.Errorf("failed to send get album info request: %v", err)).Append(flawP)
		}
	}
	flawP["response"] = errutil.HTTPResponseFlawPayload(resp)
	defer func() {
		if closeErr := resp.Body.Close(); nil != closeErr {
			flawP["err_debug_tree"] = errutil.Tree(closeErr).FlawP()
			closeErr = flaw.From(fmt.Errorf("failed to close get album info response body: %v", closeErr)).Append(flawP)
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
			return nil, flaw.From(fmt.Errorf("failed to read album info response body: %v", err)).Append(flawP)
		}
	}

	switch code := resp.StatusCode; code {
	case http.StatusOK:
	case http.StatusTooManyRequests:
		return nil, ErrTooManyRequests
	case http.StatusForbidden:
		if ok, err := errutil.IsTooManyErrorResponse(resp, respBytes); nil != err {
			return nil, must.BeFlaw(err)
		} else if ok {
			return nil, ErrTooManyRequests
		}
		flawP["response_body"] = string(respBytes)
		return nil, flaw.From(errors.New("unexpected 403 response")).Append(flawP)
	default:
		flawP["response_body"] = string(respBytes)
		return nil, flaw.From(fmt.Errorf("unexpected status code: %d", code)).Append(flawP)
	}

	var respBody struct {
		Title       string `json:"title"`
		ReleaseDate string `json:"releaseDate"`
		Cover       string `json:"cover"`
	}
	if err := json.Unmarshal(respBytes, &respBody); nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, flaw.From(fmt.Errorf("failed to decode album info response: %v", err)).Append(flawP)
	}

	releaseDate, err := time.Parse("2006-01-02", respBody.ReleaseDate)
	if nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, flaw.From(fmt.Errorf("failed to parse album release date: %v", err)).Append(flawP)
	}

	return &Album{
		ID:    id,
		Year:  releaseDate.Year(),
		Title: respBody.Title,
		Cover: respBody.Cover,
	}, nil
}

type AlbumTrack struct {
	ID           string
	Number       int
	VolumeNumber int
	Duration     int
	Title        string
	Artist       Artist
	Album        Album
	Version      *string
}

func (t *AlbumTrack) id() string {
	return t.ID
}

func (t *AlbumTrack) FileName() string {
	var fileName string
	if nil != t.Version {
		fileName = fmt.Sprintf("%d. %s - %s (%s).flac", t.Number, t.Artist.Name, t.Title, *t.Version)
	} else {
		fileName = fmt.Sprintf("%d. %s - %s.flac", t.Number, t.Artist.Name, t.Title)
	}
	return path.Join(albumTrackDir(t.Album.ID, t.VolumeNumber), fileName)
}

func (t *AlbumTrack) cover() string {
	return t.Album.Cover
}

func (t *AlbumTrack) info() TrackInfo {
	var title string
	if nil != t.Version {
		title = fmt.Sprintf("%s (%s)", t.Title, *t.Version)
	} else {
		title = t.Title
	}
	return TrackInfo{
		Duration:   t.Duration,
		Title:      title,
		ArtistName: t.Artist.Name,
		Version:    t.Version,
	}
}

func (d *Downloader) albumTracksPage(ctx context.Context, id string, page int) (tracks []AlbumTrack, remaining int, err error) {
	albumURL, err := url.JoinPath(fmt.Sprintf(albumItemsAPIFormat, id))
	if nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return nil, 0, flaw.From(fmt.Errorf("failed to join track base URL with track id: %v", err)).Append(flawP)
	}
	flawP := flaw.P{"url": albumURL}

	respBytes, err := d.getPagedItems(ctx, albumURL, page)
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
					ID    int    `json:"id"`
					Cover string `json:"cover"`
				} `json:"album"`
				Version *string `json:"version"`
			} `json:"item"`
		} `json:"items"`
	}
	if err := json.Unmarshal(respBytes, &responseBody); nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, 0, flaw.From(fmt.Errorf("failed to decode album items page response: %v", err)).Append(flawP)
	}

	thisPageItemsCount := len(responseBody.Items)
	if thisPageItemsCount == 0 {
		return nil, 0, os.ErrNotExist
	}

	for _, v := range responseBody.Items {
		if v.Type != trackTypeResponseItem {
			continue
		}

		albumTrack := AlbumTrack{
			ID:       strconv.Itoa(v.Item.ID),
			Duration: v.Item.Duration,
			Title:    v.Item.Title,
			Artist: Artist{
				Name: v.Item.Artist.Name,
			},
			Album: Album{
				ID:    strconv.Itoa(v.Item.Album.ID),
				Year:  0,  // not provided by this API
				Title: "", // not provided by this API
				Cover: v.Item.Album.Cover,
			},
			Number:       v.Item.TrackNumber,
			VolumeNumber: v.Item.VolumeNumber,
			Version:      v.Item.Version,
		}
		tracks = append(tracks, albumTrack)
	}

	return tracks, responseBody.TotalNumberOfItems - (thisPageItemsCount + page*pageSize), nil
}

type AlbumVolumes = [][]AlbumTrack

func (d *Downloader) albumVolumeTracks(ctx context.Context, id string) (AlbumVolumes, error) {
	var (
		tracks              [][]AlbumTrack
		currentVolumeTracks []AlbumTrack
		currentVolume       = 1
	)

	loopFlawPs := []flaw.P{}
	flawP := flaw.P{"loop_flaws": loopFlawPs}

	for i := 0; ; i++ {
		loopFlaw := flaw.P{"page": i}
		loopFlawPs = append(loopFlawPs, loopFlaw)
		flawP["loop_flaws"] = loopFlawPs
		pageTracks, rem, err := d.albumTracksPage(ctx, id, i)
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
		loopFlaw["remaining"] = rem

		for _, track := range pageTracks {
			switch track.VolumeNumber {
			case currentVolume:
				currentVolumeTracks = append(currentVolumeTracks, track)
			case currentVolume + 1:
				tracks = append(tracks, currentVolumeTracks)
				currentVolumeTracks = []AlbumTrack{track}
				currentVolume++
			default:
				return nil, flaw.From(fmt.Errorf("unexpected volume number: %d", track.VolumeNumber)).Append(flawP)
			}
		}

		if rem == 0 {
			break
		}
	}

	tracks = append(tracks, currentVolumeTracks)

	return tracks, nil
}
