package fs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/xeptore/flaw/v8"

	"github.com/xeptore/tgtd/errutil"
	"github.com/xeptore/tgtd/must"
)

type DownloadDir string

func From(d string) DownloadDir {
	return DownloadDir(d)
}

func (d DownloadDir) path() string {
	return string(d)
}

func (d DownloadDir) Album(id string) Album {
	path := d.path()
	return Album{
		Path:     path,
		InfoFile: InfoFile[StoredAlbum]{Path: filepath.Join(path, id+".json")},
		Cover:    Cover{Path: filepath.Join(path, id+".jpg")},
	}
}

func (d DownloadDir) Single(id string) SingleTrack {
	path := d.path()
	return SingleTrack{
		Path:     path,
		InfoFile: InfoFile[StoredSingleTrack]{Path: path + ".json"},
		Cover:    Cover{Path: path + ".jpg"},
	}
}

func (d DownloadDir) Playlist(id string) Playlist {
	path := d.path()
	return Playlist{
		Path:     path,
		InfoFile: InfoFile[StoredPlaylist]{Path: filepath.Join(path, id+".json")},
	}
}

func (d DownloadDir) Mix(id string) Mix {
	path := d.path()
	return Mix{
		Path:     path,
		InfoFile: InfoFile[StoredMix]{Path: filepath.Join(path, id+".json")},
	}
}

type Album struct {
	Path     string
	InfoFile InfoFile[StoredAlbum]
	Cover    Cover
}

func (d Album) Track(vol int, id string) AlbumTrack {
	path := d.Path
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
	path := d.Path
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
	path := d.Path
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
