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

type AuthTokenFile string

func (f AuthTokenFile) path() string {
	return string(f)
}

func AuthTokenFileFrom(dir, filename string) AuthTokenFile {
	return AuthTokenFile(filepath.Join(dir, filename))
}

type AuthTokenFileContent struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    int64  `json:"expires_at"`
}

func (f AuthTokenFile) Read() (c *AuthTokenFileContent, err error) {
	file, err := os.OpenFile(f.path(), os.O_RDONLY, 0o0600)
	if nil != err {
		if errors.Is(err, os.ErrNotExist) {
			return nil, os.ErrNotExist
		}
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return nil, flaw.From(fmt.Errorf("failed to open token file: %v", err)).Append(flawP)
	}
	defer func() {
		if closeErr := file.Close(); nil != closeErr {
			flawP := flaw.P{"err_debug_tree": errutil.Tree(closeErr).FlawP()}
			closeErr = flaw.From(fmt.Errorf("failed to close token file: %v", closeErr)).Append(flawP)
			switch {
			case nil == err:
				err = closeErr
			default:
				err = must.BeFlaw(err).Join(closeErr)
			}
		}
	}()

	if err := json.NewDecoder(file).Decode(&c); nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return nil, flaw.From(fmt.Errorf("failed to decode token file: %v", err)).Append(flawP)
	}

	return c, nil
}

func (f AuthTokenFile) Write(c AuthTokenFileContent) (err error) {
	file, err := os.OpenFile(f.path(), os.O_CREATE|os.O_WRONLY|os.O_TRUNC|os.O_SYNC, 0o0600)
	if nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return flaw.From(fmt.Errorf("failed to open token file: %v", err)).Append(flawP)
	}
	defer func() {
		if closeErr := file.Close(); nil != closeErr {
			flawP := flaw.P{"err_debug_tree": errutil.Tree(closeErr).FlawP()}
			closeErr = flaw.From(fmt.Errorf("failed to close token file: %v", closeErr)).Append(flawP)
			if nil != err {
				err = must.BeFlaw(err).Join(closeErr)
			} else {
				err = closeErr
			}
		}
	}()

	if err := json.NewEncoder(file).EncodeWithOption(c); nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return flaw.From(fmt.Errorf("failed to encode token file: %v", err)).Append(flawP)
	}
	return nil
}
