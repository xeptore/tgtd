package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/xeptore/flaw/v8"

	"github.com/xeptore/tgtd/errutil"
	"github.com/xeptore/tgtd/must"
	"github.com/xeptore/tgtd/ptr"
	"github.com/xeptore/tgtd/result"
)

const (
	clientID      = "7m7Ap0JC9j1cOM3n"
	clientSecret  = "vRAdA108tlvkJpTsGZS8rGZ7xTlbJ0qaZ2K9saEzsgY=" //nolint:gosec
	baseURL       = "https://auth.tidal.com/v1/oauth2"
	tokenFilePath = "token.json"
)

var ErrUnauthorized = errors.New("Unauthorized")

type Credentials struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64
}

type Auth struct {
	Creds Credentials
}

func Load(ctx context.Context) (*Auth, error) {
	creds, err := load(ctx)
	if nil != err {
		return nil, err
	}
	return &Auth{Creds: *creds}, nil
}

type File struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"`
}

func (f File) flawP() flaw.P {
	return flaw.P{
		"access_token":  f.AccessToken,
		"refresh_token": f.RefreshToken,
		"expires_at":    f.ExpiresAt,
	}
}

func load(ctx context.Context) (creds *Credentials, err error) {
	f, err := os.OpenFile(tokenFilePath, os.O_RDONLY, 0o0644)
	if nil != err {
		if !errors.Is(err, os.ErrNotExist) {
			flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
			return nil, flaw.From(fmt.Errorf("failed to open token file: %v", err)).Append(flawP)
		}
		return nil, ErrUnauthorized
	}
	defer func() {
		if closeErr := f.Close(); nil != closeErr {
			flawP := flaw.P{"err_debug_tree": errutil.Tree(closeErr).FlawP()}
			closeErr = flaw.From(fmt.Errorf("failed to close token file: %v", closeErr)).Append(flawP)
			switch {
			case nil == err:
				err = closeErr
			case errutil.IsContext(ctx):
				err = flaw.From(errors.New("context was ended")).Join(closeErr)
			case errors.Is(err, context.DeadlineExceeded):
				err = flaw.From(errors.New("timeout has reached")).Join(closeErr)
			case errors.Is(err, ErrUnauthorized):
				err = flaw.From(errors.New("received unauthorized error")).Join(closeErr)
			default:
				err = must.BeFlaw(err).Join(closeErr)
			}
		}
	}()
	var content File
	if err := json.NewDecoder(f).Decode(&content); nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return nil, flaw.From(fmt.Errorf("failed to decode token file: %v", err)).Append(flawP)
	}
	if time.Now().Unix() > content.ExpiresAt {
		return handleUnauthorized(ctx, content.RefreshToken)
	}
	if err := verifyAccessToken(ctx, content.AccessToken); nil != err {
		switch {
		case errutil.IsContext(ctx):
			return nil, ctx.Err()
		case errors.Is(err, context.DeadlineExceeded):
			return nil, context.DeadlineExceeded
		case errors.Is(err, ErrUnauthorized):
			return handleUnauthorized(ctx, content.RefreshToken)
		case errutil.IsFlaw(err):
			return nil, err
		default:
			panic(errutil.UnknownError(err))
		}
	}
	return ptr.Of(Credentials(content)), nil
}

func handleUnauthorized(ctx context.Context, rt string) (*Credentials, error) {
	refreshResult, err := refreshToken(ctx, rt)
	if nil != err {
		return nil, err
	}
	newFileContent := File{
		AccessToken:  refreshResult.AccessToken,
		RefreshToken: rt,
		ExpiresAt:    refreshResult.ExpiresAt,
	}
	if err := save(newFileContent); nil != err {
		return nil, err
	}
	return ptr.Of(Credentials(newFileContent)), nil
}

func save(content File) (err error) {
	f, err := os.OpenFile(tokenFilePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|os.O_SYNC, 0o0644)
	if nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return flaw.From(fmt.Errorf("failed to open token file: %w", err)).Append(flawP)
	}
	defer func() {
		if closeErr := f.Close(); nil != closeErr {
			flawP := flaw.P{"err_debug_tree": errutil.Tree(closeErr).FlawP()}
			closeErr = flaw.From(fmt.Errorf("failed to close token file: %w", closeErr)).Append(flawP)
			if nil != err {
				err = must.BeFlaw(err).Join(closeErr)
			} else {
				err = closeErr
			}
		}
	}()

	if err := json.NewEncoder(f).EncodeWithOption(content); nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return flaw.From(fmt.Errorf("failed to encode token file: %w", err)).Append(flawP)
	}
	return nil
}

