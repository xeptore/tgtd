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
	"github.com/xeptore/tgtd/tidal"
	"github.com/xeptore/tgtd/tidal/auth"
	tidaldl "github.com/xeptore/tgtd/tidal/download"
	tidalfs "github.com/xeptore/tgtd/tidal/fs"
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
	lvl, _ := zerolog.ParseLevel(cfg.LogLevel)
	logger = logger.Level(lvl)

	appID, err := strconv.Atoi(os.Getenv("APP_ID"))
	if nil != err {
		return errors.New("failed to parse APP_ID environment variable to integer")
	}

	if _, err := os.ReadDir(cfg.CredsDir); nil != err && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("failed to read credentials directory: %v", err)
	} else if errors.Is(err, os.ErrNotExist) {
		logger.Warn().Msg("Credentials directory does not exist. Creating...")
		if err := os.MkdirAll(cfg.CredsDir, 0o0700); nil != err {
			return fmt.Errorf("failed to create download base directory: %v", err)
		}
		logger.Info().Msg("Credentials directory created")
	}

	handler := func(ctx context.Context, u tg.UpdatesClass) error { return nil }
	//nolint:exhaustruct
	updatesConfig := updates.Config{
		Handler: telegram.UpdateHandlerFunc(func(ctx context.Context, u tg.UpdatesClass) error { return handler(ctx, u) }),
	}

	client := telegram.NewClient(
		appID,
		appHash,
		//nolint:exhaustruct
		telegram.Options{
			SessionStorage: &session.FileStorage{Path: filepath.Join(cfg.CredsDir, "session.json")},
			UpdateHandler:  updates.New(updatesConfig), //nolint:exhaustruct
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
		tidalAuth:  nil,
		currentJob: nil,
		cache:      cache.New(),
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

		fatherChat := w.sender.Resolve(cfg.TargetPeerID)

		if _, err = fatherChat.StyledText(clientCtx, styling.Italic("Bot has started!")); nil != err {
			switch {
			case errutil.IsContext(clientCtx):
				logger.Error().Msg("Failed to send bot startup message to specified target chat due to context cancellation")
			default:
				return fmt.Errorf("failed to send bot startup message to specified target chat: %v", err)
			}
		}

		tidlAuth, err := auth.LoadFromDir(ctx, cfg.CredsDir)
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

			_, err = fatherChat.StyledText(
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
					if _, err = fatherChat.StyledText(clientCtx, styling.Plain("Authorization URL expired. Restart the bot to initiate the authorization flow again.")); nil != err {
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
					if _, err := fatherChat.StyledText(clientCtx, lines...); nil != err {
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
			if _, err := fatherChat.StyledText(clientCtx, lines...); nil != err {
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
		w.tidalAuth = tidlAuth
		handler = buildHandler(w)

		logger.Info().Msg("Bot is running")
		<-ctx.Done()

		logger.Debug().Msg("Stopping bot due to received signal")
		if _, err = fatherChat.StyledText(clientCtx, styling.Italic("Bot is shutting down...")); nil != err {
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

func buildHandler(w *Worker) telegram.UpdateHandlerFunc {
	return func(ctx context.Context, u tg.UpdatesClass) error {
		updates, ok := u.(*tg.Updates)
		if !ok || u == nil {
			return nil
		}

		chats := updates.MapChats()
		entities := tg.Entities{
			Short:    false,
			Users:    updates.MapUsers().NotEmptyToMap(),
			Chats:    chats.ChatToMap(),
			Channels: chats.ChannelToMap(),
		}

		for _, update := range updates.Updates {
			switch us := update.(type) {
			case *tg.UpdateNewMessage:
				if err := w.process(ctx, entities, us); nil != err {
					w.logger.Error().Err(err).Msg("Failed to process new message update")
				}
			case *tg.UpdateNewChannelMessage:
				if err := w.process(ctx, entities, us); nil != err {
					w.logger.Error().Err(err).Msg("Failed to process new channel message update")
				}
			default:
				w.logger.Info().Str("type", fmt.Sprintf("%T", us)).Msg("Unsupported update type received")
			}
		}
		return nil
	}
}

func (w *Worker) process(ctx context.Context, e tg.Entities, m message.AnswerableMessageUpdate) error {
	msg, ok := m.GetMessage().(*tg.Message)
	if !ok || msg.Out {
		return nil
	}
	reply := w.sender.Reply(e, m)

	switch peer := msg.PeerID.(type) {
	case *tg.PeerChat:
		if u, ok := msg.FromID.(*tg.PeerUser); !ok || !slices.Contains(w.config.FromIDs, u.UserID) {
			return nil
		}
	case *tg.PeerChannel:
		if u, ok := msg.FromID.(*tg.PeerUser); !ok || !slices.Contains(w.config.FromIDs, u.UserID) {
			return nil
		}
	case *tg.PeerUser:
		if !slices.Contains(w.config.FromIDs, peer.UserID) {
			return nil
		}
	default:
		if _, err := reply.Text(ctx, "Unsupported invocation."); nil != err {
			if errors.Is(ctx.Err(), context.Canceled) {
				return nil
			}
			flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
			w.logger.Error().Func(log.Flaw(flaw.From(err).Append(flawP))).Msg("Failed to send reply")
		}
	}

	if msg.Message == "/start" {
		if _, err := reply.Text(ctx, "Hello!"); nil != err {
			if errors.Is(ctx.Err(), context.Canceled) {
				return nil
			}
			flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
			w.logger.Error().Func(log.Flaw(flaw.From(err).Append(flawP))).Msg("Failed to send reply")
		}
		return nil
	}

	if msg.Message == "/authorize" {
		authorization, wait, err := auth.NewAuthorizer(ctx, w.config.CredsDir)
		if nil != err {
			switch {
			case errors.Is(ctx.Err(), context.Canceled):
				if _, err := reply.StyledText(ctx, styling.Plain("Authorizer initialization canceled")); nil != err {
					if errors.Is(ctx.Err(), context.DeadlineExceeded) {
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
				if _, err := reply.StyledText(ctx, lines...); nil != err {
					if errors.Is(ctx.Err(), context.Canceled) {
						return nil
					}
					flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
					w.logger.Error().Func(log.Flaw(flaw.From(err).Append(flawP))).Msg("Failed to send reply")
					return nil
				}
				return nil
			case errutil.IsFlaw(err):
				w.logger.Error().Func(log.Flaw(err)).Msg("Failed to initialize authorizer due to unknown reason")
				if _, err := reply.StyledText(ctx, styling.Plain("Failed to initialize authorizer due to unknown reason")); nil != err {
					if errors.Is(ctx.Err(), context.Canceled) {
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
		if _, err := reply.StyledText(ctx, lines...); nil != err {
			if errors.Is(ctx.Err(), context.Canceled) {
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
				if _, err := reply.StyledText(ctx, styling.Plain("Operation canceled while was waiting for authorization.")); nil != err {
					if errors.Is(ctx.Err(), context.DeadlineExceeded) {
						w.logger.Error().Func(log.Flaw(flaw.From(err))).Msg("Timeout while sending reply")
						return nil
					}
					flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
					w.logger.Error().Func(log.Flaw(flaw.From(err).Append(flawP))).Msg("Failed to send reply")
					return nil
				}
				return nil
			case errors.Is(err, auth.ErrAuthWaitTimeout):
				if _, err := reply.StyledText(ctx, styling.Plain("Authorization URL expired. Try again with a delay.")); nil != err {
					if errors.Is(ctx.Err(), context.Canceled) {
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
				if _, err := reply.StyledText(ctx, lines...); nil != err {
					if errors.Is(ctx.Err(), context.Canceled) {
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

		w.tidalAuth = res.Unwrap()

		lines = []styling.StyledTextOption{
			styling.Plain("TIDAL authentication was successful!"),
			styling.Plain("\n"),
			styling.Plain("Waiting for your command..."),
		}
		if _, err := reply.StyledText(ctx, lines...); nil != err {
			if errors.Is(ctx.Err(), context.Canceled) {
				return nil
			}
			flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
			w.logger.Error().Func(log.Flaw(flaw.From(err).Append(flawP))).Msg("Failed to send reply")
			return nil
		}
		return nil
	}

	if msg.Message == "/cancel" {
		if err := w.cancelCurrentJob(); nil != err {
			if !errors.Is(err, os.ErrProcessDone) {
				panic(fmt.Sprintf("unexpected error of type %T: %v", err, err))
			}
			if _, err := reply.StyledText(ctx, styling.Plain("No job was running.")); nil != err {
				if errors.Is(ctx.Err(), context.Canceled) {
					return nil
				}
				flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
				w.logger.Error().Func(log.Flaw(flaw.From(err).Append(flawP))).Msg("Failed to send reply")
				return nil
			}
			return nil
		}

		if _, err := reply.StyledText(ctx, styling.Plain("Job was canceled.")); nil != err {
			if errors.Is(ctx.Err(), context.Canceled) {
				return nil
			}
			flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
			w.logger.Error().Func(log.Flaw(flaw.From(err).Append(flawP))).Msg("Failed to send reply")
			return nil
		}
	}

	if tidal.IsLink(msg.Message) {
		// Assuming it's the default type of command, i.e., download
		link, err := parseLink(msg.Message)
		if nil != err {
			if errInvalidLink := new(InvalidLinkError); errors.As(err, &errInvalidLink) {
				lines := []styling.StyledTextOption{
					styling.Plain("Failed to parse link:"),
					styling.Plain("\n"),
					styling.Code(errInvalidLink.Error()),
				}
				if _, err := reply.StyledText(ctx, lines...); nil != err {
					if errors.Is(ctx.Err(), context.Canceled) {
						return nil
					}
					flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
					w.logger.Error().Func(log.Flaw(flaw.From(err).Append(flawP))).Msg("Failed to send reply")
					return nil
				}
				return nil
			}
		}

		if err := w.run(ctx, reply, *link); nil != err {
			switch {
			case errutil.IsContext(ctx):
				// Parent context is canceled. Nothing that we need to do.
				return nil
			case errors.Is(err, context.Canceled):
				// As we checked in the previous case that the parent context is not canceled,
				// we can safely assume that the context was canceled by the user cancellation request.
				w.logger.Info().Msg("Job canceled by the /cancel command")
				if _, err := reply.StyledText(ctx, styling.Plain("Job canceled by the /cancel command")); nil != err {
					if errors.Is(ctx.Err(), context.Canceled) {
						return nil
					}
					flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
					w.logger.Error().Func(log.Flaw(flaw.From(err).Append(flawP))).Msg("Failed to send reply")
					return nil
				}
				return nil
			case errors.Is(err, context.DeadlineExceeded):
				if _, err := reply.StyledText(ctx, styling.Plain("Job has timed out.")); nil != err {
					if errors.Is(ctx.Err(), context.Canceled) {
						return nil
					}
					flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
					w.logger.Error().Func(log.Flaw(flaw.From(err).Append(flawP))).Msg("Failed to send reply")
					return nil
				}
				return nil
			case errors.Is(err, tidaldl.ErrTooManyRequests):
				if _, err := reply.StyledText(ctx, styling.Plain("Received too many requests error while downloading from TIDAL.")); nil != err {
					if errors.Is(ctx.Err(), context.Canceled) {
						return nil
					}
					flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
					w.logger.Error().Func(log.Flaw(flaw.From(err).Append(flawP))).Msg("Failed to send reply")
					return nil
				}
				return nil
			}
			// handling the rest of possible error types that are not supported by switch/case syntactically.
			if errAlreadyRunning := new(JobAlreadyRunningError); errors.As(err, &errAlreadyRunning) {
				lines := []styling.StyledTextOption{
					styling.Plain("Another job is still running."),
					styling.Plain("\n"),
					styling.Plain("Cancel with "),
					styling.BotCommand("/cancel"),
				}
				if _, err := reply.StyledText(ctx, lines...); nil != err {
					if errors.Is(ctx.Err(), context.Canceled) {
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
			if _, err := reply.Media(ctx, document); nil != err {
				if errors.Is(ctx.Err(), context.Canceled) {
					return nil
				}
				flawP := flaw.P{"err_debug_tree": errutil.Tree(err).FlawP()}
				w.logger.Error().Func(log.Flaw(flaw.From(err).Append(flawP))).Msg("Failed to send reply")
				return nil
			}
			return nil
		}

		w.logger.Info().Msg("Job succeeded")
	}

	return nil
}

type Worker struct {
	mutex      sync.Mutex
	config     *config.Config
	client     *telegram.Client
	sender     *message.Sender
	tidalAuth  *auth.Auth
	currentJob *Job
	cache      *cache.Cache
	logger     zerolog.Logger
}

func (w *Worker) newUploader(ctx context.Context) (*uploader.Uploader, func() error) {
	pool := dcpool.NewPool(w.client, 8, tgutil.DefaultMiddlewares(ctx)...)
	return uploader.NewUploader(pool.Default(ctx)).WithPartSize(uploader.MaximumPartSize).WithThreads(4), pool.Close
}

type Job struct {
	ID        string
	CreatedAt time.Time
	cancel    context.CancelFunc
}

func (j *Job) flawP() flaw.P {
	return flaw.P{
		"id":         j.ID,
		"created_at": j.CreatedAt,
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

type DownloadLink struct {
	Kind string
	ID   string
}

func parseLink(link string) (*DownloadLink, error) {
	link = strings.TrimSpace(link)

	parsedURL, err := url.Parse(link)
	if nil != err {
		return nil, &InvalidLinkError{Link: link, Err: fmt.Errorf("failed to parse URL: %v", err)}
	}

	if parsedURL.Scheme != "https" {
		return nil, &InvalidLinkError{Link: link, Err: errors.New("unsupported scheme")}
	}

	switch parsedURL.Host {
	case "listen.tidal.com", "tidal.com":
	default:
		return nil, &InvalidLinkError{Link: link, Err: errors.New("unsupported host")}
	}

	kind, id, found := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(parsedURL.Path, "/browse/"), "/"), "/")
	if !found {
		return nil, &InvalidLinkError{Link: link, Err: errors.New("failed to cut path")}
	}

	switch kind {
	case "playlist", "album", "track", "mix":
		return &DownloadLink{Kind: kind, ID: id}, nil
	default:
		return nil, &InvalidLinkError{Link: link, Err: fmt.Errorf("unsupported kind %q", kind)}
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

func (w *Worker) run(ctx context.Context, reply *message.Builder, link DownloadLink) error {
	if !w.mutex.TryLock() {
		return &JobAlreadyRunningError{ID: w.currentJob.ID}
	}
	defer func() {
		w.currentJob = nil
		w.mutex.Unlock()
	}()

	flawP := flaw.P{"id": link.ID, "kind": link.Kind}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	job := Job{
		ID:        link.ID,
		CreatedAt: time.Now(),
		cancel:    cancel,
	}
	flawP["job"] = job.flawP()
	w.currentJob = &job

	downloadBaseDir := tidalfs.DownloadDirFrom("downloads")

	dl := tidaldl.NewDownloader(
		downloadBaseDir,
		w.tidalAuth,
		&w.cache.AlbumsMeta,
		&w.cache.DownloadedCovers,
		&w.cache.TrackCredits,
	)

	switch link.Kind {
	case "playlist":
		w.logger.Info().Str("id", link.ID).Msg("Starting download playlist")
		if _, err := reply.StyledText(ctx, html.Format(nil, "<b><em>Downloading playlist...</em></b>")); nil != err {
			if errors.Is(ctx.Err(), context.Canceled) {
				return ctx.Err()
			}
			return flaw.From(fmt.Errorf("failed to send message: %v", err))
		}

		err := try.Do(func(attempt int) (retry bool, err error) {
			const maxAttempts = 3
			attemptRemained := attempt < maxAttempts
			time.Sleep(time.Duration(attempt-1) * 3 * time.Second)

			if err := dl.Playlist(ctx, link.ID); nil != err {
				switch {
				case errutil.IsContext(ctx):
					return false, err
				case errors.Is(err, auth.ErrUnauthorized):
					if err := w.tidalAuth.RefreshToken(ctx); nil != err {
						return false, err
					}
					return attemptRemained, nil
				case errors.Is(err, context.DeadlineExceeded):
					return attemptRemained, context.DeadlineExceeded
				case errors.Is(err, tidaldl.ErrTooManyRequests):
					return attemptRemained, tidaldl.ErrTooManyRequests
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

		w.logger.Info().Str("id", link.ID).Msg("Download finished. Starting playlist upload")
		if _, err := reply.StyledText(ctx, html.Format(nil, "<b><em>Download finished. Starting playlist upload...</em></b>")); nil != err {
			if errors.Is(ctx.Err(), context.Canceled) {
				return ctx.Err()
			}
			return flaw.From(fmt.Errorf("failed to send message: %v", err))
		}

		if err := w.uploadPlaylist(ctx, reply, downloadBaseDir); nil != err {
			switch {
			case errutil.IsContext(ctx), errors.Is(err, context.DeadlineExceeded):
				return err
			case errutil.IsFlaw(err):
				return must.BeFlaw(err).Append(flawP)
			default:
				panic(errutil.UnknownError(err))
			}
		}

		w.logger.Info().Str("id", link.ID).Msg("Playlist upload finished")
		if _, err := reply.StyledText(ctx, html.Format(nil, "<b><em>Playlist uploaded successfully.</em></b>")); nil != err {
			if errors.Is(ctx.Err(), context.Canceled) {
				return ctx.Err()
			}
			return flaw.From(fmt.Errorf("failed to send message: %v", err))
		}
	case "album":
		w.logger.Info().Str("id", link.ID).Msg("Starting download album")
		if _, err := reply.StyledText(ctx, html.Format(nil, "<b><em>Downloading album...</em></b>")); nil != err {
			if errors.Is(ctx.Err(), context.Canceled) {
				return ctx.Err()
			}
			return flaw.From(fmt.Errorf("failed to send message: %v", err))
		}

		err := try.Do(func(attempt int) (retry bool, err error) {
			const maxAttempts = 3
			attemptRemained := attempt < maxAttempts
			time.Sleep(time.Duration(attempt-1) * 3 * time.Second)

			if err := dl.Album(ctx, link.ID); nil != err {
				switch {
				case errutil.IsContext(ctx):
					return false, err
				case errors.Is(err, auth.ErrUnauthorized):
					if err := w.tidalAuth.RefreshToken(ctx); nil != err {
						return false, err
					}
					return attemptRemained, nil
				case errors.Is(err, context.DeadlineExceeded):
					return attemptRemained, context.DeadlineExceeded
				case errors.Is(err, tidaldl.ErrTooManyRequests):
					return attemptRemained, tidaldl.ErrTooManyRequests
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

		w.logger.Info().Str("id", link.ID).Msg("Download finished. Starting album upload")
		if _, err := reply.StyledText(ctx, html.Format(nil, "<b><em>Download finished. Starting album upload...</em></b>")); nil != err {
			if errors.Is(ctx.Err(), context.Canceled) {
				return ctx.Err()
			}
			return flaw.From(fmt.Errorf("failed to send message: %v", err))
		}

		if err := w.uploadAlbum(ctx, reply, downloadBaseDir); nil != err {
			if errutil.IsContext(ctx) {
				return ctx.Err()
			}
			return flaw.From(fmt.Errorf("failed to send message: %v", err))
		}

		w.logger.Info().Str("id", link.ID).Msg("Album upload finished")
		if _, err := reply.StyledText(ctx, html.Format(nil, "<b><em>Album uploaded successfully.</em></b>")); nil != err {
			if errors.Is(ctx.Err(), context.Canceled) {
				return ctx.Err()
			}
			return flaw.From(fmt.Errorf("failed to send message: %v", err))
		}
	case "track":
		w.logger.Info().Str("id", link.ID).Msg("Starting download track")
		if _, err := reply.StyledText(ctx, html.Format(nil, "<b><em>Downloading track...</em></b>")); nil != err {
			if errors.Is(ctx.Err(), context.Canceled) {
				return ctx.Err()
			}
			return flaw.From(fmt.Errorf("failed to send message: %v", err))
		}

		err := try.Do(func(attempt int) (retry bool, err error) {
			const maxAttempts = 3
			attemptRemained := attempt < maxAttempts
			time.Sleep(time.Duration(attempt-1) * 3 * time.Second)

			if err := dl.Single(ctx, link.ID); nil != err {
				switch {
				case errutil.IsContext(ctx):
					return false, err
				case errors.Is(err, auth.ErrUnauthorized):
					if err := w.tidalAuth.RefreshToken(ctx); nil != err {
						return false, err
					}
					return attemptRemained, nil
				case errors.Is(err, context.DeadlineExceeded):
					return attemptRemained, context.DeadlineExceeded
				case errors.Is(err, tidaldl.ErrTooManyRequests):
					return attemptRemained, tidaldl.ErrTooManyRequests
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

		w.logger.Info().Str("id", link.ID).Msg("Download finished. Starting track upload")
		if _, err := reply.StyledText(ctx, html.Format(nil, "<b><em>Download finished. Starting track upload...</em></b>")); nil != err {
			if errors.Is(ctx.Err(), context.Canceled) {
				return ctx.Err()
			}
			return flaw.From(fmt.Errorf("failed to send message: %v", err))
		}

		if err := w.uploadSingle(ctx, reply, downloadBaseDir); nil != err {
			if errutil.IsContext(ctx) {
				return ctx.Err()
			}
			return flaw.From(fmt.Errorf("failed to send message: %v", err))
		}

		w.logger.Info().Str("id", link.ID).Msg("Track upload finished")
		if _, err := reply.StyledText(ctx, html.Format(nil, "<b><em>Track uploaded successfully.</em></b>")); nil != err {
			if errors.Is(ctx.Err(), context.Canceled) {
				return ctx.Err()
			}
			return flaw.From(fmt.Errorf("failed to send message: %v", err))
		}
	case "mix":
		w.logger.Info().Str("id", link.ID).Msg("Starting download mix")
		if _, err := reply.StyledText(ctx, html.Format(nil, "<b><em>Downloading mix...</em></b>")); nil != err {
			if errors.Is(ctx.Err(), context.Canceled) {
				return ctx.Err()
			}
			return flaw.From(fmt.Errorf("failed to send message: %v", err))
		}

		err := try.Do(func(attempt int) (retry bool, err error) {
			const maxAttempts = 3
			attemptRemained := attempt < maxAttempts
			time.Sleep(time.Duration(attempt-1) * 3 * time.Second)

			if err := dl.Mix(ctx, link.ID); nil != err {
				switch {
				case errutil.IsContext(ctx):
					return false, err
				case errors.Is(err, auth.ErrUnauthorized):
					if err := w.tidalAuth.RefreshToken(ctx); nil != err {
						return false, err
					}
					return attemptRemained, nil
				case errors.Is(err, context.DeadlineExceeded):
					return attemptRemained, context.DeadlineExceeded
				case errors.Is(err, tidaldl.ErrTooManyRequests):
					return attemptRemained, tidaldl.ErrTooManyRequests
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

		w.logger.Info().Str("id", link.ID).Msg("Download finished. Starting mix upload")
		if _, err := reply.StyledText(ctx, html.Format(nil, "<b><em>Download finished. Starting mix upload...</em></b>")); nil != err {
			if errors.Is(ctx.Err(), context.Canceled) {
				return ctx.Err()
			}
			return flaw.From(fmt.Errorf("failed to send message: %v", err))
		}

		if err := w.uploadMix(ctx, reply, downloadBaseDir); nil != err {
			if errutil.IsContext(ctx) {
				return ctx.Err()
			}
			return flaw.From(fmt.Errorf("failed to send message: %v", err))
		}

		w.logger.Info().Str("id", link.ID).Msg("Mix upload finished")
		if _, err := reply.StyledText(ctx, html.Format(nil, "<b><em>Mix uploaded successfully.</em></b>")); nil != err {
			if errors.Is(ctx.Err(), context.Canceled) {
				return ctx.Err()
			}
			return flaw.From(fmt.Errorf("failed to send message: %v", err))
		}
	default:
		if _, err := reply.StyledText(ctx, html.Format(nil, "<em>Unsupported media kind: <b>%s</b>.</em>", link.Kind)); nil != err {
			if errors.Is(ctx.Err(), context.Canceled) {
				return ctx.Err()
			}
			return flaw.From(fmt.Errorf("failed to send message: %v", err))
		}
	}
	return nil
}
