package telegram

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/goccy/go-json"
	"github.com/rs/zerolog"
	"github.com/tidwall/gjson"
	"gopkg.in/yaml.v3"

	"github.com/falomen-app/backend/config"
	"github.com/falomen-app/backend/constant"
	"github.com/falomen-app/backend/env"
	"github.com/falomen-app/backend/must"
)

const (
	mdBlockQuoteDelimStart          = "```yaml\n"
	mdBlockQuoteDelimEnd            = "\n```"
	maxMarkdownMessageContentLength = 4096 - (len(mdBlockQuoteDelimEnd) + len(mdBlockQuoteDelimStart))
)

type Bot struct {
	bot    *tgbotapi.BotAPI
	logger zerolog.Logger
	chatID int64
	buffer chan []byte
	cancel context.CancelFunc
	wg     *sync.WaitGroup
}

func (b *Bot) sendMessageTooLongError(prevMsgID int) (int, error) {
	msg := tgbotapi.NewMessage(b.chatID, "Failed to send message due to exceeding maximum message length. You'll need to check logs.")
	msg.ReplyToMessageID = prevMsgID
	if sent, err := b.bot.Send(msg); nil != err {
		return 0, err
	} else {
		return sent.MessageID, nil
	}
}

func (b *Bot) send(p []byte) {
	must.Be(len(p) > 0, "message payload must be non-empty")

	if gjson.GetBytes(p, "level").Str != "error" && !gjson.GetBytes(p, "should_telegram_send").Exists() {
		return
	}
	var d map[string]any
	if err := json.UnmarshalWithOption(p, &d, json.DecodeFieldPriorityFirstWin()); nil != err {
		b.logger.Warn().Err(err).Msg("Failed to decode message as JSON. Sending raw bytes as string...")
		if _, err = b.bot.Send(tgbotapi.NewMessage(b.chatID, string(p))); nil != err {
			b.logger.Warn().Err(err).Msg("Failed to send raw message as string.")
		}
		return
	}
	delete(d, "should_telegram_send")
	if ts, ok := d["time"]; ok {
		if ts, ok := ts.(float64); ok {
			d["time"] = time.Unix(int64(ts), 0).Format(time.RFC3339)
		} else {
			panic("message time must be of type float64")
		}
	} else {
		panic("message must have a time field")
	}
	yamlEncoded, err := yaml.Marshal(d)
	if nil != err {
		b.logger.Warn().Err(err).Msg("Failed to encode error log record to YAML.")
		return
	}
	if len(yamlEncoded) >= maxMarkdownMessageContentLength {
		parts, err := b.breakLongMessage(yamlEncoded)
		if nil != err {
			b.logger.Warn().Err(err).Msg("Failed to break long message into parts.")
			return
		}
		var previousMsgID int
		for i, p := range parts {
			msg := tgbotapi.NewMessage(b.chatID, mdBlockQuoteDelimStart+tgbotapi.EscapeText(tgbotapi.ModeMarkdownV2, string(p))+mdBlockQuoteDelimEnd)
			msg.ParseMode = tgbotapi.ModeMarkdownV2
			msg.ReplyToMessageID = previousMsgID
			if sent, err := b.bot.Send(msg); nil != err {
				if tgErr := new(tgbotapi.Error); errors.As(err, &tgErr) && tgErr.Code == 400 && tgErr.Message == "Bad Request: text is too long" {
					if sentMsgID, err := b.sendMessageTooLongError(previousMsgID); nil != err {
						b.logger.Warn().Int("part", i).Err(err).Msg("Failed to send YAML-encoded message part due to too long message error.")
					} else {
						previousMsgID = sentMsgID
					}
					continue
				}
				b.logger.Warn().Int("part", i).Err(err).Msg("Failed to send YAML-encoded message part.")
			} else {
				previousMsgID = sent.MessageID
			}
		}
	} else {
		msg := tgbotapi.NewMessage(b.chatID, mdBlockQuoteDelimStart+tgbotapi.EscapeText(tgbotapi.ModeMarkdownV2, string(yamlEncoded))+mdBlockQuoteDelimEnd)
		msg.ParseMode = tgbotapi.ModeMarkdownV2
		if _, err = b.bot.Send(msg); nil != err {
			b.logger.Warn().Err(err).Msg("Failed to send YAML-encoded message.")
		}
	}
}

