package tidl

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"strconv"

	"github.com/goccy/go-json"
	"github.com/xeptore/flaw/v8"
	"golang.org/x/sync/errgroup"

	"github.com/xeptore/tgtd/errutil"
	"github.com/xeptore/tgtd/must"
	"github.com/xeptore/tgtd/ratelimit"
)

func albumTrackDir(albumID string, volumeNumber int) string {
	return path.Join("albums", albumID, strconv.Itoa(volumeNumber))
}

func (d *Downloader) Album(ctx context.Context, id string) error {
	volumes, err := d.albumVolumes(ctx, id)
	if nil != err {
		return err
	}

	wg, wgCtx := errgroup.WithContext(ctx)
	wg.SetLimit(ratelimit.AlbumDownloadConcurrency)
	for i := range volumes {
		volNum := i + 1
		flawP := flaw.P{"volume_number": volNum}
		if err := d.prepareAlbumVolumeDir(id, volNum, volumes[i]); nil != err {
			return must.BeFlaw(err).Append(flawP)
		}
	}

	for _, volumeTracks := range volumes {
		for _, track := range volumeTracks {
			wg.Go(func() error { return d.download(wgCtx, &track) })
		}
	}

	if err := wg.Wait(); nil != err {
		return err
	}

	return nil
}

func (d *Downloader) prepareAlbumVolumeDir(albumID string, volNumber int, tracks []AlbumTrack) (err error) {
	volDir := path.Join(d.basePath, albumTrackDir(albumID, volNumber))
	flawP := flaw.P{"volume_dir": volDir}
	if err := os.RemoveAll(volDir); nil != err {
		return flaw.From(fmt.Errorf("failed to delete possibly existing album volume directory: %v", err)).Append(flawP)
	}
	if err := os.MkdirAll(volDir, 0o0755); nil != err {
		return flaw.From(fmt.Errorf("failed to create album volume directory: %v", err)).Append(flawP)
	}

	volumeInfoFilePath := path.Join(volDir, "volume.json")
	flawP["volume_info_file"] = volumeInfoFilePath
	f, err := os.OpenFile(volumeInfoFilePath, os.O_CREATE|os.O_SYNC|os.O_TRUNC|os.O_WRONLY, 0o0644)
	if nil != err {
		return flaw.From(fmt.Errorf("failed to create volume info file: %v", err)).Append(flawP)
	}
	defer func() {
		if closeErr := f.Close(); nil != closeErr {
			closeErr = flaw.From(fmt.Errorf("failed to close volume info file properly: %v", err)).Append(flawP)
			if nil != err {
				err = must.BeFlaw(err).Join(closeErr)
			} else {
				err = closeErr
			}
		}
	}()
	if err := json.NewEncoder(f).Encode(tracks); nil != err {
		return flaw.From(fmt.Errorf("failed to encode and write volume info json file content: %v", err)).Append(flawP)
	}
	if err := f.Sync(); nil != err {
		return flaw.From(fmt.Errorf("failed to sync volume info file: %v", err)).Append(flawP)
	}

	return nil
}

type AlbumTrack struct {
	ID           string
	Number       int
	VolumeNumber int
	Duration     int
	Title        string
	Artist       Artist
	Album        Album
	Version      *string
}

func (t *AlbumTrack) id() string {
	return t.ID
}

func (t *AlbumTrack) FileName() string {
	var fileName string
	if nil != t.Version {
		fileName = fmt.Sprintf("%d. %s - %s (%s).flac", t.Number, t.Artist.Name, t.Title, *t.Version)
	} else {
		fileName = fmt.Sprintf("%d. %s - %s.flac", t.Number, t.Artist.Name, t.Title)
	}
	return path.Join(albumTrackDir(t.Album.ID, t.VolumeNumber), fileName)
}

func (t *AlbumTrack) cover() string {
	return t.Album.Cover
}

func (t *AlbumTrack) info() TrackInfo {
	var title string
	if nil != t.Version {
		title = fmt.Sprintf("%s (%s)", t.Title, *t.Version)
	} else {
		title = t.Title
	}
	return TrackInfo{
		Duration:   t.Duration,
		Title:      title,
		ArtistName: t.Artist.Name,
		Version:    t.Version,
	}
}

