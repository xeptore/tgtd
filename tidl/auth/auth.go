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

	"github.com/xeptore/tgtd/ptr"
)

const (
	clientID      = "7m7Ap0JC9j1cOM3n"
	clientSecret  = "vRAdA108tlvkJpTsGZS8rGZ7xTlbJ0qaZ2K9saEzsgY="
	baseURL       = "https://auth.tidal.com/v1/oauth2"
	tokenFilePath = "token.json"
)

var (
	ErrUnauthorized = errors.New("Unauthorized")
)

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

func load(ctx context.Context) (*Credentials, error) {
	f, err := os.OpenFile(tokenFilePath, os.O_RDONLY, 0o0644)
	if nil != err {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		return nil, ErrUnauthorized
	}
	var content File
	if err := json.NewDecoder(f).DecodeContext(ctx, &content); nil != err {
		return nil, err
	}
	if time.Now().Unix() > content.ExpiresAt {
		return handleUnauthorized(ctx, content.RefreshToken)
	}
	if err := verifyAccessToken(ctx, content.AccessToken); nil != err {
		if errors.Is(err, ErrUnauthorized) {
			return handleUnauthorized(ctx, content.RefreshToken)
		}
		return nil, err
	}
	return ptr.Of(Credentials(content)), nil
}

func handleUnauthorized(ctx context.Context, rt string) (*Credentials, error) {
	refreshResult, err := refreshToken(ctx, rt)
	if nil != err {
		if errors.Is(err, ErrUnauthorized) {
			return nil, ErrUnauthorized
		}
		return nil, err
	}
	newFileContent := File{
		AccessToken:  refreshResult.AccessToken,
		RefreshToken: rt,
		ExpiresAt:    refreshResult.ExpiresAt,
	}
	if err := save(ctx, newFileContent); nil != err {
		return nil, err
	}
	return ptr.Of(Credentials(newFileContent)), nil
}

func save(ctx context.Context, content File) error {
	f, err := os.OpenFile(tokenFilePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|os.O_SYNC, 0o0644)
	if nil != err {
		return err
	}
	if err := json.NewEncoder(f).EncodeContext(ctx, content); nil != err {
		return err
	}
	return nil
}

type RefreshTokenResult struct {
	AccessToken string
	ExpiresAt   int64
}

func refreshToken(ctx context.Context, refreshToken string) (*RefreshTokenResult, error) {
	requestURL, err := url.JoinPath(baseURL, "/token")
	if nil != err {
		return nil, err
	}
	requestBodyParams := make(url.Values, 4)
	requestBodyParams.Add("client_id", clientID)
	requestBodyParams.Add("refresh_token", refreshToken)
	requestBodyParams.Add("grant_type", "refresh_token")
	requestBodyParams.Add("scope", "r_usr+w_usr+w_sub")
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewBufferString(requestBodyParams.Encode()))
	if nil != err {
		return nil, err
	}
	request.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Add("Authorization", "Basic "+base64.StdEncoding.Strict().EncodeToString([]byte(clientID+":"+clientSecret)))

	client := http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(request)
	if nil != err {
		return nil, err
	}
	defer func() {
		if closeErr := response.Body.Close(); nil != closeErr {
			err = closeErr
		}
	}()

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
			return nil, err
		}
		if responseBody.Status == 400 && responseBody.SubStatus == 11101 && responseBody.Error == "invalid_grant" && responseBody.ErrorDescription == "Token could not be verified" {
			return nil, ErrUnauthorized
		}
		return nil, fmt.Errorf("unexpected response: %d %s", responseBody.Status, responseBody.ErrorDescription)
	default:
		return nil, fmt.Errorf("unexpected status code: %d", code)
	}

	var responseBody struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(response.Body).DecodeContext(ctx, &responseBody); nil != err {
		return nil, err
	}
	expiresAt, err := extractExpiresAt(ctx, responseBody.AccessToken)
	if nil != err {
		return nil, err
	}

	return &RefreshTokenResult{
		AccessToken: responseBody.AccessToken,
		ExpiresAt:   expiresAt,
	}, nil
}

func extractExpiresAt(ctx context.Context, accessToken string) (int64, error) {
	splits := strings.SplitN(accessToken, ".", 3)
	if len(splits) != 3 {
		return 0, fmt.Errorf("shit")
	}
	var obj struct {
		ExpiresAt int64 `json:"exp"`
	}
	if err := json.NewDecoder(base64.NewDecoder(base64.StdEncoding, strings.NewReader(splits[1]))).DecodeContext(ctx, &obj); nil != err {
		return 0, err
	}
	return obj.ExpiresAt, nil
}

func (a *Auth) VerifyAccessToken(ctx context.Context) error {
	return verifyAccessToken(ctx, a.Creds.AccessToken)
}

func verifyAccessToken(ctx context.Context, accessToken string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.tidal.com/v1/sessions", nil)
	if nil != err {
		return err
	}
	request.Header.Add("Authorization", "Bearer "+accessToken)

	client := http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(request)
	if nil != err {
		return err
	}
	defer func() {
		if closeErr := response.Body.Close(); nil != closeErr {
			err = closeErr
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
			return err
		}
		if responseBody.Status == 401 && responseBody.SubStatus == 11002 && responseBody.UserMessage == "Token could not be verified" {
			return ErrUnauthorized
		}
		return fmt.Errorf("unexpected response: %d %s", responseBody.Status, responseBody.UserMessage)
	default:
		return fmt.Errorf("unexpected status code: %d", code)
	}

	return nil
}

