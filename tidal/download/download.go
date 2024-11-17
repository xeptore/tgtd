package download

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/xeptore/flaw/v8"
	"golang.org/x/sync/errgroup"

	"github.com/xeptore/tgtd/cache"
	"github.com/xeptore/tgtd/config"
	"github.com/xeptore/tgtd/errutil"
	"github.com/xeptore/tgtd/httputil"
	"github.com/xeptore/tgtd/iterutil"
	"github.com/xeptore/tgtd/must"
	"github.com/xeptore/tgtd/ptr"
	"github.com/xeptore/tgtd/ratelimit"
	"github.com/xeptore/tgtd/tidal"
	"github.com/xeptore/tgtd/tidal/auth"
	"github.com/xeptore/tgtd/tidal/fs"
	"github.com/xeptore/tgtd/tidal/mpd"
)

const (
	trackAPIFormat             = "https://api.tidal.com/v1/tracks/%s"
	trackCreditsAPIFormat      = "https://api.tidal.com/v1/tracks/%s/credits" //nolint:gosec
	albumAPIFormat             = "https://api.tidal.com/v1/albums/%s"
	playlistAPIFormat          = "https://api.tidal.com/v1/playlists/%s"
	mixInfoURL                 = "https://listen.tidal.com/v1/pages/mix"
	trackStreamAPIFormat       = "https://api.tidal.com/v1/tracks/%s/playbackinfo"
	albumItemsCreditsAPIFormat = "https://api.tidal.com/v1/albums/%s/items/credits" //nolint:gosec
	playlistItemsAPIFormat     = "https://api.tidal.com/v1/playlists/%s/items"
	mixItemsAPIFormat          = "https://api.tidal.com/v1/mixes/%s/items"
	coverURLFormat             = "https://resources.tidal.com/images/%s/1280x1280.jpg"
	pageSize                   = 100
	maxBatchParts              = 10
	singlePartChunkSize        = 1024 * 1024
)

var ErrTooManyRequests = errors.New("too many requests")

type Downloader struct {
	dir                   fs.DownloadDir
	accessToken           string
	albumsMetaCache       *cache.AlbumsMetaCache
	downloadedCoversCache *cache.DownloadedCoversCache
	trackCreditsCache     *cache.TrackCreditsCache
}

func NewDownloader(
	dir fs.DownloadDir,
	accessToken string,
	albumsMetaCache *cache.AlbumsMetaCache,
	downloadedCoversCache *cache.DownloadedCoversCache,
	trackCreditsCache *cache.TrackCreditsCache,
) *Downloader {
	return &Downloader{
		dir:                   dir,
		accessToken:           accessToken,
		albumsMetaCache:       albumsMetaCache,
		downloadedCoversCache: downloadedCoversCache,
		trackCreditsCache:     trackCreditsCache,
	}
}

func (d *Downloader) Single(ctx context.Context, id string) error {
	track, err := getSingleTrackMeta(ctx, d.accessToken, id)
	if nil != err {
		return err
	}

	coverBytes, err := d.getCover(ctx, track.CoverID)
	if nil != err {
		return err
	}

	trackCredits, err := d.getTrackCredits(ctx, id)
	if nil != err {
		return err
	}

	trackFs := d.dir.Single(id)

	if err := trackFs.CreateDir(); nil != err {
		return err
	}

	if err := trackFs.Cover.Write(coverBytes); nil != err {
		return err
	}

	album, err := d.getAlbumMeta(ctx, track.AlbumID)
	if nil != err {
		return err
	}

	format, err := downloadTrack(ctx, d.accessToken, id, trackFs.Path)
	if nil != err {
		return err
	}

	attrs := TrackEmbeddedAttrs{
		LeadArtist:   track.Artist,
		Album:        track.AlbumTitle,
		AlbumArtist:  album.Artist,
		Artists:      track.Artists,
		Copyright:    track.Copyright,
		CoverPath:    trackFs.Cover.Path,
		Format:       *format,
		ISRC:         track.ISRC,
		ReleaseDate:  album.ReleaseDate,
		Title:        track.Title,
		TrackNumber:  track.TrackNumber,
		Version:      track.Version,
		VolumeNumber: track.VolumeNumber,
		Credits:      *trackCredits,
	}
	if err := embedTrackAttributes(ctx, trackFs.Path, attrs); nil != err {
		return err
	}

	info := fs.StoredSingleTrack{
		Track: fs.Track{
			Artists:  track.Artists,
			Title:    track.Title,
			Duration: track.Duration,
			Version:  track.Version,
			Format:   *format,
			CoverID:  track.CoverID,
		},
		Album: fs.StoredSingleTrackAlbum{
			Title:       album.Title,
			ReleaseDate: album.ReleaseDate.Format(time.DateOnly),
		},
		Caption: fmt.Sprintf("%s (%s)", album.Title, album.ReleaseDate.Format(time.DateOnly)),
	}
	if err := trackFs.InfoFile.Write(info); nil != err {
		return err
	}

	return nil
}

func (d *Downloader) getTrackCredits(ctx context.Context, id string) (*tidal.TrackCredits, error) {
	cachedTrackCredits, err := d.trackCreditsCache.Fetch(
		id,
		cache.DefaultTrackCreditsTTL,
		func() (*tidal.TrackCredits, error) { return fetchTrackCredits(ctx, d.accessToken, id) },
	)
	if nil != err {
		return nil, err
	}
	return cachedTrackCredits.Value(), nil
}

