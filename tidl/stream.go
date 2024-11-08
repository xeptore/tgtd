package tidl

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/xeptore/flaw/v8"

	"github.com/xeptore/tgtd/errutil"
	"github.com/xeptore/tgtd/tidl/auth"
	"github.com/xeptore/tgtd/tidl/mpd"
	"github.com/xeptore/tgtd/tidl/must"
)

type TrackStream interface {
	saveTo(ctx context.Context, fileName string) error
}

func (d *Downloader) stream(ctx context.Context, id string) (s TrackStream, err error) {
	trackURL := fmt.Sprintf(trackStreamAPIFormat, id)
	reqURL, err := url.Parse(trackURL)
	if nil != err {
		return nil, flaw.From(fmt.Errorf("failed to parse track URL to build track stream URLs: %v", err))
	}
	flawP := flaw.P{"url": trackURL}
	params := make(url.Values, 4)
	params.Add("countryCode", "US")
	params.Add("audioquality", "HI_RES_LOSSLESS")
	params.Add("playbackmode", "STREAM")
	params.Add("assetpresentation", "FULL")
	reqURL.RawQuery = params.Encode()
	flawP["encoded_query_params"] = reqURL.RawQuery
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL.String(), nil)
	if nil != err {
		if err, ok := errutil.IsAny(err, context.Canceled); ok {
			return nil, err
		}
		return nil, flaw.From(fmt.Errorf("failed to create get track stream URLs request: %v", err)).Append(flawP)
	}
	request.Header.Add("Authorization", "Bearer "+d.auth.Creds.AccessToken)

	client := http.Client{Timeout: 5 * time.Hour} // TODO: set timeout to a reasonable value
	response, err := client.Do(request)
	if nil != err {
		if err, ok := errutil.IsAny(err, context.DeadlineExceeded, context.Canceled); ok {
			return nil, err
		}
		return nil, flaw.From(fmt.Errorf("failed to send get track stream URLs request: %v", err)).Append(flawP)
	}
	defer func() {
		if closeErr := response.Body.Close(); nil != closeErr {
			closeErr = flaw.From(fmt.Errorf("failed to close get track stream URLs response body: %v", closeErr))
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
		return nil, auth.ErrUnauthorized
	default:
		return nil, flaw.From(fmt.Errorf("unexpected status code: %d", code)).Append(flawP)
	}

	var responseBody struct {
		ManifestMimeType string `json:"manifestMimeType"`
		Manifest         string `json:"manifest"`
	}
	if err := json.NewDecoder(response.Body).DecodeContext(ctx, &responseBody); nil != err {
		if err, ok := errutil.IsAny(err, context.DeadlineExceeded, context.Canceled); ok {
			return nil, err
		}
		return nil, flaw.From(fmt.Errorf("failed to decode track stream response body: %v", err)).Append(flawP)
	}
	flawP["stream"] = flaw.P{"manifest_mime_type": responseBody.ManifestMimeType}

	switch mimeType := responseBody.ManifestMimeType; mimeType {
	case "application/dash+xml", "dash+xml":
		dec := base64.NewDecoder(base64.StdEncoding, strings.NewReader(responseBody.Manifest))
		info, err := mpd.ParseStreamInfo(dec)
		if nil != err {
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
		if err := json.NewDecoder(dec).DecodeContext(ctx, &manifest); nil != err {
			if err, ok := errutil.IsAny(err, context.DeadlineExceeded, context.Canceled); ok {
				return nil, err
			}
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
