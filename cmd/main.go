package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/goccy/go-json"
	"github.com/gotd/contrib/bg"
	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/telegram/message/styling"
	"github.com/gotd/td/telegram/updates"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"github.com/joho/godotenv"
	"github.com/rs/zerolog"
	"github.com/urfave/cli/v2"
	"github.com/xeptore/flaw/v8"
	"golang.org/x/sync/errgroup"

	"github.com/xeptore/tgtd/config"
	"github.com/xeptore/tgtd/constant"
	"github.com/xeptore/tgtd/log"
	"github.com/xeptore/tgtd/sliceutil"
	"github.com/xeptore/tgtd/tidl"
	"github.com/xeptore/tgtd/tidl/auth"
	"github.com/xeptore/tgtd/tidl/must"
)

const (
	FlagConfigFilePath = "config"
)

func main() {
	logger := log.NewPretty(os.Stdout).Level(zerolog.TraceLevel)
	if err := godotenv.Load(); nil != err {
		if errors.Is(err, os.ErrNotExist) {
			logger.Warn().Msg(".env file was not found")
		} else {
			logger.Fatal().Err(err).Msg("Failed to load .env file")
		}
	}

	logger.Info().Time("compiled_at", constant.CompileTime).Msg("Starting application")

	app := &cli.App{
		Name:     "tgtd",
		Version:  constant.Version,
		Compiled: constant.CompileTime,
		Suggest:  true,
		Usage:    "Telegram Tidal Uploader",
		Action:   run,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:     FlagConfigFilePath,
				Aliases:  []string{"c"},
				Usage:    "Config File Path",
				Required: false,
			},
		},
	}

	if err := app.Run(os.Args); nil != err {
		if !errors.Is(err, context.Canceled) {
			logger.Fatal().Err(err).Msg("Application exited with error")
		}
		logger.Trace().Msg("Application canceled")
	}
}

