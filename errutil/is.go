package errutil

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"

	"github.com/xeptore/flaw/v8"
)

func IsAny(err error, target error, targets ...error) (error, bool) {
	if errors.Is(err, target) {
		return target, true
	}
	for _, t := range targets {
		if errors.Is(err, t) {
			return t, true
		}
	}
	return nil, false
}

func IsContext(ctx context.Context) bool {
	err := ctx.Err()
	return nil != err && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded))
}

func IsTooManyErrorResponse(resp *http.Response) (bool, error) {
	if !slices.Equal(resp.Header.Values("Content-Type"), []string{"application/xml"}) {
		return false, nil
	}
	if !slices.Equal(resp.Header.Values("Server"), []string{"AmazonS3"}) {
		return false, nil
	}
	bodyBytes, err := io.ReadAll(resp.Body)
	if nil != err {
		if err, ok := IsAny(err, context.DeadlineExceeded, context.Canceled); ok {
			return false, err
		}
		flawP := flaw.P{"err_debug_tree": Tree(err).FlawP()}
		return false, flaw.From(fmt.Errorf("failed to read response body: %v", err)).Append(flawP)
	}

	var responseBody struct {
		XMLName   xml.Name `xml:"Error"`
		Code      string   `xml:"Code"`
		Message   string   `xml:"Message"`
		RequestID string   `xml:"RequestId"`
		HostID    string   `xml:"HostId"`
	}
	if err := xml.Unmarshal(bodyBytes, &responseBody); nil != err {
		flawP := flaw.P{"err_debug_tree": Tree(err).FlawP()}
		return false, flaw.From(fmt.Errorf("failed to unmarshal XML response body: %v", err)).Append(flawP)
	}
	return responseBody.Code == "AccessDenied" && responseBody.Message == "Access Denied", nil
}
