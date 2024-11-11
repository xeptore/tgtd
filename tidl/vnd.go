package tidl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/xeptore/flaw/v8"
	"golang.org/x/sync/errgroup"

	"github.com/xeptore/tgtd/mathutil"
	"github.com/xeptore/tgtd/tidl/auth"
)

type VndTrackStream struct {
	URL             string
	AuthAccessToken string
}

func (d *VndTrackStream) saveTo(ctx context.Context, fileName string) error {
	wg, ctx := errgroup.WithContext(ctx)
	wg.SetLimit(-1)

	fileSize, err := d.fileSize(ctx)
	if nil != err {
		return fmt.Errorf("failed to get track file size: %v", err)
	}

	numBatches := mathutil.CeilInts(fileSize, singlePartChunkSize)

	for i := range numBatches {
		wg.Go(func() error {
			start := i * singlePartChunkSize
			end := min((i+1)*singlePartChunkSize, fileSize)
			if err := d.downloadRange(ctx, fileName, i, start, end); nil != err {
				if errors.Is(err, context.Canceled) {
					return nil
				}
				return fmt.Errorf("failed to download part: %v", err)
			}
			return nil
		})
	}

	if err := wg.Wait(); nil != err {
		return fmt.Errorf("failed to download track: %v", err)
	}

	f, err := os.OpenFile(fileName, os.O_CREATE|os.O_SYNC|os.O_TRUNC|os.O_WRONLY, 0o0644)
	if nil != err {
		return fmt.Errorf("failed to create track file: %v", err)
	}
	defer func() {
		if closeErr := f.Close(); nil != closeErr {
			err = fmt.Errorf("failed to close track file: %v", closeErr)
		}
	}()

	for i := range numBatches {
		err := func() (err error) {
			partFileName := fileName + ".part." + strconv.Itoa(i)
			fp, err := os.OpenFile(partFileName, os.O_RDONLY, 0o0644)
			if nil != err {
				return nil
			}
			defer func() {
				if closeErr := fp.Close(); nil != closeErr {
					err = fmt.Errorf("failed to close track part file: %v", closeErr)
				}
			}()

			if _, err := io.Copy(f, fp); nil != err {
				return fmt.Errorf("failed to copy track part to track file: %v", err)
			}

			if err := os.Remove(partFileName); nil != err {
				return fmt.Errorf("failed to remove track part file: %v", err)
			}

			return nil
		}()
		if nil != err {
			return fmt.Errorf("failed to copy track part to track file: %v", err)
		}
	}

	return nil
}

func (d *VndTrackStream) fileSize(ctx context.Context) (size int, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, d.URL, nil)
	if nil != err {
		return 0, fmt.Errorf("failed to create get track info request: %v", err)
	}
	req.Header.Add("Authorization", "Bearer "+d.AuthAccessToken)

	client := http.Client{Timeout: 5 * time.Second} // TODO: set timeout to a reasonable value
	resp, err := client.Do(req)
	if nil != err {
		return 0, fmt.Errorf("failed to send get track info request: %v", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); nil != closeErr {
			err = fmt.Errorf("failed to close get track info response body: %v", closeErr)
		}
	}()

	switch code := resp.StatusCode; code {
	case http.StatusOK:
		size, err := strconv.Atoi(resp.Header.Get("Content-Length"))
		if nil != err {
			return 0, fmt.Errorf("failed to parse content length: %v", err)
		}
		return size, nil
	case http.StatusUnauthorized:
		return 0, auth.ErrUnauthorized
	default:
		return 0, fmt.Errorf("unexpected status code: %d", code)
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

func (d *VndTrackStream) downloadRange(ctx context.Context, filePath string, idx, start, end int) (err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.URL, nil)
	if nil != err {
		return fmt.Errorf("failed to create get track part request: %v", err)
	}
	req.Header.Add("Authorization", "Bearer "+d.AuthAccessToken)
	req.Header.Add("Range", fmt.Sprintf("bytes=%d-%d", start, end))

	client := http.Client{Timeout: 5 * time.Hour} // TODO: set timeout to a reasonable value
	resp, err := client.Do(req)
	if nil != err {
		return fmt.Errorf("failed to send get track part request: %v", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); nil != closeErr {
			err = fmt.Errorf("failed to close get track part response body: %v", closeErr)
		}
	}()

	f, err := os.OpenFile(filePath+".part."+strconv.Itoa(idx), os.O_CREATE|os.O_WRONLY|os.O_TRUNC|os.O_SYNC, 0o0644)
	if nil != err {
		return fmt.Errorf("failed to create track part file: %v", err)
	}
	defer func() {
		if closeErr := f.Close(); nil != closeErr {
			err = fmt.Errorf("failed to close track part file: %v", closeErr)
		}
	}()

	if _, err := io.Copy(f, resp.Body); nil != err {
		return fmt.Errorf("failed to write track part to file: %v", err)
	}

	return nil
}