type RefreshTokenResult struct {
	AccessToken string
	ExpiresAt   int64
}

func refreshToken(ctx context.Context, refreshToken string) (res *RefreshTokenResult, err error) {
	requestURL, err := url.JoinPath(baseURL, "/token")
	if nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return nil, flaw.From(fmt.Errorf("failed to create token verification URL: %v", err)).Append(flawP)
	}
	flawP := flaw.P{"url": requestURL}
	requestBodyParams := make(url.Values, 4)
	requestBodyParams.Add("client_id", clientID)
	requestBodyParams.Add("refresh_token", refreshToken)
	requestBodyParams.Add("grant_type", "refresh_token")
	requestBodyParams.Add("scope", "r_usr+w_usr+w_sub")
	requestBodyParamsStr := requestBodyParams.Encode()
	flawP["request_body_params"] = requestBodyParamsStr
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewBufferString(requestBodyParamsStr))
	if nil != err {
		if errutil.IsContext(ctx) {
			return nil, ctx.Err()
		}
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, flaw.From(fmt.Errorf("failed to create refresh token request: %v", err)).Append(flawP)
	}
	request.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Add("Authorization", "Basic "+base64.StdEncoding.Strict().EncodeToString([]byte(clientID+":"+clientSecret)))

	client := http.Client{Timeout: 5 * time.Second} //nolint:exhaustruct
	response, err := client.Do(request)
	if nil != err {
		switch {
		case errutil.IsContext(ctx):
			return nil, ctx.Err()
		case errors.Is(err, context.DeadlineExceeded):
			return nil, context.DeadlineExceeded
		default:
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return nil, flaw.From(fmt.Errorf("failed to issue refresh token request: %v", err)).Append(flawP)
		}
	}
	defer func() {
		if closeErr := response.Body.Close(); nil != closeErr {
			flawP["err_debug_tree"] = errutil.Tree(closeErr).FlawP()
			closeErr = flaw.From(fmt.Errorf("failed to close response body: %v", closeErr)).Append(flawP)
			switch {
			case nil == err:
				err = closeErr
			case errutil.IsContext(ctx):
				err = flaw.From(errors.New("context was ended")).Join(closeErr)
			case errors.Is(err, context.DeadlineExceeded):
				err = flaw.From(errors.New("timeout has reached")).Join(closeErr)
			case errors.Is(err, ErrUnauthorized):
				err = flaw.From(errors.New("received unauthorized error")).Join(closeErr)
			default:
				err = must.BeFlaw(err).Join(closeErr)
			}
		}
	}()
	flawP["response"] = errutil.HTTPResponseFlawPayload(response)

	switch code := response.StatusCode; code {
	case http.StatusOK:
	case http.StatusBadRequest:
		var responseBody struct {
			Status           int    `json:"status"`
			Error            string `json:"error"`
			SubStatus        int    `json:"sub_status"`
			ErrorDescription string `json:"error_description"`
		}
		if err := json.NewDecoder(response.Body).DecodeContext(ctx, &responseBody); nil != err {
			switch {
			case errutil.IsContext(ctx):
				return nil, ctx.Err()
			case errors.Is(err, context.DeadlineExceeded):
				return nil, context.DeadlineExceeded
			default:
				flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
				return nil, flaw.From(fmt.Errorf("failed to decode 400 status code response body: %v", err)).Append(flawP)
			}
		}
		if responseBody.Status == 400 && responseBody.SubStatus == 11101 && responseBody.Error == "invalid_grant" && responseBody.ErrorDescription == "Token could not be verified" {
			return nil, ErrUnauthorized
		}
		resBytes, err := io.ReadAll(response.Body)
		if nil != err {
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return nil, flaw.From(fmt.Errorf("failed to read 400 status code response body: %v", err)).Append(flawP)
		}
		flawP["response_body"] = string(resBytes)
		return nil, flaw.From(fmt.Errorf("unexpected response: %d %s", responseBody.Status, responseBody.ErrorDescription)).Append(flawP)
	default:
		resBytes, err := io.ReadAll(response.Body)
		if nil != err {
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return nil, flaw.From(fmt.Errorf("failed to read %d status code response body: %v", code, err)).Append(flawP)
		}
		flawP["response_body"] = string(resBytes)
		return nil, flaw.From(fmt.Errorf("unexpected status code: %d", code)).Append(flawP)
	}

	var responseBody struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(response.Body).DecodeContext(ctx, &responseBody); nil != err {
		switch {
		case errutil.IsContext(ctx):
			return nil, ctx.Err()
		case errors.Is(err, context.DeadlineExceeded):
			return nil, context.DeadlineExceeded
		default:
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return nil, flaw.From(fmt.Errorf("failed to decode 200 status code response body: %v", err)).Append(flawP)
		}
	}

	expiresAt, err := extractExpiresAt(responseBody.AccessToken)
	if nil != err {
		return nil, must.BeFlaw(err).Append(flawP)
	}

	return &RefreshTokenResult{
		AccessToken: responseBody.AccessToken,
		ExpiresAt:   expiresAt,
	}, nil
}