func run(cliCtx *cli.Context) (err error) {
	logger := log.NewPretty(os.Stdout).Level(zerolog.TraceLevel)
	var (
		appHash    = os.Getenv("APP_HASH")
		cfgEnv     = os.Getenv("CONFIG")
		botToken   = os.Getenv("BOT_TOKEN")
		tidalToken = os.Getenv("TIDAL_TOKEN")
		cfg        *config.Config
	)
	cfgFilePath := cliCtx.String(FlagConfigFilePath)
	switch {
	case cfgFilePath != "" && cfgEnv != "":
		return errors.New("config file path and config environment variable are both set. specify only one")
	case cfgFilePath == "" && cfgEnv == "":
		return errors.New("config file path and config environment variable are both empty. specify one")
	case cfgFilePath != "":
		logger.Debug().Str("config_file_path", cfgFilePath).Msg("Loading config from file")
		c, err := config.FromFile(cfgFilePath)
		if nil != err {
			return fmt.Errorf("failed to load config: %v", err)
		}
		cfg = c
	default:
		logger.Debug().Msg("Loading config from environment variable")
		c, err := config.FromString(cfgEnv)
		if nil != err {
			return fmt.Errorf("failed to load config: %v", err)
		}
		cfg = c
	}

	appID, err := strconv.Atoi(os.Getenv("APP_ID"))
	if nil != err {
		return errors.New("failed to parse APP_ID environment variable to integer")
	}

	w := &Worker{
		mutex:      sync.Mutex{},
		config:     cfg,
		uploader:   nil,
		sender:     nil,
		tidlAuth:   nil,
		currentJob: nil,
		tidalToken: tidalToken,
		logger:     logger.With().Str("module", "worker").Logger(),
	}

	d := tg.NewUpdateDispatcher()
	d.OnNewMessage(buildOnMessage(w))
	updateHandler := updates.New(updates.Config{Handler: d})

	client := telegram.NewClient(
		appID,
		appHash,
		telegram.Options{
			SessionStorage: &session.FileStorage{Path: "session.json"},
			UpdateHandler:  updateHandler,
		},
	)

	ctx, cancel := signal.NotifyContext(cliCtx.Context, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	stop, err := bg.Connect(client)
	if nil != err {
		return fmt.Errorf("failed to connect to telegram: %v", err)
	}
	defer func() {
		logger.Debug().Msg("Stopping bot")
		if stopErr := stop(); nil != stopErr {
			err = fmt.Errorf("failed to stop Telegram background connection: %v", stopErr)
		}
		logger.Debug().Msg("Bot stopped")
	}()

	status, err := client.Auth().Status(ctx)
	if nil != err {
		return fmt.Errorf("failed to get auth status: %v", err)
	}
	if !status.Authorized {
		if _, authErr := client.Auth().Bot(ctx, botToken); nil != authErr {
			return fmt.Errorf("failed to authorize bot: %v", authErr)
		}
	}

	api := tg.NewClient(client)
	w.uploader = uploader.NewUploader(api)
	w.sender = message.NewSender(api).WithUploader(w.uploader)

	tidlAuth, err := auth.Load(ctx)
	if nil != err {
		if !errors.Is(err, auth.ErrUnauthorized) {
			return fmt.Errorf("failed to initialize auth service instance: %v", err)
		}
		link, wait, err := auth.NewAuthorizer(ctx)
		if nil != err {
			return fmt.Errorf("failed to initialize authorizer: %v", err)
		}
		_, err = w.sender.Resolve(cfg.TargetPeerID).StyledText(
			ctx,
			styling.Plain("Please visit the following link to authorize the application:"),
			styling.Plain("\n"),
			styling.URL(link),
		)
		if nil != err {
			return fmt.Errorf("failed to send TIDAL authentication link to specified peer: %v", err)
		}
		res := <-wait
		if err := res.Err; nil != err {
			_, err = w.sender.Resolve(cfg.TargetPeerID).StyledText(
				ctx,
				styling.Plain("TIDAL authentication failed:"),
				styling.Plain("\n"),
				styling.Code(err.Error()),
			)
			if nil != err {
				return fmt.Errorf("failed to send TIDAL authentication failure error message to specified target chat: %v", err)
			}
		}
		_, err = w.sender.Resolve(cfg.TargetPeerID).StyledText(
			ctx,
			styling.Bold("TIDAL authentication was successful!"),
		)
		if nil != err {
			return fmt.Errorf("failed to send TIDAL authentication successful message to specified target chat: %v", err)
		}
		tidlAuth = res.Auth
	}
	if err := tidlAuth.VerifyAccessToken(ctx); nil != err {
		return fmt.Errorf("failed to verify access token %v:", err)
	}
	w.tidlAuth = tidlAuth

	logger.Info().Msg("Bot is running")
	<-ctx.Done()
	logger.Debug().Msg("Stopping bot due to received signal")
	return nil
}

func buildOnMessage(w *Worker) func(ctx context.Context, e tg.Entities, update *tg.UpdateNewMessage) error {
	return func(ctx context.Context, e tg.Entities, update *tg.UpdateNewMessage) error {
		m, ok := update.Message.(*tg.Message)
		if !ok || m.Out {
			return nil
		}
		if u, ok := m.PeerID.(*tg.PeerUser); !ok || !slices.Contains(w.config.FromIDs, u.UserID) {
			return nil
		}
		msg := m.Message
		if strings.HasPrefix(msg, "/start") {
			if _, err := w.sender.Reply(e, update).Text(ctx, "Hello!"); nil != err {
				w.logger.Error().Err(err).Msg("Failed to send reply")
			}
			return nil
		}
		if strings.HasPrefix(msg, "/download ") {
			link := strings.TrimSpace(strings.TrimPrefix(msg, "/download "))
			if err := w.run(ctx, m.ID, link); nil != err {
				if errAlreadyRunning := new(JobAlreadyRunningError); errors.As(err, &errAlreadyRunning) {
					if _, err := w.sender.Reply(e, update).StyledText(ctx, styling.Plain("Another job is still running.\nCancel with "), styling.BotCommand("/cancel")); nil != err {
						w.logger.Error().Err(err).Msg("Failed to send reply")
					}
				} else if errInvalidLink := new(InvalidLinkError); errors.As(err, &errInvalidLink) {
					if _, err := w.sender.Reply(e, update).StyledText(ctx, styling.Plain("Failed to parse link"), styling.Plain("\n"), styling.Code(errInvalidLink.Error())); nil != err {
						w.logger.Error().Err(err).Msg("Failed to send reply")
					}
				} else {
					w.logger.Error().Err(err).Msg("Failed to run job")
				}
			} else {
				w.logger.Info().Str("link", link).Msg("Job succeeded")
			}
			return nil
		}
		if strings.HasPrefix(msg, "/cancel") {
			if err := w.cancelCurrentJob(); nil != err {
				if errors.Is(err, os.ErrProcessDone) {
					if _, err := w.sender.Reply(e, update).StyledText(ctx, styling.Plain("No job was running")); nil != err {
						w.logger.Error().Err(err).Msg("Failed to send reply")
					}
				} else {
					w.logger.Error().Err(err).Msg("Failed to cancel job")
				}
			} else {
				if _, err := w.sender.Reply(e, update).StyledText(ctx, styling.Plain("Job was canceled")); nil != err {
					w.logger.Error().Err(err).Msg("Failed to send reply")
				}
			}
		}
		return nil
	}
}

type Worker struct {
	mutex      sync.Mutex
	config     *config.Config
	uploader   *uploader.Uploader
	sender     *message.Sender
	tidlAuth   *auth.Auth
	currentJob *Job
	tidalToken string
	logger     zerolog.Logger
}

type Job struct {
	ID        string
	CreatedAt time.Time
	Link      string
	MessageID int
	cancel    context.CancelFunc
}

func (j *Job) flawP() flaw.P {
	return flaw.P{
		"id":         j.ID,
		"created_at": j.CreatedAt,
		"link":       j.Link,
		"message_id": j.MessageID,
	}
}

type JobAlreadyRunningError struct {
	ID string
}

func (e *JobAlreadyRunningError) Error() string {
	return fmt.Sprintf("engine: job %q is already running", e.ID)
}

type InvalidLinkError struct {
	Link string
	Err  error
}

func (e *InvalidLinkError) Error() string {
	return fmt.Sprintf("engine: invalid link %q: %v", e.Link, e.Err)
}

func parse(link string) (string, string, error) {
	parsedURL, err := url.Parse(link)
	if nil != err {
		return "", "", &InvalidLinkError{Link: link, Err: fmt.Errorf("engine: failed to parse URL: %v", err)}
	}
	kind, id, found := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(parsedURL.Path, "/browse/"), "/"), "/")
	if !found {
		return "", "", &InvalidLinkError{Link: link, Err: errors.New("engine: failed to cut path")}
	}
	switch kind {
	case "playlist", "album", "track", "mix":
		return id, kind, nil
	default:
		return "", "", &InvalidLinkError{Link: link, Err: fmt.Errorf("engine: unsupported kind %q", kind)}
	}
}

