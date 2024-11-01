package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

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
	"github.com/tidwall/sjson"
	"github.com/urfave/cli/v2"

	"github.com/xeptore/tgtd/config"
	"github.com/xeptore/tgtd/constant"
	"github.com/xeptore/tgtd/log"
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
		uploader:   nil,
		sender:     nil,
		mutex:      sync.Mutex{},
		currentJob: nil,
		config:     cfg,
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

	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	w.currentJob = &Job{
		ID:        kind + "-" + id,
		CreatedAt: time.Now(),
		Link:      link,
		MessageID: msgID,
		cancel:    cancel,
	}

	dir, err := w.downloadLink(jobCtx, link)
	if nil != err {
		return fmt.Errorf("engine: failed to download link: %v", err)
	}
	if err := w.uploadDir(jobCtx, dir); nil != err {
		return fmt.Errorf("engine: failed to upload directory: %v", err)
	}
	return nil
}

func buildDownloadProcessEnv(dir string) []string {
	parentEnv := os.Environ()
	clonedEnv := make([]string, 0, len(parentEnv))
	var set bool
	for _, env := range parentEnv {
		if strings.HasPrefix(env, "XDG_CONFIG_HOME=") {
			set = true
			clonedEnv = append(clonedEnv, "XDG_CONFIG_HOME="+dir)
		} else if strings.HasPrefix(env, "HOME=") {
			continue
		} else {
			clonedEnv = append(clonedEnv, env)
		}
	}
	if !set {
		clonedEnv = append(clonedEnv, "XDG_CONFIG_HOME="+dir)
	}
	return clonedEnv
}

//go:embed tidal-config.json
var tidalConfigJSON []byte

func createTidalDLConfig(token, downloadDir string) error {
	if err := os.WriteFile(path.Join(downloadDir, ".tidal-dl.token.json"), []byte(token), 0o644); nil != err {
		return fmt.Errorf("engine: failed to write file: %v", err)
	}
	cloned, err := sjson.SetBytes(tidalConfigJSON, "downloadPath", downloadDir)
	if nil != err {
		return fmt.Errorf("engine: failed to set download path: %v", err)
	}
	if err := os.WriteFile(path.Join(downloadDir, ".tidal-dl.json"), cloned, 0o644); nil != err {
		return fmt.Errorf("engine: failed to write file: %v", err)
	}
	return nil
}

func (w *Worker) downloadLink(ctx context.Context, link string) (string, error) {
	dir := path.Join(w.config.DownloadBaseDir, w.currentJob.ID)
	if err := os.MkdirAll(dir, 0o755); nil != err {
		return "", fmt.Errorf("engine: failed to create directory: %v", err)
	}

	if err := createTidalDLConfig(w.tidalToken, dir); nil != err {
		return "", fmt.Errorf("engine: failed to clone tidal-dl config: %v", err)
	}

	cmd := exec.CommandContext(ctx, "tidal-dl", "-l", link)
	cmd.Dir = dir
	cmd.Env = buildDownloadProcessEnv(dir)
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	// TODO: review and cleanup function body
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := cmd.Process.Signal(syscall.SIGINT); nil != err {
			return fmt.Errorf("engine: failed to send INT signal to process: %v", err)
		}
		time.Sleep(5 * time.Second)
		// TODO: check whether the job has already terminated, otherwise send KILL signal
		cmd.ProcessState.ExitCode()
		cmd.ProcessState.Success()
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
		}
		if err := cmd.Process.Signal(syscall.SIGKILL); nil != err {
			return fmt.Errorf("engine: failed to send KILL signal to process: %v", err)
		}
		time.Sleep(5 * time.Second)
		return nil
	}
	if err := cmd.Run(); nil != err {
		return "", fmt.Errorf("engine: failed to execute command: %v", err)
	}
	return dir, nil
}

