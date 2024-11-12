package main

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/telegram/message/html"
	"github.com/gotd/td/telegram/message/styling"
	"github.com/gotd/td/telegram/updates"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"github.com/iyear/tdl/core/dcpool"
	"github.com/joho/godotenv"
	"github.com/rs/zerolog"
	"github.com/urfave/cli/v2"
	"github.com/xeptore/flaw/v8"
	"gopkg.in/matryer/try.v1"

	"github.com/xeptore/tgtd/cache"
	"github.com/xeptore/tgtd/config"
	"github.com/xeptore/tgtd/constant"
	"github.com/xeptore/tgtd/ctxutil"
	"github.com/xeptore/tgtd/errutil"
	"github.com/xeptore/tgtd/log"
	"github.com/xeptore/tgtd/must"
	"github.com/xeptore/tgtd/tgutil"
	"github.com/xeptore/tgtd/tidl"
	"github.com/xeptore/tgtd/tidl/auth"
)

const (
	flagConfigFilePath = "config"
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

	//nolint:exhaustruct
	app := &cli.App{
		Name:     "tgtd",
		Version:  constant.Version,
		Compiled: constant.CompileTime,
		Suggest:  true,
		Usage:    "Telegram TIDAL Uploader",
		Commands: []*cli.Command{
			//nolint:exhaustruct
			{
				Name:    "run",
				Aliases: []string{"r"},
				Usage:   "Run the bot",
				Action:  run,
				Flags: []cli.Flag{
					//nolint:exhaustruct
					&cli.StringFlag{
						Name:     flagConfigFilePath,
						Aliases:  []string{"c"},
						Usage:    "Config file path",
						Required: false,
					},
				},
			},
		},
	}

	if err := app.Run(os.Args); nil != err {
		if errors.Is(err, context.Canceled) {
			logger.Trace().Msg("Application was canceled")
			return
		}
		if flawErr := new(flaw.Flaw); errors.As(err, &flawErr) {
			logger.Fatal().Func(log.Flaw(flawErr)).Msg("Application exited with flaw")
			return
		}
		logger.Fatal().Err(err).Msg("Application exited with error")
	}
}

