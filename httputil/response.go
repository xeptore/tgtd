package httputil

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

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
