package fs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/goccy/go-json"
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

func (a Album) Track(vol int, id string) AlbumTrack {
	trackPath := filepath.Join(a.DirPath, id)
	return AlbumTrack{
		Path:     trackPath,
		InfoFile: InfoFile[StoredSingleTrack]{Path: trackPath + ".json"},
	}
}

type AlbumTrack struct {
	Path     string
	InfoFile InfoFile[StoredSingleTrack]
}

func (t AlbumTrack) Exists() (bool, error) {
	return fileExists(t.Path)
}

func (t AlbumTrack) Remove() error {
	if err := os.Remove(t.Path); nil != err {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return flaw.From(fmt.Errorf("failed to remove album track: %v", err))
	}
	return nil
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

func (p Playlist) Track(id string) SingleTrack {
	trackPath := filepath.Join(p.DirPath, id)
	return SingleTrack{
		Path:     trackPath,
		InfoFile: InfoFile[StoredSingleTrack]{Path: trackPath + ".json"},
		Cover:    Cover{Path: trackPath + ".jpg"},
	}
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

func (d Mix) Track(id string) SingleTrack {
	trackPath := filepath.Join(d.DirPath, id)
	return SingleTrack{
		Path:     trackPath,
		InfoFile: InfoFile[StoredSingleTrack]{Path: trackPath + ".json"},
		Cover:    Cover{Path: trackPath + ".jpg"},
	}
}

type Cover struct {
	Path string
}

func (c Cover) Exists() (bool, error) {
	return fileExists(c.Path)
}

func fileExists(path string) (bool, error) {
	if _, err := os.Stat(path); nil != err {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, flaw.From(fmt.Errorf("failed to stat file: %v", err))
	}
	return true, nil
}

func (c Cover) Write(b []byte) (err error) {
	flawP := flaw.P{}

	f, err := os.OpenFile(c.Path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to open cover file for write: %v", err)).Append(flawP)
	}
	defer func() {
		if nil != err {
			if removeErr := os.Remove(c.Path); nil != removeErr {
				flawP["err_debug_tree"] = errutil.Tree(removeErr).FlawP()
				err = flaw.From(fmt.Errorf("failed to remove incomplete cover file: %v", removeErr)).Join(err).Append(flawP)
				return
			}
		}

		if closeErr := f.Close(); nil != closeErr {
			flawP["err_debug_tree"] = errutil.Tree(closeErr).FlawP()
			closeErr = flaw.From(fmt.Errorf("failed to close cover file: %v", closeErr)).Append(flawP)
			if nil != err {
				err = must.BeFlaw(err).Join(closeErr)
			} else {
				err = closeErr
			}
		}
	}()

	if _, err := f.Write(b); nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to write cover file: %v", err)).Append(flawP)
	}

	if err := f.Sync(); nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to sync cover file: %v", err)).Append(flawP)
	}

	return nil
}

func (c Cover) Read() ([]byte, error) {
	return os.ReadFile(c.Path)
}

type SingleTrack struct {
	Path     string
	InfoFile InfoFile[StoredSingleTrack]
	Cover    Cover
}

func (t SingleTrack) Exists() (bool, error) {
	return fileExists(t.Path)
}

func (t SingleTrack) Remove() error {
	return os.Remove(t.Path)
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

func readInfoFile[T any](file InfoFile[T]) (t *T, err error) {
	filePath := file.Path
	flawP := flaw.P{"file_path": filePath}

	f, err := os.OpenFile(filePath, os.O_RDONLY, 0o0600)
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

func writeInfoFile[T any](file InfoFile[T], obj any) (err error) {
	filePath := file.Path
	flawP := flaw.P{"path": filePath}

	f, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o0600)
	if nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to open info file for write: %v", err)).Append(flawP)
	}
	defer func() {
		if nil != err {
			if removeErr := os.Remove(filePath); nil != removeErr {
				flawP["err_debug_tree"] = errutil.Tree(removeErr).FlawP()
				err = flaw.From(fmt.Errorf("failed to remove incomplete info file: %v", removeErr)).Join(err).Append(flawP)
			}
		}

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
