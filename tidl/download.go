package tidl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/rs/zerolog"
	"github.com/xeptore/flaw/v8"

	"github.com/xeptore/tgtd/cache"
	"github.com/xeptore/tgtd/config"
	"github.com/xeptore/tgtd/errutil"
	"github.com/xeptore/tgtd/must"
	"github.com/xeptore/tgtd/ptr"
	"github.com/xeptore/tgtd/ratelimit"
	"github.com/xeptore/tgtd/tidl/auth"
)

const (
	trackAPIFormat         = "https://api.tidalhifi.com/v1/tracks/%s"
	albumAPIFormat         = "https://api.tidalhifi.com/v1/albums/%s"
	playlistAPIFormat      = "https://api.tidalhifi.com/v1/playlists/%s"
	mixInfoURL             = "https://listen.tidal.com/v1/pages/mix"
	trackStreamAPIFormat   = "https://api.tidalhifi.com/v1/tracks/%s/playbackinfopostpaywall"
	albumItemsAPIFormat    = "https://api.tidalhifi.com/v1/albums/%s/items"
	playlistItemsAPIFormat = "https://api.tidalhifi.com/v1/playlists/%s/items"
	mixItemsAPIFormat      = "https://api.tidalhifi.com/v1/mixes/%s/items"
	coverURLFormat         = "https://resources.tidal.com/images/%s/1280x1280.jpg"
	pageSize               = 100
	maxBatchParts          = 10
	singlePartChunkSize    = 1024 * 1024
)

var ErrTooManyRequests = errors.New("too many requests")

type Downloader struct {
	auth     *auth.Auth
	basePath string
	cache    *cache.Cache[Album]
	logger   zerolog.Logger
}

func NewDownloader(auth *auth.Auth, basePath string, cache *cache.Cache[Album], logger zerolog.Logger) *Downloader {
	return &Downloader{
		auth:     auth,
		basePath: basePath,
		cache:    cache,
		logger:   logger,
	}
}

func (d *Downloader) download(ctx context.Context, t Track) error {
	if err := d.writeInfo(t); nil != err {
		return err
	}

	if coverBytes, err := d.fetchCover(ctx, t); nil != err {
		return err
	} else {
		d.cache.DownloadedCovers.Set(t.cover(), coverBytes, cache.DefaultDownloadedCoverTTL)
		if err := d.writeCover(t, coverBytes); nil != err {
			return err
		}
	}

	stream, err := d.stream(ctx, t.id())
	if nil != err {
		return err
	}

	fileName := path.Join(d.basePath, t.FileName())
	flawP := flaw.P{"file_name": fileName}

	waitTime := ratelimit.TrackDownloadSleepMS()
	flawP["wait_time"] = waitTime
	d.logger.Debug().Dur("wait_time", waitTime).Msg("Waiting to prevent rate limit error before starting track download")
	time.Sleep(waitTime)
	d.logger.Debug().Msg("Starting track download")

	if err := stream.saveTo(ctx, fileName); nil != err {
		switch {
		case errutil.IsContext(ctx):
			return ctx.Err()
		case errors.Is(err, context.DeadlineExceeded):
			return context.DeadlineExceeded
		case errors.Is(err, ErrTooManyRequests):
			return ErrTooManyRequests
		case errutil.IsFlaw(err):
			return must.BeFlaw(err).Append(flawP)
		default:
			panic(errutil.UnknownError(err))
		}
	}

	return nil
}

func (d *Downloader) writeInfo(t Track) (err error) {
	f, err := os.OpenFile(
		path.Join(d.basePath, t.FileName()+".json"),
		os.O_CREATE|os.O_WRONLY|os.O_TRUNC,
		0o0644,
	)
	if nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return flaw.From(fmt.Errorf("failed to create track info file: %v", err)).Append(flawP)
	}
	defer func() {
		if closeErr := f.Close(); nil != closeErr {
			flawP := flaw.P{"err_debug_tree": errutil.Tree(closeErr).FlawP()}
			closeErr = flaw.From(fmt.Errorf("failed to close track info file: %v", closeErr)).Append(flawP)
			if nil != err {
				err = must.BeFlaw(err).Join(closeErr)
			} else {
				err = closeErr
			}
		}
	}()

	if err := json.NewEncoder(f).Encode(t.info()); nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP(), "track_info": ptr.Of(t.info()).FlawP()}
		return flaw.From(fmt.Errorf("failed to write track info: %v", err)).Append(flawP)
	}

	if err := f.Sync(); nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return flaw.From(fmt.Errorf("failed to sync track info file: %v", err)).Append(flawP)
	}

	return nil
}

