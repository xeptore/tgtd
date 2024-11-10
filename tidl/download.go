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
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/rs/zerolog"
	"github.com/xeptore/flaw/v8"

	"github.com/xeptore/tgtd/errutil"
	"github.com/xeptore/tgtd/must"
	"github.com/xeptore/tgtd/ratelimit"
	"github.com/xeptore/tgtd/tidl/auth"
)

const (
	trackAPIFormat         = "https://api.tidalhifi.com/v1/tracks/%s"
	trackStreamAPIFormat   = "https://api.tidalhifi.com/v1/tracks/%s/playbackinfopostpaywall"
	albumItemsAPIFormat    = "https://api.tidalhifi.com/v1/albums/%s/items"
	playlistItemsAPIFormat = "https://api.tidalhifi.com/v1/playlists/%s/items"
	mixItemsAPIFormat      = "https://api.tidalhifi.com/v1/mixes/%s/items"
	coverURLFormat         = "https://resources.tidal.com/images/%s/1280x1280.jpg"
	pageSize               = 100
	maxBatchParts          = 10
	singlePartChunkSize    = 1024 * 1024
)

type Downloader struct {
	auth     *auth.Auth
	basePath string
	logger   zerolog.Logger
}

func NewDownloader(auth *auth.Auth, basePath string, logger zerolog.Logger) *Downloader {
	return &Downloader{auth: auth, basePath: basePath, logger: logger}
}

func (d *Downloader) download(ctx context.Context, t Track) error {
	if err := d.writeInfo(t); nil != err {
		return err
	}

	if err := d.downloadCover(ctx, t); nil != err {
		return err
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
			switch {
			case nil == err:
				err = closeErr
			case errutil.IsFlaw(err):
				err = must.BeFlaw(err).Join(closeErr)
			default:
				panic(errutil.UnknownError(err))
			}
		}
	}()

	if err := json.NewEncoder(f).Encode(t.info()); nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return flaw.From(fmt.Errorf("failed to write track info: %v", err)).Append(flawP)
	}

	if err := f.Sync(); nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return flaw.From(fmt.Errorf("failed to sync track info file: %v", err)).Append(flawP)
	}

	return nil
}

func (d *Downloader) downloadCover(ctx context.Context, t Track) (err error) {
	coverURL, err := url.JoinPath(fmt.Sprintf(coverURLFormat, strings.ReplaceAll(t.cover(), "-", "/")))
	if nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return flaw.From(fmt.Errorf("failed to join cover base URL with cover path: %v", err)).Append(flawP)
	}
	flawP := flaw.P{"cover_url": coverURL}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, coverURL, nil)
	if nil != err {
		if errutil.IsContext(ctx) {
			return ctx.Err()
		}
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to create get cover request: %v", err)).Append(flawP)
	}
	request.Header.Add("Authorization", "Bearer "+d.auth.Creds.AccessToken)

	client := http.Client{Timeout: 5 * time.Hour} // TODO: set timeout to a reasonable value
	response, err := client.Do(request)
	if nil != err {
		switch {
		case errutil.IsContext(ctx):
			return ctx.Err()
		case errors.Is(err, context.DeadlineExceeded):
			return context.DeadlineExceeded
		default:
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return flaw.From(fmt.Errorf("failed to send get cover request: %v", err)).Append(flawP)
		}
	}
	defer func() {
		if closeErr := response.Body.Close(); nil != closeErr {
			flawP["err_debug_tree"] = errutil.Tree(closeErr).FlawP()
			closeErr = flaw.From(fmt.Errorf("failed to close get cover response body: %v", closeErr)).Append(flawP)
			switch {
			case nil == err:
				err = closeErr
			case errutil.IsContext(ctx):
				err = flaw.From(errors.New("context has ended")).Join(closeErr)
			case errors.Is(err, context.DeadlineExceeded):
				err = flaw.From(errors.New("timeout has reached")).Join(closeErr)
			case errutil.IsFlaw(err):
				err = must.BeFlaw(err).Join(closeErr)
			default:
				panic(errutil.UnknownError(err))
			}
		}
	}()
	flawP["response"] = errutil.HTTPResponseFlawPayload(response)

	switch code := response.StatusCode; code {
	case http.StatusOK:
	case http.StatusUnauthorized:
		resBytes, err := io.ReadAll(response.Body)
		if nil != err {
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return flaw.From(fmt.Errorf("failed to read get track cover response body: %v", err)).Append(flawP)
		}
		flawP["response_body"] = string(resBytes)
		return flaw.From(errors.New("received 401 response")).Append(flawP)
	default:
		resBytes, err := io.ReadAll(response.Body)
		if nil != err {
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return flaw.From(fmt.Errorf("failed to read get track cover response body: %v", err)).Append(flawP)
		}
		flawP["response_body"] = string(resBytes)
		return flaw.From(fmt.Errorf("unexpected status code received from get track cover: %d", code)).Append(flawP)
	}

	if err := d.writeCover(t, response.Body); nil != err {
		return err
	}

	return nil
}

