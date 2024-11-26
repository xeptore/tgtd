package httputil

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/goccy/go-json"
	"github.com/xeptore/flaw/v8"

	"github.com/xeptore/tgtd/errutil"
)

func readResponseBody(ctx context.Context, resp *http.Response) ([]byte, error) {
	respBody, err := io.ReadAll(resp.Body)
	if nil != err {
		switch {
		case errors.Is(err, io.EOF):
			return nil, io.EOF
		case errutil.IsContext(ctx):
			return nil, ctx.Err()
		case errors.Is(err, context.DeadlineExceeded):
			return nil, context.DeadlineExceeded
		default:
			flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
			return nil, flaw.From(fmt.Errorf("failed to read response body: %v", err)).Append(flawP)
		}
	}
	return respBody, nil
}

func ReadResponseBody(ctx context.Context, resp *http.Response) ([]byte, error) {
	respBody, err := readResponseBody(ctx, resp)
	if nil != err {
		if errors.Is(err, io.EOF) {
			return nil, flaw.From(errors.New("unexpected empty response body"))
		}
	}
	return respBody, nil
}

func ReadOptionalResponseBody(ctx context.Context, resp *http.Response) ([]byte, error) {
	respBody, err := ReadResponseBody(ctx, resp)
	if nil != err && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return respBody, nil
}

func IsTokenExpiredUnauthorizedResponse(b []byte) (bool, error) {
	var body struct {
		Status      int    `json:"status"`
		SubStatus   int    `json:"subStatus"`
		UserMessage string `json:"userMessage"`
	}
	if err := json.Unmarshal(b, &body); nil != err {
		flawP := flaw.P{"response_body": string(b), "err_debug_tree": errutil.Tree(err).FlawP()}
		return false, flaw.From(fmt.Errorf("failed to decode 401 status code response body: %v", err)).Append(flawP)
	}
	return body.Status == 401 && body.SubStatus == 11003 && body.UserMessage == "The token has expired. (Expired on time)", nil
}

func IsTokenInvalidUnauthorizedResponse(b []byte) (bool, error) {
	var body struct {
		Status      int    `json:"status"`
		SubStatus   int    `json:"subStatus"`
		UserMessage string `json:"userMessage"`
	}
	if err := json.Unmarshal(b, &body); nil != err {
		flawP := flaw.P{"response_body": string(b), "err_debug_tree": errutil.Tree(err).FlawP()}
		return false, flaw.From(fmt.Errorf("failed to decode 401 status code response body: %v", err)).Append(flawP)
	}
	return body.Status == 401 && body.SubStatus == 11002 && body.UserMessage == "Token could not be verified", nil
}