func (d *Downloader) fetchCover(ctx context.Context, t Track) (b []byte, err error) {
	c := d.cache.DownloadedCovers.Get(t.cover())
	if nil != c {
		return c.Value(), nil
	}
	return d.downloadCover(ctx, t)
}

func (d *Downloader) downloadCover(ctx context.Context, t Track) (b []byte, err error) {
	coverURL, err := url.JoinPath(fmt.Sprintf(coverURLFormat, strings.ReplaceAll(t.cover(), "-", "/")))
	if nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return nil, flaw.From(fmt.Errorf("failed to join cover base URL with cover path: %v", err)).Append(flawP)
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
	req.Header.Add("Authorization", "Bearer "+d.auth.Creds.AccessToken)

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

	respBytes, err := io.ReadAll(resp.Body)
	if nil != err {
		switch {
		case errutil.IsContext(ctx):
			return nil, ctx.Err()
		case errors.Is(err, context.DeadlineExceeded):
			return nil, context.DeadlineExceeded
		default:
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return nil, flaw.From(fmt.Errorf("failed to read get track cover response body: %v", err)).Append(flawP)
		}
	}

	switch code := resp.StatusCode; code {
	case http.StatusOK:
	case http.StatusUnauthorized:
		flawP["response_body"] = string(respBytes)
		return nil, flaw.From(errors.New("received 401 response")).Append(flawP)
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
		return nil, flaw.From(errors.New("unexpected 403 response")).Append(flawP)
	default:
		flawP["response_body"] = string(respBytes)
		return nil, flaw.From(fmt.Errorf("unexpected status code received from get track cover: %d", code)).Append(flawP)
	}

	return respBytes, nil
}

func (d *Downloader) writeCover(t Track, b []byte) (err error) {
	f, err := os.OpenFile(
		path.Join(d.basePath, t.FileName()+".jpg"),
		os.O_CREATE|os.O_WRONLY|os.O_TRUNC,
		0o0644,
	)
	if nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return flaw.From(fmt.Errorf("failed to create track cover file: %v", err)).Append(flawP)
	}
	defer func() {
		if closeErr := f.Close(); nil != closeErr {
			flawP := flaw.P{"err_debug_tree": errutil.Tree(closeErr).FlawP()}
			closeErr = flaw.From(fmt.Errorf("failed to close track cover file: %v", closeErr)).Append(flawP)
			if nil != err {
				err = must.BeFlaw(err).Join(closeErr)
			} else {
				err = closeErr
			}
		}
	}()

	if _, err := io.Copy(f, bytes.NewReader(b)); nil != err {
		flawP := flaw.P{
			"err_debug_tree": errutil.Tree(err).FlawP(),
			"bytes":          string(b),
		}
		return flaw.From(fmt.Errorf("failed to write track cover: %v", err)).Append(flawP)
	}

	if err := f.Sync(); nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return flaw.From(fmt.Errorf("failed to sync track cover file: %v", err)).Append(flawP)
	}

	return nil
}

func (d *Downloader) getPagedItems(ctx context.Context, itemsURL string, page int) ([]byte, error) {
	reqURL, err := url.Parse(itemsURL)
	if nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return nil, flaw.From(fmt.Errorf("failed to parse page items URL: %v", err)).Append(flawP)
	}

	reqParams := make(url.Values, 3)
	reqParams.Add("countryCode", "US")
	reqParams.Add("limit", strconv.Itoa(pageSize))
	reqParams.Add("offset", strconv.Itoa(page*pageSize))
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
	req.Header.Add("Authorization", "Bearer "+d.auth.Creds.AccessToken)

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

	respBytes, err := io.ReadAll(resp.Body)
	if nil != err {
		switch {
		case errutil.IsContext(ctx):
			return nil, ctx.Err()
		case errors.Is(err, context.DeadlineExceeded):
			return nil, context.DeadlineExceeded
		default:
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return nil, flaw.From(fmt.Errorf("failed to read get page items response body: %v", err)).Append(flawP)
		}
	}

	switch code := resp.StatusCode; code {
	case http.StatusOK:
	case http.StatusUnauthorized:
		flawP["response_body"] = string(respBytes)
		return nil, flaw.From(errors.New("received 401 response")).Append(flawP)
	case http.StatusTooManyRequests:
		return nil, ErrTooManyRequests
	case http.StatusForbidden:
		if ok, err := errutil.IsTooManyErrorResponse(resp, respBytes); nil != err {
			flawP["response_body"] = string(respBytes)
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

	return respBytes, nil
}
