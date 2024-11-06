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

	"github.com/xeptore/tgtd/tidl/auth"

	"github.com/goccy/go-json"
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
	if err := os.MkdirAll(filepath.Dir(path.Join(basePath, t.FileName())), 0o755); nil != err {
		return fmt.Errorf("failed to create track directory: %v", err)
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
		title = fmt.Sprintf("%s", t.Title)
	}
	return TrackInfo{
		Duration:   t.Duration,
		Title:      title,
		ArtistName: t.Artist.Name,
		Version:    t.Version,
	}
}

func (d *Downloader) single(ctx context.Context, id string) (*SingleTrack, error) {
	trackURL := fmt.Sprintf(trackAPIFormat, id)
	reqURL, err := url.Parse(trackURL)
	if nil != err {
		return nil, fmt.Errorf("failed to parse track URL: %v", err)
	}
	params := make(url.Values, 1)
	params.Add("countryCode", "US")
	reqURL.RawQuery = params.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL.String(), nil)
	if nil != err {
		return nil, fmt.Errorf("failed to create get track info request: %v", err)
	}
	request.Header.Add("Authorization", "Bearer "+d.auth.Creds.AccessToken)

	client := http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(request)
	if nil != err {
		return nil, fmt.Errorf("failed to send get track info request: %v", err)
	}
	defer func() {
		if closeErr := response.Body.Close(); nil != closeErr {
			err = fmt.Errorf("failed to close get track info response body: %v", closeErr)
		}
	}()

	switch code := response.StatusCode; code {
	case http.StatusOK:
	case http.StatusUnauthorized:
		var responseBody struct {
			Status      int    `json:"status"`
			SubStatus   int    `json:"subStatus"`
			UserMessage string `json:"userMessage"`
		}
		if err := json.NewDecoder(response.Body).DecodeContext(ctx, &responseBody); nil != err {
			return nil, fmt.Errorf("failed to decode 401 unauthorized response body: %v", err)
		}
		if responseBody.Status == 401 && responseBody.SubStatus == 11002 && responseBody.UserMessage == "Token could not be verified" {
			return nil, auth.ErrUnauthorized
		}
		return nil, fmt.Errorf("unexpected response: %d %s", responseBody.Status, responseBody.UserMessage)
	default:
		return nil, fmt.Errorf("unexpected status code: %d", code)
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
		return nil, err
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
