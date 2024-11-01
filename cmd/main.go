package main

import (
	"context"
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
	"github.com/gotd/td/telegram/updates"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"github.com/joho/godotenv"
	"github.com/rs/zerolog/log"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"github.com/urfave/cli/v2"

	"github.com/xeptore/tgtd/config"
)

var (
	AppVersion     = "1.0.0"
	AppCompileTime = ""
)

const (
	FlagConfigFilePath = "config"
)

func main() {
	if err := godotenv.Load(); nil != err {
		log.Fatal().Err(err).Msg("Failed to load .env file")
	}

	compileTime, err := time.Parse(time.RFC3339, AppCompileTime)
	if nil != err {
		log.Fatal().Err(err).Msg("") // TODO
	}

	app := &cli.App{
		Name:     "tgtd",
		Version:  AppVersion,
		Compiled: compileTime,
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
			log.Fatal().Err(err).Msg("") // TODO
		}
		log.Trace().Msg("Application canceled")
	}
}

func run(cliCtx *cli.Context) (err error) {
	var (
		appHash  = os.Getenv("APP_HASH")
		cfgEnv   = os.Getenv("CONFIG")
		botToken = os.Getenv("BOT_TOKEN")
		cfg      *config.Config
	)
	cfgFilePath := cliCtx.String(FlagConfigFilePath)
	switch {
	case cfgFilePath != "" && cfgEnv != "":
		return errors.New("config file path and config environment variable are both set. specify only one")
	case cfgFilePath == "" && cfgEnv == "":
		return errors.New("config file path and config environment variable are both empty. specify one")
	case cfgFilePath != "":
		c, err := config.FromYML(cfgFilePath)
		if nil != err {
			return fmt.Errorf("failed to load config: %v", err)
		}
		cfg = c
	default:
		c, err := config.FromYMLString(cfgEnv)
		if nil != err {
			return fmt.Errorf("failed to load config: %v", err)
		}
		cfg = c
	}

	appID, err := strconv.Atoi(os.Getenv("APP_ID"))
	if nil != err {
		log.Fatal().Err(err).Msg("Error converting APP_ID environment variable to int")
	}

	w := &Worker{
		uploader:   nil,
		sender:     nil,
		mutex:      sync.Mutex{},
		currentJob: nil,
		config:     cfg,
	}

	d := tg.NewUpdateDispatcher()
	d.OnNewMessage(buildOnMessage(w))
	updateHandler := updates.New(updates.Config{Handler: d})

	client := telegram.NewClient(
		appID,
		appHash,
		//
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
		if stopErr := stop(); nil != stopErr {
			err = fmt.Errorf("failed to stop Telegram background connection: %v", stopErr)
		}
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

	<-ctx.Done()
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
				log.Error().Err(err).Msg("Failed to send reply")
			}
			return nil
		}
		if strings.HasPrefix(msg, "/download ") {
			link := strings.TrimSpace(strings.TrimPrefix(msg, "/download "))
			if err := w.run(ctx, m.ID, link); nil != err {
				if errAlreadyRunning := new(JobAlreadyRunningError); errors.As(err, &errAlreadyRunning) {
					if _, err := w.sender.Reply(e, update).Text(ctx, "Job is already running.\nCancel with /cancel"); nil != err {
						log.Error().Err(err).Msg("Failed to send reply")
					}
				} else {
					log.Error().Err(err).Msg("Failed to run job")
				}
			}
			return nil
		}
		if strings.HasPrefix(msg, "/cancel") {
			if err := w.cancelCurrentJob(); nil != err {
				if errors.Is(err, os.ErrProcessDone) {
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
}

type Job struct {
	ID        string
	CreatedAt time.Time
	Link      string
	MessageID int
	ctx       context.Context
	cancel    context.CancelFunc
}

type JobAlreadyRunningError struct {
	ID string
}

func (e *JobAlreadyRunningError) Error() string {
	return fmt.Sprintf("engine: job %q is already running", e.ID)
}

func parse(link string) (string, string, error) {
	parsedURL, err := url.Parse(link)
	if nil != err {
		return "", "", err // TODO
	}
	parsedURL.RawQuery = ""
	link = parsedURL.String()
	after, found := strings.CutPrefix(link, "https://tidal.com/browse/")
	if !found {
		return "", "", err // TODO
	}
	kind, id, found := strings.Cut(after, "/")
	if !found {
		return "", "", err // TODO
	}
	switch kind {
	case "playlist", "album", "track", "mix":
		return id, kind, nil
	default:
		return "", "", err // TODO
	}
}

func (w *Worker) cancelCurrentJob() error {
	if !w.mutex.TryLock() {
		w.currentJob.cancel()
	}
	w.mutex.Unlock()
	return nil
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
		return fmt.Errorf("engine: failed to parse link: %v", err)
	}

	jobCtx, cancel := context.WithCancel(ctx)

	w.currentJob = &Job{
		ID:        kind + "-" + id,
		CreatedAt: time.Now(),
		Link:      link,
		MessageID: msgID,
		ctx:       jobCtx,
		cancel:    cancel,
	}

	dir, err := w.downloadLink(ctx, link)
	if nil != err {
		return fmt.Errorf("engine: failed to download link: %v", err)
	}
	if err := w.uploadDir(ctx, dir); nil != err {
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
			set = true
			clonedEnv = append(clonedEnv, "HOME="+dir)
		} else {
			clonedEnv = append(clonedEnv, env)
		}
	}
	if !set {
		clonedEnv = append(clonedEnv, "XDG_CONFIG_HOME="+dir)
	}
	return clonedEnv
}

