package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/goccy/go-json"
	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/telegram/message/html"
	"github.com/gotd/td/telegram/message/styling"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"github.com/xeptore/flaw/v8"
	"golang.org/x/sync/errgroup"

	"github.com/xeptore/tgtd/errutil"
	"github.com/xeptore/tgtd/iterutil"
	"github.com/xeptore/tgtd/mathutil"
	"github.com/xeptore/tgtd/must"
	"github.com/xeptore/tgtd/ratelimit"
	"github.com/xeptore/tgtd/sliceutil"
	"github.com/xeptore/tgtd/tidl"
)

func (w *Worker) uploadAlbum(ctx context.Context, baseDir string) error {
	albumDir := path.Join(path.Join(baseDir, "albums", w.currentJob.ID))
	flawP := flaw.P{"album_dir": albumDir}
	files, err := os.ReadDir(albumDir)
	if nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to read directory: %v", err)).Append(flawP)
	}

	volumesCount := len(files)
	flawP["volumes_count"] = volumesCount
	loopFlawPs := make([]flaw.P, volumesCount)
	flawP["loop_payloads"] = loopFlawPs
	for volIdx := range volumesCount {
		if !files[volIdx].IsDir() {
			continue
		}

		volNum := volIdx + 1
		volDirPath := path.Join(albumDir, strconv.Itoa(volNum))
		loopFlawP := flaw.P{"volume_dir": volDirPath}
		loopFlawPs[volIdx] = loopFlawP

		txt := html.Format(nil, "<em>Uploading album volume <b>%d</b></em>", volNum)
		if _, err := w.sender.Resolve(w.config.TargetPeerID).Reply(w.currentJob.MessageID).StyledText(ctx, txt); nil != err {
			if errutil.IsContext(ctx) {
				return ctx.Err()
			}
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return flaw.From(fmt.Errorf("failed to send message: %v", err)).Append(flawP)
		}

		vol, err := w.readVolumeInfo(volDirPath)
		if nil != err {
			return must.BeFlaw(err).Append(flawP)
		}

		if err := w.uploadVolumeTracks(ctx, baseDir, *vol); nil != err {
			if errutil.IsContext(ctx) {
				return ctx.Err()
			}
			return must.BeFlaw(err).Append(flawP)
		}

		txt = html.Format(nil, "<em>Album volume <b>%d</b> uploaded</em>", volNum)
		if _, err := w.sender.Resolve(w.config.TargetPeerID).Reply(w.currentJob.MessageID).StyledText(ctx, txt); nil != err {
			if errutil.IsContext(ctx) {
				return ctx.Err()
			}
			flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
			return flaw.From(fmt.Errorf("failed to send message: %v", err)).Append(flawP)
		}
	}
	return nil
}

func (w *Worker) readVolumeInfo(dirPath string) (vol *tidl.Volume, err error) {
	volumeInfoFilePath := path.Join(dirPath, "volume.json")
	flawP := flaw.P{"volume_info_file_path": volumeInfoFilePath}
	f, err := os.OpenFile(volumeInfoFilePath, os.O_RDONLY, 0o644)
	if nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, flaw.From(fmt.Errorf("failed to open volume file: %v", err)).Append(flawP)
	}
	defer func() {
		if closeErr := f.Close(); nil != closeErr {
			flawP["err_debug_tree"] = errutil.Tree(closeErr).FlawP()
			closeErr = flaw.From(fmt.Errorf("failed to close volume file: %v", closeErr)).Append(flawP)
			if nil != err {
				err = must.BeFlaw(err).Join(closeErr)
			} else {
				err = closeErr
			}
		}
	}()

	var out tidl.Volume
	if err := json.NewDecoder(f).Decode(&out); nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return nil, flaw.From(fmt.Errorf("failed to unmarshal volume file: %v", err)).Append(flawP)
	}
	return &out, nil
}

func (w *Worker) uploadVolumeTracks(ctx context.Context, baseDir string, vol tidl.Volume) error {
	batchSize := mathutil.OptimalAlbumSize(len(vol.Tracks))
	numBatches := mathutil.CeilInts(len(vol.Tracks), batchSize)
	loopFlawPs := make([]flaw.P, 0, numBatches)
	flawP := flaw.P{"loop_payloads": loopFlawPs}

	batches := slices.Chunk(vol.Tracks, batchSize)
	idx := iterutil.Int(0)
	for batch := range batches {
		fileNames := sliceutil.Map(batch, func(track tidl.AlbumTrack) string { return track.FileName() })
		loopFlawP := flaw.P{"file_names": fileNames}
		loopFlawPs = append(loopFlawPs, loopFlawP)
		flawP["loop_payloads"] = loopFlawPs

		caption := []styling.StyledTextOption{
			styling.Plain(vol.Album.Title),
			styling.Plain(" "),
		}
		if nil != vol.Album.Version {
			caption = append(
				caption,
				styling.Plain(fmt.Sprintf("(%s)", *vol.Album.Version)),
				styling.Plain(" "),
			)
		}
		caption = append(
			caption,
			styling.Plain(fmt.Sprintf("(%d)", vol.Album.Year)),
			styling.Plain(" "),
			styling.Plain(fmt.Sprintf("#%d", vol.Number)),
			styling.Plain("\n"),
			styling.Italic(fmt.Sprintf("Part: %d/%d", idx.Next(), numBatches)),
		)
		if err := w.uploadTracksBatch(ctx, baseDir, fileNames, caption); nil != err {
			if errutil.IsContext(ctx) {
				return ctx.Err()
			}
			return must.BeFlaw(err).Append(flawP)
		}
	}
	return nil
}