func (d *Downloader) albumTracksPage(ctx context.Context, id string, page int) (tracks []AlbumTrack, remaining int, err error) {
	albumURL, err := url.JoinPath(fmt.Sprintf(albumItemsAPIFormat, id))
	if nil != err {
		return nil, 0, flaw.From(fmt.Errorf("failed to join track base URL with track id: %v", err))
	}
	flawP := flaw.P{"url": albumURL}

	response, err := d.getPagedItems(ctx, albumURL, page)
	if nil != err {
		switch {
		case errutil.IsContext(ctx):
			return nil, 0, ctx.Err()
		case errors.Is(err, context.DeadlineExceeded):
			return nil, 0, context.DeadlineExceeded
		case errutil.IsFlaw(err):
			return nil, 0, must.BeFlaw(err).Append(flawP)
		default:
			panic(errutil.UnknownError(err))
		}
	}
	defer func() {
		if closeErr := response.Body.Close(); nil != closeErr {
			closeErr = flaw.From(fmt.Errorf("failed to close get album page items response body: %v", closeErr)).Append(flawP)
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
	flawP["response"] = errutil.HTTPResponseFlawPayload(response)

	var responseBody struct {
		TotalNumberOfItems int `json:"totalNumberOfItems"`
		Items              []struct {
			Type string `json:"type"`
			Item struct {
				ID           int    `json:"id"`
				TrackNumber  int    `json:"trackNumber"`
				VolumeNumber int    `json:"volumeNumber"`
				Title        string `json:"title"`
				Duration     int    `json:"duration"`
				Artist       struct {
					Name string `json:"name"`
				} `json:"artist"`
				Album struct {
					ID    int    `json:"id"`
					Cover string `json:"cover"`
				} `json:"album"`
				Version *string `json:"version"`
			} `json:"item"`
		} `json:"items"`
	}
	if err := json.NewDecoder(response.Body).DecodeContext(ctx, &responseBody); nil != err {
		switch {
		case errutil.IsContext(ctx):
			return nil, 0, ctx.Err()
		case errors.Is(err, context.DeadlineExceeded):
			return nil, 0, context.DeadlineExceeded
		default:
			return nil, 0, flaw.From(fmt.Errorf("failed to decode album response: %v", err)).Append(flawP)
		}
	}
	thisPageItems := len(responseBody.Items)
	if thisPageItems == 0 {
		return nil, 0, os.ErrNotExist
	}

	for _, v := range responseBody.Items {
		if v.Type != trackTypeResponseItem {
			continue
		}

		albumTrack := AlbumTrack{
			ID:       strconv.Itoa(v.Item.ID),
			Duration: v.Item.Duration,
			Title:    v.Item.Title,
			Artist: Artist{
				Name: v.Item.Artist.Name,
			},
			Album: Album{
				Cover: v.Item.Album.Cover,
				ID:    strconv.Itoa(v.Item.Album.ID),
			},
			Number:       v.Item.TrackNumber,
			VolumeNumber: v.Item.VolumeNumber,
			Version:      v.Item.Version,
		}
		tracks = append(tracks, albumTrack)
	}

	return tracks, responseBody.TotalNumberOfItems - (thisPageItems + page*pageSize), nil
}

type AlbumVolumes = [][]AlbumTrack

func (d *Downloader) albumVolumes(ctx context.Context, id string) (AlbumVolumes, error) {
	var (
		tracks              [][]AlbumTrack
		currentVolumeTracks []AlbumTrack
		currentVolume       = 1
	)

	loopFlawPs := []flaw.P{}
	flawP := flaw.P{"loop_flaws": loopFlawPs}

	for i := 0; ; i++ {
		loopFlaw := flaw.P{"page": i}
		loopFlawPs = append(loopFlawPs, loopFlaw)
		flawP["loop_flaws"] = loopFlawPs
		pageTracks, rem, err := d.albumTracksPage(ctx, id, i)
		if nil != err {
			switch {
			case errutil.IsContext(ctx):
				return nil, ctx.Err()
			case errors.Is(err, os.ErrNotExist):
				break
			case errors.Is(err, context.DeadlineExceeded):
				return nil, context.DeadlineExceeded
			case errutil.IsFlaw(err):
				return nil, must.BeFlaw(err).Append(flawP)
			default:
				panic(errutil.UnknownError(err))
			}
		}
		loopFlaw["remaining"] = rem

		for _, track := range pageTracks {
			switch track.VolumeNumber {
			case currentVolume:
				currentVolumeTracks = append(currentVolumeTracks, track)
			case currentVolume + 1:
				tracks = append(tracks, currentVolumeTracks)
				currentVolumeTracks = []AlbumTrack{track}
				currentVolume++
			default:
				return nil, flaw.From(fmt.Errorf("unexpected volume number: %d", track.VolumeNumber)).Append(flawP)
			}
		}

		if rem == 0 {
			break
		}
	}

	tracks = append(tracks, currentVolumeTracks)

	return tracks, nil
}