func (w *Worker) cancelCurrentJob() error {
	if !w.mutex.TryLock() {
		w.currentJob.cancel()
		return nil
	}
	w.mutex.Unlock()
	return os.ErrProcessDone
}

func (w *Worker) run(ctx context.Context, msgID int, link string) error {
	if !w.mutex.TryLock() {
		return &JobAlreadyRunningError{ID: w.currentJob.ID}
	}
	defer func() {
		w.currentJob = nil
		w.mutex.Unlock()
	}()

	id, kind, err := parse(link)
	if nil != err {
		return fmt.Errorf("engine: failed to parse link: %w", err)
	}

	flawP := flaw.P{"id": id, "kind": kind}

	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	job := Job{
		ID:        id,
		CreatedAt: time.Now(),
		Link:      link,
		MessageID: msgID,
		cancel:    cancel,
	}
	flawP["job"] = job.flawP()
	w.currentJob = &job

	const downloadBaseDir = "downloads"
	downloader := tidl.NewDownloader(w.tidlAuth, downloadBaseDir)

	switch kind {
	case "playlist":
		w.logger.Info().Str("id", id).Str("link", link).Msg("Starting download playlist")
		if err := downloader.Playlist(jobCtx, id); nil != err {
			return fmt.Errorf("engine: failed to download playlist link: %v", err)
		}
		w.logger.Info().Str("id", id).Str("link", link).Msg("Download finished. Starting playlist upload")
		if err := w.uploadPlaylist(jobCtx, downloadBaseDir); nil != err {
			return fmt.Errorf("engine: failed to upload playlist tracks: %v", err)
		}
		w.logger.Info().Str("id", id).Str("link", link).Msg("Playlist upload finished")
	case "album":
		w.logger.Info().Str("id", id).Str("link", link).Msg("Starting download album")
		if err := downloader.Album(jobCtx, id); nil != err {
			return fmt.Errorf("engine: failed to download album link: %v", err)
		}
		w.logger.Info().Str("id", id).Str("link", link).Msg("Download finished. Starting album upload")
		if err := w.uploadAlbum(jobCtx, downloadBaseDir); nil != err {
			return fmt.Errorf("engine: failed to upload album volume tracks: %v", err)
		}
		w.logger.Info().Str("id", id).Str("link", link).Msg("Album upload finished")
	case "track":
		w.logger.Info().Str("id", id).Str("link", link).Msg("Starting download track")
		if err := downloader.Track(jobCtx, id); nil != err {
			return fmt.Errorf("engine: failed to download track link: %v", err)
		}
		w.logger.Info().Str("id", id).Str("link", link).Msg("Download finished. Starting track upload")
		if err := w.uploadSingle(jobCtx, downloadBaseDir); nil != err {
			return fmt.Errorf("engine: failed to upload directory: %v", err)
		}
		w.logger.Info().Str("id", id).Str("link", link).Msg("Album upload finished")
	case "mix":
		w.logger.Info().Str("id", id).Str("link", link).Msg("Starting download mix")
		if err := downloader.Mix(jobCtx, id); nil != err {
			if errors.Is(err, auth.ErrUnauthorized) {
				return auth.ErrUnauthorized
			}
			return must.BeFlaw(err).Append(flawP)
		}
		w.logger.Info().Str("id", id).Str("link", link).Msg("Download finished. Starting track upload")
		if err := w.uploadMix(jobCtx, downloadBaseDir); nil != err {
			return fmt.Errorf("engine: failed to upload mix tracks: %v", err)
		}
	default:
		panic(fmt.Sprintf("unsupported download link kind: %s", kind))
	}
	return nil
}