func extractExpiresAt(accessToken string) (int64, error) {
	splits := strings.SplitN(accessToken, ".", 3)
	if len(splits) != 3 {
		return 0, flaw.From(fmt.Errorf("unexpected access token format: %s", accessToken))
	}
	var obj struct {
		ExpiresAt int64 `json:"exp"`
	}
	if err := json.NewDecoder(base64.NewDecoder(base64.StdEncoding, strings.NewReader(splits[1]))).Decode(&obj); nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return 0, flaw.From(fmt.Errorf("failed to decode access token payload: %v", err)).Append(flawP)
	}
	return obj.ExpiresAt, nil
}

func (a *Auth) VerifyAccessToken(ctx context.Context) error {
	return verifyAccessToken(ctx, a.Creds.AccessToken)
}

func verifyAccessToken(ctx context.Context, accessToken string) (err error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.tidal.com/v1/sessions", nil)
	if nil != err {
		if errutil.IsContext(ctx) {
			return ctx.Err()
		}
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return flaw.From(fmt.Errorf("failed to create verify access token request: %w", err)).Append(flawP)
	}
	request.Header.Add("Authorization", "Bearer "+accessToken)

	client := http.Client{Timeout: 5 * time.Second} //nolint:exhaustruct
	response, err := client.Do(request)
	if nil != err {
		switch {
		case errutil.IsContext(ctx):
			return ctx.Err()
		case errors.Is(err, context.DeadlineExceeded):
			return context.DeadlineExceeded
		default:
			flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
			return flaw.From(fmt.Errorf("failed to issue verify access token request: %w", err)).Append(flawP)
		}
	}
	defer func() {
		if closeErr := response.Body.Close(); nil != closeErr {
			flawP := flaw.P{"err_debug_tree": errutil.Tree(closeErr).FlawP()}
			closeErr = flaw.From(fmt.Errorf("failed to close response body: %w", closeErr)).Append(flawP)
			switch {
			case nil == err:
				err = closeErr
			case errutil.IsContext(ctx):
				err = flaw.From(errors.New("context has ended")).Join(closeErr)
			case errors.Is(err, context.DeadlineExceeded):
				err = flaw.From(errors.New("timeout has reached")).Join(closeErr)
			case errors.Is(err, ErrUnauthorized):
				err = flaw.From(errors.New("received unauthorized error")).Join(closeErr)
			default:
				err = must.BeFlaw(err).Join(closeErr)
			}
		}
	}()
	flawP := flaw.P{"response": errutil.HTTPResponseFlawPayload(response)}

	switch code := response.StatusCode; code {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized:
		var responseBody struct {
			Status      int    `json:"status"`
			SubStatus   int    `json:"subStatus"`
			UserMessage string `json:"userMessage"`
		}
		if err := json.NewDecoder(response.Body).DecodeContext(ctx, &responseBody); nil != err {
			switch {
			case errutil.IsContext(ctx):
				return ctx.Err()
			case errors.Is(err, context.DeadlineExceeded):
				return context.DeadlineExceeded
			default:
				flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
				return flaw.From(fmt.Errorf("failed to decode 401 status code response body: %w", err)).Append(flawP)
			}
		}
		if responseBody.Status == 401 && responseBody.SubStatus == 11002 && responseBody.UserMessage == "Token could not be verified" {
			return ErrUnauthorized
		}
		resBytes, err := io.ReadAll(response.Body)
		if nil != err {
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return flaw.From(fmt.Errorf("failed to read 401 status code response body: %w", err)).Append(flawP)
		}
		flawP["response_body"] = string(resBytes)
		return flaw.From(fmt.Errorf("unexpected response: %d %s", responseBody.Status, responseBody.UserMessage)).Append(flawP)
	default:
		resBytes, err := io.ReadAll(response.Body)
		if nil != err {
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return flaw.From(fmt.Errorf("failed to read %d status code response body: %w", code, err)).Append(flawP)
		}
		flawP["response_body"] = string(resBytes)
		return flaw.From(fmt.Errorf("unexpected status code: %d", code)).Append(flawP)
	}
}

