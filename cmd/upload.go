package main

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/telegram/message/styling"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"github.com/xeptore/flaw/v8"
	"golang.org/x/sync/errgroup"

	"github.com/xeptore/tgtd/cache"
	"github.com/xeptore/tgtd/errutil"
	"github.com/xeptore/tgtd/iterutil"
	"github.com/xeptore/tgtd/mathutil"
	"github.com/xeptore/tgtd/must"
	"github.com/xeptore/tgtd/ratelimit"
	"github.com/xeptore/tgtd/tidal"
	tidalfs "github.com/xeptore/tgtd/tidal/fs"
)

func (w *Worker) uploadAlbum(ctx context.Context, dir tidalfs.DownloadDir) error {
	albumFs := dir.Album(w.currentJob.ID)

	info, err := albumFs.InfoFile.Read()
	if nil != err {
		return err
	}

	for volIdx, trackIDs := range info.VolumeTrackIDs {
		var (
			volNum     = volIdx + 1
			batchSize  = mathutil.OptimalAlbumSize(len(trackIDs))
			numBatches = mathutil.CeilInts(len(trackIDs), batchSize)
			loopFlawPs = make([]flaw.P, numBatches)
			flawP      = flaw.P{"loop_payloads": loopFlawPs}
			batches    = iterutil.WithIndex(slices.Chunk(trackIDs, batchSize))
		)
		for i, trackIDs := range batches {
			caption := []styling.StyledTextOption{
				styling.Plain(info.Caption),
				styling.Plain("\n"),
				styling.Italic(fmt.Sprintf("Part: %d/%d", i+1, numBatches)),
			}

			items := make([]TrackUploadInfo, len(trackIDs))
			for i, trackID := range trackIDs {
				trackFs := albumFs.Track(volNum, trackID)
				track, err := trackFs.InfoFile.Read()
				if nil != err {
					return err
				}
				info := TrackUploadInfo{
					FilePath:   trackFs.Path,
					ArtistName: tidal.JoinArtists(track.Artists),
					Title:      track.Title,
					Version:    track.Version,
					Duration:   track.Duration,
					Format:     track.Format,
					CoverID:    track.CoverID,
					CoverPath:  albumFs.Cover.Path,
				}
				items[i] = info
			}

			if err := w.uploadTracksBatch(ctx, items, caption); nil != err {
				if errutil.IsContext(ctx) {
					return ctx.Err()
				}
				return must.BeFlaw(err).Append(flawP)
			}
		}
	}
	return nil
}

func (w *Worker) uploadPlaylist(ctx context.Context, dir tidalfs.DownloadDir) error {
	playlistFs := dir.Playlist(w.currentJob.ID)

	info, err := playlistFs.InfoFile.Read()
	if nil != err {
		return err
	}

	var (
		batchSize  = mathutil.OptimalAlbumSize(len(info.TrackIDs))
		batches    = iterutil.WithIndex(slices.Chunk(info.TrackIDs, batchSize))
		numBatches = mathutil.CeilInts(len(info.TrackIDs), batchSize)
	)
	for i, trackIDs := range batches {
		caption := []styling.StyledTextOption{
			styling.Plain(info.Caption),
			styling.Plain("\n"),
			styling.Italic(fmt.Sprintf("Part: %d/%d", i+1, numBatches)),
		}

		items := make([]TrackUploadInfo, len(trackIDs))
		for i, trackID := range trackIDs {
			trackFs := playlistFs.Track(trackID)
			track, err := trackFs.InfoFile.Read()
			if nil != err {
				return err
			}
			info := TrackUploadInfo{
				FilePath:   trackFs.Path,
				ArtistName: tidal.JoinArtists(track.Artists),
				Title:      track.Title,
				Version:    track.Version,
				Duration:   track.Duration,
				Format:     track.Format,
				CoverID:    track.CoverID,
				CoverPath:  trackFs.Cover.Path,
			}
			items[i] = info
		}

		if err := w.uploadTracksBatch(ctx, items, caption); nil != err {
			if errutil.IsContext(ctx) {
				return ctx.Err()
			}
			return must.BeFlaw(err)
		}
	}
	return nil
}