func fetchTrackCredits(ctx context.Context, accessToken string, id string) (*tidal.TrackCredits, error) {
	reqURL, err := url.Parse(fmt.Sprintf(trackCreditsAPIFormat, id))
	if nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return nil, flaw.From(fmt.Errorf("failed to parse track credits URL: %v", err)).Append(flawP)
	}

	reqParams := make(url.Values, 2)
	reqParams.Add("countryCode", "US")
	reqParams.Add("includeContributors", "true")
	reqURL.RawQuery = reqParams.Encode()
	flawP := flaw.P{"encoded_query_params": reqURL.RawQuery}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL.String(), nil)
	if nil != err {
		if errutil.IsContext(ctx) {
			return nil, ctx.Err()
		}

		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, flaw.From(fmt.Errorf("failed to create get track credits request: %v", err)).Append(flawP)
	}
	req.Header.Add("Authorization", "Bearer "+accessToken)

	client := http.Client{Timeout: config.GetPageTracksRequestTimeout} //nolint:exhaustruct
	resp, err := client.Do(req)
	if nil != err {
		switch {
		case errutil.IsContext(ctx):
			return nil, ctx.Err()
		case errors.Is(err, context.DeadlineExceeded):
			return nil, context.DeadlineExceeded
		default:
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return nil, flaw.From(fmt.Errorf("failed to send get track credits request: %v", err)).Append(flawP)
		}
	}
	defer func() {
		if closeErr := resp.Body.Close(); nil != closeErr {
			flawP["err_debug_tree"] = errutil.Tree(closeErr).FlawP()
			closeErr = flaw.From(fmt.Errorf("failed to close get track credits response body: %v", closeErr)).Append(flawP)
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
	flawP["response"] = errutil.HTTPResponseFlawPayload(resp)

	switch code := resp.StatusCode; code {
	case http.StatusOK:
	case http.StatusUnauthorized:
		respBytes, err := httputil.ReadOptionalResponseBody(ctx, resp)
		if nil != err {
			return nil, err
		}
		flawP["response_body"] = string(respBytes)
		return nil, flaw.From(errors.New("received 401 response")).Append(flawP)
	case http.StatusTooManyRequests:
		return nil, ErrTooManyRequests
	case http.StatusForbidden:
		respBytes, err := httputil.ReadResponseBody(ctx, resp)
		if nil != err {
			return nil, err
		}
		if ok, err := errutil.IsTooManyErrorResponse(resp, respBytes); nil != err {
			flawP["response_body"] = string(respBytes)
			return nil, must.BeFlaw(err)
		} else if ok {
			return nil, ErrTooManyRequests
		}

		flawP["response_body"] = string(respBytes)
		return nil, flaw.From(errors.New("unexpected 403 response")).Append(flawP)
	default:
		respBytes, err := httputil.ReadOptionalResponseBody(ctx, resp)
		if nil != err {
			return nil, err
		}
		flawP["response_body"] = string(respBytes)
		return nil, flaw.From(fmt.Errorf("unexpected status code: %d", code)).Append(flawP)
	}

	respBytes, err := httputil.ReadResponseBody(ctx, resp)
	if nil != err {
		return nil, err
	}

	var respBody TrackCreditsResponse
	if err := json.Unmarshal(respBytes, &respBody); nil != err {
		flawP["response_body"] = string(respBytes)
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, flaw.From(fmt.Errorf("failed to decode track credits response: %v", err)).Append(flawP)
	}

	return ptr.Of(respBody.toTrackCredits()), nil
}

type TrackCreditsResponse []struct {
	Type         string `json:"type"`
	Contributors []struct {
		Name string `json:"name"`
		ID   string `json:"id"`
	} `json:"contributors"`
}

func (t TrackCreditsResponse) toTrackCredits() tidal.TrackCredits {
	var out tidal.TrackCredits
	for _, v := range t {
		switch v.Type {
		case "Producer":
			for _, v := range v.Contributors {
				out.Producers = append(out.Producers, v.Name)
			}
		case "Composer":
			for _, v := range v.Contributors {
				out.Composers = append(out.Composers, v.Name)
			}
		case "Lyricist":
			for _, v := range v.Contributors {
				out.Lyricists = append(out.Lyricists, v.Name)
			}
		case "Additional Producer":
			for _, v := range v.Contributors {
				out.AdditionalProducers = append(out.AdditionalProducers, v.Name)
			}
		}
	}
	return out
}

type TrackEmbeddedAttrs struct {
	LeadArtist   string
	Album        string
	AlbumArtist  string
	Artists      []tidal.TrackArtist
	Copyright    string
	CoverPath    string
	Format       tidal.TrackFormat
	ISRC         string
	ReleaseDate  time.Time
	Title        string
	TrackNumber  int
	Version      *string
	VolumeNumber int
	Credits      tidal.TrackCredits
}

func embedTrackAttributes(ctx context.Context, trackFilePath string, attrs TrackEmbeddedAttrs) error {
	ext := attrs.Format.InferTrackExt()
	trackFilePathWithExt := trackFilePath + "." + ext

	metaTags := []string{
		"LEAD_PERFORMER=" + attrs.LeadArtist,
		"TITLE=" + attrs.Title,
		"ALBUM=" + attrs.Album,
		"ALBUMARTIST=" + attrs.AlbumArtist,
		"COPYRIGHT=" + attrs.Copyright,
		"ISRC=" + attrs.ISRC,
		"TRACKNUMBER=" + strconv.Itoa(attrs.TrackNumber),
		"DISCNUMBER=" + strconv.Itoa(attrs.VolumeNumber),
		"DATE=" + attrs.ReleaseDate.Format(time.DateOnly),
		"YEAR=" + strconv.Itoa(attrs.ReleaseDate.Year()),
	}
	for _, artist := range attrs.Artists {
		metaTags = append(metaTags, "ARTIST="+artist.Name)
	}
	for _, composer := range attrs.Credits.Composers {
		metaTags = append(metaTags, "COMPOSER="+composer)
	}
	for _, lyricist := range attrs.Credits.Lyricists {
		metaTags = append(metaTags, "LYRICIST="+lyricist)
	}
	for _, producer := range attrs.Credits.Producers {
		metaTags = append(metaTags, "PRODUCER="+producer)
	}
	for _, additionalProducer := range attrs.Credits.AdditionalProducers {
		metaTags = append(metaTags, "COPRODUCER="+additionalProducer)
	}

	if nil != attrs.Version {
		metaTags = append(metaTags, "version="+*attrs.Version)
	}

	metaArgs := make([]string, 0, len(metaTags)*2)
	for _, tag := range metaTags {
		metaArgs = append(metaArgs, "-metadata", "+"+tag)
	}

	args := []string{
		"-i",
		trackFilePath,
		"-i",
		attrs.CoverPath,
		"-map",
		"0:a",
		"-map",
		"1",
		"-c",
		"copy",
		"-disposition:v",
		"attached_pic",
	}
	args = append(args, metaArgs...)
	args = append(args, trackFilePathWithExt)

	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	if err := cmd.Run(); nil != err {
		flawP := flaw.P{
			"err_debug_tree": errutil.Tree(err).FlawP(),
			"cmd":            cmd.String(),
		}
		return flaw.From(fmt.Errorf("failed to write track attributes: %v", err)).Append(flawP)
	}
	if err := os.Rename(trackFilePathWithExt, trackFilePath); nil != err {
		flawP := flaw.P{
			"err_debug_tree": errutil.Tree(err).FlawP(),
			"old_path":       trackFilePathWithExt,
			"new_path":       trackFilePath,
		}
		return flaw.From(fmt.Errorf("failed to rename track file: %v", err)).Append(flawP)
	}
	return nil
}

func getSingleTrackMeta(ctx context.Context, accessToken, id string) (*SingleTrackMeta, error) {
	trackURL := fmt.Sprintf(trackAPIFormat, id)
	flawP := flaw.P{"url": trackURL}

	reqURL, err := url.Parse(trackURL)
	if nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, flaw.From(fmt.Errorf("failed to parse track URL: %v", err)).Append(flawP)
	}

	reqParams := make(url.Values, 1)
	reqParams.Add("countryCode", "US")
	reqURL.RawQuery = reqParams.Encode()
	flawP["encoded_query_params"] = reqURL.RawQuery

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL.String(), nil)
	if nil != err {
		if errutil.IsContext(ctx) {
			return nil, ctx.Err()
		}

		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, flaw.From(fmt.Errorf("failed to create get track info request: %v", err)).Append(flawP)
	}
	req.Header.Add("Authorization", "Bearer "+accessToken)

	client := http.Client{Timeout: 5 * time.Second} //nolint:exhaustruct
	resp, err := client.Do(req)
	if nil != err {
		switch {
		case errutil.IsContext(ctx):
			return nil, ctx.Err()
		case errors.Is(err, context.DeadlineExceeded):
			return nil, context.DeadlineExceeded
		default:
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return nil, flaw.From(fmt.Errorf("failed to send get track info request: %v", err)).Append(flawP)
		}
	}
	defer func() {
		if closeErr := resp.Body.Close(); nil != closeErr {
			flawP["err_debug_tree"] = errutil.Tree(closeErr).FlawP()
			closeErr = flaw.From(fmt.Errorf("failed to close get track info response body: %v", closeErr)).Append(flawP)
			switch {
			case nil == err:
				err = closeErr
			case errutil.IsContext(ctx):
				err = flaw.From(errors.New("context was ended")).Join(closeErr)
			case errors.Is(err, context.DeadlineExceeded):
				err = flaw.From(errors.New("timeout has reached")).Join(closeErr)
			case errors.Is(err, auth.ErrUnauthorized):
				err = flaw.From(errors.New("unauthorized")).Join(closeErr)
			case errors.Is(err, ErrTooManyRequests):
				err = flaw.From(errors.New("too many requests")).Join(closeErr)
			case errutil.IsFlaw(err):
				err = must.BeFlaw(err).Join(closeErr)
			default:
				panic(errutil.UnknownError(err))
			}
		}
	}()
	flawP["response"] = errutil.HTTPResponseFlawPayload(resp)

	switch code := resp.StatusCode; code {
	case http.StatusOK:
	case http.StatusUnauthorized:
		respBytes, err := httputil.ReadResponseBody(ctx, resp)
		if nil != err {
			return nil, err
		}
		var respBody struct {
			Status      int    `json:"status"`
			SubStatus   int    `json:"subStatus"`
			UserMessage string `json:"userMessage"`
		}
		if err := json.Unmarshal(respBytes, &respBody); nil != err {
			flawP["response_body"] = string(respBytes)
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return nil, flaw.From(fmt.Errorf("failed to decode 401 unauthorized response body: %v", err)).Append(flawP)
		}
		if respBody.Status == 401 && respBody.SubStatus == 11002 && respBody.UserMessage == "Token could not be verified" {
			return nil, auth.ErrUnauthorized
		}

		flawP["response_body"] = string(respBytes)
		return nil, flaw.From(errors.New("received 401 response")).Append(flawP)
	case http.StatusTooManyRequests:
		return nil, ErrTooManyRequests
	case http.StatusForbidden:
		respBytes, err := httputil.ReadResponseBody(ctx, resp)
		if nil != err {
			return nil, err
		}
		if ok, err := errutil.IsTooManyErrorResponse(resp, respBytes); nil != err {
			flawP["response_body"] = string(respBytes)
			return nil, must.BeFlaw(err).Append(flawP)
		} else if ok {
			return nil, ErrTooManyRequests
		}

		flawP["response_body"] = string(respBytes)
		return nil, flaw.From(errors.New("unexpected 403 response")).Append(flawP)
	default:
		respBytes, err := httputil.ReadOptionalResponseBody(ctx, resp)
		if nil != err {
			return nil, err
		}
		flawP["response_body"] = string(respBytes)
		return nil, flaw.From(fmt.Errorf("unexpected status code: %d", code)).Append(flawP)
	}

	respBytes, err := httputil.ReadResponseBody(ctx, resp)
	if nil != err {
		return nil, err
	}
	var respBody struct {
		Duration     int    `json:"duration"`
		Title        string `json:"title"`
		TrackNumber  int    `json:"trackNumber"`
		VolumeNumber int    `json:"volumeNumber"`
		Copyright    string `json:"copyright"`
		ISRC         string `json:"isrc"`
		Artist       struct {
			Name string `json:"name"`
		} `json:"artist"`
		Artists []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"artists"`
		Album struct {
			ID      int    `json:"id"`
			CoverID string `json:"cover"`
			Title   string `json:"title"`
		} `json:"album"`
		Version *string `json:"version"`
	}
	if err := json.Unmarshal(respBytes, &respBody); nil != err {
		flawP["response_body"] = string(respBytes)
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, flaw.From(fmt.Errorf("failed to decode track info response body: %v", err)).Append(flawP)
	}

	artists := make([]tidal.TrackArtist, len(respBody.Artists))
	for i, artist := range respBody.Artists {
		switch artist.Type {
		case tidal.ArtistTypeMain, tidal.ArtistTypeFeatured:
		default:
			return nil, flaw.From(fmt.Errorf("unexpected artist type: %s", artist.Type)).Append(flawP)
		}
		artists[i] = tidal.TrackArtist{Name: artist.Name, Type: artist.Type}
	}

	track := SingleTrackMeta{
		Artist:       respBody.Artist.Name,
		AlbumID:      strconv.Itoa(respBody.Album.ID),
		AlbumTitle:   respBody.Album.Title,
		Artists:      artists,
		ISRC:         respBody.ISRC,
		Copyright:    respBody.Copyright,
		CoverID:      respBody.Album.CoverID,
		Duration:     respBody.Duration,
		Title:        respBody.Title,
		TrackNumber:  respBody.TrackNumber,
		Version:      respBody.Version,
		VolumeNumber: respBody.VolumeNumber,
	}
	return &track, nil
}

func (d *Downloader) getCover(ctx context.Context, coverID string) (b []byte, err error) {
	cachedCoverBytes, err := d.downloadedCoversCache.Fetch(
		coverID,
		cache.DefaultDownloadedCoverTTL,
		func() ([]byte, error) { return downloadCover(ctx, d.accessToken, coverID) },
	)
	if nil != err {
		return nil, err
	}
	return cachedCoverBytes.Value(), nil
}

func downloadCover(ctx context.Context, accessToken, coverID string) (b []byte, err error) {
	coverURL, err := url.JoinPath(fmt.Sprintf(coverURLFormat, strings.ReplaceAll(coverID, "-", "/")))
	if nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return nil, flaw.From(fmt.Errorf("failed to join cover base URL with cover filepath: %v", err)).Append(flawP)
	}
	flawP := flaw.P{"cover_url": coverURL}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, coverURL, nil)
	if nil != err {
		if errutil.IsContext(ctx) {
			return nil, ctx.Err()
		}

		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, flaw.From(fmt.Errorf("failed to create get cover request: %v", err)).Append(flawP)
	}
	req.Header.Add("Authorization", "Bearer "+accessToken)

	client := http.Client{Timeout: config.CoverDownloadTimeout} //nolint:exhaustruct
	resp, err := client.Do(req)
	if nil != err {
		switch {
		case errutil.IsContext(ctx):
			return nil, ctx.Err()
		case errors.Is(err, context.DeadlineExceeded):
			return nil, context.DeadlineExceeded
		default:
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return nil, flaw.From(fmt.Errorf("failed to send get track cover request: %v", err)).Append(flawP)
		}
	}
	defer func() {
		if closeErr := resp.Body.Close(); nil != closeErr {
			flawP["err_debug_tree"] = errutil.Tree(closeErr).FlawP()
			closeErr = flaw.From(fmt.Errorf("failed to close get track cover response body: %v", closeErr)).Append(flawP)
			switch {
			case nil == err:
				err = closeErr
			case errutil.IsContext(ctx):
				err = flaw.From(errors.New("context has ended")).Join(closeErr)
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
	flawP["response"] = errutil.HTTPResponseFlawPayload(resp)

	switch code := resp.StatusCode; code {
	case http.StatusOK:
	case http.StatusUnauthorized:
		respBytes, err := httputil.ReadOptionalResponseBody(ctx, resp)
		if nil != err {
			return nil, err
		}
		flawP["response_body"] = string(respBytes)
		return nil, flaw.From(errors.New("received 401 response")).Append(flawP)
	case http.StatusTooManyRequests:
		return nil, ErrTooManyRequests
	case http.StatusForbidden:
		respBytes, err := httputil.ReadResponseBody(ctx, resp)
		if nil != err {
			return nil, err
		}
		if ok, err := errutil.IsTooManyErrorResponse(resp, respBytes); nil != err {
			flawP["response_body"] = string(respBytes)
			return nil, must.BeFlaw(err).Append(flawP)
		} else if ok {
			return nil, ErrTooManyRequests
		}

		flawP["response_body"] = string(respBytes)
		return nil, flaw.From(errors.New("unexpected 403 response")).Append(flawP)
	default:
		respBytes, err := httputil.ReadOptionalResponseBody(ctx, resp)
		if nil != err {
			return nil, err
		}
		flawP["response_body"] = string(respBytes)
		return nil, flaw.From(fmt.Errorf("unexpected status code received from get track cover: %d", code)).Append(flawP)
	}

	respBytes, err := httputil.ReadResponseBody(ctx, resp)
	if nil != err {
		return nil, err
	}
	return respBytes, nil
}

func (d *Downloader) getAlbumMeta(ctx context.Context, id string) (*tidal.AlbumMeta, error) {
	cachedAlbumMeta, err := d.albumsMetaCache.Fetch(
		id,
		cache.DefaultAlbumTTL,
		func() (*tidal.AlbumMeta, error) { return fetchAlbumMeta(ctx, d.accessToken, id) },
	)
	if nil != err {
		return nil, err
	}
	return cachedAlbumMeta.Value(), nil
}

func fetchAlbumMeta(ctx context.Context, accessToken, id string) (*tidal.AlbumMeta, error) {
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
	req.Header.Add("Authorization", "Bearer "+accessToken)

	client := http.Client{Timeout: config.AlbumInfoRequestTimeout} //nolint:exhaustruct
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
	flawP["response"] = errutil.HTTPResponseFlawPayload(resp)

	switch code := resp.StatusCode; code {
	case http.StatusOK:
	case http.StatusTooManyRequests:
		return nil, ErrTooManyRequests
	case http.StatusForbidden:
		respBytes, err := httputil.ReadResponseBody(ctx, resp)
		if nil != err {
			return nil, err
		}
		if ok, err := errutil.IsTooManyErrorResponse(resp, respBytes); nil != err {
			flawP["response_body"] = string(respBytes)
			return nil, must.BeFlaw(err)
		} else if ok {
			return nil, ErrTooManyRequests
		}

		flawP["response_body"] = string(respBytes)
		return nil, flaw.From(errors.New("unexpected 403 response")).Append(flawP)
	default:
		respBytes, err := httputil.ReadOptionalResponseBody(ctx, resp)
		if nil != err {
			return nil, err
		}
		flawP["response_body"] = string(respBytes)
		return nil, flaw.From(fmt.Errorf("unexpected status code: %d", code)).Append(flawP)
	}

	respBytes, err := httputil.ReadResponseBody(ctx, resp)
	if nil != err {
		return nil, err
	}
	var respBody struct {
		Artist struct {
			Name string `json:"name"`
		} `json:"artist"`
		Title       string `json:"title"`
		ReleaseDate string `json:"releaseDate"`
		CoverID     string `json:"cover"`
	}
	if err := json.Unmarshal(respBytes, &respBody); nil != err {
		flawP["response_body"] = string(respBytes)
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, flaw.From(fmt.Errorf("failed to decode album info response: %v", err)).Append(flawP)
	}

	releaseDate, err := time.Parse("2006-01-02", respBody.ReleaseDate)
	if nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, flaw.From(fmt.Errorf("failed to parse album release date: %v", err)).Append(flawP)
	}

	return &tidal.AlbumMeta{
		Artist:      respBody.Artist.Name,
		Title:       respBody.Title,
		ReleaseDate: releaseDate,
		CoverID:     respBody.CoverID,
	}, nil
}

func downloadTrack(ctx context.Context, accessToken, id string, fileName string) (*tidal.TrackFormat, error) {
	flawP := make(flaw.P)
	stream, format, err := getStream(ctx, accessToken, id)
	if nil != err {
		return nil, err
	}

	waitTime := ratelimit.TrackDownloadSleepMS()
	flawP["wait_time"] = waitTime
	time.Sleep(waitTime)

	if err := stream.saveTo(ctx, accessToken, fileName); nil != err {
		switch {
		case errutil.IsContext(ctx):
			return nil, ctx.Err()
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

	return format, nil
}

type Stream interface {
	saveTo(ctx context.Context, accessToken string, fileName string) error
}

func getStream(ctx context.Context, accessToken, id string) (s Stream, f *tidal.TrackFormat, err error) {
	trackURL := fmt.Sprintf(trackStreamAPIFormat, id)
	flawP := flaw.P{"url": trackURL}

	reqURL, err := url.Parse(trackURL)
	if nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, nil, flaw.From(fmt.Errorf("failed to parse track URL to build track stream URLs: %v", err)).Append(flawP)
	}

	params := make(url.Values, 6)
	params.Add("countryCode", "US")
	params.Add("audioquality", "HI_RES_LOSSLESS")
	params.Add("playbackmode", "STREAM")
	params.Add("assetpresentation", "FULL")
	params.Add("immersiveaudio", "false")
	params.Add("locale", "en")

	reqURL.RawQuery = params.Encode()
	flawP["encoded_query_params"] = reqURL.RawQuery
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL.String(), nil)
	if nil != err {
		if errutil.IsContext(ctx) {
			return nil, nil, ctx.Err()
		}

		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, nil, flaw.From(fmt.Errorf("failed to create get track stream URLs request: %v", err)).Append(flawP)
	}
	req.Header.Add("Authorization", "Bearer "+accessToken)

	client := http.Client{Timeout: config.GetStreamURLsRequestTimeout} //nolint:exhaustruct
	resp, err := client.Do(req)
	if nil != err {
		switch {
		case errutil.IsContext(ctx):
			return nil, nil, ctx.Err()
		case errors.Is(err, context.DeadlineExceeded):
			return nil, nil, context.DeadlineExceeded
		case errors.Is(err, ErrTooManyRequests):
			return nil, nil, ErrTooManyRequests
		default:
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return nil, nil, flaw.From(fmt.Errorf("failed to send get track stream URLs request: %v", err)).Append(flawP)
		}
	}
	defer func() {
		if closeErr := resp.Body.Close(); nil != closeErr {
			flawP["err_debug_tree"] = errutil.Tree(closeErr).FlawP()
			closeErr = flaw.From(fmt.Errorf("failed to close get track stream URLs response body: %v", closeErr)).Append(flawP)
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
	flawP["response"] = errutil.HTTPResponseFlawPayload(resp)

	switch code := resp.StatusCode; code {
	case http.StatusOK:
	case http.StatusUnauthorized:
		respBytes, err := httputil.ReadOptionalResponseBody(ctx, resp)
		if nil != err {
			return nil, nil, err
		}
		flawP["response_body"] = string(respBytes)
		return nil, nil, flaw.From(errors.New("received 401 response")).Append(flawP)
	case http.StatusTooManyRequests:
		return nil, nil, ErrTooManyRequests
	case http.StatusForbidden:
		respBytes, err := httputil.ReadResponseBody(ctx, resp)
		if nil != err {
			return nil, nil, err
		}
		if ok, err := errutil.IsTooManyErrorResponse(resp, respBytes); nil != err {
			flawP["response_body"] = string(respBytes)
			return nil, nil, must.BeFlaw(err).Append(flawP)
		} else if ok {
			return nil, nil, ErrTooManyRequests
		}

		flawP["response_body"] = string(respBytes)
		return nil, nil, flaw.From(errors.New("unexpected 403 response")).Append(flawP)
	default:
		respBytes, err := httputil.ReadOptionalResponseBody(ctx, resp)
		if nil != err {
			return nil, nil, err
		}
		flawP["response_body"] = string(respBytes)
		return nil, nil, flaw.From(fmt.Errorf("unexpected status code received from get track stream URLs: %d", code)).Append(flawP)
	}

	respBytes, err := httputil.ReadResponseBody(ctx, resp)
	if nil != err {
		return nil, nil, err
	}
	var respBody struct {
		ManifestMimeType string `json:"manifestMimeType"`
		Manifest         string `json:"manifest"`
	}
	if err := json.Unmarshal(respBytes, &respBody); nil != err {
		flawP["response_body"] = string(respBytes)
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, nil, flaw.From(fmt.Errorf("failed to decode track stream response body: %v", err)).Append(flawP)
	}
	flawP["stream"] = flaw.P{"manifest_mime_type": respBody.ManifestMimeType}

	switch mimeType := respBody.ManifestMimeType; mimeType {
	case "application/dash+xml", "dash+xml":
		dec := base64.NewDecoder(base64.StdEncoding, strings.NewReader(respBody.Manifest))
		info, err := mpd.ParseStreamInfo(dec)
		if nil != err {
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return nil, nil, flaw.From(fmt.Errorf("failed to parse stream info: %v", err)).Append(flawP)
		}
		flawP["stream_info"] = flaw.P{"info": info.FlawP()}

		if _, err := tidal.InferTrackExt(info.MimeType, info.Codec); nil != err {
			return nil, nil, flaw.From(err).Append(flawP)
		}
		format := tidal.TrackFormat{MimeType: info.MimeType, Codec: info.Codec}

		return &DashTrackStream{Info: *info}, &format, nil
	case "application/vnd.tidal.bts", "vnd.tidal.bt":
		var manifest VNDManifest
		dec := base64.NewDecoder(base64.StdEncoding, strings.NewReader(respBody.Manifest))
		if err := json.NewDecoder(dec).Decode(&manifest); nil != err {
			flawP["manifest"] = manifest.FlawP()
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return nil, nil, flaw.From(fmt.Errorf("failed to decode vnd.tidal.bt manifest: %v", err)).Append(flawP)
		}
		flawP["manifest"] = flaw.P{
			"mime_type":       manifest.MimeType,
			"key_id":          manifest.KeyID,
			"encryption_type": manifest.EncryptionType,
			"urls":            manifest.URLs,
		}

		switch manifest.EncryptionType {
		case "NONE":
		default:
			return nil, nil, flaw.
				From(fmt.Errorf("encrypted vnd.tidal.bt manifest is not yet implemented: %s", manifest.EncryptionType)).
				Append(flawP)
		}

		if _, err := tidal.InferTrackExt(manifest.MimeType, manifest.Codec); nil != err {
			return nil, nil, flaw.From(err).Append(flawP)
		}
		format := &tidal.TrackFormat{MimeType: manifest.MimeType, Codec: manifest.Codec}

		if len(manifest.URLs) == 0 {
			return nil, nil, flaw.From(errors.New("empty vnd.tidal.bt manifest URLs")).Append(flawP)
		}
		return &VndTrackStream{URL: manifest.URLs[0]}, format, nil
	default:
		return nil, nil, flaw.From(fmt.Errorf("unexpected manifest mime type: %s", mimeType)).Append(flawP)
	}
}

type SingleTrackMeta struct {
	Artist       string
	AlbumID      string
	AlbumTitle   string
	Artists      []tidal.TrackArtist
	ISRC         string
	Copyright    string
	CoverID      string
	Duration     int
	Title        string
	TrackNumber  int
	Version      *string
	VolumeNumber int
}

type AlbumTrackMeta struct {
	Artist       string
	Artists      []tidal.TrackArtist
	Duration     int
	ID           string
	Title        string
	Copyright    string
	ISRC         string
	TrackNumber  int
	Version      *string
	VolumeNumber int
	Credits      tidal.TrackCredits
}

type ListTrackMeta struct {
	AlbumID      string
	AlbumTitle   string
	ISRC         string
	Copyright    string
	Artist       string
	Artists      []tidal.TrackArtist
	CoverID      string
	Duration     int
	ID           string
	Title        string
	TrackNumber  int
	Version      *string
	VolumeNumber int
}

func (d *Downloader) Playlist(ctx context.Context, id string) error {
	playlist, err := getPlaylistMeta(ctx, d.accessToken, id)
	if nil != err {
		return err
	}

	tracks, err := getPlaylistTracks(ctx, d.accessToken, id)
	if nil != err {
		return err
	}

	playlistFs := d.dir.Playlist(id)
	if err := playlistFs.CreateDir(); nil != err {
		return err
	}

	var (
		wg, wgCtx = errgroup.WithContext(ctx)
		formats   = make(map[int]tidal.TrackFormat, len(tracks))
	)

	wg.SetLimit(ratelimit.PlaylistDownloadConcurrency)
	for i, track := range tracks {
		wg.Go(func() error {
			coverBytes, err := d.getCover(wgCtx, track.CoverID)
			if nil != err {
				return err
			}

			trackCredits, err := d.getTrackCredits(ctx, track.ID)
			if nil != err {
				return err
			}

			trackFs := playlistFs.Track(track.ID)
			if err := trackFs.Cover.Write(coverBytes); nil != err {
				return err
			}

			format, err := downloadTrack(wgCtx, d.accessToken, track.ID, trackFs.Path)
			if nil != err {
				return err
			}

			album, err := d.getAlbumMeta(ctx, track.AlbumID)
			if nil != err {
				return err
			}

			attrs := TrackEmbeddedAttrs{
				LeadArtist:   track.Artist,
				Album:        track.AlbumTitle,
				AlbumArtist:  album.Artist,
				Artists:      track.Artists,
				Copyright:    track.Copyright,
				CoverPath:    trackFs.Cover.Path,
				Format:       *format,
				ISRC:         track.ISRC,
				ReleaseDate:  album.ReleaseDate,
				Title:        track.Title,
				TrackNumber:  track.TrackNumber,
				Version:      track.Version,
				VolumeNumber: track.VolumeNumber,
				Credits:      *trackCredits,
			}
			if err := embedTrackAttributes(ctx, trackFs.Path, attrs); nil != err {
				return err
			}

			formats[i] = *format
			return nil
		})
	}

	if err := wg.Wait(); nil != err {
		return err
	}

	info := fs.StoredPlaylist{
		Caption: fmt.Sprintf("%s (%d - %d)", playlist.Title, playlist.StartYear, playlist.EndYear),
		Tracks: iterutil.Map(tracks, func(i int, v ListTrackMeta) fs.StoredPlaylistTrack {
			return fs.StoredPlaylistTrack{
				Track: fs.Track{
					Artists:  v.Artists,
					Title:    v.Title,
					Duration: v.Duration,
					Version:  v.Version,
					Format:   formats[i],
					CoverID:  v.CoverID,
				},
				ID: v.ID,
			}
		}),
	}
	if err := playlistFs.InfoFile.Write(info); nil != err {
		return err
	}

	return nil
}

func getPlaylistMeta(ctx context.Context, accessToken, id string) (*PlaylistMeta, error) {
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

	queryParams := make(url.Values, 1)
	queryParams.Add("countryCode", "US")
	reqURL.RawQuery = queryParams.Encode()
	flawP["encoded_query_params"] = reqURL.RawQuery

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL.String(), nil)
	if nil != err {
		if errutil.IsContext(ctx) {
			return nil, ctx.Err()
		}

		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, flaw.From(fmt.Errorf("failed to create get playlist info request: %v", err)).Append(flawP)
	}
	req.Header.Add("Authorization", "Bearer "+accessToken)

	client := http.Client{Timeout: config.PlaylistInfoRequestTimeout} //nolint:exhaustruct
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
	flawP["response"] = errutil.HTTPResponseFlawPayload(resp)

	switch code := resp.StatusCode; code {
	case http.StatusOK:
	case http.StatusTooManyRequests:
		return nil, ErrTooManyRequests
	case http.StatusForbidden:
		respBytes, err := httputil.ReadResponseBody(ctx, resp)
		if nil != err {
			return nil, err
		}
		if ok, err := errutil.IsTooManyErrorResponse(resp, respBytes); nil != err {
			flawP["response_body"] = string(respBytes)
			return nil, must.BeFlaw(err).Append(flawP)
		} else if ok {
			return nil, ErrTooManyRequests
		}

		flawP["response_body"] = string(respBytes)
		return nil, flaw.From(errors.New("received 403 response")).Append(flawP)
	default:
		respBytes, err := httputil.ReadOptionalResponseBody(ctx, resp)
		if nil != err {
			return nil, err
		}
		flawP["response_body"] = string(respBytes)
		return nil, flaw.From(fmt.Errorf("unexpected status code: %d", code)).Append(flawP)
	}

	respBytes, err := httputil.ReadResponseBody(ctx, resp)
	if nil != err {
		return nil, err
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

	return &PlaylistMeta{
		Title:     respBody.Title,
		StartYear: createdAt.Year(),
		EndYear:   lastUpdatedAt.Year(),
	}, nil
}

type PlaylistMeta struct {
	Title     string
	StartYear int
	EndYear   int
}

func getPlaylistTracks(ctx context.Context, accessToken, id string) ([]ListTrackMeta, error) {
	var tracks []ListTrackMeta
	var loopFlawPs []flaw.P
	flawP := flaw.P{"loop_flaw_payloads": loopFlawPs}
	for i := 0; ; i++ {
		loopFlawP := flaw.P{"page": i}
		loopFlawPs = append(loopFlawPs, loopFlawP)
		flawP["loop_flaw_payloads"] = loopFlawPs

		pageTracks, rem, err := playlistTracksPage(ctx, accessToken, id, i)
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

const pageItemTypeTrack = "track"

func playlistTracksPage(ctx context.Context, accessToken, id string, page int) (ts []ListTrackMeta, rem int, err error) {
	playlistURL, err := url.JoinPath(fmt.Sprintf(playlistItemsAPIFormat, id))
	if nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return nil, 0, flaw.From(fmt.Errorf("failed to create playlist URL: %v", err)).Append(flawP)
	}
	flawP := flaw.P{"url": playlistURL}

	respBytes, err := getListPagedItems(ctx, accessToken, playlistURL, page)
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

	var respBody struct {
		TotalNumberOfItems int `json:"totalNumberOfItems"`
		Items              []struct {
			Type string `json:"type"`
			Cut  any    `json:"any"`
			Item struct {
				ID           int    `json:"id"`
				StreamReady  bool   `json:"streamReady"`
				TrackNumber  int    `json:"trackNumber"`
				VolumeNumber int    `json:"volumeNumber"`
				Title        string `json:"title"`
				ISRC         string `json:"isrc"`
				Copyright    string `json:"copyright"`
				Duration     int    `json:"duration"`
				Artist       struct {
					Name string `json:"name"`
				} `json:"artist"`
				Artists []struct {
					Name string `json:"name"`
					Type string `json:"type"`
				} `json:"artists"`
				Album struct {
					ID      int    `json:"id"`
					CoverID string `json:"cover"`
					Title   string `json:"title"`
				} `json:"album"`
				Version *string `json:"version"`
			} `json:"item"`
		} `json:"items"`
	}
	if err := json.Unmarshal(respBytes, &respBody); nil != err {
		flawP["response_body"] = string(respBytes)
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, 0, flaw.From(fmt.Errorf("failed to decode playlist response: %v", err)).Append(flawP)
	}

	thisPageItemsCount := len(respBody.Items)
	if thisPageItemsCount == 0 {
		return nil, 0, os.ErrNotExist
	}

	for _, v := range respBody.Items {
		if v.Type != pageItemTypeTrack || !v.Item.StreamReady {
			continue
		}
		if v.Cut != nil {
			return nil, 0, flaw.From(errors.New("cut items are not supported")).Append(flawP)
		}

		artists := make([]tidal.TrackArtist, len(v.Item.Artists))
		for i, a := range v.Item.Artists {
			switch a.Type {
			case tidal.ArtistTypeMain, tidal.ArtistTypeFeatured:
			default:
				return nil, 0, flaw.From(fmt.Errorf("unexpected artist type: %s", a.Type)).Append(flawP)
			}
			artists[i] = tidal.TrackArtist{Name: a.Name, Type: a.Type}
		}

		t := ListTrackMeta{
			AlbumID:      strconv.Itoa(v.Item.Album.ID),
			AlbumTitle:   v.Item.Album.Title,
			ISRC:         v.Item.ISRC,
			Copyright:    v.Item.Copyright,
			Artist:       v.Item.Artist.Name,
			Artists:      artists,
			CoverID:      v.Item.Album.CoverID,
			Duration:     v.Item.Duration,
			ID:           strconv.Itoa(v.Item.ID),
			Title:        v.Item.Title,
			TrackNumber:  v.Item.TrackNumber,
			Version:      v.Item.Version,
			VolumeNumber: v.Item.VolumeNumber,
		}
		ts = append(ts, t)
	}

	return ts, respBody.TotalNumberOfItems - (thisPageItemsCount + page*pageSize), nil
}

func (d *Downloader) Mix(ctx context.Context, id string) error {
	mix, err := getMixMeta(ctx, d.accessToken, id)
	if nil != err {
		return err
	}

	tracks, err := getMixTracks(ctx, d.accessToken, id)
	if nil != err {
		return err
	}

	mixFs := d.dir.Mix(id)
	if err := mixFs.CreateDir(); nil != err {
		return err
	}

	var (
		wg, wgCtx = errgroup.WithContext(ctx)
		formats   = make(map[int]tidal.TrackFormat, len(tracks))
	)
	wg.SetLimit(ratelimit.MixDownloadConcurrency)
	for i, track := range tracks {
		wg.Go(func() error {
			coverBytes, err := d.getCover(wgCtx, track.CoverID)
			if nil != err {
				return err
			}

			trackCredits, err := d.getTrackCredits(ctx, track.ID)
			if nil != err {
				return err
			}

			trackFs := mixFs.Track(track.ID)
			if err := trackFs.Cover.Write(coverBytes); nil != err {
				return err
			}

			format, err := downloadTrack(wgCtx, d.accessToken, track.ID, trackFs.Path)
			if nil != err {
				return err
			}

			album, err := d.getAlbumMeta(ctx, track.AlbumID)
			if nil != err {
				return err
			}

			attrs := TrackEmbeddedAttrs{
				LeadArtist:   track.Artist,
				Album:        track.AlbumTitle,
				AlbumArtist:  album.Artist,
				Artists:      track.Artists,
				Copyright:    track.Copyright,
				CoverPath:    trackFs.Cover.Path,
				Format:       *format,
				ISRC:         track.ISRC,
				ReleaseDate:  album.ReleaseDate,
				Title:        track.Title,
				TrackNumber:  track.TrackNumber,
				Version:      track.Version,
				VolumeNumber: track.VolumeNumber,
				Credits:      *trackCredits,
			}
			if err := embedTrackAttributes(ctx, trackFs.Path, attrs); nil != err {
				return err
			}

			formats[i] = *format
			return nil
		})
	}

	if err := wg.Wait(); nil != err {
		return err
	}

	info := fs.StoredMix{
		Caption: mix.Title,
		Tracks: iterutil.Map(tracks, func(i int, v ListTrackMeta) fs.StoredMixTrack {
			return fs.StoredMixTrack{
				Track: fs.Track{
					Artists:  v.Artists,
					Title:    v.Title,
					Duration: v.Duration,
					Version:  v.Version,
					Format:   formats[i],
					CoverID:  v.CoverID,
				},
				ID: v.ID,
			}
		}),
	}
	if err := mixFs.InfoFile.Write(info); nil != err {
		return err
	}

	return nil
}

func getMixMeta(ctx context.Context, accessToken, id string) (*MixMeta, error) {
	flawP := flaw.P{}
	reqURL, err := url.Parse(mixInfoURL)
	if nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, flaw.From(fmt.Errorf("failed to parse playlist URL: %v", err)).Append(flawP)
	}

	reqParams := make(url.Values, 4)
	reqParams.Add("mixId", id)
	reqParams.Add("countryCode", "US")
	reqParams.Add("locale", "en_US")
	reqParams.Add("deviceType", "BROWSER")
	reqURL.RawQuery = reqParams.Encode()
	flawP["encoded_query_params"] = reqURL.RawQuery

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL.String(), nil)
	if nil != err {
		if errutil.IsContext(ctx) {
			return nil, ctx.Err()
		}

		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, flaw.From(fmt.Errorf("failed to create get mix info request: %v", err)).Append(flawP)
	}
	req.Header.Add("Authorization", "Bearer "+accessToken)
	req.Header.Add("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:132.0) Gecko/20100101 Firefox/132.0")
	req.Header.Add("Accept", "application/json")

	client := http.Client{Timeout: config.MixInfoRequestTimeout} //nolint:exhaustruct
	resp, err := client.Do(req)
	if nil != err {
		switch {
		case errutil.IsContext(ctx):
			return nil, ctx.Err()
		case errors.Is(err, context.DeadlineExceeded):
			return nil, context.DeadlineExceeded
		default:
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return nil, flaw.From(fmt.Errorf("failed to send get mix info request: %v", err)).Append(flawP)
		}
	}
	defer func() {
		if closeErr := resp.Body.Close(); nil != closeErr {
			flawP["err_debug_tree"] = errutil.Tree(closeErr).FlawP()
			closeErr = flaw.From(fmt.Errorf("failed to close get mix info response body: %v", closeErr)).Append(flawP)
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
	flawP["response"] = errutil.HTTPResponseFlawPayload(resp)

	switch code := resp.StatusCode; code {
	case http.StatusOK:
	case http.StatusTooManyRequests:
		return nil, ErrTooManyRequests
	case http.StatusForbidden:
		respBytes, err := httputil.ReadResponseBody(ctx, resp)
		if nil != err {
			return nil, err
		}
		if ok, err := errutil.IsTooManyErrorResponse(resp, respBytes); nil != err {
			flawP["response_body"] = string(respBytes)
			return nil, must.BeFlaw(err).Append(flawP)
		} else if ok {
			return nil, ErrTooManyRequests
		}

		flawP["response_body"] = string(respBytes)
		return nil, flaw.From(errors.New("unexpected 403 response")).Append(flawP)
	default:
		respBytes, err := httputil.ReadOptionalResponseBody(ctx, resp)
		if nil != err {
			return nil, err
		}
		flawP["response_body"] = string(respBytes)
		return nil, flaw.From(fmt.Errorf("unexpected status code: %d", code)).Append(flawP)
	}

	respBytes, err := httputil.ReadResponseBody(ctx, resp)
	if nil != err {
		return nil, err
	}
	if !gjson.ValidBytes(respBytes) {
		flawP["response_body"] = string(respBytes)
		return nil, flaw.From(fmt.Errorf("invalid mix info response json: %v", err)).Append(flawP)
	}

	var title string
	switch titleKey := gjson.GetBytes(respBytes, "title"); titleKey.Type { //nolint:exhaustive
	case gjson.String:
		title = titleKey.Str
	default:
		flawP["response_body"] = string(respBytes)
		return nil, flaw.From(fmt.Errorf("unexpected mix info response: %v", err)).Append(flawP)
	}

	return &MixMeta{Title: title}, nil
}

type MixMeta struct {
	Title string
}

func getMixTracks(ctx context.Context, accessToken, id string) ([]ListTrackMeta, error) {
	var (
		tracks     []ListTrackMeta
		loopFlawPs []flaw.P
		flawP      = flaw.P{"loop_flaw_payloads": loopFlawPs}
	)

	for i := 0; ; i++ {
		loopFlawP := flaw.P{"page": i}
		loopFlawPs = append(loopFlawPs, loopFlawP)

		pageTracks, rem, err := mixTracksPage(ctx, accessToken, id, i)
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
		tracks = append(tracks, pageTracks...)

		if rem == 0 {
			break
		}
	}

	return tracks, nil
}

func mixTracksPage(ctx context.Context, accessToken, id string, page int) (ts []ListTrackMeta, rem int, err error) {
	mixURL, err := url.JoinPath(fmt.Sprintf(mixItemsAPIFormat, id))
	if nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return nil, 0, flaw.From(fmt.Errorf("failed to create mix URL: %v", err)).Append(flawP)
	}
	flawP := flaw.P{"mix_url": mixURL}

	respBytes, err := getListPagedItems(ctx, accessToken, mixURL, page)
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

	var respBody struct {
		TotalNumberOfItems int `json:"totalNumberOfItems"`
		Items              []struct {
			Type string `json:"type"`
			Item struct {
				ID           int    `json:"id"`
				StreamReady  bool   `json:"streamReady"`
				TrackNumber  int    `json:"trackNumber"`
				VolumeNumber int    `json:"volumeNumber"`
				Title        string `json:"title"`
				Copyright    string `json:"copyright"`
				ISRC         string `json:"isrc"`
				Duration     int    `json:"duration"`
				Artist       struct {
					Name string `json:"name"`
				} `json:"artist"`
				Artists []struct {
					Name string `json:"name"`
					Type string `json:"type"`
				} `json:"artists"`
				Album struct {
					ID    int    `json:"id"`
					Cover string `json:"cover"`
					Title string `json:"title"`
				} `json:"album"`
				Version *string `json:"version"`
			} `json:"item"`
		} `json:"items"`
	}
	if err := json.Unmarshal(respBytes, &respBody); nil != err {
		flawP["response_body"] = string(respBytes)
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, 0, flaw.From(fmt.Errorf("failed to decode mix response: %v", err)).Append(flawP)
	}

	thisPageItemsCount := len(respBody.Items)
	if thisPageItemsCount == 0 {
		return nil, 0, nil
	}

	for _, v := range respBody.Items {
		if v.Type != pageItemTypeTrack || !v.Item.StreamReady {
			continue
		}

		artists := make([]tidal.TrackArtist, len(v.Item.Artists))
		for i, a := range v.Item.Artists {
			switch a.Type {
			case tidal.ArtistTypeMain, tidal.ArtistTypeFeatured:
			default:
				return nil, 0, flaw.From(fmt.Errorf("unexpected artist type: %s", a.Type)).Append(flawP)
			}
			artists[i] = tidal.TrackArtist{Name: a.Name, Type: a.Type}
		}

		t := ListTrackMeta{
			AlbumID:      strconv.Itoa(v.Item.Album.ID),
			AlbumTitle:   v.Item.Album.Title,
			ISRC:         v.Item.ISRC,
			Copyright:    v.Item.Copyright,
			Artist:       v.Item.Artist.Name,
			Artists:      artists,
			CoverID:      v.Item.Album.Cover,
			Duration:     v.Item.Duration,
			ID:           strconv.Itoa(v.Item.ID),
			Title:        v.Item.Title,
			TrackNumber:  v.Item.TrackNumber,
			Version:      v.Item.Version,
			VolumeNumber: v.Item.VolumeNumber,
		}
		ts = append(ts, t)
	}

	return ts, respBody.TotalNumberOfItems - (thisPageItemsCount + page*pageSize), nil
}

func (d *Downloader) Album(ctx context.Context, id string) error {
	album, err := d.getAlbumMeta(ctx, id)
	if nil != err {
		return err
	}

	coverBytes, err := d.getCover(ctx, album.CoverID)
	if nil != err {
		return err
	}

	albumFs := d.dir.Album(id)
	if err := albumFs.CreateDir(); nil != err {
		return err
	}

	if err := albumFs.Cover.Write(coverBytes); nil != err {
		return err
	}

	volumes, err := getAlbumVolumes(ctx, d.accessToken, id)
	if nil != err {
		return err
	}

	for _, volTracks := range volumes {
		for _, track := range volTracks {
			d.trackCreditsCache.Set(track.ID, &track.Credits, cache.DefaultTrackCreditsTTL)
		}
	}

	if err := albumFs.CreateVolDirs(len(volumes)); nil != err {
		return err
	}

	wg, wgCtx := errgroup.WithContext(ctx)
	wg.SetLimit(ratelimit.AlbumDownloadConcurrency)
	albumVolumes := make([][]fs.StoredAlbumVolumeTrack, len(volumes))
	for i, tracks := range volumes {
		volNum := i + 1
		volumeTracks := make([]fs.StoredAlbumVolumeTrack, len(tracks))
		for j, track := range tracks {
			wg.Go(func() error {
				trackFs := albumFs.Track(volNum, track.ID)
				format, err := downloadTrack(wgCtx, d.accessToken, track.ID, trackFs.Path)
				if nil != err {
					return err
				}

				attrs := TrackEmbeddedAttrs{
					LeadArtist:   track.Artist,
					Album:        album.Title,
					AlbumArtist:  album.Artist,
					Artists:      track.Artists,
					Copyright:    track.Copyright,
					CoverPath:    albumFs.Cover.Path,
					Format:       *format,
					ISRC:         track.ISRC,
					ReleaseDate:  album.ReleaseDate,
					Title:        track.Title,
					TrackNumber:  track.TrackNumber,
					Version:      track.Version,
					VolumeNumber: track.VolumeNumber,
					Credits:      track.Credits,
				}
				if err := embedTrackAttributes(ctx, trackFs.Path, attrs); nil != err {
					return err
				}

				volumeTracks[j] = fs.StoredAlbumVolumeTrack{
					Track: fs.Track{
						Artists:  track.Artists,
						Title:    track.Title,
						Duration: track.Duration,
						Version:  track.Version,
						Format:   *format,
						CoverID:  id,
					},
					ID: track.ID,
				}
				return nil
			})
		}
		albumVolumes[i] = volumeTracks
	}

	if err := wg.Wait(); nil != err {
		return err
	}

	info := fs.StoredAlbum{
		Caption: fmt.Sprintf("%s (%s)", album.Title, album.ReleaseDate.Format(time.DateOnly)),
		Volumes: albumVolumes,
	}
	if err := albumFs.InfoFile.Write(info); nil != err {
		return err
	}

	return nil
}

func getAlbumVolumes(ctx context.Context, accessToken, id string) ([][]AlbumTrackMeta, error) {
	var (
		tracks              [][]AlbumTrackMeta
		currentVolumeTracks []AlbumTrackMeta
		currentVolume       = 1
	)

	loopFlawPs := []flaw.P{}
	flawP := flaw.P{"loop_flaws": loopFlawPs}

	for i := 0; ; i++ {
		loopFlaw := flaw.P{"page": i}
		loopFlawPs = append(loopFlawPs, loopFlaw)
		flawP["loop_flaws"] = loopFlawPs
		pageTracks, rem, err := albumTracksPage(ctx, accessToken, id, i)
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
				currentVolumeTracks = []AlbumTrackMeta{track}
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

func albumTracksPage(ctx context.Context, accessToken, id string, page int) (ts []AlbumTrackMeta, rem int, err error) {
	albumURL, err := url.JoinPath(fmt.Sprintf(albumItemsCreditsAPIFormat, id))
	if nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return nil, 0, flaw.From(fmt.Errorf("failed to join album tracks credits URL with id: %v", err)).Append(flawP)
	}
	flawP := flaw.P{"url": albumURL}

	respBytes, err := getAlbumPagedItems(ctx, accessToken, albumURL, page)
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

	var respBody struct {
		TotalNumberOfItems int `json:"totalNumberOfItems"`
		Items              []struct {
			Type    string               `json:"type"`
			Credits TrackCreditsResponse `json:"credits"`
			Item    struct {
				ID           int    `json:"id"`
				StreamReady  bool   `json:"streamReady"`
				TrackNumber  int    `json:"trackNumber"`
				VolumeNumber int    `json:"volumeNumber"`
				Title        string `json:"title"`
				Copyright    string `json:"copyright"`
				ISRC         string `json:"isrc"`
				Duration     int    `json:"duration"`
				Artist       struct {
					Name string `json:"name"`
				} `json:"artist"`
				Artists []struct {
					Name string `json:"name"`
					Type string `json:"type"`
				} `json:"artists"`
				Album struct {
					ID    int    `json:"id"`
					Cover string `json:"cover"`
				} `json:"album"`
				Version *string `json:"version"`
			} `json:"item"`
		} `json:"items"`
	}
	if err := json.Unmarshal(respBytes, &respBody); nil != err {
		flawP["response_body"] = string(respBytes)
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, 0, flaw.From(fmt.Errorf("failed to decode album items page response: %v", err)).Append(flawP)
	}

	thisPageItemsCount := len(respBody.Items)
	if thisPageItemsCount == 0 {
		return nil, 0, os.ErrNotExist
	}

	for _, v := range respBody.Items {
		if v.Type != pageItemTypeTrack || !v.Item.StreamReady {
			continue
		}

		artists := make([]tidal.TrackArtist, len(v.Item.Artists))
		for i, a := range v.Item.Artists {
			switch a.Type {
			case tidal.ArtistTypeMain, tidal.ArtistTypeFeatured:
			default:
				return nil, 0, flaw.From(fmt.Errorf("unexpected artist type: %s", a.Type)).Append(flawP)
			}
			artists[i] = tidal.TrackArtist{Name: a.Name, Type: a.Type}
		}

		t := AlbumTrackMeta{
			Artist:       v.Item.Artist.Name,
			Artists:      artists,
			Duration:     v.Item.Duration,
			ID:           strconv.Itoa(v.Item.ID),
			Title:        v.Item.Title,
			Copyright:    v.Item.Copyright,
			ISRC:         v.Item.ISRC,
			TrackNumber:  v.Item.TrackNumber,
			Version:      v.Item.Version,
			VolumeNumber: v.Item.VolumeNumber,
			Credits:      v.Credits.toTrackCredits(),
		}
		ts = append(ts, t)
	}

	return ts, respBody.TotalNumberOfItems - (thisPageItemsCount + page*pageSize), nil
}

func getAlbumPagedItems(ctx context.Context, accessToken, itemsURL string, page int) ([]byte, error) {
	reqParams := make(url.Values, 3)
	reqParams.Add("countryCode", "US")
	reqParams.Add("limit", strconv.Itoa(pageSize))
	reqParams.Add("offset", strconv.Itoa(page*pageSize))
	return getPagedItems(ctx, accessToken, itemsURL, reqParams)
}

func getListPagedItems(ctx context.Context, accessToken, itemsURL string, page int) ([]byte, error) {
	reqParams := make(url.Values, 3)
	reqParams.Add("countryCode", "US")
	reqParams.Add("limit", strconv.Itoa(pageSize))
	reqParams.Add("offset", strconv.Itoa(page*pageSize))
	return getPagedItems(ctx, accessToken, itemsURL, reqParams)
}

func getPagedItems(ctx context.Context, accessToken, itemsURL string, reqParams url.Values) ([]byte, error) {
	reqURL, err := url.Parse(itemsURL)
	if nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return nil, flaw.From(fmt.Errorf("failed to parse page items URL: %v", err)).Append(flawP)
	}

	reqURL.RawQuery = reqParams.Encode()
	flawP := flaw.P{"encoded_query_params": reqURL.RawQuery}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL.String(), nil)
	if nil != err {
		if errutil.IsContext(ctx) {
			return nil, ctx.Err()
		}

		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, flaw.From(fmt.Errorf("failed to create get page items request: %v", err)).Append(flawP)
	}
	req.Header.Add("Authorization", "Bearer "+accessToken)

	client := http.Client{Timeout: config.GetPageTracksRequestTimeout} //nolint:exhaustruct
	resp, err := client.Do(req)
	if nil != err {
		switch {
		case errutil.IsContext(ctx):
			return nil, ctx.Err()
		case errors.Is(err, context.DeadlineExceeded):
			return nil, context.DeadlineExceeded
		default:
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return nil, flaw.From(fmt.Errorf("failed to send get page items request: %v", err)).Append(flawP)
		}
	}
	defer func() {
		if closeErr := resp.Body.Close(); nil != closeErr {
			flawP["err_debug_tree"] = errutil.Tree(closeErr).FlawP()
			closeErr = flaw.From(fmt.Errorf("failed to close get page items response body: %v", closeErr)).Append(flawP)
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
	flawP["response"] = errutil.HTTPResponseFlawPayload(resp)

	switch code := resp.StatusCode; code {
	case http.StatusOK:
	case http.StatusUnauthorized:
		respBytes, err := httputil.ReadOptionalResponseBody(ctx, resp)
		if nil != err {
			return nil, err
		}
		flawP["response_body"] = string(respBytes)
		return nil, flaw.From(errors.New("received 401 response")).Append(flawP)
	case http.StatusTooManyRequests:
		return nil, ErrTooManyRequests
	case http.StatusForbidden:
		respBytes, err := httputil.ReadResponseBody(ctx, resp)
		if nil != err {
			return nil, err
		}
		if ok, err := errutil.IsTooManyErrorResponse(resp, respBytes); nil != err {
			flawP["response_body"] = string(respBytes)
			return nil, must.BeFlaw(err)
		} else if ok {
			return nil, ErrTooManyRequests
		}

		flawP["response_body"] = string(respBytes)
		return nil, flaw.From(errors.New("unexpected 403 response")).Append(flawP)
	default:
		respBytes, err := httputil.ReadOptionalResponseBody(ctx, resp)
		if nil != err {
			return nil, err
		}
		flawP["response_body"] = string(respBytes)
		return nil, flaw.From(fmt.Errorf("unexpected status code: %d", code)).Append(flawP)
	}

	respBytes, err := httputil.ReadResponseBody(ctx, resp)
	if nil != err {
		return nil, err
	}
	return respBytes, nil
}
