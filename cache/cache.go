package cache

import (
	"sync"
	"time"

	"github.com/gotd/td/tg"
	"github.com/karlseguin/ccache/v3"
	"github.com/xeptore/tgtd/tidal"
)

var (
	DefaultDownloadedCoverTTL = 1 * time.Hour
	DefaultAlbumTTL           = 1 * time.Hour
	DefaultUploadedCoverTTL   = 1 * time.Hour
)

type Cache struct {
	AlbumsMeta       AlbumsMetaCache
	DownloadedCovers DownloadedCoversCache
	UploadedCovers   UploadedCoversCache
}

func New() *Cache {
	albumsMetaCache := ccache.New(
		ccache.Configure[*tidal.AlbumMeta]().
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

	return &Cache{
		AlbumsMeta: AlbumsMetaCache{
			c:   albumsMetaCache,
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

type AlbumsMetaCache struct {
	c   *ccache.Cache[*tidal.AlbumMeta]
	mux sync.Mutex
}

func (c *AlbumsMetaCache) Fetch(k string, ttl time.Duration, fetch func() (*tidal.AlbumMeta, error)) (*ccache.Item[*tidal.AlbumMeta], error) {
	c.mux.Lock()
	defer c.mux.Unlock()
	return c.c.Fetch(k, ttl, fetch)
}
