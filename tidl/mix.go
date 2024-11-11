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
	"github.com/tidwall/gjson"
	"github.com/xeptore/flaw/v8"
	"golang.org/x/sync/errgroup"

	"github.com/xeptore/tgtd/errutil"
	"github.com/xeptore/tgtd/must"
	"github.com/xeptore/tgtd/ratelimit"
)

func mixTrackDir(mixID string) string {
	return path.Join("mixes", mixID)
}

func (d *Downloader) Mix(ctx context.Context, id string) error {
	tracks, err := d.mixTracks(ctx, id)
	if nil != err {
		return err
	}

	meta, err := d.mixInfo(ctx, id)
	if nil != err {
		return err
	}

	mix := Mix{
		ID:     id,
		Title:  meta.Title,
		Tracks: tracks,
	}

	if err := d.prepareMixDir(mix); nil != err {
		return err
	}

	wg, ctx := errgroup.WithContext(ctx)
	wg.SetLimit(ratelimit.MixDownloadConcurrency)
	for _, track := range tracks {
		wg.Go(func() error { return d.download(ctx, &track) })
	}

	if err := wg.Wait(); nil != err {
		return err
	}

	return nil
}

func (d *Downloader) prepareMixDir(m Mix) error {
	mixDir := path.Join(d.basePath, mixTrackDir(m.ID))
	if err := os.RemoveAll(mixDir); nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return flaw.From(fmt.Errorf("failed to delete possibly existing mix directory: %v", err)).Append(flawP)
	}
	flawP := flaw.P{"mix_dir": mixDir}
	if err := os.MkdirAll(mixDir, 0o0755); nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to create mix directory: %v", err)).Append(flawP)
	}

	f, err := os.OpenFile(path.Join(mixDir, "info.json"), os.O_CREATE|os.O_SYNC|os.O_TRUNC|os.O_WRONLY, 0o0644)
	if nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to create mix info file: %v", err)).Append(flawP)
	}
	if err := json.NewEncoder(f).Encode(m); nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to encode mix info: %v", err)).Append(flawP)
	}
	if err := f.Close(); nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to close mix info file: %v", err)).Append(flawP)
	}
	return nil
}

type Mix struct {
	ID     string     `json:"id"`
	Title  string     `json:"title"`
	Tracks []MixTrack `json:"tracks"`
}

type MixTrack struct {
	ID         string  `json:"id"`
	MixID      string  `json:"mix_id"`
	Duration   int     `json:"duration"`
	Title      string  `json:"title"`
	ArtistName string  `json:"artist_name"`
	Cover      string  `json:"cover"`
	Version    *string `json:"version"`
}

func (t *MixTrack) id() string {
	return t.ID
}

func (t *MixTrack) FileName() string {
	var fileName string
	if nil != t.Version {
		fileName = fmt.Sprintf("%s - %s (%s).flac", t.ArtistName, t.Title, *t.Version)
	} else {
		fileName = fmt.Sprintf("%s - %s.flac", t.ArtistName, t.Title)
	}
	return path.Join(mixTrackDir(t.MixID), fileName)
}

func (t *MixTrack) cover() string {
	return t.Cover
}