func run(cliCtx *cli.Context) (err error) {
	ctx, cancel := signal.NotifyContext(cliCtx.Context, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	logger := log.NewPretty(os.Stdout).Level(zerolog.TraceLevel)
	var (
		appHash  = os.Getenv("APP_HASH")
		cfgEnv   = os.Getenv("CONFIG")
		botToken = os.Getenv("BOT_TOKEN")
		cfg      *config.Config
	)
	cfgFilePath := cliCtx.String(flagConfigFilePath)
	switch {
	case cfgFilePath != "" && cfgEnv != "":
		return errors.New("config file path and config environment variable are both set. specify only one")
	case cfgFilePath == "" && cfgEnv == "":
		return errors.New("config file path and config environment variable are both empty. specify one")
	case cfgFilePath != "":
		logger.Debug().Str("config_file_path", cfgFilePath).Msg("Loading config from file")
		c, err := config.FromFile(cfgFilePath)
		if nil != err {
			return fmt.Errorf("failed to load config file: %v", err)
		}
		cfg = c
	default:
		logger.Debug().Msg("Loading config from environment variable")
		c, err := config.FromString(cfgEnv)
		if nil != err {
			return fmt.Errorf("failed to load config from environment variable: %v", err)
		}
		cfg = c
	}

	appID, err := strconv.Atoi(os.Getenv("APP_ID"))
	if nil != err {
		return errors.New("failed to parse APP_ID environment variable to integer")
	}

	if _, err := os.ReadDir(cfg.CredsDir); nil != err && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("failed to read credentials directory: %v", err)
	} else if errors.Is(err, os.ErrNotExist) {
		logger.Warn().Msg("Credentials directory does not exist. Creating...")
		if err := os.MkdirAll(cfg.CredsDir, 0o0755); nil != err {
			return fmt.Errorf("failed to create download base directory: %v", err)
		}
		logger.Info().Msg("Credentials directory created")
	}

	d := tg.NewUpdateDispatcher()
	d.OnNewMessage(func(context.Context, tg.Entities, *tg.UpdateNewMessage) error { return nil })
	updateHandler := updates.New(updates.Config{Handler: d}) //nolint:exhaustruct

	client := telegram.NewClient(
		appID,
		appHash,
		//nolint:exhaustruct
		telegram.Options{
			SessionStorage: &session.FileStorage{Path: filepath.Join(cfg.CredsDir, "session.json")},
			UpdateHandler:  updateHandler,
			MaxRetries:     -1,
			AckBatchSize:   100,
			AckInterval:    10 * time.Second,
			RetryInterval:  5 * time.Second,
			DialTimeout:    10 * time.Second,
			Device:         tgutil.Device,
			Middlewares:    tgutil.DefaultMiddlewares(ctx),
		},
	)
	logger.Debug().Msg("Telegram client initialized.")

	w := &Worker{
		mutex:      sync.Mutex{},
		config:     cfg,
		client:     client,
		sender:     nil,
		tidlAuth:   nil,
		currentJob: nil,
		cache:      cache.New[*tidl.Album](),
		logger:     logger.With().Str("module", "worker").Logger(),
	}

	clientCtx, cancel := ctxutil.WithDelayedTimeout(ctx, 5*time.Second)
	defer cancel()

	// Intentionally ignore client-inherited context, which is inherited from clientCtx
	// for the run function to force it to use the parent context, which is inherited
	// from cli context. This allows all Telegram messaging operations context to still
	// be active a bit more after parent context cancellation.
	return client.Run(clientCtx, func(_ context.Context) error {
		status, err := client.Auth().Status(ctx)
		if nil != err {
			if errors.Is(ctx.Err(), context.Canceled) {
				return context.Canceled
			}
			return fmt.Errorf("failed to get Telegram client auth status: %v", err)
		}
		if !status.Authorized {
			if _, authErr := client.Auth().Bot(ctx, botToken); nil != authErr {
				if errors.Is(ctx.Err(), context.Canceled) {
					return context.Canceled
				}
				return fmt.Errorf("failed to authorize Telegram bot: %v", authErr)
			}
			logger.Debug().Msg("Telegram client authorized.")
		} else {
			logger.Debug().Msg("Telegram client has already been authorized.")
		}

		api := tg.NewClient(client)
		w.sender = message.NewSender(api)

		chat := w.sender.Resolve(cfg.TargetPeerID)

		if _, err = chat.StyledText(clientCtx, styling.Italic("Bot has started!")); nil != err {
			switch {
			case errutil.IsContext(clientCtx):
				logger.Error().Msg("Failed to send bot startup message to specified target chat due to context cancellation")
			default:
				return fmt.Errorf("failed to send bot startup message to specified target chat: %v", err)
			}
		}

		tidlAuth, err := auth.Load(ctx, cfg.CredsDir)
		if nil != err {
			switch {
			case errors.Is(ctx.Err(), context.Canceled):
				return context.Canceled
			case errors.Is(err, context.DeadlineExceeded):
				return fmt.Errorf("failed to load TIDAL auth due to deadline exceeded: %v", err)
			case errors.Is(err, auth.ErrUnauthorized):
				// continue as we're gonna kick off the authorization flow
			case errutil.IsFlaw(err):
				return err
			default:
				panic(errutil.UnknownError(err))
			}

			logger.Debug().Msg("Need to authenticate TIDAL. Initiating TIDAL authorization flow")
			authorization, wait, err := auth.NewAuthorizer(ctx, cfg.CredsDir)
			if nil != err {
				switch {
				case errors.Is(ctx.Err(), context.Canceled):
					return context.Canceled
				case errors.Is(err, context.DeadlineExceeded):
					return fmt.Errorf("failed to initialize TIDAL authorizer due to deadline exceeded: %v", err)
				case errutil.IsFlaw(err):
					return err
				default:
					panic(errutil.UnknownError(err))
				}
			}

			_, err = chat.StyledText(
				clientCtx,
				styling.Plain("Please visit the following link to authorize the application:"),
				styling.Plain("\n"),
				styling.URL(authorization.URL),
				styling.Plain("\n"),
				styling.Plain("Authorization link will expire in "),
				styling.Code(authorization.ExpiresIn.String()),
				styling.Plain("\n"),
				styling.Plain("\n"),
				styling.Italic("Waiting for authentication..."),
			)
			if nil != err {
				if errors.Is(clientCtx.Err(), context.Canceled) {
					return context.Canceled
				}
				return fmt.Errorf("failed to send TIDAL authentication link to specified peer: %v", err)
			}

			logger.Info().Msg("Waiting for TIDAL authentication")
			res := <-wait
			if err := res.Err(); nil != err {
				switch {
				case errors.Is(ctx.Err(), context.Canceled):
					return context.Canceled
				case errors.Is(err, auth.ErrAuthWaitTimeout):
					if _, err = chat.StyledText(clientCtx, styling.Plain("Authorization URL expired. Restart the bot to initiate the authorization flow again.")); nil != err {
						if errors.Is(clientCtx.Err(), context.Canceled) {
							return context.Canceled
						}
						return fmt.Errorf("failed to send TIDAL authentication URL expired message to specified target chat: %v", err)
					}
					return errors.New("TIDAL authorization URL expired")
				case errutil.IsFlaw(err):
					logger.Error().Func(log.Flaw(err)).Msg("TIDAL authentication failed")
					lines := []styling.StyledTextOption{
						styling.Plain("TIDAL authentication failed:"),
						styling.Plain("\n"),
						styling.Code(err.Error()),
						styling.Plain("\n"),
						styling.Plain("Restart the bot to initiate the authorization flow again."),
					}
					if _, err := chat.StyledText(clientCtx, lines...); nil != err {
						if errors.Is(clientCtx.Err(), context.Canceled) {
							return context.Canceled
						}
						return fmt.Errorf("failed to send TIDAL authentication failure error message to specified target chat: %v", err)
					}
					return err
				default:
					panic(errutil.UnknownError(err))
				}
			}

			logger.Info().Msg("TIDAL authentication was successful")
			lines := []styling.StyledTextOption{
				styling.Plain("TIDAL authentication was successful!"),
				styling.Plain("\n"),
				styling.Plain("Waiting for your command..."),
			}
			if _, err := chat.StyledText(clientCtx, lines...); nil != err {
				if errors.Is(clientCtx.Err(), context.Canceled) {
					return context.Canceled
				}
				return fmt.Errorf("failed to send TIDAL authentication successful message to specified target chat: %v", err)
			}
			tidlAuth = res.Unwrap()
		}

		if err := tidlAuth.VerifyAccessToken(ctx); nil != err {
			switch {
			case errors.Is(ctx.Err(), context.Canceled):
				return context.Canceled
			case errors.Is(err, context.DeadlineExceeded):
				return fmt.Errorf("failed to verify TIDAL access token due to deadline exceeded: %v", err)
			case errors.Is(err, auth.ErrUnauthorized):
				return errors.New("TIDAL authentication expired. Please reauthorize the application")
			case errutil.IsFlaw(err):
				return err
			default:
				panic(errutil.UnknownError(err))
			}
		}

		logger.Debug().Msg("TIDAL access token verified")
		w.tidlAuth = tidlAuth
		d.OnNewMessage(buildOnMessage(w, clientCtx, *cfg))

		logger.Info().Msg("Bot is running")
		<-ctx.Done()

		logger.Debug().Msg("Stopping bot due to received signal")
		if _, err = chat.StyledText(clientCtx, styling.Italic("Bot is shutting down...")); nil != err {
			switch {
			case errors.Is(clientCtx.Err(), context.Canceled):
				logger.Error().Msg("Failed to send shutdown message to specified target chat due to context cancellation")
			case errors.Is(clientCtx.Err(), context.DeadlineExceeded):
				logger.Error().Msg("Failed to send shutdown message to specified target chat due to context deadline exceeded")
			default:
				return fmt.Errorf("failed to send bot shutdown message to specified target chat: %v", err)
			}
		}
		return nil
	})
}