func (w *Worker) uploadMix(ctx context.Context, dir tidalfs.DownloadDir) error {
	mixFs := dir.Mix(w.currentJob.ID)

	info, err := mixFs.InfoFile.Read()
	if nil != err {
		return err
	}

	var (
		batchSize  = mathutil.OptimalAlbumSize(len(info.TrackIDs))
		batches    = iterutil.WithIndex(slices.Chunk(info.TrackIDs, batchSize))
		numBatches = mathutil.CeilInts(len(info.TrackIDs), batchSize)
	)
	for i, trackIDs := range batches {
		caption := []styling.StyledTextOption{
			styling.Plain(info.Caption),
			styling.Plain("\n"),
			styling.Italic(fmt.Sprintf("Part: %d/%d", i+1, numBatches)),
		}

		items := make([]TrackUploadInfo, len(trackIDs))
		for i, trackID := range trackIDs {
			trackFs := mixFs.Track(trackID)
			track, err := trackFs.InfoFile.Read()
			if nil != err {
				return err
			}
			info := TrackUploadInfo{
				FilePath:   trackFs.Path,
				ArtistName: tidal.JoinArtists(track.Artists),
				Title:      track.Title,
				Version:    track.Version,
				Duration:   track.Duration,
				Format:     track.Format,
				CoverID:    track.CoverID,
				CoverPath:  trackFs.Cover.Path,
			}
			items[i] = info
		}

		if err := w.uploadTracksBatch(ctx, items, caption); nil != err {
			if errutil.IsContext(ctx) {
				return ctx.Err()
			}
			return must.BeFlaw(err)
		}
	}
	return nil
}

func (w *Worker) uploadTracksBatch(ctx context.Context, batch []TrackUploadInfo, caption []styling.StyledTextOption) (err error) {
	var (
		album = make([]message.MultiMediaOption, len(batch))
		flawP = make(flaw.P)
	)

	up, cancel := w.newUploader(ctx)
	defer func() {
		if cancelErr := cancel(); nil != cancelErr {
			flawP["err_debug_tree"] = errutil.Tree(cancelErr).FlawP()
			cancelErr = flaw.From(fmt.Errorf("failed to close uploader pool: %v", cancelErr)).Append(flawP)
			switch {
			case nil == err:
				err = cancelErr
			case errutil.IsContext(ctx):
				err = flaw.From(errors.New("context ended")).Join(cancelErr)
			case errutil.IsFlaw(err):
				err = must.BeFlaw(err).Join(cancelErr)
			default:
				panic(errutil.UnknownError(err))
			}
		}
	}()

	wg, wgCtx := errgroup.WithContext(ctx)
	wg.SetLimit(ratelimit.AlbumUploadConcurrency)

	loopFlawPs := make([]flaw.P, len(batch))
	flawP["loop_payloads"] = loopFlawPs
	for i, item := range batch {
		wg.Go(func() error {
			builder := newTrackUploadBuilder(&w.cache.UploadedCovers)
			if i == len(batch)-1 { // last track in this batch
				captionWithSignature := append(caption, styling.Plain("\n"), styling.Italic(w.config.Signature))
				builder.WithCaption(captionWithSignature)
			}
			document, err := builder.uploadTrack(wgCtx, up, item)
			if nil != err {
				if errutil.IsContext(wgCtx) {
					return wgCtx.Err()
				}
				return must.BeFlaw(err).Append(flawP)
			}
			album[i] = document
			return nil
		})
	}

	if err := wg.Wait(); nil != err {
		if errutil.IsContext(ctx) {
			return ctx.Err()
		}
		return must.BeFlaw(err).Append(flawP)
	}

	var rest []message.MultiMediaOption
	if len(album) > 1 {
		rest = album[1:]
	}

	target := w.config.TargetPeerID
	if _, err := w.sender.Resolve(target).Reply(w.currentJob.MessageID).Clear().Album(ctx, album[0], rest...); nil != err {
		if errutil.IsContext(ctx) {
			return ctx.Err()
		}

		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to send media album to specified target %q: %v", target, err)).Append(flawP)
	}
	return nil
}