func (t *MixTrack) info() TrackInfo {
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

func (d *Downloader) mixInfo(ctx context.Context, id string) (m *Mix, err error) {
	flawP := flaw.P{}
	reqURL, err := url.Parse(mixInfoURL)
	if nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, flaw.From(fmt.Errorf("failed to parse playlist URL: %v", err)).Append(flawP)
	}

	params := make(url.Values, 4)
	params.Add("mixId", id)
	params.Add("countryCode", "US")
	params.Add("locale", "en_US")
	params.Add("deviceType", "BROWSER")
	reqURL.RawQuery = params.Encode()
	flawP["encoded_query_params"] = reqURL.RawQuery
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL.String(), nil)
	if nil != err {
		if errutil.IsContext(ctx) {
			return nil, ctx.Err()
		}
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, flaw.From(fmt.Errorf("failed to create get mix info request: %v", err)).Append(flawP)
	}
	req.Header.Add("Authorization", "Bearer "+d.auth.Creds.AccessToken)
	req.Header.Add("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:132.0) Gecko/20100101 Firefox/132.0")
	req.Header.Add("Accept", "application/json")

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
			return nil, flaw.From(fmt.Errorf("failed to send get mix info request: %v", err)).Append(flawP)
		}
	}
	flawP["response"] = errutil.HTTPResponseFlawPayload(resp)
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

	respBytes, err := io.ReadAll(resp.Body)
	if nil != err {
		switch {
		case errutil.IsContext(ctx):
			return nil, ctx.Err()
		case errors.Is(err, context.DeadlineExceeded):
			return nil, context.DeadlineExceeded
		default:
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return nil, flaw.From(fmt.Errorf("failed to read mix info response body: %v", err)).Append(flawP)
		}
	}

	switch code := resp.StatusCode; code {
	case http.StatusOK:
	case http.StatusTooManyRequests:
		return nil, ErrTooManyRequests
	case http.StatusForbidden:
		if ok, err := errutil.IsTooManyErrorResponse(resp, respBytes); nil != err {
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

	if !gjson.ValidBytes(respBytes) {
		flawP["response_body"] = string(respBytes)
		return nil, flaw.From(fmt.Errorf("invalid mix info response json: %v", err)).Append(flawP)
	}

	var title string
	switch titleKey := gjson.GetBytes(respBytes, "title"); titleKey.Type {
	case gjson.String:
		title = titleKey.Str
	default:
		flawP["response_body"] = string(respBytes)
		return nil, flaw.From(fmt.Errorf("unexpected mix info response: %v", err)).Append(flawP)
	}

	return &Mix{
		ID:     id,
		Title:  title,
		Tracks: nil,
	}, nil
}

func (d *Downloader) mixTracksPage(ctx context.Context, id string, page int) (tracks []MixTrack, remaining int, err error) {
	mixURL, err := url.JoinPath(fmt.Sprintf(mixItemsAPIFormat, id))
	if nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return nil, 0, flaw.From(fmt.Errorf("failed to create mix URL: %v", err)).Append(flawP)
	}
	flawP := flaw.P{"mix_url": mixURL}

	respBytes, err := d.getPagedItems(ctx, mixURL, page)
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
					Cover string `json:"cover"`
				} `json:"album"`
				Version *string `json:"version"`
			} `json:"item"`
		} `json:"items"`
	}
	if err := json.Unmarshal(respBytes, &responseBody); nil != err {
		flawP["response_body"] = string(respBytes)
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, 0, flaw.From(fmt.Errorf("failed to decode mix response: %v", err)).Append(flawP)
	}

	thisPageItemsCount := len(responseBody.Items)
	if thisPageItemsCount == 0 {
		return nil, 0, nil
	}

	for _, v := range responseBody.Items {
		if v.Type != trackTypeResponseItem {
			continue
		}
		mixTrack := MixTrack{
			ID:         strconv.Itoa(v.Item.ID),
			Duration:   v.Item.Duration,
			Title:      v.Item.Title,
			ArtistName: v.Item.Artist.Name,
			Cover:      v.Item.Album.Cover,
			MixID:      id,
			Version:    v.Item.Version,
		}
		tracks = append(tracks, mixTrack)
	}

	return tracks, responseBody.TotalNumberOfItems - (thisPageItemsCount + page*pageSize), nil
}

func (d *Downloader) mixTracks(ctx context.Context, id string) ([]MixTrack, error) {
	var tracks []MixTrack
	var loopFlawPs []flaw.P
	flawP := flaw.P{"loop_flaw_payloads": loopFlawPs}

	for i := 0; ; i++ {
		loopFlawP := flaw.P{"page": i}
		loopFlawPs = append(loopFlawPs, loopFlawP)
		pageTracks, rem, err := d.mixTracksPage(ctx, id, i)
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