type authorizationResponse struct {
	URL        string
	DeviceCode string
	ExpiresIn  int
	Interval   int
}

var ErrAuthWaitTimeout = errors.New("authorization wait timeout")

func NewAuthorizer(ctx context.Context) (link string, wait <-chan result.Of[Auth], err error) {
	res, err := issueAuthorizationRequest(ctx)
	if nil != err {
		return "", nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, time.Duration(res.ExpiresIn)*time.Second)
	ticker := time.NewTicker(time.Duration(res.Interval) * time.Second * 5)
	done := make(chan result.Of[Auth])

	go func() {
		defer close(done)
		defer ticker.Stop()
		defer cancel()
	waitloop:
		for {
			select {
			case <-ctx.Done():
				err := ctx.Err()
				if errors.Is(err, context.DeadlineExceeded) {
					done <- result.Err[Auth](ErrAuthWaitTimeout)
					return
				}
				flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
				done <- result.Err[Auth](flaw.From(fmt.Errorf("authorization wait context errored with unknown error: %v", err)).Append(flawP))
				return
			case <-ticker.C:
				creds, err := res.poll(ctx)
				if nil != err {
					switch {
					case errors.Is(ctx.Err(), context.Canceled):
						done <- result.Err[Auth](context.Canceled)
						return
					case errors.Is(err, context.Canceled):
						panic("Unexpected poller context cancellation when an error is already returned from it")
					case errors.Is(err, context.DeadlineExceeded):
						// The poller has timed out, not the auth-wait context
						done <- result.Err[Auth](flaw.From(errors.New("failed to poll authorization status due to timeout")))
						return
					case errors.Is(err, ErrUnauthorized):
						continue waitloop
					case errutil.IsFlaw(err):
						flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
						done <- result.Err[Auth](must.BeFlaw(err).Append(flawP))
						return
					default:
						panic(errutil.UnknownError(err))
					}
				}
				f := File(*creds)
				flawP := flaw.P{"creds": f.flawP()}
				if err := save(f); nil != err {
					flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
					done <- result.Err[Auth](must.BeFlaw(err).Append(flawP))
					return
				}
				done <- result.Ok(&Auth{Creds: *creds})
				return
			}
		}
	}()

	return res.URL, done, nil
}