func cloneTidalDLConfig(originalPath, downloadDir string) error {
	newConfigFilePath := path.Join(downloadDir, ".config", "tidal-dl-ng")
	if err := os.MkdirAll(newConfigFilePath, 0o755); nil != err {
		return fmt.Errorf("engine: failed to create directory: %v", err)
	}
	data, err := os.ReadFile(originalPath)
	if nil != err {
		return fmt.Errorf("engine: failed to read file: %v", err)
	}
	if !gjson.ValidBytes(data) {
		return errors.New("engine: invalid JSON data")
	}
	copied, err := sjson.SetBytes(data, "download_dir", downloadDir)
	if nil != err {
		return fmt.Errorf("engine: failed to set JSON data: %v", err)
	}
	if err := os.WriteFile(path.Join(newConfigFilePath, "config.json"), copied, 0o644); nil != err {
		return fmt.Errorf("engine: failed to write file: %v", err)
	}
	return nil
}

func (w *Worker) downloadLink(ctx context.Context, link string) (string, error) {
	dir := path.Join(w.config.DownloadBaseDir, w.currentJob.ID)
	if err := os.MkdirAll(dir, 0o755); nil != err {
		return "", fmt.Errorf("engine: failed to create directory: %v", err)
	}

	if err := cloneTidalDLConfig(w.config.OriginalTidalDLConfigPath, dir); nil != err {
		return "", fmt.Errorf("engine: failed to clone tidal-dl config: %v", err)
	}

	cmd := exec.CommandContext(ctx, w.config.TidalDLPath, "-l", link)
	cmd.Dir = dir
	cmd.Env = buildDownloadProcessEnv(dir)
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
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
	cmd.Cancel()
	return dir, nil
}

func (w *Worker) uploadDir(ctx context.Context, dir string) error {
	cover, err := w.uploader.FromPath(ctx, path.Join(dir, "cover.jpg"))
	if nil != err {
		return fmt.Errorf("engine: failed to upload cover: %v", err)
	}

	files, err := os.ReadDir(dir)
	if nil != err {
		return fmt.Errorf("engine: failed to read directory: %v", err)
	}
	var audioFilePaths []string
	for _, file := range files {
		if file.IsDir() {
			continue
		}
		filePath := path.Join(dir, file.Name())
		audioFilePaths = append(audioFilePaths, filePath)
	}
	if len(audioFilePaths) == 0 {
		return os.ErrExist
	}
	if err := w.uploadAudioFiles(ctx, cover, audioFilePaths); nil != err {
		return fmt.Errorf("engine: failed to upload audio files: %v", err)
	}
	return nil
}

func (w *Worker) uploadAudioFiles(ctx context.Context, cover tg.InputFileClass, filePaths []string) error {
	album := make([]message.MultiMediaOption, 0, len(filePaths))
	for _, filePath := range filePaths {
		upload, err := w.uploader.FromPath(ctx, filePath)
		if nil != err {
			return fmt.Errorf("engine: failed to upload file: %v", err)
		}

		cmd := exec.CommandContext(
			ctx,
			"ffprobe",
			"-i",
			"-show_entries",
			"format=duration:format_tags=artist,album:format=format_name",
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

		fileName := filepath.Base(filePath)

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