func (b *Bot) sendInitialMessage() {
	data := map[string]string{
		"time":        time.Now().Format(time.RFC3339),
		"message":     "Bot was just started.",
		"version":     constant.Version,
		"compiled_at": constant.CompileTime.Format(time.RFC3339),
		"revision":    constant.Revision,
	}
	yamlEncoded, err := yaml.Marshal(data)
	if nil != err {
		b.logger.Warn().Err(err).Msg("Failed to encode initial message to YAML.")
		return
	}
	msg := tgbotapi.NewMessage(b.chatID, mdBlockQuoteDelimStart+tgbotapi.EscapeText(tgbotapi.ModeMarkdownV2, string(yamlEncoded))+mdBlockQuoteDelimEnd)
	msg.ParseMode = tgbotapi.ModeMarkdownV2
	if _, err = b.bot.Send(msg); nil != err {
		b.logger.Warn().Err(err).Msg("Failed to send initial message.")
	}
}

func (b *Bot) sendShutdownMessage() {
	data := map[string]string{
		"time":    time.Now().Format(time.RFC3339),
		"message": "Bot was just shutdown.",
	}
	yamlEncoded, err := yaml.Marshal(data)
	if nil != err {
		b.logger.Warn().Err(err).Msg("Failed to encode initial message to YAML.")
		return
	}
	msg := tgbotapi.NewMessage(b.chatID, mdBlockQuoteDelimStart+tgbotapi.EscapeText(tgbotapi.ModeMarkdownV2, string(yamlEncoded))+mdBlockQuoteDelimEnd)
	msg.ParseMode = tgbotapi.ModeMarkdownV2
	if _, err = b.bot.Send(msg); nil != err {
		b.logger.Warn().Err(err).Msg("Failed to send shutdown message.")
	}
}

func (b *Bot) Write(p []byte) (int, error) {
	must.Be(len(p) > 0, "write bytes payload must be non-empty")

	cloned := bytes.Clone(p)
	b.buffer <- cloned
	return len(cloned), nil
}

func (b *Bot) Close() error {
	b.bot.StopReceivingUpdates()
	b.logger.Debug().Msg("Closed Telegram updates stream.")
	b.cancel()
	b.wg.Wait()
	b.logger.Debug().Msg("Worker routines were closed.")
	b.sendShutdownMessage()
	return nil
}

func LaunchBot(logger zerolog.Logger, cfg config.TelegramBot) (*Bot, error) {
	httpTransport := http.Transport{IdleConnTimeout: 10 * time.Second, ResponseHeaderTimeout: 30 * time.Second} //nolint:exhaustruct
	httpClient := http.Client{Timeout: time.Second * 35, Transport: &httpTransport}                             //nolint:exhaustruct
	if proxyURL := cfg.ProxyURL; proxyURL != "" {
		httpProxyURL, err := url.Parse(proxyURL)
		if nil != err {
			return nil, errors.New("telegram: failed to parse bot http proxy url")
		}
		httpTransport.Proxy = http.ProxyURL(httpProxyURL)
	}

	token := os.Getenv(env.TelegramBotKey)
	if err := tgbotapi.SetLogger(log.New(io.Discard, "", log.LstdFlags)); nil != err {
		return nil, fmt.Errorf("telegram: failed to set bot logger: %v", err)
	}
	bot, err := tgbotapi.NewBotAPIWithClient(token, tgbotapi.APIEndpoint, &httpClient)
	if nil != err {
		return nil, fmt.Errorf("telegram: failed to instantiate bot: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	buffer := make(chan []byte, 100)
	b := &Bot{
		bot:    bot,
		logger: logger.Output(os.Stderr),
		chatID: cfg.PublishChatID,
		buffer: buffer,
		cancel: cancel,
		wg:     &wg,
	}

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 30
	wg.Add(1)
	go func() {
		updates := bot.GetUpdatesChan(u)
		defer wg.Done()
	loop:
		for {
			select {
			case <-ctx.Done():
				break loop
			case <-updates:
			}
		}
		b.logger.Trace().Msg("Update receiver worker routine has finished.")
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
	loop:
		for {
			select {
			case <-ctx.Done():
				// Let's drain it off.
				close(buffer)
				var remainedMsgs int
				for v := range buffer {
					remainedMsgs++
					b.send(v)
				}
				b.logger.Trace().Int("remained_message", remainedMsgs).Msg("Drained messages from buffer.")
				break loop
			case msg, ok := <-buffer:
				if !ok {
					break loop
				}
				b.send(msg)
			}
		}
		b.logger.Trace().Msg("Listen buffer worker routine has finished.")
	}()

	b.sendInitialMessage()

	return b, nil
}