func (w *Worker) uploadDir(ctx context.Context, dir string) error {
	dlDir := path.Join(dir, "x")
	files, err := os.ReadDir(dlDir)
	if nil != err {
		return fmt.Errorf("engine: failed to read directory: %v", err)
	}
	var audioFilePaths []string
	for _, file := range files {
		if file.IsDir() {
			continue
		}
		filePath := path.Join(dlDir, file.Name())
		audioFilePaths = append(audioFilePaths, filePath)
	}
	if len(audioFilePaths) == 0 {
		return os.ErrExist
	}
	for i := 0; i < len(audioFilePaths); i += 10 {
		j := i + 10
		if j > len(audioFilePaths) {
			j = len(audioFilePaths)
		}
		if err := w.uploadAudioFiles(ctx, audioFilePaths[i:j]); nil != err {
			return fmt.Errorf("engine: failed to upload audio files: %v", err)
		}
	}
	return nil
}

func (w *Worker) uploadAudioFiles(ctx context.Context, filePaths []string) error {
	album := make([]message.MultiMediaOption, 0, len(filePaths))
	for _, filePath := range filePaths {
		fileName := filepath.Base(filePath)
		nameWithoutExt := strings.TrimSuffix(fileName, filepath.Ext(fileName))
		coverFileName := path.Join(filepath.Dir(filePath), nameWithoutExt+".jpg")

		cmd := exec.CommandContext(
			ctx,
			"ffmpeg",
			"-an",
			"-vcodec",
			"copy",
			coverFileName,
			"-i",
			filePath,
		)
		if err := cmd.Run(); nil != err {
			return fmt.Errorf("engine: failed to execute command: %v", err)
		}
		if cmd.ProcessState.ExitCode() != 0 {
			return fmt.Errorf("engine: failed to execute command process: %v", cmd.ProcessState.String())
		}

		coverBytes, err := os.ReadFile(coverFileName)
		if nil != err {
			return fmt.Errorf("engine: failed to read cover file: %v", err)
		}
		cover, err := w.uploader.FromBytes(ctx, "cover.jpg", coverBytes)
		if nil != err {
			return fmt.Errorf("engine: failed to upload cover: %v", err)
		}

		cmd = exec.CommandContext(
			ctx,
			"ffprobe",
			"-show_entries",
			"format=duration:format_tags=artist,title:format=format_name",
			"-v",
			"quiet",
			"-of",
			"default=noprint_wrappers=1:nokey=1",
			"-i",
			filePath,
		)
		output, err := cmd.Output()
		if nil != err {
			return fmt.Errorf("engine: failed to execute command: %v", err)
		}
		if cmd.ProcessState.ExitCode() != 0 {
			return fmt.Errorf("engine: failed to execute command: %v", string(output))
		}
		lines := strings.SplitN(strings.TrimSpace(string(output)), "\n", 4)
		if len(lines) != 4 {
			return errors.New("engine: unexpected ffmpeg output format")
		}

		var mime string
		switch format := lines[0]; format {
		case "flac":
			mime = "audio/flac"
		default:
			return fmt.Errorf("engine: unsupported audio format: %q", format)
		}

		durFloat, err := strconv.ParseFloat(lines[1], 64)
		if nil != err {
			return fmt.Errorf("engine: failed to parse duration line: %v", err)
		}
		duration := int(durFloat)

		title := lines[2]
		artist := lines[3]

		upload, err := w.uploader.FromPath(ctx, filePath)
		if nil != err {
			return fmt.Errorf("engine: failed to upload file: %v", err)
		}

		document := message.UploadedDocument(upload)
		document.
			MIME(mime).
			Attributes(
				&tg.DocumentAttributeFilename{
					FileName: fileName,
				},
				&tg.DocumentAttributeAudio{
					Title:     title,
					Performer: artist,
					Duration:  duration,
				},
			).
			Thumb(cover).
			Audio()
		album = append(album, document)
	}

	var rest []message.MultiMediaOption
	if len(album) > 1 {
		rest = album[1:]
	}

	target := w.config.TargetPeerID
	if _, err := w.sender.Resolve(target).Album(ctx, album[0], rest...); nil != err {
		return fmt.Errorf("engine: failed to send media to specified target %q: %v", target, err)
	}
	return nil
}
