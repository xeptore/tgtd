package tidl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/goccy/go-json"
	"github.com/xeptore/flaw/v8"

	"github.com/xeptore/tgtd/errutil"
	"github.com/xeptore/tgtd/must"
	"github.com/xeptore/tgtd/ptr"
	"github.com/xeptore/tgtd/tidl/auth"
)

type Artist struct {
	Name string `json:"name"`
}

func (a Artist) FlawP() flaw.P {
	return flaw.P{"name": a.Name}
}

type Album struct {
	ID    string `json:"id"`
	Year  int    `json:"year"`
	Title string `json:"title"`
	Cover string `json:"cover"`
}

func (a Album) FlawP() flaw.P {
	return flaw.P{
		"id":    a.ID,
		"year":  a.Year,
		"title": a.Title,
		"cover": a.Cover,
	}
}

type SingleTrack struct {
	ID       string  `json:"id"`
	Duration int     `json:"duration"`
	Title    string  `json:"title"`
	Artist   Artist  `json:"artist"`
	Album    Album   `json:"album"`
	Version  *string `json:"version"`
}

func (t *SingleTrack) id() string {
	return t.ID
}

func (t *SingleTrack) FileName() string {
	return filepath.Join("singles", t.ID+".flac")
}

func (d *Downloader) prepareTrackDir(t Track, a Album) error {
	trackDir := filepath.Dir(filepath.Join(d.basePath, t.FileName()))
	flawP := flaw.P{"track_dir": trackDir}
	if err := os.RemoveAll(trackDir); nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to delete possibly existing track directory: %v", err)).Append(flawP)
	}
	if err := os.MkdirAll(trackDir, 0o0755); nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to create track directory: %v", err)).Append(flawP)
	}

	infoFilePath := filepath.Join(trackDir, "album.json")
	flawP["info_file_path"] = infoFilePath
	f, err := os.OpenFile(infoFilePath, os.O_CREATE|os.O_SYNC|os.O_TRUNC|os.O_WRONLY, 0o0644)
	if nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to create track album info file: %v", err)).Append(flawP)
	}
	defer func() {
		if closeErr := f.Close(); nil != closeErr {
			flawP["err_debug_tree"] = errutil.Tree(closeErr).FlawP()
			closeErr = flaw.From(fmt.Errorf("failed to close track album info file: %v", closeErr)).Append(flawP)
			if nil != err {
				err = must.BeFlaw(err).Join(closeErr)
			} else {
				err = closeErr
			}
		}
	}()

	if err := json.NewEncoder(f).Encode(a); nil != err {
		flawP["album"] = a.FlawP()
		flawP["track"] = ptr.Of(t.info()).FlawP()
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to encode track album info: %v", err)).Append(flawP)
	}
	if err := f.Sync(); nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to sync track album info file: %v", err)).Append(flawP)
	}
	return nil
}

func ReadTrackAlbumInfoFile(trackDir string) (inf *Album, err error) {
	infoFilePath := filepath.Join(trackDir, "album.json")
	f, err := os.OpenFile(infoFilePath, os.O_RDONLY, 0o644)
	if nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return nil, flaw.From(fmt.Errorf("failed to open track album info file: %v", err)).Append(flawP)
	}
	defer func() {
		if closeErr := f.Close(); nil != closeErr {
			flawP := flaw.P{"err_debug_tree": errutil.Tree(closeErr).FlawP()}
			closeErr = flaw.From(fmt.Errorf("failed to close track album info file: %v", closeErr)).Append(flawP)
			if nil != err {
				err = must.BeFlaw(err).Join(closeErr)
			} else {
				err = closeErr
			}
		}
	}()

	if err := json.NewDecoder(f).Decode(&inf); nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP(), "track": inf.FlawP()}
		return nil, flaw.From(fmt.Errorf("failed to unmarshal track album info file: %v", err)).Append(flawP)
	}

	return inf, nil
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

	respBytes, err := io.ReadAll(resp.Body)
	if nil != err {
		switch {
		case errutil.IsContext(ctx):
			return nil, ctx.Err()
		case errors.Is(err, context.DeadlineExceeded):
			return nil, context.DeadlineExceeded
		default:
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return nil, flaw.From(fmt.Errorf("failed to read track info response body: %v", err)).Append(flawP)
		}
	}

	switch code := resp.StatusCode; code {
	case http.StatusOK:
	case http.StatusUnauthorized:
		var responseBody struct {
			Status      int    `json:"status"`
			SubStatus   int    `json:"subStatus"`
			UserMessage string `json:"userMessage"`
		}
		if err := json.Unmarshal(respBytes, &responseBody); nil != err {
			flawP["response_body"] = string(respBytes)
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return nil, flaw.From(fmt.Errorf("failed to decode 401 unauthorized response body: %v", err)).Append(flawP)
		}
		if responseBody.Status == 401 && responseBody.SubStatus == 11002 && responseBody.UserMessage == "Token could not be verified" {
			return nil, auth.ErrUnauthorized
		}

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
	if err := json.Unmarshal(respBytes, &responseBody); nil != err {
		flawP["response_body"] = string(respBytes)
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
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
			Year:  0,  // not provided by this API
			Title: "", // not provided by this API
			Cover: responseBody.Album.Cover,
		},
		Version: responseBody.Version,
	}
	return &track, nil
}
