package tidl

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path"
	"strconv"

	"github.com/goccy/go-json"
	"golang.org/x/sync/errgroup"
)

func albumTrackDir(albumID string, volumeNumber int) string {
	return path.Join("albums", albumID, strconv.Itoa(volumeNumber))
}

func (d *Downloader) Album(ctx context.Context, id string) error {
	volumes, err := d.albumVolumes(ctx, id)
	if nil != err {
		return fmt.Errorf("failed to get album info: %v", err)
	}

	wg, ctx := errgroup.WithContext(ctx)
	wg.SetLimit(albumDownloadConcurrency)

	for i := range len(volumes) {
		if err := d.prepareAlbumVolumeDir(ctx, id, i+1, volumes[i]); nil != err {
			return fmt.Errorf("failed to prepare album volume directory: %v", err)
		}
	}

	for _, volumeTracks := range volumes {
		for _, track := range volumeTracks {
			wg.Go(func() error {
				if err := d.download(ctx, &track); nil != err {
					return fmt.Errorf("failed to download album track: %v", err)
				}
				return nil
			})
		}
	}

	if err := wg.Wait(); nil != err {
		return fmt.Errorf("failed to download album tracks: %v", err)
	}

	return nil
}

func (d *Downloader) prepareAlbumVolumeDir(ctx context.Context, albumID string, volNumber int, tracks []AlbumTrack) (err error) {
	volDir := path.Join(d.basePath, albumTrackDir(albumID, volNumber))
	if err := os.MkdirAll(volDir, 0o0755); nil != err {
		return fmt.Errorf("failed to create album volume directory: %v", err)
	}
	f, err := os.OpenFile(path.Join(volDir, "volume.json"), os.O_CREATE|os.O_SYNC|os.O_TRUNC|os.O_WRONLY, 0o0644)
	if nil != err {
		return fmt.Errorf("failed to create volume info file: %v", err)
	}
	defer func() {
		if closeErr := f.Close(); nil != closeErr {
			err = fmt.Errorf("failed to close volume info file properly: %v", err)
		}
	}()
	if err := json.NewEncoder(f).EncodeContext(ctx, tracks); nil != err {
		return fmt.Errorf("failed to encode and write volume info json file content: %v", err)
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
		title = fmt.Sprintf("%s", t.Title)
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
		return nil, 0, fmt.Errorf("failed to join track base URL with track id: %v", err)
	}

	response, err := d.getPagedItems(ctx, albumURL, page)
	if nil != err {
		return nil, 0, fmt.Errorf("failed to get album items: %v", err)
	}
	defer func() {
		if closeErr := response.Body.Close(); nil != closeErr {
			err = fmt.Errorf("failed to close get album page items response body: %v", closeErr)
		}
	}()

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
		return nil, 0, fmt.Errorf("failed to decode album response: %v", err)
	}
	thisPageItems := len(responseBody.Items)
	if thisPageItems == 0 {
		return nil, 0, fmt.Errorf("no items found in album response")
	}

	for _, v := range responseBody.Items {
		if v.Type != "track" {
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
		currentVolume       int = 1
	)
	for i := 0; ; i++ {
		pageTracks, rem, err := d.albumTracksPage(ctx, id, i)
		if nil != err {
			return nil, fmt.Errorf("failed to get album items: %v", err)
		}

		for _, track := range pageTracks {
			switch track.VolumeNumber {
			case currentVolume:
				currentVolumeTracks = append(currentVolumeTracks, track)
			case currentVolume + 1:
				tracks = append(tracks, currentVolumeTracks)
				currentVolumeTracks = []AlbumTrack{track}
				currentVolume++
			default:
				return nil, fmt.Errorf("unexpected volume number: %d", track.VolumeNumber)
			}
		}

		if rem == 0 {
			break
		}
	}

	tracks = append(tracks, currentVolumeTracks)

	return AlbumVolumes(tracks), nil
}