func (w *Worker) uploadAlbum(ctx context.Context, baseDir string) error {
	albumDir := path.Join(path.Join(baseDir, "albums", w.currentJob.ID))
	files, err := os.ReadDir(albumDir)
	if nil != err {
		return fmt.Errorf("engine: failed to read directory: %v", err)
	}
	volumesCount := len(files)
	for volIdx := range volumesCount {
		if !files[volIdx].IsDir() {
			continue
		}
		volDirPath := path.Join(albumDir, strconv.Itoa(volIdx+1))
		tracks, err := w.readVolumeInfo(ctx, volDirPath)
		if nil != err {
			return fmt.Errorf("engine: failed to read volume info: %v", err)
		}
		if err := w.uploadVolumeTracks(ctx, baseDir, tracks); nil != err {
			return fmt.Errorf("engine: failed to upload volume tracks: %v", err)
		}
	}
	return nil
}

func (w *Worker) readVolumeInfo(ctx context.Context, dirPath string) (tracks []tidl.AlbumTrack, err error) {
	f, err := os.OpenFile(path.Join(dirPath, "volume.json"), os.O_RDONLY, 0o644)
	if nil != err {
		return nil, fmt.Errorf("engine: failed to open volume file: %v", err)
	}
	defer func() {
		if closeErr := f.Close(); nil != closeErr {
			err = fmt.Errorf("engine: failed to close volume file: %v", closeErr)
		}
	}()
	if err := json.NewDecoder(f).DecodeContext(ctx, &tracks); nil != err {
		return nil, fmt.Errorf("engine: failed to unmarshal volume file: %v", err)
	}
	return tracks, nil
}

func (w *Worker) uploadVolumeTracks(ctx context.Context, baseDir string, tracks []tidl.AlbumTrack) error {
	batches := slices.Chunk(tracks, 10)
	for batch := range batches {
		fileNames := sliceutil.Map(batch, func(track tidl.AlbumTrack) string { return track.FileName() })
		if err := w.uploadTracksBatch(ctx, baseDir, fileNames); nil != err {
			return fmt.Errorf("engine: failed to upload audio files: %v", err)
		}
	}
	return nil
}

func (w *Worker) uploadTracksBatch(ctx context.Context, baseDir string, fileNames []string) error {
	album := make([]message.MultiMediaOption, len(fileNames))
	wg, wgCtx := errgroup.WithContext(ctx)
	wg.SetLimit(-1)
	loopFlawPs := make([]flaw.P, len(fileNames))
	flawP := flaw.P{"loop_payloads": loopFlawPs}
	for i, trackFileName := range fileNames {
		wg.Go(func() error {
			fileName := path.Join(baseDir, trackFileName)
			loopFlawP := flaw.P{"file_name": fileName}
			loopFlawPs[i] = loopFlawP
			info, err := tidl.ReadTrackInfoFile(wgCtx, fileName)
			if nil != err {
				return must.BeFlaw(err).Append(flawP)
			}
			loopFlawP["info"] = info.FlawP()
			document, err := w.uploadTrack(wgCtx, fileName, *info)
			if nil != err {
				return must.BeFlaw(err).Append(flawP)
			}
			album[i] = document
			return nil
		})
	}

	if err := wg.Wait(); nil != err {
		return must.BeFlaw(err).Append(flawP)
	}

	var rest []message.MultiMediaOption
	if len(album) > 1 {
		rest = album[1:]
	}

	target := w.config.TargetPeerID
	if _, err := w.sender.Resolve(target).Reply(w.currentJob.MessageID).Reply(w.currentJob.MessageID).Album(ctx, album[0], rest...); nil != err {
		return flaw.From(fmt.Errorf("engine: failed to send media album to specified target %q: %v", target, err)).Append(flawP)
	}
	return nil
}