func (w *Worker) uploadTracksBatch(ctx context.Context, baseDir string, fileNames []string, caption []styling.StyledTextOption) (err error) {
	album := make([]message.MultiMediaOption, len(fileNames))

	flawP := flaw.P{}

	uploader, cancel := w.newUploader(ctx)
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

	loopFlawPs := make([]flaw.P, len(fileNames))
	flawP["loop_payloads"] = loopFlawPs
	for i, trackFileName := range fileNames {
		wg.Go(func() error {
			fileName := path.Join(baseDir, trackFileName)
			loopFlawP := flaw.P{"file_name": fileName}
			loopFlawPs[i] = loopFlawP

			info, err := tidl.ReadTrackInfoFile(fileName)
			if nil != err {
				return must.BeFlaw(err).Append(flawP)
			}
			loopFlawP["info"] = info.FlawP()

			w.logger.Info().Str("file_name", fileName).Func(info.Log).Msg("Uploading track")

			up := newTrackUpload()
			if i == len(fileNames)-1 { // last track in this batch
				up.WithCaption(caption)
			}
			document, err := up.uploadTrack(wgCtx, uploader, fileName, *info)
			if nil != err {
				if errutil.IsContext(wgCtx) {
					return wgCtx.Err()
				}
				return must.BeFlaw(err).Append(flawP)
			}
			album[i] = document
			w.logger.Info().Str("file_name", fileName).Func(info.Log).Msg("Track uploaded")
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

func (w *Worker) uploadPlaylist(ctx context.Context, baseDir string) error {
	playlistDir := path.Join(path.Join(baseDir, "playlists", w.currentJob.ID))
	flawP := flaw.P{"playlist_dir": playlistDir}

	playlist, err := readDirInfo[tidl.Playlist](playlistDir)
	if nil != err {
		return must.BeFlaw(err).Append(flawP)
	}

	batchSize := mathutil.OptimalAlbumSize(len(playlist.Tracks))
	batches := slices.Chunk(playlist.Tracks, batchSize)
	numBatches := mathutil.CeilInts(len(playlist.Tracks), batchSize)
	loopFlawPs := make([]flaw.P, 0, numBatches)
	flawP["loop_payloads"] = loopFlawPs
	idx := iterutil.Int(0)
	for batch := range batches {
		fileNames := sliceutil.Map(batch, func(track tidl.PlaylistTrack) string { return track.FileName() })
		loopFlawP := flaw.P{"file_names": fileNames}
		loopFlawPs = append(loopFlawPs, loopFlawP)
		flawP["loop_payloads"] = loopFlawPs

		caption := []styling.StyledTextOption{
			styling.Plain(playlist.Title),
			styling.Plain(" "),
			styling.Plain(fmt.Sprintf("(%d - %d)", playlist.CreatedAtYear, playlist.LastUpdatedAtYear)),
			styling.Plain("\n"),
			styling.Italic(fmt.Sprintf("Part: %d/%d", idx.Next(), numBatches)),
		}
		if err := w.uploadTracksBatch(ctx, baseDir, fileNames, caption); nil != err {
			if errutil.IsContext(ctx) {
				return ctx.Err()
			}
			return must.BeFlaw(err).Append(flawP)
		}
	}
	return nil
}

func (w *Worker) uploadMix(ctx context.Context, baseDir string) error {
	mixDir := path.Join(path.Join(baseDir, "mixes", w.currentJob.ID))
	flawP := flaw.P{"mix_dir": mixDir}

	mix, err := readDirInfo[tidl.Mix](mixDir)
	if nil != err {
		return must.BeFlaw(err).Append(flawP)
	}

	batchSize := mathutil.OptimalAlbumSize(len(mix.Tracks))
	batches := slices.Chunk(mix.Tracks, batchSize)
	numBatches := mathutil.CeilInts(len(mix.Tracks), batchSize)
	loopFlawPs := make([]flaw.P, 0, numBatches)
	flawP["loop_payloads"] = loopFlawPs
	idx := iterutil.Int(0)
	for batch := range batches {
		fileNames := sliceutil.Map(batch, func(track tidl.MixTrack) string { return track.FileName() })
		loopFlawP := flaw.P{"file_names": fileNames}
		loopFlawPs = append(loopFlawPs, loopFlawP)
		flawP["loop_payloads"] = loopFlawPs

		caption := []styling.StyledTextOption{
			styling.Plain(mix.Title),
			styling.Plain("\n"),
			styling.Italic(fmt.Sprintf("Part: %d/%d", idx.Next(), numBatches)),
		}
		if err := w.uploadTracksBatch(ctx, baseDir, fileNames, caption); nil != err {
			if errutil.IsContext(ctx) {
				return ctx.Err()
			}
			return must.BeFlaw(err).Append(flawP)
		}
	}
	return nil
}

func readDirInfo[T any](dirPath string) (inf *T, err error) {
	f, err := os.OpenFile(path.Join(dirPath, "info.json"), os.O_RDONLY, 0o644)
	if nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return nil, flaw.From(fmt.Errorf("failed to open dir info file: %v", err)).Append(flawP)
	}
	defer func() {
		if closeErr := f.Close(); nil != closeErr {
			flawP := flaw.P{"err_debug_tree": errutil.Tree(closeErr).FlawP()}
			closeErr = flaw.From(fmt.Errorf("failed to close dir info file: %v", closeErr)).Append(flawP)
			if nil != err {
				err = must.BeFlaw(err).Join(closeErr)
			} else {
				err = closeErr
			}
		}
	}()

	if err := json.NewDecoder(f).Decode(&inf); nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return nil, flaw.From(fmt.Errorf("failed to unmarshal dir info file: %v", err)).Append(flawP)
	}

	return inf, nil
}

func (w *Worker) uploadSingle(ctx context.Context, basePath string) error {
	trackDir := path.Join(path.Join(basePath, "singles", w.currentJob.ID))
	flawP := flaw.P{"track_dir": trackDir}
	entries, err := os.ReadDir(trackDir)
	if nil != err {
		flawP["err_debug_tree"] = errutil.Tree(err).FlawP()
		return flaw.From(fmt.Errorf("failed to read directory: %v", err)).Append(flawP)
	}

	uploader, cancel := w.newUploader(ctx)
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

	var document *message.UploadedDocumentBuilder
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		fileName := path.Join(trackDir, strings.TrimSuffix(entry.Name(), ".json"))
		flawP["track_file_name"] = fileName

		track, err := tidl.ReadTrackInfoFile(fileName)
		if nil != err {
			return must.BeFlaw(err).Append(flawP)
		}
		flawP["track_info"] = track.FlawP()

		album, err := tidl.ReadTrackAlbumInfoFile(trackDir)
		if nil != err {
			return must.BeFlaw(err).Append(flawP)
		}
		flawP["album_info"] = album.FlawP()

		caption := []styling.StyledTextOption{
			styling.Plain(album.Title),
			styling.Plain(" "),
		}
		if nil != album.Version {
			caption = append(
				caption,
				styling.Plain(fmt.Sprintf("(%s)", *album.Version)),
				styling.Plain(" "),
				styling.Plain(fmt.Sprintf("(%d)", album.Year)),
			)
		}
		doc, err := newTrackUpload().WithCaption(caption).uploadTrack(ctx, uploader, fileName, *track)
		if nil != err {
			if errutil.IsContext(ctx) {
				return ctx.Err()
			}
			return must.BeFlaw(err).Append(flawP)
		}
		document = doc
		break
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

type UploadTrack struct {
	caption []styling.StyledTextOption
}

func newTrackUpload() *UploadTrack {
	return &UploadTrack{caption: nil}
}

func (u *UploadTrack) WithCaption(c []styling.StyledTextOption) *UploadTrack {
	u.caption = c
	return u
}

func (u *UploadTrack) uploadTrack(ctx context.Context, uploader *uploader.Uploader, fileName string, info tidl.TrackInfo) (*message.UploadedDocumentBuilder, error) {
	coverBytes, err := os.ReadFile(fileName + ".jpg")
	if nil != err {
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return nil, flaw.From(fmt.Errorf("failed to read track cover file: %v", err)).Append(flawP)
	}
	cover, err := uploader.FromBytes(ctx, "cover.jpg", coverBytes)
	if nil != err {
		if errutil.IsContext(ctx) {
			return nil, ctx.Err()
		}
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return nil, flaw.From(fmt.Errorf("failed to upload track cover: %v", err)).Append(flawP)
	}

	upload, err := uploader.FromPath(ctx, fileName)
	if nil != err {
		if errutil.IsContext(ctx) {
			return nil, ctx.Err()
		}
		flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
		return nil, flaw.From(fmt.Errorf("failed to upload track file: %v", err)).Append(flawP)
	}

	var document *message.UploadedDocumentBuilder
	if nil != u.caption {
		document = message.UploadedDocument(upload, u.caption...)
	} else {
		document = message.UploadedDocument(upload)
	}
	document.
		MIME("audio/flac").
		Attributes(
			&tg.DocumentAttributeFilename{
				FileName: filepath.Base(fileName),
			},
			//nolint:exhaustruct
			&tg.DocumentAttributeAudio{
				Title:     info.Title,
				Performer: info.ArtistName,
				Duration:  info.Duration,
			},
		).
		Thumb(cover).
		Audio()
	return document, nil
}
