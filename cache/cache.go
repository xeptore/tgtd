package cache

import (
	"sync"
	"time"

	"github.com/gotd/td/tg"
	"github.com/karlseguin/ccache/v3"
)

var (
	DefaultDownloadedCoverTTL = 5 * time.Minute
	DefaultAlbumTTL           = 5 * time.Minute
	DefaultUploadedCoverTTL   = 5 * time.Minute
)

type Cache[TAlbum any] struct {
	Albums           AlbumsCache[TAlbum]
	DownloadedCovers DownloadedCoversCache
	UploadedCovers   UploadedCoversCache
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
		Albums: AlbumsCache[TAlbum]{
			c:   albumInfoCache,
			mux: sync.Mutex{},
		},
		DownloadedCovers: DownloadedCoversCache{
			c:   downloadedCoversCache,
			mux: sync.Mutex{},
		},
		UploadedCovers: UploadedCoversCache{
			c:   uploadedCoversCache,
			mux: sync.Mutex{},
		},
	}
}

type UploadedCoversCache struct {
	c   *ccache.Cache[tg.InputFileClass]
	mux sync.Mutex
}

func (c *UploadedCoversCache) Fetch(k string, ttl time.Duration, fetch func() (tg.InputFileClass, error)) (*ccache.Item[tg.InputFileClass], error) {
	c.mux.Lock()
	defer c.mux.Unlock()
	return c.c.Fetch(k, ttl, fetch)
}

type DownloadedCoversCache struct {
	c   *ccache.Cache[[]byte]
	mux sync.Mutex
}

func (c *DownloadedCoversCache) Fetch(k string, ttl time.Duration, fetch func() ([]byte, error)) (*ccache.Item[[]byte], error) {
	c.mux.Lock()
	defer c.mux.Unlock()
	return c.c.Fetch(k, ttl, fetch)
}

type AlbumsCache[T any] struct {
	c   *ccache.Cache[T]
	mux sync.Mutex
}

func (c *AlbumsCache[T]) Fetch(k string, ttl time.Duration, fetch func() (T, error)) (*ccache.Item[T], error) {
	c.mux.Lock()
	defer c.mux.Unlock()
	return c.c.Fetch(k, ttl, fetch)
}
