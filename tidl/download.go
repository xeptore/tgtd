package tidl

import (
	"context"
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
	"github.com/xeptore/tgtd/ratelimit"
	"github.com/xeptore/tgtd/tidl/auth"
	"github.com/xeptore/tgtd/tidl/must"
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
	if err := d.writeInfo(ctx, t); nil != err {
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

	if err := stream.saveTo(ctx, fileName); nil != err {
		if err, ok := errutil.IsAny(err, auth.ErrUnauthorized, context.DeadlineExceeded, context.Canceled); ok {
			return err
		}
		return must.BeFlaw(err).Append(flawP)
	}

	return nil
}

func (d *Downloader) writeInfo(ctx context.Context, t Track) (err error) {
	f, err := os.OpenFile(
		path.Join(d.basePath, t.FileName()+".json"),
		os.O_CREATE|os.O_WRONLY|os.O_TRUNC,
		0o0644,
	)
	if nil != err {
		return flaw.From(fmt.Errorf("failed to create track info file: %v", err))
	}
	defer func() {
		if closeErr := f.Close(); nil != closeErr {
			closeErr = flaw.From(fmt.Errorf("failed to close track info file: %v", closeErr))
			if nil != err {
				if _, ok := errutil.IsAny(err, context.DeadlineExceeded, context.Canceled); !ok {
					err = must.BeFlaw(err).Join(closeErr)
					return
				}
			}
			err = closeErr
		}
	}()

	if err := json.NewEncoder(f).EncodeContext(ctx, t.info()); nil != err {
		if err, ok := errutil.IsAny(err, context.Canceled); ok {
			return err
		}
		return flaw.From(fmt.Errorf("failed to write track info: %v", err))
	}

	if err := f.Sync(); nil != err {
		return flaw.From(fmt.Errorf("failed to sync track info file: %v", err))
	}

	return nil
}

func (d *Downloader) downloadCover(ctx context.Context, t Track) (err error) {
	coverURL, err := url.JoinPath(fmt.Sprintf(coverURLFormat, strings.ReplaceAll(t.cover(), "-", "/")))
	if nil != err {
		return flaw.From(fmt.Errorf("failed to join cover base URL with cover path: %v", err))
	}
	flawP := flaw.P{"cover_url": coverURL}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, coverURL, nil)
	if nil != err {
		if err, ok := errutil.IsAny(err, context.Canceled); ok {
			return err
		}
		return flaw.From(fmt.Errorf("failed to create get cover request: %v", err)).Append(flawP)
	}
	request.Header.Add("Authorization", "Bearer "+d.auth.Creds.AccessToken)

	client := http.Client{Timeout: 5 * time.Hour} // TODO: set timeout to a reasonable value
	response, err := client.Do(request)
	if nil != err {
		if err, ok := errutil.IsAny(err, context.DeadlineExceeded, context.Canceled); ok {
			return err
		}
		return flaw.From(fmt.Errorf("failed to send get cover request: %v", err)).Append(flawP)
	}
	defer func() {
		if closeErr := response.Body.Close(); nil != closeErr {
			closeErr = flaw.From(fmt.Errorf("failed to close get cover response body: %v", closeErr))
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

	switch code := response.StatusCode; code {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return auth.ErrUnauthorized
	default:
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
		return flaw.From(fmt.Errorf("failed to create track cover file: %v", err))
	}
	defer func() {
		if closeErr := f.Close(); nil != closeErr {
			closeErr = flaw.From(fmt.Errorf("failed to close track cover file: %v", closeErr))
			if nil != err {
				if _, ok := errutil.IsAny(err, context.DeadlineExceeded, context.Canceled); !ok {
					err = must.BeFlaw(err).Join(closeErr)
					return
				}
			}
			err = closeErr
		}
	}()

	if _, err := io.Copy(f, r); nil != err {
		if err, ok := errutil.IsAny(err, context.DeadlineExceeded, context.Canceled); ok {
			return err
		}
		return flaw.From(fmt.Errorf("failed to write track cover: %v", err))
	}

	if err := f.Sync(); nil != err {
		return flaw.From(fmt.Errorf("failed to sync track cover file: %v", err))
	}

	return nil
}

func (d *Downloader) getPagedItems(ctx context.Context, itemsURL string, page int) (*http.Response, error) {
	reqURL, err := url.Parse(itemsURL)
	if nil != err {
		return nil, flaw.From(fmt.Errorf("failed to parse items URL: %v", err))
	}
	params := make(url.Values, 3)
	params.Add("countryCode", "US")
	params.Add("limit", strconv.Itoa(pageSize))
	params.Add("offset", strconv.Itoa(page*pageSize))
	reqURL.RawQuery = params.Encode()
	flawP := flaw.P{"encoded_query_params": reqURL.RawQuery}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL.String(), nil)
	if nil != err {
		if err, ok := errutil.IsAny(err, context.Canceled); ok {
			return nil, err
		}
		return nil, flaw.From(fmt.Errorf("failed to create get track info request: %v", err)).Append(flawP)
	}
	request.Header.Add("Authorization", "Bearer "+d.auth.Creds.AccessToken)

	client := http.Client{Timeout: 5 * time.Minute} // TODO: set it to a reasonable value
	response, err := client.Do(request)
	if nil != err {
		if err, ok := errutil.IsAny(err, context.DeadlineExceeded, context.Canceled); ok {
			return nil, err
		}
		return nil, flaw.From(fmt.Errorf("failed to send get track info request: %v", err)).Append(flawP)
	}
	flawP["response"] = errutil.HTTPResponseFlawPayload(response)

	switch code := response.StatusCode; code {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return nil, auth.ErrUnauthorized
	default:
		return nil, flaw.From(fmt.Errorf("unexpected status code: %d", code)).Append(flawP)
	}

	return response, nil
}
