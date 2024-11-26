package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/xeptore/flaw/v8"

	"github.com/xeptore/tgtd/errutil"
	"github.com/xeptore/tgtd/httputil"
	"github.com/xeptore/tgtd/log"
	"github.com/xeptore/tgtd/must"
	"github.com/xeptore/tgtd/ptr"
	"github.com/xeptore/tgtd/result"
	tidalfs "github.com/xeptore/tgtd/tidal/fs"
)

const (
	clientID      = "zU4XHVVkc2tDPo4t"
	clientSecret  = "VJKhDFqJPqvsPVNBV6ukXTJmwlvbttP7wlMlrc72se4=" //nolint:gosec
	baseURL       = "https://auth.tidal.com/v1/oauth2"
	tokenFileName = "token.json"
)

var ErrUnauthorized = errors.New("Unauthorized")

type Credentials struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64
}

func credentialsFromTokenFileContent(content tidalfs.AuthTokenFileContent) Credentials {
	return Credentials{
		AccessToken:  content.AccessToken,
		RefreshToken: content.RefreshToken,
		ExpiresAt:    content.ExpiresAt,
	}
}

type Auth struct {
	file  tidalfs.AuthTokenFile
	creds Credentials
}

func (a *Auth) AccessToken(ctx context.Context) (string, error) {
	if time.Now().After(time.Unix(a.creds.ExpiresAt, 0)) {
		creds, err := handleUnauthorized(ctx, a.creds.RefreshToken, a.file)
		if nil != err {
			return "", err
		}
		a.creds = *creds
		return a.creds.AccessToken, nil
	}

	return a.creds.AccessToken, nil
}

func (a *Auth) RefreshToken(ctx context.Context) error {
	creds, err := handleUnauthorized(ctx, a.creds.RefreshToken, a.file)
	if nil != err {
		return err
	}
	a.creds = *creds
	return nil
}

func LoadFromDir(ctx context.Context, credsDir string) (*Auth, error) {
	tokenFile := tidalfs.AuthTokenFileFrom(credsDir, tokenFileName)
	creds, err := load(ctx, tokenFile)
	if nil != err {
		return nil, err
	}
	return &Auth{
		file:  tokenFile,
		creds: *creds,
	}, nil
}

func load(ctx context.Context, tokenFile tidalfs.AuthTokenFile) (creds *Credentials, err error) {
	content, err := tokenFile.Read()
	if nil != err {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrUnauthorized
		}
		return nil, err
	}

	if time.Now().After(time.Unix(content.ExpiresAt, 0)) {
		return handleUnauthorized(ctx, content.RefreshToken, tokenFile)
	}

	if err := verifyAccessToken(ctx, content.AccessToken); nil != err {
		switch {
		case errutil.IsContext(ctx):
			return nil, ctx.Err()
		case errors.Is(err, context.DeadlineExceeded):
			return nil, context.DeadlineExceeded
		case errors.Is(err, ErrUnauthorized):
			return handleUnauthorized(ctx, content.RefreshToken, tokenFile)
		case errutil.IsFlaw(err):
			return nil, err
		default:
			panic(errutil.UnknownError(err))
		}
	}
	return ptr.Of(credentialsFromTokenFileContent(*content)), nil
}

func handleUnauthorized(ctx context.Context, rt string, tokenFile tidalfs.AuthTokenFile) (*Credentials, error) {
	refreshResult, err := refreshToken(ctx, rt)
	if nil != err {
		return nil, err
	}
	newFileContent := tidalfs.AuthTokenFileContent{
		AccessToken:  refreshResult.AccessToken,
		RefreshToken: rt,
		ExpiresAt:    refreshResult.ExpiresAt,
	}
	if err := tokenFile.Write(newFileContent); nil != err {
		return nil, err
	}
	return ptr.Of(Credentials(newFileContent)), nil
}

type RefreshTokenResult struct {
	AccessToken string
	ExpiresAt   int64
}

