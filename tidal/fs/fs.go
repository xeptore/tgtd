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

func (dir DownloadDir) path() string {
	return string(dir)
}

func (dir DownloadDir) Album(id string) Album {
	dirPath := dir.path()
	return Album{
		DirPath:  dirPath,
		InfoFile: InfoFile[StoredAlbum]{Path: filepath.Join(dirPath, id+".json")},
		Cover:    Cover{Path: filepath.Join(dirPath, id+".jpg")},
	}
}

type Album struct {
	DirPath  string
	InfoFile InfoFile[StoredAlbum]
	Cover    Cover
}

func (d Album) Track(vol int, id string) AlbumTrack {
	trackPath := filepath.Join(d.DirPath, id)
	return AlbumTrack{
		Path:     trackPath,
		InfoFile: InfoFile[StoredAlbumVolumeTrack]{Path: trackPath + ".json"},
	}
}

type AlbumTrack struct {
	Path     string
	InfoFile InfoFile[StoredAlbumVolumeTrack]
}

func (dir DownloadDir) Single(id string) SingleTrack {
	trackPath := filepath.Join(dir.path(), id)
	return SingleTrack{
		Path:     trackPath,
		InfoFile: InfoFile[StoredSingleTrack]{Path: trackPath + ".json"},
		Cover:    Cover{Path: trackPath + ".jpg"},
	}
}

func (dir DownloadDir) Playlist(id string) Playlist {
	dirPath := dir.path()
	return Playlist{
		DirPath:  dirPath,
		InfoFile: InfoFile[StoredPlaylist]{Path: filepath.Join(dirPath, id+".json")},
	}
}

type Playlist struct {
	DirPath  string
	InfoFile InfoFile[StoredPlaylist]
}

func (p Playlist) Track(id string) PlaylistTrack {
	trackPath := filepath.Join(p.DirPath, id)
	return PlaylistTrack{
		Path:     trackPath,
		InfoFile: InfoFile[StoredPlaylistTrack]{Path: trackPath + ".json"},
		Cover:    Cover{Path: trackPath + ".jpg"},
	}
}

type PlaylistTrack struct {
	Path     string
	InfoFile InfoFile[StoredPlaylistTrack]
	Cover    Cover
}

func (dir DownloadDir) Mix(id string) Mix {
	dirPath := dir.path()
	return Mix{
		DirPath:  dirPath,
		InfoFile: InfoFile[StoredMix]{Path: filepath.Join(dirPath, id+".json")},
	}
}

type Mix struct {
	DirPath  string
	InfoFile InfoFile[StoredMix]
}

func (d Mix) Track(id string) MixTrack {
	trackPath := filepath.Join(d.DirPath, id)
	return MixTrack{
		Path:     trackPath,
		InfoFile: InfoFile[StoredMixTrack]{Path: trackPath + ".json"},
		Cover:    Cover{Path: trackPath + ".jpg"},
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
