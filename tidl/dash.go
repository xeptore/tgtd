package tidl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/xeptore/flaw/v8"
	"golang.org/x/sync/errgroup"

	"github.com/xeptore/tgtd/errutil"
	"github.com/xeptore/tgtd/mathutil"
	"github.com/xeptore/tgtd/must"
	"github.com/xeptore/tgtd/tidl/mpd"
)

type DashTrackStream struct {
	Info            mpd.StreamInfo
	AuthAccessToken string
}

func (d *DashTrackStream) saveTo(ctx context.Context, fileName string) (err error) {
	wg, wgCtx := errgroup.WithContext(ctx)
	wg.SetLimit(-1)

	numBatches := mathutil.CeilInts(d.Info.Parts.Count, maxBatchParts)
	flawP := flaw.P{"num_batches": numBatches}
	for i := range numBatches {
		wg.Go(func() error {
			if err := d.downloadBatch(wgCtx, fileName, i); nil != err {
				switch {
				case errutil.IsContext(ctx):
					return ctx.Err()
				case errors.Is(err, context.DeadlineExceeded):
					return context.DeadlineExceeded
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
		case errutil.IsFlaw(err):
			return must.BeFlaw(err).Append(flawP)
		default:
			panic(errutil.UnknownError(err))
		}
	}

	f, err := os.OpenFile(fileName, os.O_CREATE|os.O_SYNC|os.O_TRUNC|os.O_WRONLY, 0o0644)
	if nil != err {
		return flaw.From(fmt.Errorf("failed to create track file: %v", err)).Append(flawP)
	}
	defer func() {
		if closeErr := f.Close(); nil != closeErr {
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
		if err := d.writePartToTrackFile(f, partFileName); nil != err {
			return must.BeFlaw(err).Append(flawP)
		}
	}

	if err := f.Sync(); nil != err {
		return flaw.From(fmt.Errorf("failed to sync track file: %v", err)).Append(flawP)
	}

	return nil
}

func (d *DashTrackStream) writePartToTrackFile(f *os.File, partFileName string) (err error) {
	fp, err := os.OpenFile(partFileName, os.O_RDONLY, 0o0644)
	if nil != err {
		return flaw.From(fmt.Errorf("failed to open track part file: %v", err))
	}
	defer func() {
		if closeErr := fp.Close(); nil != closeErr {
			closeErr = flaw.From(fmt.Errorf("failed to close track part file: %v", closeErr))
			if nil != err {
				err = must.BeFlaw(err).Join(closeErr)
			} else {
				err = closeErr
			}
		}
	}()

	if _, err := io.Copy(f, fp); nil != err {
		return flaw.From(fmt.Errorf("failed to copy track part to track file: %v", err))
	}

	if err := os.Remove(partFileName); nil != err {
		return flaw.From(fmt.Errorf("failed to remove track part file: %v", err))
	}

	return nil
}

func (d *DashTrackStream) downloadBatch(ctx context.Context, fileName string, idx int) (err error) {
	f, err := os.OpenFile(
		fileName+".part."+strconv.Itoa(idx),
		os.O_CREATE|os.O_WRONLY|os.O_TRUNC|os.O_SYNC,
		0o644,
	)
	if nil != err {
		return flaw.From(fmt.Errorf("failed to create track part file: %v", err))
	}
	defer func() {
		if closeErr := f.Close(); nil != closeErr {
			closeErr = flaw.From(fmt.Errorf("failed to close track part file: %v", closeErr))
			switch {
			case nil == err:
				err = closeErr
			case errutil.IsContext(ctx):
				err = flaw.From(errors.New("context was ended")).Join(closeErr)
			case errors.Is(err, context.DeadlineExceeded):
				err = flaw.From(errors.New("timeout has reached")).Join(closeErr)
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
		if err := d.downloadSegment(ctx, link, f); nil != err {
			switch {
			case errutil.IsContext(ctx):
				return ctx.Err()
			case errors.Is(err, context.DeadlineExceeded):
				return context.DeadlineExceeded
			case errutil.IsFlaw(err):
				return must.BeFlaw(err).Append(flawP)
			default:
				panic(errutil.UnknownError(err))
			}
		}
	}

	return nil
}

func (d *DashTrackStream) downloadSegment(ctx context.Context, link string, f *os.File) (err error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if nil != err {
		if errutil.IsContext(ctx) {
			return ctx.Err()
		}
		return flaw.From(fmt.Errorf("failed to create get track part request: %v", err))
	}
	request.Header.Add("Authorization", "Bearer "+d.AuthAccessToken)
	flawP := flaw.P{}

	client := http.Client{Timeout: 5 * time.Hour} // TODO: set timeout to a reasonable value
	response, err := client.Do(request)
	if nil != err {
		switch {
		case errutil.IsContext(ctx):
			return ctx.Err()
		case errors.Is(err, context.DeadlineExceeded):
			return context.DeadlineExceeded
		default:
			return flaw.From(fmt.Errorf("failed to send get track part request: %v", err)).Append(flawP)
		}
	}
	flawP["response"] = errutil.HTTPResponseFlawPayload(response)

	switch status := response.StatusCode; status {
	case http.StatusOK:
	case http.StatusUnauthorized:
		resBytes, err := io.ReadAll(response.Body)
		if nil != err {
			return flaw.From(fmt.Errorf("failed to read get track part response body: %v", err)).Append(flawP)
		}
		flawP["response_body"] = string(resBytes)
		return flaw.From(errors.New("received 401 response")).Append(flawP)
	default:
		resBytes, err := io.ReadAll(response.Body)
		if nil != err {
			return flaw.From(fmt.Errorf("failed to read get track part response body: %v", err)).Append(flawP)
		}
		flawP["response_body"] = string(resBytes)
		return flaw.From(fmt.Errorf("unexpected status code received from get track part: %d", status)).Append(flawP)
	}

	defer func() {
		if closeErr := response.Body.Close(); nil != closeErr {
			closeErr = flaw.From(fmt.Errorf("failed to close get track part response body: %v", closeErr)).Append(flawP)
			switch {
			case nil == err:
				err = closeErr
			case errutil.IsContext(ctx):
				err = flaw.From(errors.New("context was ended")).Join(closeErr)
			case errutil.IsFlaw(err):
				err = must.BeFlaw(err).Join(closeErr)
			default:
				panic(errutil.UnknownError(err))
			}
		}
	}()

	if n, err := io.Copy(f, response.Body); nil != err {
		if errutil.IsContext(ctx) {
			return ctx.Err()
		}
		return flaw.From(fmt.Errorf("failed to write track part to file: %v", err)).Append(flawP)
	} else if n == 0 {
		return flaw.From(errors.New("empty track part")).Append(flawP)
	} else {
		flawP["bytes_written"] = n
	}
	return nil
}
