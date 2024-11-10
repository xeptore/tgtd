package tidl

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/samber/lo"
	"github.com/xeptore/flaw/v8"

	"github.com/xeptore/tgtd/errutil"
	"github.com/xeptore/tgtd/must"
	"github.com/xeptore/tgtd/tidl/mpd"
)

type TrackStream interface {
	saveTo(ctx context.Context, fileName string) error
}

func (d *Downloader) stream(ctx context.Context, id string) (s TrackStream, err error) {
	trackURL := fmt.Sprintf(trackStreamAPIFormat, id)
	flawP := flaw.P{"url": trackURL}
	reqURL, err := url.Parse(trackURL)
	if nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, flaw.From(fmt.Errorf("failed to parse track URL to build track stream URLs: %v", err)).Append(flawP)
	}
	params := make(url.Values, 4)
	params.Add("countryCode", "US")
	params.Add("audioquality", "HI_RES_LOSSLESS")
	params.Add("playbackmode", "STREAM")
	params.Add("assetpresentation", "FULL")
	reqURL.RawQuery = params.Encode()
	flawP["encoded_query_params"] = reqURL.RawQuery
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL.String(), nil)
	if nil != err {
		if errutil.IsContext(ctx) {
			return nil, ctx.Err()
		}
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, flaw.From(fmt.Errorf("failed to create get track stream URLs request: %v", err)).Append(flawP)
	}
	req.Header.Add("Authorization", "Bearer "+d.auth.Creds.AccessToken)

	client := http.Client{Timeout: 5 * time.Hour} // TODO: set timeout to a reasonable value
	resp, err := client.Do(req)
	if nil != err {
		switch {
		case errutil.IsContext(ctx):
			return nil, ctx.Err()
		case errors.Is(err, context.DeadlineExceeded):
			return nil, context.DeadlineExceeded
		case errors.Is(err, ErrTooManyRequests):
			return nil, ErrTooManyRequests
		default:
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return nil, flaw.From(fmt.Errorf("failed to send get track stream URLs request: %v", err)).Append(flawP)
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
		resBytes, err := io.ReadAll(resp.Body)
		if nil != err {
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return nil, flaw.From(fmt.Errorf("failed to read get track stream URLs response body: %v", err)).Append(flawP)
		}
		flawP["response_body"] = string(resBytes)
		return nil, flaw.From(errors.New("received 401 response")).Append(flawP)
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
			return nil, flaw.From(fmt.Errorf("failed to read get stream URLs response body: %v", err)).Append(flawP)
		}
		flawP["response_body"] = lo.Ternary(len(resBytes) > 0, string(resBytes), "")
		return nil, flaw.From(errors.New("unexpected 403 response")).Append(flawP)
	default:
		resBytes, err := io.ReadAll(resp.Body)
		if nil != err {
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return nil, flaw.From(fmt.Errorf("failed to read get track stream URLs response body: %v", err)).Append(flawP)
		}
		flawP["response_body"] = string(resBytes)
		return nil, flaw.From(fmt.Errorf("unexpected status code received from get track stream URLs: %d", code)).Append(flawP)
	}

	var responseBody struct {
		ManifestMimeType string `json:"manifestMimeType"`
		Manifest         string `json:"manifest"`
	}
	if err := json.NewDecoder(resp.Body).DecodeContext(ctx, &responseBody); nil != err {
		switch {
		case errutil.IsContext(ctx):
			return nil, ctx.Err()
		case errors.Is(err, context.DeadlineExceeded):
			return nil, context.DeadlineExceeded
		default:
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return nil, flaw.From(fmt.Errorf("failed to decode track stream response body: %v", err)).Append(flawP)
		}
	}
	flawP["stream"] = flaw.P{"manifest_mime_type": responseBody.ManifestMimeType}

	switch mimeType := responseBody.ManifestMimeType; mimeType {
	case "application/dash+xml", "dash+xml":
		dec := base64.NewDecoder(base64.StdEncoding, strings.NewReader(responseBody.Manifest))
		info, err := mpd.ParseStreamInfo(dec)
		if nil != err {
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return nil, flaw.From(fmt.Errorf("failed to parse stream info: %v", err)).Append(flawP)
		}
		flawP["stream_info"] = flaw.P{"info": info.FlawP()}
		return &DashTrackStream{Info: *info, AuthAccessToken: d.auth.Creds.AccessToken}, nil
	case "application/vnd.tidal.bts", "vnd.tidal.bt":
		var manifest struct {
			MimeType       string   `json:"mimeType"`
			KeyID          *string  `json:"keyId"`
			EncryptionType string   `json:"encryptionType"`
			URLs           []string `json:"urls"`
		}
		dec := base64.NewDecoder(base64.StdEncoding, strings.NewReader(responseBody.Manifest))
		if err := json.NewDecoder(dec).Decode(&manifest); nil != err {
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return nil, flaw.From(fmt.Errorf("failed to decode vnd.tidal.bt manifest: %v", err)).Append(flawP)
		}
		flawP["manifest"] = flaw.P{
			"mime_type":       manifest.MimeType,
			"key_id":          manifest.KeyID,
			"encryption_type": manifest.EncryptionType,
			"urls":            manifest.URLs,
		}
		switch manifest.MimeType {
		case "audio/flac":
		default:
			return nil, flaw.
				From(fmt.Errorf("unexpected vnd.tidal.bt manifest mime type: %s", manifest.MimeType)).
				Append(flawP)
		}

		switch manifest.EncryptionType {
		case "NONE":
		default:
			return nil, flaw.
				From(fmt.Errorf("encrypted vnd.tidal.bt manifest is not yet implemented: %s", manifest.EncryptionType)).
				Append(flawP)
		}

		if len(manifest.URLs) == 0 {
			return nil, flaw.From(errors.New("empty vnd.tidal.bt manifest URLs")).Append(flawP)
		}
		return &VndTrackStream{
			URL:             manifest.URLs[0],
			AuthAccessToken: d.auth.Creds.AccessToken,
		}, nil
	default:
		return nil, flaw.From(fmt.Errorf("unexpected manifest mime type: %s", mimeType)).Append(flawP)
	}
}