func refreshToken(ctx context.Context, refreshToken string) (res *RefreshTokenResult, err error) {
	reqURL, err := url.JoinPath(baseURL, "/token")
	if nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return nil, flaw.From(fmt.Errorf("failed to create token verification URL: %v", err)).Append(flawP)
	}
	flawP := flaw.P{"url": reqURL}

	reqParams := make(url.Values, 4)
	reqParams.Add("client_id", clientID)
	reqParams.Add("refresh_token", refreshToken)
	reqParams.Add("grant_type", "refresh_token")
	reqParams.Add("scope", "r_usr+w_usr+w_sub")
	reqParamsStr := reqParams.Encode()
	flawP["request_params"] = reqParamsStr

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewBufferString(reqParamsStr))
	if nil != err {
		if errutil.IsContext(ctx) {
			return nil, ctx.Err()
		}

		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, flaw.From(fmt.Errorf("failed to create refresh token request: %v", err)).Append(flawP)
	}
	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Add("Authorization", "Basic "+base64.StdEncoding.Strict().EncodeToString([]byte(clientID+":"+clientSecret)))

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
			return nil, flaw.From(fmt.Errorf("failed to issue refresh token request: %v", err)).Append(flawP)
		}
	}
	defer func() {
		if closeErr := resp.Body.Close(); nil != closeErr {
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
	flawP["response"] = errutil.HTTPResponseFlawPayload(resp)

	switch code := resp.StatusCode; code {
	case http.StatusOK:
	case http.StatusUnauthorized:
		respBytes, err := httputil.ReadResponseBody(ctx, resp)
		if nil != err {
			return nil, err
		}

		if ok, err := httputil.IsTokenExpiredUnauthorizedResponse(respBytes); nil != err {
			return nil, err
		} else if ok {
			return nil, ErrUnauthorized
		}

		if ok, err := httputil.IsTokenInvalidUnauthorizedResponse(respBytes); nil != err {
			return nil, err
		} else if ok {
			return nil, ErrUnauthorized
		}

		flawP["response_body"] = string(respBytes)
		return nil, flaw.From(errors.New("received 401 response")).Append(flawP)
	case http.StatusBadRequest:
		respBytes, err := httputil.ReadResponseBody(ctx, resp)
		if nil != err {
			return nil, err
		}
		var respBody struct {
			Status           int    `json:"status"`
			Error            string `json:"error"`
			SubStatus        int    `json:"sub_status"`
			ErrorDescription string `json:"error_description"`
		}
		if err := json.Unmarshal(respBytes, &respBody); nil != err {
			flawP["response_body"] = string(respBytes)
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return nil, flaw.From(fmt.Errorf("failed to decode 400 status code response body: %v", err)).Append(flawP)
		}
		if respBody.Status == 400 && respBody.SubStatus == 11101 && respBody.Error == "invalid_grant" && respBody.ErrorDescription == "Token could not be verified" {
			return nil, ErrUnauthorized
		}

		flawP["response_body"] = string(respBytes)
		return nil, flaw.From(errors.New("unexpected 400 response")).Append(flawP)
	default:
		respBytes, err := httputil.ReadOptionalResponseBody(ctx, resp)
		if nil != err {
			return nil, err
		}
		flawP["response_body"] = string(respBytes)
		return nil, flaw.From(fmt.Errorf("unexpected status code: %d", code)).Append(flawP)
	}

	respBytes, err := httputil.ReadResponseBody(ctx, resp)
	if nil != err {
		return nil, err
	}
	var respBody struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(respBytes, &respBody); nil != err {
		flawP["response_body"] = string(respBytes)
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, flaw.From(fmt.Errorf("failed to decode 200 status code response body: %v", err)).Append(flawP)
	}

	expiresAt, err := extractExpiresAt(respBody.AccessToken)
	if nil != err {
		flawP["access_token"] = log.RedactString(respBody.AccessToken)
		return nil, must.BeFlaw(err).Append(flawP)
	}

	return &RefreshTokenResult{
		AccessToken: respBody.AccessToken,
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
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP(), "2nd_split": splits[1]}
		return 0, flaw.From(fmt.Errorf("failed to decode access token payload: %v", err)).Append(flawP)
	}
	return obj.ExpiresAt, nil
}

func (a *Auth) VerifyAccessToken(ctx context.Context) error {
	return verifyAccessToken(ctx, a.creds.AccessToken)
}