func issueAuthorizationRequest(ctx context.Context) (out *authorizationResponse, err error) {
	requestURL, err := url.JoinPath(baseURL, "/device_authorization")
	if nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return nil, flaw.From(fmt.Errorf("failed to create device authorization URL: %v", err)).Append(flawP)
	}
	flawP := flaw.P{"url": requestURL}
	requestBodyParams := make(url.Values, 2)
	requestBodyParams.Add("client_id", clientID)
	requestBodyParams.Add("scope", "r_usr+w_usr+w_sub")
	requestBodyParamsStr := requestBodyParams.Encode()
	flawP["request_body_params"] = requestBodyParamsStr
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewBufferString(requestBodyParamsStr))
	if nil != err {
		if errutil.IsContext(ctx) {
			return nil, ctx.Err()
		}
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, flaw.From(fmt.Errorf("failed to create device authorization request: %v", err)).Append(flawP)
	}
	request.Header.Add("Content-Type", "application/x-www-form-urlencoded")

	client := http.Client{Timeout: 5 * time.Second} //nolint:exhaustruct
	response, err := client.Do(request)
	if nil != err {
		switch {
		case errutil.IsContext(ctx):
			return nil, ctx.Err()
		case errors.Is(err, context.DeadlineExceeded):
			return nil, context.DeadlineExceeded
		default:
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return nil, flaw.From(fmt.Errorf("failed to issue device authorization request: %v", err)).Append(flawP)
		}
	}
	defer func() {
		if closeErr := response.Body.Close(); nil != closeErr {
			flawP["err_debug_tree"] = errutil.Tree(closeErr).FlawP()
			closeErr = flaw.From(fmt.Errorf("failed to close response body: %v", closeErr)).Append(flawP)
			switch {
			case nil == err:
				err = closeErr
			case errutil.IsContext(ctx):
				err = flaw.From(errors.New("context was ended")).Join(closeErr)
			case errors.Is(err, context.DeadlineExceeded):
				err = flaw.From(errors.New("timeout has reached")).Join(closeErr)
			default:
				err = must.BeFlaw(err).Join(closeErr)
			}
		}
	}()
	flawP["response"] = errutil.HTTPResponseFlawPayload(response)

	if response.StatusCode != http.StatusOK {
		resBytes, err := io.ReadAll(response.Body)
		if nil != err {
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return nil, flaw.From(fmt.Errorf("failed to read response body: %v", err)).Append(flawP)
		}
		flawP["response_body"] = string(resBytes)
		return nil, flaw.From(fmt.Errorf("unexpected status code: %d", response.StatusCode)).Append(flawP)
	}

	var responseBody struct {
		DeviceCode      string `json:"deviceCode"`
		UserCode        string `json:"userCode"`
		VerificationURI string `json:"verificationUri"`
		ExpiresIn       int    `json:"expiresIn"`
		Interval        int    `json:"interval"`
	}
	if err := json.NewDecoder(response.Body).DecodeContext(ctx, &responseBody); nil != err {
		switch {
		case errutil.IsContext(ctx):
			return nil, ctx.Err()
		case errors.Is(err, context.DeadlineExceeded):
			return nil, context.DeadlineExceeded
		default:
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return nil, flaw.From(fmt.Errorf("failed to decode response body: %v", err)).Append(flawP)
		}
	}
	flawP["response_body"] = flaw.P{
		"device_code":      responseBody.DeviceCode,
		"user_code":        responseBody.UserCode,
		"verification_uri": responseBody.VerificationURI,
		"expires_in":       responseBody.ExpiresIn,
		"interval":         responseBody.Interval,
	}

	//nolint:exhaustruct
	authorizationURL := url.URL{
		Scheme: "https",
		Host:   responseBody.VerificationURI,
		Path:   responseBody.UserCode,
	}
	authorizationURLStr := authorizationURL.String()
	flawP["authorizationURL"] = authorizationURLStr
	return &authorizationResponse{
		URL:        authorizationURLStr,
		DeviceCode: responseBody.DeviceCode,
		ExpiresIn:  responseBody.ExpiresIn,
		Interval:   responseBody.Interval,
	}, nil
}