func buildOnMessage(w *Worker, msgCtx context.Context, cfg config.Config) func(context.Context, tg.Entities, *tg.UpdateNewMessage) error {
	return func(ctx context.Context, e tg.Entities, update *tg.UpdateNewMessage) error {
		m, ok := update.Message.(*tg.Message)
		if !ok || m.Out {
			return nil
		}
		if u, ok := m.PeerID.(*tg.PeerUser); !ok || !slices.Contains(w.config.FromIDs, u.UserID) {
			return nil
		}
		msg := m.Message
		reply := w.sender.Reply(e, update)

		if msg == "/start" {
			if _, err := reply.Text(msgCtx, "Hello!"); nil != err {
				if errors.Is(msgCtx.Err(), context.Canceled) {
					return nil
				}
				flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
				w.logger.Error().Func(log.Flaw(flaw.From(err).Append(flawP))).Msg("Failed to send reply")
			}
			return nil
		}

		if msg == "/authorize" {
			authorization, wait, err := auth.NewAuthorizer(ctx, cfg.CredsDir)
			if nil != err {
				switch {
				case errors.Is(ctx.Err(), context.Canceled):
					if _, err := reply.StyledText(msgCtx, styling.Plain("Authorizer initialization canceled")); nil != err {
						if errors.Is(msgCtx.Err(), context.DeadlineExceeded) {
							w.logger.Error().Func(log.Flaw(flaw.From(err))).Msg("Timeout while sending reply")
							return nil
						}
						flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
						w.logger.Error().Func(log.Flaw(flaw.From(err).Append(flawP))).Msg("Failed to send reply")
						return nil
					}
					return nil
				case errors.Is(err, context.DeadlineExceeded):
					lines := []styling.StyledTextOption{
						styling.Plain("Issuing authorization request took too much time to respond."),
						styling.Plain("\n"),
						styling.Plain("Execute the command again with a delay."),
					}
					w.logger.Error().Func(log.Flaw(flaw.From(err))).Msg("TIDAL authorizer initialization timed out")
					if _, err := reply.StyledText(msgCtx, lines...); nil != err {
						if errors.Is(msgCtx.Err(), context.Canceled) {
							return nil
						}
						flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
						w.logger.Error().Func(log.Flaw(flaw.From(err).Append(flawP))).Msg("Failed to send reply")
						return nil
					}
					return nil
				case errutil.IsFlaw(err):
					w.logger.Error().Func(log.Flaw(err)).Msg("Failed to initialize authorizer due to unknown reason")
					if _, err := reply.StyledText(msgCtx, styling.Plain("Failed to initialize authorizer due to unknown reason")); nil != err {
						if errors.Is(msgCtx.Err(), context.Canceled) {
							return nil
						}
						flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
						w.logger.Error().Func(log.Flaw(flaw.From(err).Append(flawP))).Msg("Failed to send reply")
						return nil
					}
					return nil
				default:
					panic(errutil.UnknownError(err))
				}
			}

			lines := []styling.StyledTextOption{
				styling.Plain("Please visit the following link to authorize the application:"),
				styling.Plain("\n"),
				styling.URL(authorization.URL),
				styling.Plain("\n"),
				styling.Plain("Authorization link will expire in "),
				styling.Code(authorization.ExpiresIn.String()),
				styling.Plain("\n"),
				styling.Plain("\n"),
				styling.Italic("Waiting for authentication..."),
			}
			if _, err := reply.StyledText(msgCtx, lines...); nil != err {
				if errors.Is(msgCtx.Err(), context.Canceled) {
					return nil
				}
				flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
				w.logger.Error().Func(log.Flaw(flaw.From(err).Append(flawP))).Msg("Failed to send reply")
				return nil
			}

			res := <-wait
			if err := res.Err(); nil != err {
				switch {
				case errors.Is(ctx.Err(), context.Canceled):
					if _, err := reply.StyledText(msgCtx, styling.Plain("Operation canceled while was waiting for authorization.")); nil != err {
						if errors.Is(msgCtx.Err(), context.DeadlineExceeded) {
							w.logger.Error().Func(log.Flaw(flaw.From(err))).Msg("Timeout while sending reply")
							return nil
						}
						flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
						w.logger.Error().Func(log.Flaw(flaw.From(err).Append(flawP))).Msg("Failed to send reply")
						return nil
					}
					return nil
				case errors.Is(err, auth.ErrAuthWaitTimeout):
					if _, err := reply.StyledText(msgCtx, styling.Plain("Authorization URL expired. Try again with a delay.")); nil != err {
						if errors.Is(msgCtx.Err(), context.Canceled) {
							return nil
						}
						flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
						w.logger.Error().Func(log.Flaw(flaw.From(err).Append(flawP))).Msg("Failed to send reply")
						return nil
					}
					return nil
				case errutil.IsFlaw(err):
					w.logger.Error().Func(log.Flaw(err)).Msg("TIDAL authentication has failed")
					lines := []styling.StyledTextOption{
						styling.Plain("TIDAL authentication has failed due to unknown reason:"),
						styling.Plain("\n"),
						styling.Code(err.Error()),
					}
					if _, err := reply.StyledText(msgCtx, lines...); nil != err {
						if errors.Is(msgCtx.Err(), context.Canceled) {
							return nil
						}
						flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
						w.logger.Error().Func(log.Flaw(flaw.From(err).Append(flawP))).Msg("Failed to send reply")
						return nil
					}
					return nil
				default:
					panic(errutil.UnknownError(err))
				}
			}

			w.tidlAuth = res.Unwrap()

			lines = []styling.StyledTextOption{
				styling.Plain("TIDAL authentication was successful!"),
				styling.Plain("\n"),
				styling.Plain("Waiting for your command..."),
			}
			if _, err := reply.StyledText(msgCtx, lines...); nil != err {
				if errors.Is(msgCtx.Err(), context.Canceled) {
					return nil
				}
				flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
				w.logger.Error().Func(log.Flaw(flaw.From(err).Append(flawP))).Msg("Failed to send reply")
				return nil
			}
			return nil
		}

		if strings.HasPrefix(msg, "/download ") {
			link := strings.TrimSpace(strings.TrimPrefix(msg, "/download "))
			if err := w.run(ctx, m.ID, link); nil != err {
				switch {
				case errors.Is(err, context.Canceled):
					cause := context.Cause(ctx)
					if cause == nil {
						panic("expected cause to be non-nil when the error is context.Canceled")
					}
					if errors.Is(cause, errJobCanceled) {
						w.logger.Info().Str("link", link).Msg("Job canceled by the /cancel command")
						if _, err := reply.StyledText(msgCtx, styling.Plain("Job canceled by the /cancel command")); nil != err {
							if errors.Is(msgCtx.Err(), context.Canceled) {
								return nil
							}
							flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
							w.logger.Error().Func(log.Flaw(flaw.From(err).Append(flawP))).Msg("Failed to send reply")
							return nil
						}
						return nil
					}
					return nil
				case errors.Is(err, context.DeadlineExceeded):
					if _, err := reply.StyledText(msgCtx, styling.Plain("Job has timed out.")); nil != err {
						if errors.Is(msgCtx.Err(), context.Canceled) {
							return nil
						}
						flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
						w.logger.Error().Func(log.Flaw(flaw.From(err).Append(flawP))).Msg("Failed to send reply")
						return nil
					}
					return nil
				case errors.Is(err, tidl.ErrTooManyRequests):
					if _, err := reply.StyledText(msgCtx, styling.Plain("Received too many requests error while downloading from TIDAL.")); nil != err {
						if errors.Is(msgCtx.Err(), context.Canceled) {
							return nil
						}
						flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
						w.logger.Error().Func(log.Flaw(flaw.From(err).Append(flawP))).Msg("Failed to send reply")
						return nil
					}
					return nil
				}
				// handling the rest of possible error types that are not supported by switch/case syntactically.
				if errInvalidLink := new(InvalidLinkError); errors.As(err, &errInvalidLink) {
					lines := []styling.StyledTextOption{
						styling.Plain("Failed to parse link:"),
						styling.Plain("\n"),
						styling.Code(errInvalidLink.Error()),
					}
					if _, err := reply.StyledText(msgCtx, lines...); nil != err {
						if errors.Is(msgCtx.Err(), context.Canceled) {
							return nil
						}
						flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
						w.logger.Error().Func(log.Flaw(flaw.From(err).Append(flawP))).Msg("Failed to send reply")
						return nil
					}
					return nil
				}

				if errAlreadyRunning := new(JobAlreadyRunningError); errors.As(err, &errAlreadyRunning) {
					lines := []styling.StyledTextOption{
						styling.Plain("Another job is still running."),
						styling.Plain("\n"),
						styling.Plain("Cancel with "),
						styling.BotCommand("/cancel"),
					}
					if _, err := reply.StyledText(msgCtx, lines...); nil != err {
						if errors.Is(msgCtx.Err(), context.Canceled) {
							return nil
						}
						flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
						w.logger.Error().Func(log.Flaw(flaw.From(err).Append(flawP))).Msg("Failed to send reply")
						return nil
					}
					return nil
				}

				w.logger.Error().Func(log.Flaw(err)).Msg("Failed to run job")
				flawBytes, err := errutil.FlawToYAML(must.BeFlaw(err))
				if nil != err {
					w.logger.Error().Func(log.Flaw(err)).Msg("Failed to convert flaw to TOML")
					return nil
				}
				up, cancel := w.newUploader(ctx)
				defer func() {
					if cancelErr := cancel(); nil != cancelErr {
						flawP := flaw.P{"err_debug_tree": errutil.Tree(cancelErr).FlawP()}
						w.logger.Error().Func(log.Flaw(flaw.From(cancelErr).Append(flawP))).Msg("Failed to close uploader pool")
					}
				}()

				upload, err := up.FromReader(ctx, "flaw.yaml", bytes.NewReader(flawBytes))
				if nil != err {
					flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
					w.logger.Error().Func(log.Flaw(flaw.From(err).Append(flawP))).Msg("Failed to upload flaw to YAML")
					return nil
				}
				document := message.UploadedDocument(upload)
				document.
					MIME("application/yaml").
					Attributes(
						&tg.DocumentAttributeFilename{
							FileName: filepath.Base(
								fmt.Sprintf("flaw-%s.yaml", time.Now().Format("2006-01-02-15-04-05")),
							),
						},
					).
					ForceFile(true)
				if _, err := reply.Media(msgCtx, document); nil != err {
					if errors.Is(msgCtx.Err(), context.Canceled) {
						return nil
					}
					flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
					w.logger.Error().Func(log.Flaw(flaw.From(err).Append(flawP))).Msg("Failed to send reply")
					return nil
				}
				return nil
			}

			w.logger.Info().Str("link", link).Msg("Job succeeded")
			return nil
		}

		if msg == "/cancel" {
			if err := w.cancelCurrentJob(); nil != err {
				if !errors.Is(err, os.ErrProcessDone) {
					panic(fmt.Sprintf("unexpected error of type %T: %v", err, err))
				}
				if _, err := reply.StyledText(msgCtx, styling.Plain("No job was running.")); nil != err {
					if errors.Is(msgCtx.Err(), context.Canceled) {
						return nil
					}
					flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
					w.logger.Error().Func(log.Flaw(flaw.From(err).Append(flawP))).Msg("Failed to send reply")
					return nil
				}
				return nil
			}

			if _, err := reply.StyledText(msgCtx, styling.Plain("Job was canceled.")); nil != err {
				if errors.Is(msgCtx.Err(), context.Canceled) {
					return nil
				}
				flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
				w.logger.Error().Func(log.Flaw(flaw.From(err).Append(flawP))).Msg("Failed to send reply")
				return nil
			}
		}

		return nil
	}
}