func verifyAccessToken(ctx context.Context, accessToken string) (err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.tidal.com/v1/sessions", nil)
	if nil != err {
		if errutil.IsContext(ctx) {
			return ctx.Err()
		}
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return flaw.From(fmt.Errorf("failed to create verify access token request: %v", err)).Append(flawP)
	}
	req.Header.Add("Authorization", "Bearer "+accessToken)

	client := http.Client{Timeout: 5 * time.Second} //nolint:exhaustruct
	resp, err := client.Do(req)
	if nil != err {
		switch {
		case errutil.IsContext(ctx):
			return ctx.Err()
		case errors.Is(err, context.DeadlineExceeded):
			return context.DeadlineExceeded
		default:
			flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
			return flaw.From(fmt.Errorf("failed to issue verify access token request: %v", err)).Append(flawP)
		}
	}
	defer func() {
		if closeErr := resp.Body.Close(); nil != closeErr {
			flawP := flaw.P{"err_debug_tree": errutil.Tree(closeErr).FlawP()}
			closeErr = flaw.From(fmt.Errorf("failed to close response body: %v", closeErr)).Append(flawP)
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
	flawP := flaw.P{"response": errutil.HTTPResponseFlawPayload(resp)}

	switch code := resp.StatusCode; code {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized:
		respBytes, err := httputil.ReadResponseBody(ctx, resp)
		if nil != err {
			return err
		}

		if ok, err := httputil.IsTokenExpiredUnauthorizedResponse(respBytes); nil != err {
			return err
		} else if ok {
			return ErrUnauthorized
		}

		if ok, err := httputil.IsTokenInvalidUnauthorizedResponse(respBytes); nil != err {
			return err
		} else if ok {
			return ErrUnauthorized
		}

		flawP["response_body"] = string(respBytes)
		return flaw.From(errors.New("received 401 response")).Append(flawP)
	default:
		respBytes, err := httputil.ReadOptionalResponseBody(ctx, resp)
		if nil != err {
			return err
		}
		flawP["response_body"] = string(respBytes)
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

type AuthorizationResult struct {
	URL       string
	ExpiresIn time.Duration
}

func NewAuthorizer(ctx context.Context, credsDir string) (link *AuthorizationResult, wait <-chan result.Of[Auth], err error) {
	res, err := issueAuthorizationRequest(ctx)
	if nil != err {
		return nil, nil, err
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
				content := tidalfs.AuthTokenFileContent(*creds)
				tokenFile := tidalfs.AuthTokenFileFrom(credsDir, tokenFileName)
				if err := tokenFile.Write(content); nil != err {
					flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
					done <- result.Err[Auth](must.BeFlaw(err).Append(flawP))
					return
				}
				done <- result.Ok(&Auth{
					file:  tokenFile,
					creds: *creds,
				})
				return
			}
		}
	}()

	return &AuthorizationResult{
		URL:       res.URL,
		ExpiresIn: time.Duration(res.ExpiresIn) * time.Second,
	}, done, nil
}

func issueAuthorizationRequest(ctx context.Context) (out *authorizationResponse, err error) {
	reqURL, err := url.JoinPath(baseURL, "/device_authorization")
	if nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return nil, flaw.From(fmt.Errorf("failed to create device authorization URL: %v", err)).Append(flawP)
	}
	flawP := flaw.P{"url": reqURL}

	reqParams := make(url.Values, 2)
	reqParams.Add("client_id", clientID)
	reqParams.Add("scope", "r_usr+w_usr+w_sub")
	reqParamsStr := reqParams.Encode()
	flawP["request_body_params"] = reqParamsStr

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewBufferString(reqParamsStr))
	if nil != err {
		if errutil.IsContext(ctx) {
			return nil, ctx.Err()
		}

		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, flaw.From(fmt.Errorf("failed to create device authorization request: %v", err)).Append(flawP)
	}
	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")

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
			return nil, flaw.From(fmt.Errorf("failed to issue device authorization request: %v", err)).Append(flawP)
		}
	}
	defer func() {
		if closeErr := resp.Body.Close(); nil != closeErr {
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
	flawP["response"] = errutil.HTTPResponseFlawPayload(resp)

	if resp.StatusCode != http.StatusOK {
		respBytes, err := httputil.ReadOptionalResponseBody(ctx, resp)
		if nil != err {
			return nil, err
		}
		flawP["response_body"] = string(respBytes)
		return nil, flaw.From(fmt.Errorf("unexpected status code: %d", resp.StatusCode)).Append(flawP)
	}

	respBytes, err := httputil.ReadResponseBody(ctx, resp)
	if nil != err {
		return nil, err
	}
	var respBody struct {
		DeviceCode      string `json:"deviceCode"`
		UserCode        string `json:"userCode"`
		VerificationURI string `json:"verificationUri"`
		ExpiresIn       int    `json:"expiresIn"`
		Interval        int    `json:"interval"`
	}
	if err := json.Unmarshal(respBytes, &respBody); nil != err {
		flawP["response_body"] = string(respBytes)
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, flaw.From(fmt.Errorf("failed to decode response body: %v", err)).Append(flawP)
	}
	flawP["response_body"] = flaw.P{
		"device_code":      respBody.DeviceCode,
		"user_code":        respBody.UserCode,
		"verification_uri": respBody.VerificationURI,
		"expires_in":       respBody.ExpiresIn,
		"interval":         respBody.Interval,
	}

	//nolint:exhaustruct
	authorizationURL := url.URL{
		Scheme: "https",
		Host:   respBody.VerificationURI,
		Path:   respBody.UserCode,
	}
	authorizationURLStr := authorizationURL.String()
	flawP["authorization_url"] = authorizationURLStr
	return &authorizationResponse{
		URL:        authorizationURLStr,
		DeviceCode: respBody.DeviceCode,
		ExpiresIn:  respBody.ExpiresIn,
		Interval:   respBody.Interval,
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

	reqURL, err := url.JoinPath(baseURL, "/token")
	if nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return nil, flaw.From(fmt.Errorf("failed to create token URL: %v", err)).Append(flawP)
	}
	flawP := flaw.P{"url": reqURL}

	reqParams := make(url.Values, 4)
	reqParams.Add("client_id", clientID)
	reqParams.Add("scope", "r_usr+w_usr+w_sub")
	reqParams.Add("grant_type", "urn:ietf:params:oauth:grant-type:device_code")
	reqParams.Add("device_code", r.DeviceCode)
	reqParamsStr := reqParams.Encode()
	flawP["request_body_str"] = reqParamsStr

	req, err := http.NewRequestWithContext(pollCtx, http.MethodPost, reqURL, bytes.NewBufferString(reqParamsStr))
	if nil != err {
		if errutil.IsContext(pollCtx) {
			return nil, pollCtx.Err()
		}

		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, flaw.From(fmt.Errorf("failed to create token request: %v", err)).Append(flawP)
	}
	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Add("Authorization", "Basic "+base64.StdEncoding.Strict().EncodeToString([]byte(clientID+":"+clientSecret)))

	client := http.Client{Timeout: 5 * time.Second} //nolint:exhaustruct
	resp, err := client.Do(req)
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
		if closeErr := resp.Body.Close(); nil != closeErr {
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
	flawP["response"] = errutil.HTTPResponseFlawPayload(resp)

	switch code := resp.StatusCode; code {
	case http.StatusOK:
	case http.StatusBadRequest:
		respBytes, err := httputil.ReadResponseBody(ctx, resp)
		if nil != err {
			return nil, err
		}
		var respBody struct {
			Status           int    `json:"status"`
			Error            string `json:"error"`
			SubStatus        int    `json:"sub_status"`
			ErrorDescription string `json:"error_description"`
		}
		if err := json.Unmarshal(respBytes, &respBody); nil != err {
			flawP["response_body"] = string(respBytes)
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return nil, flaw.From(fmt.Errorf("failed to decode 400 status code response body: %v", err)).Append(flawP)
		}
		if respBody.Status == 400 && respBody.Error == "authorization_pending" && respBody.SubStatus == 1002 && respBody.ErrorDescription == "Device Authorization code is not authorized yet" {
			return nil, ErrUnauthorized
		}

		flawP["response_body"] = string(respBytes)
		return nil, flaw.From(errors.New("unexpected 400 response")).Append(flawP)
	default:
		respBytes, err := httputil.ReadOptionalResponseBody(ctx, resp)
		if nil != err {
			return nil, err
		}
		flawP["response_body"] = string(respBytes)
		return nil, flaw.From(fmt.Errorf("unexpected status code: %d", code)).Append(flawP)
	}

	respBytes, err := httputil.ReadResponseBody(ctx, resp)
	if nil != err {
		return nil, err
	}
	var respBody struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(respBytes, &respBody); nil != err {
		flawP["response_body"] = string(respBytes)
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, flaw.From(fmt.Errorf("failed to decode 200 status code response body: %v", err)).Append(flawP)
	}

	expiresAt, err := extractExpiresAt(respBody.AccessToken)
	if nil != err {
		flawP["response_body"] = string(respBytes)
		return nil, must.BeFlaw(err).Append(flawP)
	}

	return &Credentials{
		AccessToken:  respBody.AccessToken,
		RefreshToken: respBody.RefreshToken,
		ExpiresAt:    expiresAt,
	}, nil
}
