package tidl

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"time"

	"github.com/goccy/go-json"
	"github.com/xeptore/flaw/v8"

	"github.com/xeptore/tgtd/errutil"
	"github.com/xeptore/tgtd/tidl/auth"
	"github.com/xeptore/tgtd/tidl/must"
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
		return nil, flaw.From(fmt.Errorf("failed to parse track URL: %v", err)).Append(flawP)
	}
	params := make(url.Values, 1)
	params.Add("countryCode", "US")
	reqURL.RawQuery = params.Encode()
	flawP["encoded_query_params"] = reqURL.RawQuery
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL.String(), nil)
	if nil != err {
		if err, ok := errutil.IsAny(err, context.Canceled); ok {
			return nil, err
		}
		return nil, flaw.From(fmt.Errorf("failed to create get track info request: %v", err)).Append(flawP)
	}
	request.Header.Add("Authorization", "Bearer "+d.auth.Creds.AccessToken)

	client := http.Client{Timeout: 5 * time.Second} //nolint:exhaustruct
	response, err := client.Do(request)
	if nil != err {
		if err, ok := errutil.IsAny(err, context.DeadlineExceeded, context.Canceled); ok {
			return nil, err
		}
		return nil, flaw.From(fmt.Errorf("failed to send get track info request: %v", err)).Append(flawP)
	}
	defer func() {
		if closeErr := response.Body.Close(); nil != closeErr {
			closeErr = flaw.From(fmt.Errorf("failed to close get track info response body: %v", closeErr))
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
		var responseBody struct {
			Status      int    `json:"status"`
			SubStatus   int    `json:"subStatus"`
			UserMessage string `json:"userMessage"`
		}
		if err := json.NewDecoder(response.Body).DecodeContext(ctx, &responseBody); nil != err {
			if err, ok := errutil.IsAny(err, context.DeadlineExceeded, context.Canceled); ok {
				return nil, err
			}
			return nil, flaw.From(fmt.Errorf("failed to decode 401 unauthorized response body: %v", err)).Append(flawP)
		}
		if responseBody.Status == 401 && responseBody.SubStatus == 11002 && responseBody.UserMessage == "Token could not be verified" {
			return nil, auth.ErrUnauthorized
		}
		return nil, flaw.From(fmt.Errorf("unexpected response: %d %s", responseBody.Status, responseBody.UserMessage)).Append(flawP)
	default:
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
	if err := json.NewDecoder(response.Body).DecodeContext(ctx, &responseBody); nil != err {
		if err, ok := errutil.IsAny(err, context.DeadlineExceeded, context.Canceled); ok {
			return nil, err
		}
		return nil, flaw.From(fmt.Errorf("failed to decode track info response body: %v", err)).Append(flawP)
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