func (w *Worker) uploadSingle(ctx context.Context, dir tidalfs.DownloadDir) (err error) {
	trackFs := dir.Single(w.currentJob.ID)

	info, err := trackFs.InfoFile.Read()
	if nil != err {
		return err
	}

	flawP := flaw.P{}

	up, cancel := w.newUploader(ctx)
	defer func() {
		if cancelErr := cancel(); nil != cancelErr {
			flawP["err_debug_tree"] = errutil.Tree(cancelErr).FlawP()
			cancelErr = flaw.From(fmt.Errorf("failed to cancel uploader pool: %v", cancelErr)).Append(flawP)
			switch {
			case nil == err:
				err = cancelErr
			case errutil.IsContext(ctx):
				err = flaw.From(errors.New("context ended")).Join(cancelErr)
			case errutil.IsFlaw(err):
				err = must.BeFlaw(err).Join(cancelErr)
			default:
				panic(errutil.UnknownError(err))
			}
		}
	}()

	caption := []styling.StyledTextOption{
		styling.Plain(info.Caption),
		styling.Plain("\n"),
		styling.Italic(w.config.Signature),
	}
	uploadInfo := TrackUploadInfo{
		FilePath:   trackFs.Path,
		ArtistName: tidal.JoinArtists(info.Artists),
		Title:      info.Title,
		Version:    info.Version,
		Duration:   info.Duration,
		Format:     info.Format,
		CoverID:    info.CoverID,
		CoverPath:  trackFs.Cover.Path,
	}
	document, err := newTrackUploadBuilder(&w.cache.UploadedCovers).WithCaption(caption).uploadTrack(ctx, up, uploadInfo)
	if nil != err {
		if errutil.IsContext(ctx) {
			return ctx.Err()
		}
		return must.BeFlaw(err).Append(flawP)
	}

	target := w.config.TargetPeerID
	if _, err := w.sender.Resolve(target).Reply(w.currentJob.MessageID).Media(ctx, document); nil != err {
		if errutil.IsContext(ctx) {
			return ctx.Err()
		}

		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to send media to specified target %q: %v", target, err)).Append(flawP)
	}
	return nil
}

type TrackUploadBuilder struct {
	caption []styling.StyledTextOption
	cache   *cache.UploadedCoversCache
}

func newTrackUploadBuilder(cache *cache.UploadedCoversCache) *TrackUploadBuilder {
	return &TrackUploadBuilder{caption: nil, cache: cache}
}

func (u *TrackUploadBuilder) WithCaption(c []styling.StyledTextOption) *TrackUploadBuilder {
	u.caption = c
	return u
}

type TrackUploadInfo struct {
	FilePath   string
	ArtistName string
	Title      string
	Version    *string
	Duration   int
	Format     tidal.TrackFormat
	CoverID    string
	CoverPath  string
}

func (u *TrackUploadBuilder) uploadTrack(ctx context.Context, uploader *uploader.Uploader, info TrackUploadInfo) (*message.UploadedDocumentBuilder, error) {
	flawP := flaw.P{}

	cachedCover, err := u.cache.Fetch(info.CoverID, cache.DefaultUploadedCoverTTL, func() (tg.InputFileClass, error) {
		uploadedCover, err := uploader.FromPath(ctx, info.CoverPath)
		if nil != err {
			if errutil.IsContext(ctx) {
				return nil, ctx.Err()
			}

			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return nil, flaw.From(fmt.Errorf("failed to upload track cover: %v", err)).Append(flawP)
		}
		return uploadedCover, nil
	})
	if nil != err {
		return nil, err
	}
	cover := cachedCover.Value()

	upload, err := uploader.FromPath(ctx, info.FilePath)
	if nil != err {
		if errutil.IsContext(ctx) {
			return nil, ctx.Err()
		}

		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, flaw.From(fmt.Errorf("failed to upload track file: %v", err)).Append(flawP)
	}

	var document *message.UploadedDocumentBuilder
	if nil != u.caption {
		document = message.UploadedDocument(upload, u.caption...)
	} else {
		document = message.UploadedDocument(upload)
	}

	title := info.Title
	if nil != info.Version {
		title += " (" + *info.Version + ")"
	}

	document.
		MIME(info.Format.MimeType).
		Attributes(
			&tg.DocumentAttributeFilename{
				FileName: uploadTrackFileName(info),
			},
			//nolint:exhaustruct
			&tg.DocumentAttributeAudio{
				Title:     title,
				Performer: info.ArtistName,
				Duration:  info.Duration,
			},
		).
		Thumb(cover).
		Audio()
	return document, nil
}

func uploadTrackFileName(info TrackUploadInfo) string {
	ext := info.Format.InferTrackExt()
	if nil != info.Version {
		return fmt.Sprintf("%s - %s (%s).%s", info.ArtistName, info.Title, *info.Version, ext)
	}
	return fmt.Sprintf("%s - %s.%s", info.ArtistName, info.Title, ext)
}
