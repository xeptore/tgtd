package tidal

import (
	"net/url"
	"strings"
)

func IsLink(text string) bool {
	u, err := url.Parse(text)
	if nil != err {
		return false
	}

	switch u.Scheme {
	case "https":
	default:
		return false
	}

	switch u.Host {
	case "tidal.com", "www.tidal.com", "listen.tidal.com":
	default:
		return false
	}

	if pathParts := strings.SplitN(strings.Trim(u.Path, "/"), "/", 3); len(pathParts) < 2 {
		return false
	} else if len(pathParts) == 2 {
		switch pathParts[0] {
		case "mix", "playlist", "album", "artist", "track", "video":
		default:
			return false
		}
	} else if len(pathParts) == 3 {
		switch pathParts[0] {
		case "browse":
		default:
			return false
		}

		switch pathParts[1] {
		case "mix", "playlist", "album", "artist", "track", "video":
		default:
			return false
		}
	} else {
		return false
	}
	return true
}
