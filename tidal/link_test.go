package tidal_test

import (
	"testing"

	"github.com/xeptore/tgtd/tidal"
)

func TestIsLink(t *testing.T) {
	t.Parallel()

	t.Run("valid links", func(t *testing.T) {
		t.Parallel()

		tests := []string{
			"https://tidal.com/browse/mix/0160636492b28a3b2fe1e24ef6b94d",
			"https://www.tidal.com/browse/album/123456789",
			"https://listen.tidal.com/artist/123456789",
		}

		for _, test := range tests {
			if !tidal.IsLink(test) {
				t.Errorf("expected %s to be a valid Tidal link", test)
			}
		}
	})
}