type Worker struct {
	mutex      sync.Mutex
	config     *config.Config
	client     *telegram.Client
	sender     *message.Sender
	tidlAuth   *auth.Auth
	currentJob *Job
	cache      *cache.Cache[*tidl.Album]
	logger     zerolog.Logger
}

func (w *Worker) newUploader(ctx context.Context) (*uploader.Uploader, func() error) {
	pool := dcpool.NewPool(w.client, 8, tgutil.DefaultMiddlewares(ctx)...)
	return uploader.NewUploader(pool.Default(ctx)).WithPartSize(uploader.MaximumPartSize).WithThreads(4), pool.Close
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
	return fmt.Sprintf("job %q is already running", e.ID)
}

type InvalidLinkError struct {
	Link string
	Err  error
}

func (e *InvalidLinkError) Error() string {
	return fmt.Sprintf("invalid link %q: %v", e.Link, e.Err)
}

func parse(link string) (string, string, error) {
	parsedURL, err := url.Parse(link)
	if nil != err {
		return "", "", &InvalidLinkError{Link: link, Err: fmt.Errorf("failed to parse URL: %v", err)}
	}
	kind, id, found := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(parsedURL.Path, "/browse/"), "/"), "/")
	if !found {
		return "", "", &InvalidLinkError{Link: link, Err: errors.New("failed to cut path")}
	}
	switch kind {
	case "playlist", "album", "track", "mix":
		return id, kind, nil
	default:
		return "", "", &InvalidLinkError{Link: link, Err: fmt.Errorf("unsupported kind %q", kind)}
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

var errJobCanceled = errors.New("job canceled by the user")

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
		return err
	}

	flawP := flaw.P{"id": id, "kind": kind}

	jobCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(errJobCanceled)

	job := Job{
		ID:        id,
		CreatedAt: time.Now(),
		Link:      link,
		MessageID: msgID,
		cancel:    func() { cancel(errJobCanceled) },
	}
	flawP["job"] = job.flawP()
	w.currentJob = &job

	const downloadBaseDir = "downloads"
	downloader := tidl.NewDownloader(
		w.tidlAuth,
		downloadBaseDir,
		w.cache,
		w.logger.With().Logger(),
	)

	reply := w.sender.Resolve(w.config.TargetPeerID).Reply(msgID)

	switch kind {
	case "playlist":
		w.logger.Info().Str("id", id).Str("link", link).Msg("Starting download playlist")
		if _, err := reply.StyledText(jobCtx, html.Format(nil, "<b><em>Downloading playlist...</em></b>")); nil != err {
			if errors.Is(jobCtx.Err(), context.Canceled) {
				return jobCtx.Err()
			}
			return flaw.From(fmt.Errorf("failed to send message: %v", err))
		}

		err = try.Do(func(attempt int) (retry bool, err error) {
			const maxAttempts = 3
			attemptRemained := attempt < maxAttempts
			time.Sleep(time.Duration(attempt-1) * 3 * time.Second)
			if err := downloader.Playlist(jobCtx, id); nil != err {
				switch {
				case errutil.IsContext(ctx):
					return false, err
				case errors.Is(err, context.DeadlineExceeded):
					return attemptRemained, context.DeadlineExceeded
				case errors.Is(err, tidl.ErrTooManyRequests):
					return attemptRemained, tidl.ErrTooManyRequests
				case errutil.IsFlaw(err):
					return false, must.BeFlaw(err).Append(flawP)
				default:
					panic(errutil.UnknownError(err))
				}
			}
			return false, nil
		})
		if nil != err {
			return err
		}

		w.logger.Info().Str("id", id).Str("link", link).Msg("Download finished. Starting playlist upload")
		if _, err := reply.StyledText(jobCtx, html.Format(nil, "<b><em>Download finished. Starting playlist upload...</em></b>")); nil != err {
			if errors.Is(jobCtx.Err(), context.Canceled) {
				return jobCtx.Err()
			}
			return flaw.From(fmt.Errorf("failed to send message: %v", err))
		}

		if err := w.uploadPlaylist(jobCtx, downloadBaseDir); nil != err {
			switch {
			case errutil.IsContext(ctx), errors.Is(err, context.DeadlineExceeded):
				return err
			case errutil.IsFlaw(err):
				return must.BeFlaw(err).Append(flawP)
			default:
				panic(errutil.UnknownError(err))
			}
		}

		w.logger.Info().Str("id", id).Str("link", link).Msg("Playlist upload finished")
		if _, err := reply.StyledText(jobCtx, html.Format(nil, "<b><em>Playlist uploaded successfully.</em></b>")); nil != err {
			if errors.Is(jobCtx.Err(), context.Canceled) {
				return jobCtx.Err()
			}
			return flaw.From(fmt.Errorf("failed to send message: %v", err))
		}
	case "album":
		w.logger.Info().Str("id", id).Str("link", link).Msg("Starting download album")
		if _, err := reply.StyledText(jobCtx, html.Format(nil, "<b><em>Downloading album...</em></b>")); nil != err {
			if errors.Is(jobCtx.Err(), context.Canceled) {
				return jobCtx.Err()
			}
			return flaw.From(fmt.Errorf("failed to send message: %v", err))
		}

		err = try.Do(func(attempt int) (retry bool, err error) {
			const maxAttempts = 3
			attemptRemained := attempt < maxAttempts
			time.Sleep(time.Duration(attempt-1) * 3 * time.Second)
			if err := downloader.Album(jobCtx, id); nil != err {
				switch {
				case errutil.IsContext(ctx):
					return false, err
				case errors.Is(err, context.DeadlineExceeded):
					return attemptRemained, context.DeadlineExceeded
				case errors.Is(err, tidl.ErrTooManyRequests):
					return attemptRemained, tidl.ErrTooManyRequests
				case errutil.IsFlaw(err):
					return false, must.BeFlaw(err).Append(flawP)
				default:
					panic(errutil.UnknownError(err))
				}
			}
			return false, nil
		})
		if nil != err {
			return err
		}

		w.logger.Info().Str("id", id).Str("link", link).Msg("Download finished. Starting album upload")
		if _, err := reply.StyledText(jobCtx, html.Format(nil, "<b><em>Download finished. Starting album upload...</em></b>")); nil != err {
			if errors.Is(jobCtx.Err(), context.Canceled) {
				return jobCtx.Err()
			}
			return flaw.From(fmt.Errorf("failed to send message: %v", err))
		}

		if err := w.uploadAlbum(jobCtx, downloadBaseDir); nil != err {
			if errutil.IsContext(ctx) {
				return ctx.Err()
			}
			return flaw.From(fmt.Errorf("failed to send message: %v", err))
		}

		w.logger.Info().Str("id", id).Str("link", link).Msg("Album upload finished")
		if _, err := reply.StyledText(jobCtx, html.Format(nil, "<b><em>Album uploaded successfully.</em></b>")); nil != err {
			if errors.Is(jobCtx.Err(), context.Canceled) {
				return jobCtx.Err()
			}
			return flaw.From(fmt.Errorf("failed to send message: %v", err))
		}
	case "track":
		w.logger.Info().Str("id", id).Str("link", link).Msg("Starting download track")
		if _, err := reply.StyledText(jobCtx, html.Format(nil, "<b><em>Downloading track...</em></b>")); nil != err {
			if errors.Is(jobCtx.Err(), context.Canceled) {
				return jobCtx.Err()
			}
			return flaw.From(fmt.Errorf("failed to send message: %v", err))
		}

		err = try.Do(func(attempt int) (retry bool, err error) {
			const maxAttempts = 3
			attemptRemained := attempt < maxAttempts
			time.Sleep(time.Duration(attempt-1) * 3 * time.Second)
			if err := downloader.Track(jobCtx, id); nil != err {
				switch {
				case errutil.IsContext(ctx):
					return false, err
				case errors.Is(err, context.DeadlineExceeded):
					return attemptRemained, context.DeadlineExceeded
				case errors.Is(err, tidl.ErrTooManyRequests):
					return attemptRemained, tidl.ErrTooManyRequests
				case errutil.IsFlaw(err):
					return false, must.BeFlaw(err).Append(flawP)
				default:
					panic(errutil.UnknownError(err))
				}
			}
			return false, nil
		})
		if nil != err {
			return err
		}

		w.logger.Info().Str("id", id).Str("link", link).Msg("Download finished. Starting track upload")
		if _, err := reply.StyledText(jobCtx, html.Format(nil, "<b><em>Download finished. Starting track upload...</em></b>")); nil != err {
			if errors.Is(jobCtx.Err(), context.Canceled) {
				return jobCtx.Err()
			}
			return flaw.From(fmt.Errorf("failed to send message: %v", err))
		}

		if err := w.uploadSingle(jobCtx, downloadBaseDir); nil != err {
			if errutil.IsContext(ctx) {
				return ctx.Err()
			}
			return flaw.From(fmt.Errorf("failed to send message: %v", err))
		}

		w.logger.Info().Str("id", id).Str("link", link).Msg("Track upload finished")
		if _, err := reply.StyledText(jobCtx, html.Format(nil, "<b><em>Track uploaded successfully.</em></b>")); nil != err {
			if errors.Is(jobCtx.Err(), context.Canceled) {
				return jobCtx.Err()
			}
			return flaw.From(fmt.Errorf("failed to send message: %v", err))
		}
	case "mix":
		w.logger.Info().Str("id", id).Str("link", link).Msg("Starting download mix")
		if _, err := reply.StyledText(jobCtx, html.Format(nil, "<b><em>Downloading mix...</em></b>")); nil != err {
			if errors.Is(jobCtx.Err(), context.Canceled) {
				return jobCtx.Err()
			}
			return flaw.From(fmt.Errorf("failed to send message: %v", err))
		}

		err = try.Do(func(attempt int) (retry bool, err error) {
			const maxAttempts = 3
			attemptRemained := attempt < maxAttempts
			time.Sleep(time.Duration(attempt-1) * 3 * time.Second)
			if err := downloader.Mix(jobCtx, id); nil != err {
				switch {
				case errutil.IsContext(ctx):
					return false, err
				case errors.Is(err, context.DeadlineExceeded):
					return attemptRemained, context.DeadlineExceeded
				case errors.Is(err, tidl.ErrTooManyRequests):
					return attemptRemained, tidl.ErrTooManyRequests
				case errutil.IsFlaw(err):
					return false, must.BeFlaw(err).Append(flawP)
				default:
					panic(errutil.UnknownError(err))
				}
			}
			return false, nil
		})
		if nil != err {
			return err
		}

		w.logger.Info().Str("id", id).Str("link", link).Msg("Download finished. Starting mix upload")
		if _, err := reply.StyledText(jobCtx, html.Format(nil, "<b><em>Download finished. Starting mix upload...</em></b>")); nil != err {
			if errors.Is(jobCtx.Err(), context.Canceled) {
				return jobCtx.Err()
			}
			return flaw.From(fmt.Errorf("failed to send message: %v", err))
		}

		if err := w.uploadMix(jobCtx, downloadBaseDir); nil != err {
			if errutil.IsContext(ctx) {
				return ctx.Err()
			}
			return flaw.From(fmt.Errorf("failed to send message: %v", err))
		}

		w.logger.Info().Str("id", id).Str("link", link).Msg("Mix upload finished")
		if _, err := reply.StyledText(jobCtx, html.Format(nil, "<b><em>Mix uploaded successfully.</em></b>")); nil != err {
			if errors.Is(jobCtx.Err(), context.Canceled) {
				return jobCtx.Err()
			}
			return flaw.From(fmt.Errorf("failed to send message: %v", err))
		}
	default:
		panic("unsupported link kind to download: " + kind)
	}
	return nil
}
