package download

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/xeptore/flaw/v8"
	"golang.org/x/sync/errgroup"

	"github.com/xeptore/tgtd/config"
	"github.com/xeptore/tgtd/errutil"
	"github.com/xeptore/tgtd/httputil"
	"github.com/xeptore/tgtd/mathutil"
	"github.com/xeptore/tgtd/must"
	"github.com/xeptore/tgtd/tidal/mpd"
)

type DashTrackStream struct {
	Info mpd.StreamInfo
}

func (d *DashTrackStream) saveTo(ctx context.Context, accessToken string, fileName string) (err error) {
	var (
		numBatches = mathutil.CeilInts(d.Info.Parts.Count, maxBatchParts)
		flawP      = flaw.P{"num_batches": numBatches}
		wg, wgCtx  = errgroup.WithContext(ctx)
	)

	wg.SetLimit(numBatches)
	for i := range numBatches {
		wg.Go(func() error {
			if err := d.downloadBatch(wgCtx, accessToken, fileName, i); nil != err {
				switch {
				case errutil.IsContext(ctx):
					return ctx.Err()
				case errors.Is(err, context.DeadlineExceeded):
					return context.DeadlineExceeded
				case errors.Is(err, ErrTooManyRequests):
					return ErrTooManyRequests
				case errutil.IsFlaw(err):
					flawP["batch_index"] = i
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

	f, err := os.OpenFile(fileName, os.O_CREATE|os.O_SYNC|os.O_TRUNC|os.O_WRONLY, 0o0600)
	if nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to create track file: %v", err)).Append(flawP)
	}
	defer func() {
		if nil != err {
			if removeErr := os.Remove(fileName); nil != removeErr {
				flawP["err_debug_tree"] = errutil.Tree(removeErr).FlawP()
				err = flaw.From(fmt.Errorf("failed to remove incomplete track file: %v", removeErr)).Join(err).Append(flawP)
			}
		}

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

	loopFlawPs := make([]flaw.P, numBatches)
	flawP["loop_flaws"] = loopFlawPs
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

func writePartToTrackFile(f *os.File, partFileName string) (err error) {
	fp, err := os.OpenFile(partFileName, os.O_RDONLY, 0o0600)
	if nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return flaw.From(fmt.Errorf("failed to open track part file: %v", err)).Append(flawP)
	}
	defer func() {
		if closeErr := fp.Close(); nil != closeErr {
			flawP := flaw.P{"err_debug_tree": errutil.Tree(closeErr).FlawP()}
			closeErr = flaw.From(fmt.Errorf("failed to close track part file: %v", closeErr)).Append(flawP)
			if nil != err {
				err = must.BeFlaw(err).Join(closeErr)
			} else {
				err = closeErr
			}
		}
	}()

	if _, err := io.Copy(f, fp); nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return flaw.From(fmt.Errorf("failed to copy track part to track file: %v", err)).Append(flawP)
	}

	if err := os.Remove(partFileName); nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return flaw.From(fmt.Errorf("failed to remove track part file: %v", err)).Append(flawP)
	}

	return nil
}

func (d *DashTrackStream) downloadBatch(ctx context.Context, accessToken, fileName string, idx int) (err error) {
	f, err := os.OpenFile(
		fileName+".part."+strconv.Itoa(idx),
		os.O_CREATE|os.O_WRONLY|os.O_TRUNC|os.O_SYNC,
		0o600,
	)
	if nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return flaw.From(fmt.Errorf("failed to create track part file: %v", err)).Append(flawP)
	}
	defer func() {
		if nil != err {
			if removeErr := os.Remove(f.Name()); nil != removeErr {
				flawP := flaw.P{"err_debug_tree": errutil.Tree(removeErr).FlawP()}
				err = flaw.From(fmt.Errorf("failed to remove incomplete track part file: %v", removeErr)).Join(err).Append(flawP)
			}
		}

		if closeErr := f.Close(); nil != closeErr {
			flawP := flaw.P{"err_debug_tree": errutil.Tree(closeErr).FlawP()}
			closeErr = flaw.From(fmt.Errorf("failed to close track part file: %v", closeErr)).Append(flawP)
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

	start := idx * maxBatchParts
	end := min(d.Info.Parts.Count, (idx+1)*maxBatchParts)

	flawP := flaw.P{"start_part_index": start, "end_part_index": end}
	loopFlawPs := make([]flaw.P, end-start)
	flawP["loop_flaws"] = loopFlawPs

	for i := range end - start {
		segmentIdx := start + i
		loopFlawP := flaw.P{"segment_index": segmentIdx}
		loopFlawPs[i] = loopFlawP

		link := strings.Replace(d.Info.Parts.InitializationURLTemplate, "$Number$", strconv.Itoa(segmentIdx), 1)
		loopFlawP["link"] = link
		if err := d.downloadSegment(ctx, accessToken, link, f); nil != err {
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
	}

	return nil
}

func (d *DashTrackStream) downloadSegment(ctx context.Context, accessToken, link string, f *os.File) (err error) {
	flawP := flaw.P{}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if nil != err {
		if errutil.IsContext(ctx) {
			return ctx.Err()
		}

		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to create get track part request: %v", err)).Append(flawP)
	}
	req.Header.Add("Authorization", "Bearer "+accessToken)

	client := http.Client{Timeout: config.DashSegmentDownloadTimeout} //nolint:exhaustruct
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
	flawP["response"] = errutil.HTTPResponseFlawPayload(resp)

	switch status := resp.StatusCode; status {
	case http.StatusOK:
	case http.StatusUnauthorized:
		respBytes, err := httputil.ReadOptionalResponseBody(ctx, resp)
		if nil != err {
			return err
		}
		flawP["response_body"] = string(respBytes)
		return flaw.From(errors.New("received 401 response")).Append(flawP)
	case http.StatusTooManyRequests:
		return ErrTooManyRequests
	case http.StatusForbidden:
		respBytes, err := httputil.ReadResponseBody(ctx, resp)
		if nil != err {
			return err
		}
		if ok, err := errutil.IsTooManyErrorResponse(resp, respBytes); nil != err {
			flawP["response_body"] = string(respBytes)
			return must.BeFlaw(err).Append(flawP)
		} else if ok {
			return ErrTooManyRequests
		}

		flawP["response_body"] = string(respBytes)
		return flaw.From(errors.New("unexpected 403 response")).Append(flawP)
	default:
		respBytes, err := httputil.ReadOptionalResponseBody(ctx, resp)
		if nil != err {
			return err
		}
		flawP["response_body"] = string(respBytes)
		return flaw.From(fmt.Errorf("unexpected status code received from get track part: %d", status)).Append(flawP)
	}

	respBytes, err := httputil.ReadResponseBody(ctx, resp)
	if nil != err {
		return err
	}
	if n, err := io.Copy(f, bytes.NewReader(respBytes)); nil != err {
		flawP["response_body"] = string(respBytes)
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to write track part to file: %v", err)).Append(flawP)
	} else if n == 0 {
		return flaw.From(errors.New("empty track part")).Append(flawP)
	} else {
		flawP["bytes_written"] = n
	}
	return nil
}