func (r *authorizationResponse) poll(ctx context.Context) (creds *Credentials, err error) {
	// Create a detached context which is only canceled when parent context is canceled, not when it's deadline exceeded.
	pollCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-ctx.Done():
			err := ctx.Err()
			switch {
			case errors.Is(err, context.DeadlineExceeded):
				// Ignore
			case errors.Is(err, context.Canceled):
				cancel()
				return
			default:
				panic("unexpected error value for ended parent context:" + err.Error())
			}
		case <-pollCtx.Done():
			// When outer function returns
			return
		}
	}()

	requestURL, err := url.JoinPath(baseURL, "/token")
	if nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return nil, flaw.From(fmt.Errorf("failed to create token URL: %w", err)).Append(flawP)
	}
	flawP := flaw.P{"url": requestURL}
	requestBodyParams := make(url.Values, 4)
	requestBodyParams.Add("client_id", clientID)
	requestBodyParams.Add("scope", "r_usr+w_usr+w_sub")
	requestBodyParams.Add("grant_type", "urn:ietf:params:oauth:grant-type:device_code")
	requestBodyParams.Add("device_code", r.DeviceCode)
	requestBodyStr := requestBodyParams.Encode()
	flawP["request_body_str"] = requestBodyStr
	request, err := http.NewRequestWithContext(pollCtx, http.MethodPost, requestURL, bytes.NewBufferString(requestBodyStr))
	if nil != err {
		if errutil.IsContext(pollCtx) {
			return nil, pollCtx.Err()
		}
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, flaw.From(fmt.Errorf("failed to create token request: %v", err)).Append(flawP)
	}
	request.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Add("Authorization", "Basic "+base64.StdEncoding.Strict().EncodeToString([]byte(clientID+":"+clientSecret)))

	client := http.Client{Timeout: 5 * time.Second} //nolint:exhaustruct
	response, err := client.Do(request)
	if nil != err {
		switch {
		case errutil.IsContext(pollCtx):
			return nil, pollCtx.Err()
		case errors.Is(err, context.DeadlineExceeded):
			return nil, context.DeadlineExceeded
		default:
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return nil, flaw.From(fmt.Errorf("failed to issue token request: %v", err)).Append(flawP)
		}
	}
	defer func() {
		if closeErr := response.Body.Close(); nil != closeErr {
			flawP["err_debug_tree"] = errutil.Tree(closeErr).FlawP()
			closeErr = flaw.From(fmt.Errorf("failed to close response body: %v", closeErr)).Append(flawP)
			switch {
			case nil == err:
				err = closeErr
			case errutil.IsContext(pollCtx):
				err = flaw.From(errors.New("context was ended")).Join(closeErr)
			case errors.Is(err, context.DeadlineExceeded):
				err = flaw.From(errors.New("timeout has reached")).Join(closeErr)
			case errors.Is(err, ErrUnauthorized):
				err = flaw.From(errors.New("received unauthorized error")).Join(closeErr)
			default:
				err = must.BeFlaw(err).Join(closeErr)
			}
		}
	}()
	flawP["response"] = errutil.HTTPResponseFlawPayload(response)

	switch code := response.StatusCode; code {
	case http.StatusOK:
	case http.StatusBadRequest:
		var responseBody struct {
			Status           int    `json:"status"`
			Error            string `json:"error"`
			SubStatus        int    `json:"sub_status"`
			ErrorDescription string `json:"error_description"`
		}
		if err := json.NewDecoder(response.Body).DecodeContext(pollCtx, &responseBody); nil != err {
			switch {
			case errutil.IsContext(pollCtx):
				return nil, pollCtx.Err()
			case errors.Is(err, context.DeadlineExceeded):
				return nil, context.DeadlineExceeded
			default:
				flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
				return nil, flaw.From(fmt.Errorf("failed to decode 400 status code response body: %v", err)).Append(flawP)
			}
		}
		if responseBody.Status == 400 && responseBody.Error == "authorization_pending" && responseBody.SubStatus == 1002 && responseBody.ErrorDescription == "Device Authorization code is not authorized yet" {
			return nil, ErrUnauthorized
		}
		resBytes, err := io.ReadAll(response.Body)
		if nil != err {
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return nil, flaw.From(fmt.Errorf("failed to read 400 status code response body: %v", err)).Append(flawP)
		}
		flawP["response_body"] = string(resBytes)
		return nil, flaw.From(fmt.Errorf("unexpected response: %d %s", responseBody.Status, responseBody.Error)).Append(flawP)
	default:
		resBytes, err := io.ReadAll(response.Body)
		if nil != err {
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return nil, flaw.From(fmt.Errorf("failed to read %d status code response body: %v", code, err)).Append(flawP)
		}
		flawP["response_body"] = string(resBytes)
		return nil, flaw.From(fmt.Errorf("unexpected status code: %d", code)).Append(flawP)
	}

	var responseBody struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.NewDecoder(response.Body).DecodeContext(pollCtx, &responseBody); nil != err {
		switch {
		case errors.Is(pollCtx.Err(), context.Canceled):
			return nil, pollCtx.Err()
		case errors.Is(err, context.DeadlineExceeded):
			return nil, context.DeadlineExceeded
		default:
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return nil, flaw.From(fmt.Errorf("failed to decode 200 status code response body: %v", err)).Append(flawP)
		}
	}

	expiresAt, err := extractExpiresAt(responseBody.AccessToken)
	if nil != err {
		return nil, err
	}

	return &Credentials{
		AccessToken:  responseBody.AccessToken,
		RefreshToken: responseBody.RefreshToken,
		ExpiresAt:    expiresAt,
	}, nil
}