type authorizationResponse struct {
	URL        string
	DeviceCode string
	ExpiresIn  int
	Interval   int
}

type AuthorizationResult struct {
	Auth *Auth
	Err  error
}

func NewAuthorizer(ctx context.Context) (link string, wait <-chan AuthorizationResult, err error) {
	res, err := issueAuthorizationRequest(ctx)
	if nil != err {
		return "", nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, time.Duration(res.ExpiresIn)*time.Second)
	ticker := time.NewTicker(time.Duration(res.Interval) * time.Second * 5)
	done := make(chan AuthorizationResult)

	go func() {
		defer close(done)
		defer ticker.Stop()
		defer cancel()
	waitloop:
		for {
			select {
			case <-ctx.Done():
				done <- AuthorizationResult{Err: ctx.Err()}
				return
			case <-ticker.C:
				creds, err := res.poll(ctx)
				if nil != err {
					if errors.Is(err, ErrUnauthorized) {
						continue waitloop
					}
					done <- AuthorizationResult{Err: err}
					return
				}
				if err := save(ctx, File(*creds)); nil != err {
					done <- AuthorizationResult{Err: err}
					return
				}
				done <- AuthorizationResult{Auth: &Auth{Creds: *creds}}
				return
			}
		}
	}()

	return res.URL, done, nil
}

func issueAuthorizationRequest(ctx context.Context) (out *authorizationResponse, err error) {
	requestURL, err := url.JoinPath(baseURL, "/device_authorization")
	if nil != err {
		return nil, err
	}
	requestBodyParams := make(url.Values, 2)
	requestBodyParams.Add("client_id", clientID)
	requestBodyParams.Add("scope", "r_usr+w_usr+w_sub")
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewBufferString(requestBodyParams.Encode()))
	if nil != err {
		return nil, err
	}
	request.Header.Add("Content-Type", "application/x-www-form-urlencoded")

	client := http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(request)
	if nil != err {
		return nil, err
	}
	defer func() {
		if closeErr := response.Body.Close(); nil != closeErr {
			err = closeErr
		}
	}()

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("TODO")
	}

	var responseBody struct {
		DeviceCode      string `json:"deviceCode"`
		UserCode        string `json:"userCode"`
		VerificationURI string `json:"verificationUri"`
		ExpiresIn       int    `json:"expiresIn"`
		Interval        int    `json:"interval"`
	}
	if err := json.NewDecoder(response.Body).DecodeContext(ctx, &responseBody); nil != err {
		return nil, err
	}

	authorizationURL := url.URL{
		Scheme: "https",
		Host:   responseBody.VerificationURI,
		Path:   responseBody.UserCode,
	}
	return &authorizationResponse{
		URL:        authorizationURL.String(),
		DeviceCode: responseBody.DeviceCode,
		ExpiresIn:  responseBody.ExpiresIn,
		Interval:   responseBody.Interval,
	}, nil
}

func (r *authorizationResponse) poll(ctx context.Context) (creds *Credentials, err error) {
	requestURL, err := url.JoinPath(baseURL, "/token")
	if nil != err {
		return nil, err
	}
	requestBodyParams := make(url.Values, 4)
	requestBodyParams.Add("client_id", clientID)
	requestBodyParams.Add("scope", "r_usr+w_usr+w_sub")
	requestBodyParams.Add("grant_type", "urn:ietf:params:oauth:grant-type:device_code")
	requestBodyParams.Add("device_code", r.DeviceCode)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewBufferString(requestBodyParams.Encode()))
	if nil != err {
		return nil, err
	}
	request.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Add("Authorization", "Basic "+base64.StdEncoding.Strict().EncodeToString([]byte(clientID+":"+clientSecret)))

	client := http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(request)
	if nil != err {
		return nil, err
	}
	defer func() {
		if closeErr := response.Body.Close(); nil != closeErr {
			err = closeErr
		}
	}()

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
			return nil, err
		}
		if responseBody.Status == 400 && responseBody.Error == "authorization_pending" && responseBody.SubStatus == 1002 && responseBody.ErrorDescription == "Device Authorization code is not authorized yet" {
			return nil, ErrUnauthorized
		}
		return nil, fmt.Errorf("unexpected response: %d %s", responseBody.Status, responseBody.Error)
	default:
		return nil, fmt.Errorf("unexpected status code: %d", code)
	}

	var responseBody struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.NewDecoder(response.Body).DecodeContext(ctx, &responseBody); nil != err {
		return nil, err
	}
	expiresAt, err := extractExpiresAt(ctx, responseBody.AccessToken)
	if nil != err {
		return nil, err
	}

	return &Credentials{
		AccessToken:  responseBody.AccessToken,
		RefreshToken: responseBody.RefreshToken,
		ExpiresAt:    expiresAt,
	}, nil
}
