package cache

import (
	"time"

	"github.com/gotd/td/tg"
	"github.com/karlseguin/ccache/v3"
)

var (
	DefaultDownloadedCoverTTL = 5 * time.Minute
)

type Cache[TAlbum any] struct {
	Albums           *ccache.Cache[TAlbum]
	DownloadedCovers *ccache.Cache[[]byte]
	UploadedCovers   *ccache.Cache[tg.InputFileClass]
}

func New[TAlbum any]() *Cache[TAlbum] {
	albumInfoCache := ccache.New(
		ccache.Configure[TAlbum]().
			MaxSize(1000).
			GetsPerPromote(3).
			ItemsToPrune(1),
	)
	downloadedCoversCache := ccache.New(
		ccache.Configure[[]byte]().
			MaxSize(100).
			GetsPerPromote(3).
			ItemsToPrune(1),
	)
	uploadedCoversCache := ccache.New(
		ccache.Configure[tg.InputFileClass]().
			MaxSize(100).
			GetsPerPromote(3).
			ItemsToPrune(1),
	)

	return &Cache[TAlbum]{
		Albums:           albumInfoCache,
		DownloadedCovers: downloadedCoversCache,
		UploadedCovers:   uploadedCoversCache,
	}
}
