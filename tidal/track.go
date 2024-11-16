package tidal

import (
	"strings"

	"github.com/samber/lo"
)

type TrackArtist struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

func JoinArtists(artists []TrackArtist) string {
	mainArtists := lo.FilterMap(artists, func(a TrackArtist, _ int) (string, bool) { return a.Name, a.Type == "MAIN" })
	featArtists := lo.FilterMap(artists, func(a TrackArtist, _ int) (string, bool) { return a.Name, a.Type == "FEATURED" })
	out := strings.Join(mainArtists, " & ")
	if len(featArtists) > 0 {
		out += " (feat. " + strings.Join(featArtists, " & ") + ")"
	}
	return out
}