func (d *Downloader) writeCover(t Track, r io.Reader) (err error) {
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
			switch {
			case nil == err:
				err = closeErr
			case errors.Is(err, context.DeadlineExceeded):
				err = flaw.From(errors.New("timeout has reached")).Join(closeErr)
			case errors.Is(err, context.Canceled):
				err = flaw.From(errors.New("context has been canceled")).Join(closeErr)
			case errutil.IsFlaw(err):
				err = must.BeFlaw(err).Join(closeErr)
			default:
				panic(errutil.UnknownError(err))
			}
		}
	}()

	if _, err := io.Copy(f, r); nil != err {
		if err, ok := errutil.IsAny(err, context.DeadlineExceeded, context.Canceled); ok {
			return err
		}
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return flaw.From(fmt.Errorf("failed to write track cover: %v", err)).Append(flawP)
	}

	if err := f.Sync(); nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return flaw.From(fmt.Errorf("failed to sync track cover file: %v", err)).Append(flawP)
	}

	return nil
}

func (d *Downloader) getPagedItems(ctx context.Context, itemsURL string, page int) (*http.Response, error) {
	reqURL, err := url.Parse(itemsURL)
	if nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return nil, flaw.From(fmt.Errorf("failed to parse items URL: %v", err)).Append(flawP)
	}
	params := make(url.Values, 3)
	params.Add("countryCode", "US")
	params.Add("limit", strconv.Itoa(pageSize))
	params.Add("offset", strconv.Itoa(page*pageSize))
	reqURL.RawQuery = params.Encode()
	flawP := flaw.P{"encoded_query_params": reqURL.RawQuery}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL.String(), nil)
	if nil != err {
		if errutil.IsContext(ctx) {
			return nil, ctx.Err()
		}
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, flaw.From(fmt.Errorf("failed to create get track info request: %v", err)).Append(flawP)
	}
	request.Header.Add("Authorization", "Bearer "+d.auth.Creds.AccessToken)

	client := http.Client{Timeout: 5 * time.Minute} // TODO: set it to a reasonable value
	response, err := client.Do(request)
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
	flawP["response"] = errutil.HTTPResponseFlawPayload(response)

	switch code := response.StatusCode; code {
	case http.StatusOK:
	case http.StatusUnauthorized:
		resBytes, err := io.ReadAll(response.Body)
		if nil != err {
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return nil, flaw.From(fmt.Errorf("failed to read get track info response body: %v", err)).Append(flawP)
		}
		flawP["response_body"] = string(resBytes)
		return nil, flaw.From(errors.New("received 401 response")).Append(flawP)
	default:
		resBytes, err := io.ReadAll(response.Body)
		if nil != err {
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return nil, flaw.From(fmt.Errorf("failed to read get track info response body: %v", err)).Append(flawP)
		}
		flawP["response_body"] = string(resBytes)
		return nil, flaw.From(fmt.Errorf("unexpected status code: %d", code)).Append(flawP)
	}

	return response, nil
}
