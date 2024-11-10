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
	"path/filepath"
	"strconv"
	"time"

	"github.com/goccy/go-json"
	"github.com/samber/lo"
	"github.com/xeptore/flaw/v8"

	"github.com/xeptore/tgtd/errutil"
	"github.com/xeptore/tgtd/must"
	"github.com/xeptore/tgtd/tidl/auth"
)

type Artist struct {
	Name string
}

type Album struct {
	ID    string
	Cover string
}

type SingleTrack struct {
	ID       string
	Duration int
	Title    string
	Artist   Artist
	Album    Album
	Version  *string
}

func (t *SingleTrack) id() string {
	return t.ID
}

func (t *SingleTrack) FileName() string {
	var fileName string
	if nil != t.Version {
		fileName = fmt.Sprintf("%s - %s (%s).flac", t.Artist.Name, t.Title, *t.Version)
	} else {
		fileName = fmt.Sprintf("%s - %s.flac", t.Artist.Name, t.Title)
	}
	return path.Join("singles", t.ID, fileName)
}

func (t *SingleTrack) createDir(basePath string) error {
	dirPath := filepath.Dir(path.Join(basePath, t.FileName()))
	flawP := flaw.P{"file_path": dirPath}

	if err := os.MkdirAll(dirPath, 0o755); nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to create track directory: %v", err)).Append(flawP)
	}
	return nil
}

func (t *SingleTrack) cover() string {
	return t.Album.Cover
}

func (t *SingleTrack) info() TrackInfo {
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

func (d *Downloader) single(ctx context.Context, id string) (st *SingleTrack, err error) {
	trackURL := fmt.Sprintf(trackAPIFormat, id)
	flawP := flaw.P{"url": trackURL}
	reqURL, err := url.Parse(trackURL)
	if nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, flaw.From(fmt.Errorf("failed to parse track URL: %v", err)).Append(flawP)
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
		return nil, flaw.From(fmt.Errorf("failed to create get track info request: %v", err)).Append(flawP)
	}
	req.Header.Add("Authorization", "Bearer "+d.auth.Creds.AccessToken)

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
		var responseBody struct {
			Status      int    `json:"status"`
			SubStatus   int    `json:"subStatus"`
			UserMessage string `json:"userMessage"`
		}
		if err := json.NewDecoder(resp.Body).DecodeContext(ctx, &responseBody); nil != err {
			switch {
			case errutil.IsContext(ctx):
				return nil, ctx.Err()
			case errors.Is(err, context.DeadlineExceeded):
				return nil, context.DeadlineExceeded
			default:
				flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
				return nil, flaw.From(fmt.Errorf("failed to decode 401 unauthorized response body: %v", err)).Append(flawP)
			}
		}
		if responseBody.Status == 401 && responseBody.SubStatus == 11002 && responseBody.UserMessage == "Token could not be verified" {
			return nil, auth.ErrUnauthorized
		}
		resBytes, err := io.ReadAll(resp.Body)
		if nil != err {
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return nil, flaw.From(fmt.Errorf("failed to read get track info response body: %v", err)).Append(flawP)
		}
		flawP["response_body"] = string(resBytes)
		return nil, flaw.From(fmt.Errorf("unexpected response: %d %s", responseBody.Status, responseBody.UserMessage)).Append(flawP)
	case http.StatusTooManyRequests:
		return nil, ErrTooManyRequests
	case http.StatusForbidden:
		ok, err := errutil.IsTooManyErrorResponse(resp)
		if nil != err {
			switch {
			case errutil.IsContext(ctx):
				return nil, ctx.Err()
			case errors.Is(err, context.DeadlineExceeded):
				return nil, context.DeadlineExceeded
			case errutil.IsFlaw(err):
				return nil, err
			default:
				panic(errutil.UnknownError(err))
			}
		}
		if ok {
			return nil, ErrTooManyRequests
		}
		resBytes, err := io.ReadAll(resp.Body)
		if nil != err && !errors.Is(err, io.EOF) {
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return nil, flaw.From(fmt.Errorf("failed to read get track response body: %v", err)).Append(flawP)
		}
		flawP["response_body"] = lo.Ternary(len(resBytes) > 0, string(resBytes), "")
		return nil, flaw.From(fmt.Errorf("unexpected 403 response: %s", string(resBytes))).Append(flawP)
	default:
		resBytes, err := io.ReadAll(resp.Body)
		if nil != err {
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return nil, flaw.From(fmt.Errorf("failed to read get track info response body: %v", err)).Append(flawP)
		}
		flawP["response_body"] = string(resBytes)
		return nil, flaw.From(fmt.Errorf("unexpected status code: %d", code)).Append(flawP)
	}

	var responseBody struct {
		ID       int    `json:"id"`
		Duration int    `json:"duration"`
		Title    string `json:"title"`
		Artist   struct {
			Name string `json:"name"`
		} `json:"artist"`
		Album struct {
			ID    int    `json:"id"`
			Cover string `json:"cover"`
		} `json:"album"`
		Version *string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).DecodeContext(ctx, &responseBody); nil != err {
		switch {
		case errutil.IsContext(ctx):
			return nil, ctx.Err()
		case errors.Is(err, context.DeadlineExceeded):
			return nil, context.DeadlineExceeded
		default:
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return nil, flaw.From(fmt.Errorf("failed to decode track info response body: %v", err)).Append(flawP)
		}
	}

	track := SingleTrack{
		ID:       strconv.Itoa(responseBody.ID),
		Duration: responseBody.Duration,
		Title:    responseBody.Title,
		Artist: Artist{
			Name: responseBody.Artist.Name,
		},
		Album: Album{
			ID:    strconv.Itoa(responseBody.Album.ID),
			Cover: responseBody.Album.Cover,
		},
		Version: responseBody.Version,
	}
	return &track, nil
}
