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
	"github.com/xeptore/tgtd/tidl/auth"
	"github.com/xeptore/tgtd/tidl/mpd"
	"github.com/xeptore/tgtd/tidl/must"
)

type DashTrackStream struct {
	Info            mpd.StreamInfo
	AuthAccessToken string
}

func (d *DashTrackStream) saveTo(ctx context.Context, fileName string) (err error) {
	wg, ctx := errgroup.WithContext(ctx)
	wg.SetLimit(-1)

	numBatches := mathutil.CeilInts(d.Info.Parts.Count, maxBatchParts)
	flawP := flaw.P{"num_batches": numBatches}
	for i := range numBatches {
		wg.Go(func() error {
			if err := d.downloadBatch(ctx, fileName, i); nil != err {
				if errors.Is(err, context.Canceled) {
					return nil
				}
				if errors.Is(err, auth.ErrUnauthorized) {
					return auth.ErrUnauthorized
				}
				return must.BeFlaw(err).Append(flawP)
			}
			return nil
		})
	}

	if err := wg.Wait(); nil != err {
		if errors.Is(err, auth.ErrUnauthorized) {
			return auth.ErrUnauthorized
		}
		return must.BeFlaw(err).Append(flawP)
	}

	f, err := os.OpenFile(fileName, os.O_CREATE|os.O_SYNC|os.O_TRUNC|os.O_WRONLY, 0o0644)
	if nil != err {
		return flaw.From(fmt.Errorf("failed to create track file: %v", err)).Append(flawP)
	}
	defer func() {
		if closeErr := f.Close(); nil != closeErr {
			closeErr = flaw.From(fmt.Errorf("failed to close track file: %v", closeErr)).Append(flawP)
			if nil != err {
				err = must.BeFlaw(err).Join(closeErr)
			} else {
				err = closeErr
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
			if nil != err && !errors.Is(err, auth.ErrUnauthorized) {
				err = must.BeFlaw(err).Join(closeErr)
			} else {
				err = closeErr
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
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
		if nil != err {
			return flaw.From(fmt.Errorf("failed to create get track part request: %v", err)).Append(flawP)
		}
		request.Header.Add("Authorization", "Bearer "+d.AuthAccessToken)

		client := http.Client{Timeout: 5 * time.Hour} // TODO: set timeout to a reasonable value
		response, err := client.Do(request)
		if nil != err {
			return flaw.From(fmt.Errorf("failed to send get track part request: %v", err)).Append(flawP)
		}
		loopFlawP["response"] = errutil.HTTPResponseFlawPayload(response)
		if err := d.writeSegmentResponse(response, f); nil != err {
			if !errors.Is(err, auth.ErrUnauthorized) {
				return must.BeFlaw(err).Append(flawP)
			}
			return auth.ErrUnauthorized
		}
	}

	return nil
}

func (d *DashTrackStream) writeSegmentResponse(response *http.Response, f *os.File) (err error) {
	defer func() {
		if closeErr := response.Body.Close(); nil != closeErr {
			closeErr = flaw.From(fmt.Errorf("failed to close get track part response body: %v", closeErr))
			if nil != err && !errors.Is(err, auth.ErrUnauthorized) {
				err = must.BeFlaw(err).Join(closeErr)
			} else {
				err = closeErr
			}
		}
	}()

	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return auth.ErrUnauthorized
	default:
		return flaw.From(fmt.Errorf("unexpected status code: %d", response.StatusCode))
	}

	if n, err := io.Copy(f, response.Body); nil != err {
		return flaw.From(fmt.Errorf("failed to write track part to file: %v", err))
	} else if n == 0 {
		return flaw.From(fmt.Errorf("empty track part"))
	}
	return nil
}
