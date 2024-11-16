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

func (d Album) CreateDir() error {
	flawP := flaw.P{"album_dir": d.Path}
	if err := os.RemoveAll(d.Path); nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to remove album directory: %v", err))
	}
	if err := os.MkdirAll(d.Path, 0o0700); nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to create album directory: %v", err))
	}
	return nil
}

func (d Album) CreateVolDirs(numVols int) error {
	for i := range numVols {
		volNum := i + 1
		volDir := filepath.Join(d.Path, strconv.Itoa(volNum))
		flawP := flaw.P{"volume_dir": volDir}

		if err := os.RemoveAll(volDir); nil != err {
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return flaw.From(fmt.Errorf("failed to delete possibly existing album volume directory: %v", err)).Append(flawP)
		}
		if err := os.MkdirAll(volDir, 0o0700); nil != err {
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return flaw.From(fmt.Errorf("failed to create album volume directory: %v", err)).Append(flawP)
		}
	}
	return nil
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

func (d Playlist) CreateDir() error {
	flawP := flaw.P{"playlist_dir": d.Path}
	if err := os.RemoveAll(d.Path); nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to remove playlist directory: %v", err)).Append(flawP)
	}
	if err := os.MkdirAll(d.Path, 0o0700); nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to create playlist directory: %v", err)).Append(flawP)
	}
	return nil
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

func (d Mix) CreateDir() error {
	flawP := flaw.P{"mix_dir": d.Path}
	if err := os.RemoveAll(d.Path); nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to remove mix directory: %v", err)).Append(flawP)
	}
	if err := os.MkdirAll(d.Path, 0o0700); nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to create mix directory: %v", err)).Append(flawP)
	}
	return nil
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
	return os.WriteFile(c.Path, b, 0o0600)
}

func (c Cover) Read() ([]byte, error) {
	return os.ReadFile(c.Path)
}

type SingleTrack struct {
	Path     string
	InfoFile InfoFile[StoredSingleTrack]
	Cover    Cover
}

func (d SingleTrack) CreateDir() error {
	flawP := flaw.P{"single_dir": d.Path}
	dir := filepath.Dir(d.Path)
	flawP["dir"] = dir

	if err := os.RemoveAll(dir); nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to remove single directory: %v", err)).Append(flawP)
	}
	if err := os.MkdirAll(dir, 0o0700); nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to create single directory: %v", err)).Append(flawP)
	}
	return nil
}

type InfoFile[T any] struct {
	Path string
}

func (p InfoFile[T]) Read() (*T, error) {
	return readInfoFile(p)
}

func (p InfoFile[T]) Write(v T) error {
	return writeInfoFile(p, v)
}

func readInfoFile[T any](file InfoFile[T]) (*T, error) {
	filePath := file.Path
	flawP := flaw.P{"file_path": filePath}

	f, err := os.OpenFile(filePath, os.O_RDONLY, 0o0644)
	if nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, flaw.From(fmt.Errorf("failed to open info file for read: %v", err)).Append(flawP)
	}
	defer func() {
		if closeErr := f.Close(); nil != closeErr {
			flawP["err_debug_tree"] = errutil.Tree(closeErr).FlawP()
			closeErr = flaw.From(fmt.Errorf("failed to close info file: %v", closeErr)).Append(flawP)
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
		return nil, flaw.From(fmt.Errorf("failed to decode info file contents: %v", err)).Append(flawP)
	}

	return &out, nil
}

func writeInfoFile[T any](file InfoFile[T], obj any) error {
	filePath := file.Path
	flawP := flaw.P{"path": filePath}

	f, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o0644)
	if nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to open info file for write: %v", err)).Append(flawP)
	}
	defer func() {
		if closeErr := f.Close(); nil != closeErr {
			flawP["err_debug_tree"] = errutil.Tree(closeErr).FlawP()
			closeErr = flaw.From(fmt.Errorf("failed to close info file: %v", closeErr)).Append(flawP)
			if nil != err {
				err = must.BeFlaw(err).Join(closeErr)
			} else {
				err = closeErr
			}
		}
	}()

	if err := json.NewEncoder(f).Encode(obj); nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to write info content: %v", err)).Append(flawP)
	}

	if err := f.Sync(); nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to sync info file: %v", err)).Append(flawP)
	}

	return nil
}
