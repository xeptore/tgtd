package tidl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"

	"github.com/xeptore/flaw/v8"
	"golang.org/x/sync/errgroup"

	"github.com/xeptore/tgtd/config"
	"github.com/xeptore/tgtd/errutil"
	"github.com/xeptore/tgtd/httputil"
	"github.com/xeptore/tgtd/mathutil"
	"github.com/xeptore/tgtd/must"
	"github.com/xeptore/tgtd/ratelimit"
)

type VndTrackStream struct {
	URL             string
	AuthAccessToken string
}

func (d *VndTrackStream) saveTo(ctx context.Context, fileName string) error {
	fileSize, err := d.fileSize(ctx)
	if nil != err {
		return err
	}

	wg, wgCtx := errgroup.WithContext(ctx)
	wg.SetLimit(ratelimit.MultipartTrackDownloadConcurrency)

	numBatches := mathutil.CeilInts(fileSize, singlePartChunkSize)
	loopFlawPs := make([]flaw.P, numBatches)
	flawP := flaw.P{"download_loop_flaw_ps": loopFlawPs, "num_batches": numBatches}
	for i := range numBatches {
		wg.Go(func() error {
			start := i * singlePartChunkSize
			end := min((i+1)*singlePartChunkSize, fileSize)
			loopFlawP := flaw.P{"start": start, "end": end}
			loopFlawPs[i] = loopFlawP

			fileName := fileName + ".part." + strconv.Itoa(i)
			loopFlawP["file_name"] = fileName

			f, err := os.OpenFile(fileName, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|os.O_SYNC, 0o0644)
			if nil != err {
				flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
				return flaw.From(fmt.Errorf("failed to create track part file: %v", err)).Append(flawP)
			}
			defer func() {
				if closeErr := f.Close(); nil != closeErr {
					flawP := flaw.P{"err_debug_tree": errutil.Tree(closeErr).FlawP()}
					closeErr = flaw.From(fmt.Errorf("failed to close track part file: %v", closeErr)).Append(flawP)
					switch {
					case nil == err:
						err = closeErr
					case errutil.IsContext(wgCtx):
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

			if err := d.downloadRange(wgCtx, start, end, f); nil != err {
				switch {
				case errutil.IsContext(wgCtx):
					return wgCtx.Err()
				case errors.Is(err, context.DeadlineExceeded):
					return context.DeadlineExceeded
				case errors.Is(err, ErrTooManyRequests):
					return ErrTooManyRequests
				case errutil.IsFlaw(err):
					return must.BeFlaw(err).Append(flawP)
				default:
					panic(errutil.UnknownError(err))
				}
			}
			return nil
		})
	}

	if err := wg.Wait(); nil != err {
		switch {
		case errutil.IsContext(ctx):
			return ctx.Err()
		case errors.Is(err, context.DeadlineExceeded):
			return context.DeadlineExceeded
		case errors.Is(err, ErrTooManyRequests):
			return ErrTooManyRequests
		case errutil.IsFlaw(err):
			return must.BeFlaw(err).Append(flawP)
		default:
			panic(errutil.UnknownError(err))
		}
	}

	f, err := os.OpenFile(fileName, os.O_CREATE|os.O_SYNC|os.O_TRUNC|os.O_WRONLY, 0o0644)
	if nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to create track file: %v", err)).Append(flawP)
	}
	defer func() {
		if closeErr := f.Close(); nil != closeErr {
			flawP["err_debug_tree"] = errutil.Tree(closeErr).FlawP()
			closeErr = flaw.From(fmt.Errorf("failed to close track file: %v", closeErr)).Append(flawP)
			switch {
			case nil == err:
				err = closeErr
			case errutil.IsFlaw(err):
				err = must.BeFlaw(err).Join(closeErr).Append(flawP)
			default:
				panic(errutil.UnknownError(err))
			}
		}
	}()

	mergeLoopFlawPs := make([]flaw.P, numBatches)
	flawP["merge_loop_flaws"] = mergeLoopFlawPs
	for i := range numBatches {
		partFileName := fileName + ".part." + strconv.Itoa(i)
		loopFlawP := flaw.P{"part_file_name": partFileName}
		loopFlawPs[i] = loopFlawP

		if err := writePartToTrackFile(f, partFileName); nil != err {
			return must.BeFlaw(err).Append(flawP)
		}
	}

	if err := f.Sync(); nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to sync track file: %v", err)).Append(flawP)
	}

	return nil
}

func (d *VndTrackStream) fileSize(ctx context.Context) (size int, err error) {
	flawP := flaw.P{}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, d.URL, nil)
	if nil != err {
		if errutil.IsContext(ctx) {
			return 0, ctx.Err()
		}

		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return 0, flaw.From(fmt.Errorf("failed to create get track metada request: %v", err)).Append(flawP)
	}
	req.Header.Add("Authorization", "Bearer "+d.AuthAccessToken)

	client := http.Client{Timeout: config.GetTrackFileSizeRequestTimeout} //nolint:exhaustruct
	resp, err := client.Do(req)
	if nil != err {
		switch {
		case errutil.IsContext(ctx):
			return 0, ctx.Err()
		case errors.Is(err, context.DeadlineExceeded):
			return 0, context.DeadlineExceeded
		default:
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return 0, flaw.From(fmt.Errorf("failed to send get track metadata request: %v", err)).Append(flawP)
		}
	}
	defer func() {
		if closeErr := resp.Body.Close(); nil != closeErr {
			flawP["err_debug_tree"] = errutil.Tree(closeErr).FlawP()
			closeErr = flaw.From(fmt.Errorf("failed to close get track metadata response body: %v", closeErr)).Append(flawP)
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
		size, err := strconv.Atoi(resp.Header.Get("Content-Length"))
		if nil != err {
			respBody, err := httputil.ReadOptionalResponseBody(ctx, resp)
			if nil != err {
				return 0, err
			}
			flawP["response_body"] = respBody
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return 0, flaw.From(fmt.Errorf("failed to parse content length: %v", err)).Append(flawP)
		}
		return size, nil
	case http.StatusTooManyRequests:
		return 0, ErrTooManyRequests
	case http.StatusForbidden:
		respBody, err := httputil.ReadResponseBody(ctx, resp)
		if nil != err {
			return 0, err
		}

		if ok, err := errutil.IsTooManyErrorResponse(resp, respBody); nil != err {
			flawP["response_body"] = string(respBody)
			return 0, must.BeFlaw(err)
		} else if ok {
			return 0, ErrTooManyRequests
		}

		flawP["response_body"] = string(respBody)
		return 0, flaw.From(errors.New("unexpected 403 response")).Append(flawP)
	default:
		respBody, err := httputil.ReadOptionalResponseBody(ctx, resp)
		if nil != err {
			return 0, err
		}
		flawP["response_body"] = respBody
		return 0, flaw.From(fmt.Errorf("unexpected status code: %d", code)).Append(flawP)
	}
}

type VNDManifest struct {
	MimeType       string   `json:"mimeType"`
	KeyID          *string  `json:"keyId"`
	EncryptionType string   `json:"encryptionType"`
	URLs           []string `json:"urls"`
}

func (m *VNDManifest) FlawP() flaw.P {
	return flaw.P{
		"mimeType":       m.MimeType,
		"keyId":          m.KeyID,
		"encryptionType": m.EncryptionType,
		"urls":           m.URLs,
	}
}

func (d *VndTrackStream) downloadRange(ctx context.Context, start, end int, f *os.File) (err error) {
	flawP := flaw.P{}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.URL, nil)
	if nil != err {
		if errutil.IsContext(ctx) {
			return ctx.Err()
		}

		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to create get track part request: %v", err)).Append(flawP)
	}
	req.Header.Add("Authorization", "Bearer "+d.AuthAccessToken)
	req.Header.Add("Range", fmt.Sprintf("bytes=%d-%d", start, end))

	client := http.Client{Timeout: config.VNDSegmentDownloadTimeout} //nolint:exhaustruct
	resp, err := client.Do(req)
	if nil != err {
		switch {
		case errutil.IsContext(ctx):
			return ctx.Err()
		case errors.Is(err, context.DeadlineExceeded):
			return context.DeadlineExceeded
		default:
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return flaw.From(fmt.Errorf("failed to send get track part request: %v", err)).Append(flawP)
		}
	}
	defer func() {
		if closeErr := resp.Body.Close(); nil != closeErr {
			flawP["err_debug_tree"] = errutil.Tree(closeErr).FlawP()
			closeErr = flaw.From(fmt.Errorf("failed to close get track part response body: %v", closeErr)).Append(flawP)
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

	switch status := resp.StatusCode; status {
	case http.StatusPartialContent:
	case http.StatusUnauthorized:
		respBody, err := httputil.ReadOptionalResponseBody(ctx, resp)
		if nil != err {
			return err
		}
		flawP["response_body"] = string(respBody)
		return flaw.From(errors.New("received 401 response")).Append(flawP)
	case http.StatusTooManyRequests:
		return ErrTooManyRequests
	case http.StatusForbidden:
		respBody, err := httputil.ReadResponseBody(ctx, resp)
		if nil != err {
			return err
		}

		if ok, err := errutil.IsTooManyErrorResponse(resp, respBody); nil != err {
			flawP["response_body"] = string(respBody)
			return must.BeFlaw(err).Append(flawP)
		} else if ok {
			return ErrTooManyRequests
		}

		flawP["response_body"] = string(respBody)
		return flaw.From(errors.New("unexpected 403 response")).Append(flawP)
	default:
		respBody, err := httputil.ReadOptionalResponseBody(ctx, resp)
		if nil != err {
			return err
		}

		flawP["response_body"] = string(respBody)
		return flaw.From(fmt.Errorf("unexpected status code received from get track part: %d", status)).Append(flawP)
	}

	respBody, err := httputil.ReadResponseBody(ctx, resp)
	if nil != err {
		return err
	}

	if n, err := io.Copy(f, bytes.NewReader(respBody)); nil != err {
		flawP["response_body"] = string(respBody)
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to write track part to file: %v", err)).Append(flawP)
	} else if n == 0 {
		return flaw.From(errors.New("empty track part")).Append(flawP)
	} else {
		flawP["bytes_written"] = n
	}

	return nil
}