func (w *Worker) uploadPlaylist(ctx context.Context, baseDir string) error {
	playlistDir := path.Join(path.Join(baseDir, "playlists", w.currentJob.ID))
	tracks, err := readTracksDirInfo[tidl.PlaylistTrack](ctx, playlistDir)
	if nil != err {
		return fmt.Errorf("engine: failed to read playlist info: %v", err)
	}

	batches := slices.Chunk(tracks, 10)
	for batch := range batches {
		fileNames := sliceutil.Map(batch, func(track tidl.PlaylistTrack) string { return track.FileName() })
		if err := w.uploadTracksBatch(ctx, baseDir, fileNames); nil != err {
			return fmt.Errorf("engine: failed to upload playlist batch files: %v", err)
		}
	}
	return nil
}

func (w *Worker) uploadMix(ctx context.Context, baseDir string) error {
	mixDir := path.Join(path.Join(baseDir, "mixes", w.currentJob.ID))
	flawP := flaw.P{"mix_dir": mixDir}
	tracks, err := readTracksDirInfo[tidl.MixTrack](ctx, mixDir)
	if nil != err {
		return must.BeFlaw(err).Append(flawP)
	}

	batches := slices.Chunk(tracks, 10)
	var loopFlawPs []flaw.P
	flawP["loop_payloads"] = loopFlawPs
	for batch := range batches {
		fileNames := sliceutil.Map(batch, func(track tidl.MixTrack) string { return track.FileName() })
		loopFlawP := flaw.P{"file_names": fileNames}
		loopFlawPs = append(loopFlawPs, loopFlawP)
		if err := w.uploadTracksBatch(ctx, baseDir, fileNames); nil != err {
			return must.BeFlaw(err).Append(flawP)
		}
	}
	return nil
}

func readTracksDirInfo[T any](ctx context.Context, dirPath string) (tracks []T, err error) {
	f, err := os.OpenFile(path.Join(dirPath, "info.json"), os.O_RDONLY, 0o644)
	if nil != err {
		return nil, flaw.From(fmt.Errorf("failed to open dir info file: %v", err))
	}
	defer func() {
		if closeErr := f.Close(); nil != closeErr {
			closeErr = flaw.From(fmt.Errorf("failed to close dir info file: %v", closeErr))
			if nil != err {
				err = must.BeFlaw(err).Join(closeErr)
			} else {
				err = closeErr
			}
		}
	}()
	if err := json.NewDecoder(f).DecodeContext(ctx, &tracks); nil != err {
		return nil, flaw.From(fmt.Errorf("failed to unmarshal dir info file: %v", err))
	}
	return tracks, nil
}

func (w *Worker) uploadSingle(ctx context.Context, basePath string) error {
	trackDir := path.Join(path.Join(basePath, "singles", w.currentJob.ID))
	entries, err := os.ReadDir(trackDir)
	if nil != err {
		return fmt.Errorf("engine: failed to read directory: %v", err)
	}
	var document *message.UploadedDocumentBuilder
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		fileName := path.Join(trackDir, strings.TrimSuffix(entry.Name(), ".json"))
		track, err := tidl.ReadTrackInfoFile(ctx, fileName)
		if nil != err {
			return fmt.Errorf("engine: failed to read track info: %v", err)
		}
		doc, err := w.uploadTrack(ctx, fileName, *track)
		if nil != err {
			return fmt.Errorf("engine: failed to upload track: %v", err)
		}
		document = doc
		break
	}
	target := w.config.TargetPeerID
	if _, err := w.sender.Resolve(target).Reply(w.currentJob.MessageID).Media(ctx, document); nil != err {
		return fmt.Errorf("engine: failed to send media to specified target %q: %v", target, err)
	}
	return nil
}

func (w *Worker) uploadTrack(ctx context.Context, fileName string, info tidl.TrackInfo) (*message.UploadedDocumentBuilder, error) {
	coverBytes, err := os.ReadFile(fileName + ".jpg")
	if nil != err {
		return nil, flaw.From(fmt.Errorf("engine: failed to read track cover file: %v", err))
	}
	cover, err := w.uploader.FromBytes(ctx, "cover.jpg", coverBytes)
	if nil != err {
		return nil, flaw.From(fmt.Errorf("engine: failed to upload track cover: %v", err))
	}

	upload, err := w.uploader.FromPath(ctx, fileName)
	if nil != err {
		return nil, flaw.From(fmt.Errorf("engine: failed to upload track file: %v", err))
	}

	document := message.UploadedDocument(upload)
	document.
		MIME("audio/flac").
		Attributes(
			&tg.DocumentAttributeFilename{
				FileName: filepath.Base(fileName),
			},
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
