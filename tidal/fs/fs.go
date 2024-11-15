package fs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/xeptore/flaw/v8"

	"github.com/xeptore/tgtd/errutil"
	"github.com/xeptore/tgtd/must"
)

type DownloadDir string

func From(d string) DownloadDir {
	return DownloadDir(d)
}

func (d DownloadDir) Album(id string) Album {
	path := filepath.Join(string(d), "albums", id)
	return Album{
		Path:     path,
		InfoFile: InfoFile[StoredAlbum]{Path: filepath.Join(path, "info.json")},
		Cover:    Cover{Path: filepath.Join(path, "cover.jpg")},
	}
}

func (d DownloadDir) Single(id string) SingleTrack {
	path := filepath.Join(string(d), "singles", id)
	return SingleTrack{
		Path:     path,
		InfoFile: InfoFile[StoredSingleTrack]{Path: path + ".json"},
		Cover:    Cover{Path: path + ".jpg"},
	}
}

func (d DownloadDir) Playlist(id string) Playlist {
	path := filepath.Join(string(d), "playlists", id)
	return Playlist{
		Path:     path,
		InfoFile: InfoFile[StoredPlaylist]{Path: filepath.Join(path, "info.json")},
	}
}

func (d DownloadDir) Mix(id string) Mix {
	path := filepath.Join(string(d), "mixes", id)
	return Mix{
		Path:     path,
		InfoFile: InfoFile[StoredMix]{Path: filepath.Join(path, "info.json")},
	}
}

type Album struct {
	Path     string
	InfoFile InfoFile[StoredAlbum]
	Cover    Cover
}

func (d Album) Track(vol int, id string) AlbumTrack {
	path := filepath.Join(d.Path, strconv.Itoa(vol), id)
	return AlbumTrack{
		Path:     path,
		InfoFile: InfoFile[StoredAlbumVolumeTrack]{Path: path + ".json"},
	}
}

type AlbumTrack struct {
	Path     string
	InfoFile InfoFile[StoredAlbumVolumeTrack]
}

type Playlist struct {
	Path     string
	InfoFile InfoFile[StoredPlaylist]
}

func (d Playlist) Track(id string) PlaylistTrack {
	path := filepath.Join(d.Path, id)
	return PlaylistTrack{
		Path:     path,
		InfoFile: InfoFile[StoredPlaylistTrack]{Path: path + ".json"},
		Cover:    Cover{Path: path + ".jpg"},
	}
}

type PlaylistTrack struct {
	Path     string
	InfoFile InfoFile[StoredPlaylistTrack]
	Cover    Cover
}

type Mix struct {
	Path     string
	InfoFile InfoFile[StoredMix]
}

func (d Mix) Track(id string) MixTrack {
	path := filepath.Join(d.Path, id)
	return MixTrack{
		Path:     path,
		InfoFile: InfoFile[StoredMixTrack]{Path: path + ".json"},
		Cover:    Cover{Path: path + ".jpg"},
	}
}

type MixTrack struct {
	Path     string
	InfoFile InfoFile[StoredMixTrack]
	Cover    Cover
}

type Cover struct {
	Path string
}

func (c Cover) Write(b []byte) error {
	return os.WriteFile(c.Path, b, 0o0644)
}

func (c Cover) Read() ([]byte, error) {
	return os.ReadFile(c.Path)
}

type SingleTrack struct {
	Path     string
	InfoFile InfoFile[StoredSingleTrack]
	Cover    Cover
}

type InfoFile[T any] struct {
	Path string
}

func (p InfoFile[T]) Read() (*T, error) {
	return readInfoFile(p)
}

func (p InfoFile[T]) Write(v T) error {
	return writeInfoFile[T](p, v)
}

func readInfoFile[T any](file InfoFile[T]) (*T, error) {
	pathStr := file.Path
	flawP := flaw.P{"path": pathStr}

	f, err := os.OpenFile(pathStr, os.O_RDONLY, 0o0644)
	if nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, flaw.From(fmt.Errorf("failed to create track info file: %v", err)).Append(flawP)
	}
	defer func() {
		if closeErr := f.Close(); nil != closeErr {
			flawP["err_debug_tree"] = errutil.Tree(closeErr).FlawP()
			closeErr = flaw.From(fmt.Errorf("failed to close track info file: %v", closeErr)).Append(flawP)
			if nil != err {
				err = must.BeFlaw(err).Join(closeErr)
			} else {
				err = closeErr
			}
		}
	}()

	var out T
	if err := json.NewDecoder(f).Decode(&out); nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, flaw.From(fmt.Errorf("failed to write track info: %v", err)).Append(flawP)
	}

	if err := f.Sync(); nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, flaw.From(fmt.Errorf("failed to sync track info file: %v", err)).Append(flawP)
	}

	return &out, nil
}

func writeInfoFile[T any](file InfoFile[T], obj any) error {
	pathStr := file.Path
	flawP := flaw.P{"path": pathStr}
	f, err := os.OpenFile(pathStr, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o0644)
	if nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to create track info file: %v", err)).Append(flawP)
	}
	defer func() {
		if closeErr := f.Close(); nil != closeErr {
			flawP["err_debug_tree"] = errutil.Tree(closeErr).FlawP()
			closeErr = flaw.From(fmt.Errorf("failed to close track info file: %v", closeErr)).Append(flawP)
			if nil != err {
				err = must.BeFlaw(err).Join(closeErr)
			} else {
				err = closeErr
			}
		}
	}()

	if err := json.NewEncoder(f).Encode(obj); nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to write track info: %v", err)).Append(flawP)
	}

	if err := f.Sync(); nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to sync track info file: %v", err)).Append(flawP)
	}

	return nil
}
