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
	"golang.org/x/sync/errgroup"
)

func playlistTrackDir(playlistID string) string {
	return path.Join("playlists", playlistID)
}

func (d *Downloader) Playlist(ctx context.Context, id string) error {
	tracks, err := d.playlistTracks(ctx, id)
	if nil != err {
		return fmt.Errorf("failed to get playlist info: %v", err)
	}

	wg, ctx := errgroup.WithContext(ctx)
	wg.SetLimit(playlistDownloadConcurrency)

	if err := d.preparePlaylistDir(ctx, id, tracks); nil != err {
		return fmt.Errorf("failed to prepare playlist directory: %v", err)
	}

	for _, track := range tracks {
		wg.Go(func() error {
			if err := d.download(ctx, &track); nil != err {
				return fmt.Errorf("failed to download playlist track: %v", err)
			}
			return nil
		})
	}

	if err := wg.Wait(); nil != err {
		return fmt.Errorf("failed to download playlist tracks: %v", err)
	}

	return nil
}

func (d *Downloader) preparePlaylistDir(ctx context.Context, id string, tracks []PlaylistTrack) error {
	playlistDir := path.Join(d.basePath, playlistTrackDir(id))
	if err := os.MkdirAll(playlistDir, 0o0755); nil != err {
		return fmt.Errorf("failed to create playlist directory: %v", err)
	}
	f, err := os.OpenFile(path.Join(playlistDir, "info.json"), os.O_CREATE|os.O_SYNC|os.O_TRUNC|os.O_WRONLY, 0o0644)
	if nil != err {
		return fmt.Errorf("failed to create playlist info file: %v", err)
	}
	if err := json.NewEncoder(f).EncodeContext(ctx, tracks); nil != err {
		return fmt.Errorf("failed to encode playlist info: %v", err)
	}
	return nil
}

type PlaylistTrack struct {
	ID         string
	PlayListID string
	Duration   int
	Title      string
	ArtistName string
	Cover      string
	Version    *string
}

func (t *PlaylistTrack) id() string {
	return t.ID
}

func (t *PlaylistTrack) FileName() string {
	var fileName string
	if nil != t.Version {
		fileName = fmt.Sprintf("%s - %s (%s).flac", t.ArtistName, t.Title, *t.Version)
	} else {
		fileName = fmt.Sprintf("%s - %s.flac", t.ArtistName, t.Title)
	}
	return path.Join(playlistTrackDir(t.PlayListID), fileName)
}

func (t *PlaylistTrack) cover() string {
	return t.Cover
}

func (t *PlaylistTrack) info() TrackInfo {
	var title string
	if nil != t.Version {
		title = fmt.Sprintf("%s (%s)", t.Title, *t.Version)
	} else {
		title = fmt.Sprintf("%s", t.Title)
	}
	return TrackInfo{
		Duration:   t.Duration,
		Title:      title,
		ArtistName: t.ArtistName,
		Version:    t.Version,
	}
}

func (d *Downloader) playlistTracksPage(ctx context.Context, id string, page int) (tracks []PlaylistTrack, remaining int, err error) {
	playlistURL, err := url.JoinPath(fmt.Sprintf(playlistItemsAPIFormat, id))
	if nil != err {
		return nil, 0, fmt.Errorf("failed to create playlist URL: %v", err)
	}

	response, err := d.getPagedItems(ctx, playlistURL, page)
	if nil != err {
		return nil, 0, fmt.Errorf("failed to get playlist items: %w", err)
	}
	defer func() {
		if closeErr := response.Body.Close(); nil != closeErr {
			err = fmt.Errorf("failed to close get playlist page items response body: %v", closeErr)
		}
	}()

	var responseBody struct {
		TotalNumberOfItems int `json:"totalNumberOfItems"`
		Items              []struct {
			Type string `json:"type"`
			Cut  any    `json:"any"`
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
					Cover string `json:"cover"`
				} `json:"album"`
				Version *string `json:"version"`
			} `json:"item"`
		} `json:"items"`
	}
	if err := json.NewDecoder(response.Body).DecodeContext(ctx, &responseBody); nil != err {
		return nil, 0, fmt.Errorf("failed to decode mix response: %v", err)
	}
	thisPageItems := len(responseBody.Items)
	if thisPageItems == 0 {
		return nil, 0, fmt.Errorf("no items found in mix response")
	}

	for _, v := range responseBody.Items {
		if v.Type != "track" {
			continue
		}
		if v.Cut != nil {
			return nil, 0, errors.New("cut items are not supported")
		}

		playlistTrack := PlaylistTrack{
			ID:         strconv.Itoa(v.Item.ID),
			Duration:   v.Item.Duration,
			Title:      v.Item.Title,
			ArtistName: v.Item.Artist.Name,
			Cover:      v.Item.Album.Cover,
			PlayListID: id,
			Version:    v.Item.Version,
		}
		tracks = append(tracks, playlistTrack)
	}

	return tracks, responseBody.TotalNumberOfItems - (thisPageItems + page*pageSize), nil
}

func (d *Downloader) playlistTracks(ctx context.Context, id string) ([]PlaylistTrack, error) {
	var tracks []PlaylistTrack
	for i := 0; ; i++ {
		pageTracks, rem, err := d.playlistTracksPage(ctx, id, i)
		if nil != err {
			return nil, fmt.Errorf("failed to get playlist items: %v", err)
		}

		tracks = append(tracks, pageTracks...)

		if rem == 0 {
			break
		}
	}

	return tracks, nil
}
