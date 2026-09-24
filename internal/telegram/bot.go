// Package telegram runs the long-polling bot that ingests documents posted
// in the channel. Bots cannot read history, so every document uploaded to
// the channel *after* the bot is added as admin arrives as a channel_post
// update and gets archived here.
package telegram

import (
	"context"
	"fmt"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"teledoc/internal/rules"
	"teledoc/internal/store"
)

// Bot wraps the Telegram client and the ingestion pipeline.
type Bot struct {
	api    *bot.Bot
	store  *store.Store
	engine *rules.Engine
	log    *log.Logger

	mu        sync.Mutex
	usernames map[int64]string // chat id -> public username, cached for links
}

// telegramHTTPClient wraps the default transport with sane dial timeouts so a
// blocked/unreachable api.telegram.org fails fast instead of hanging.
type telegramHTTPClient struct {
	http *http.Client
}

func (c *telegramHTTPClient) Do(req *http.Request) (*http.Response, error) {
	return c.http.Do(req)
}

// New builds the bot client with long polling and a default update handler.
// bot.New performs a getMe handshake, which fails when api.telegram.org is
// unreachable (firewall, ISP block, VPN down); it is retried with backoff so
// a transient network blip does not crash startup.
func New(token string, st *store.Store, engine *rules.Engine, logger *log.Logger) (*Bot, error) {
	if logger == nil {
		logger = log.Default()
	}
	b := &Bot{
		store:     st,
		engine:    engine,
		log:       logger,
		usernames: map[int64]string{},
	}

	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment, // honors HTTP(S)_PROXY vars
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		// Long polling holds each getUpdates request open for ~59s. A header
		// timeout shorter than that causes periodic "timeout awaiting response
		// headers" churn (seen in the wild), so there is no header timeout;
		// the client-level timeout below is the dead-connection safety net.
		ResponseHeaderTimeout: 0,
		ForceAttemptHTTP2:     false, // plain HTTP/1.1: h2 + proxies break long polls
		MaxIdleConns:          10,
		IdleConnTimeout:       90 * time.Second,
	}
	httpClient := &telegramHTTPClient{http: &http.Client{Timeout: 90 * time.Second, Transport: transport}}

	opts := []bot.Option{
		bot.WithDefaultHandler(func(ctx context.Context, _ *bot.Bot, update *models.Update) {
			b.handleUpdate(ctx, update)
		}),
		bot.WithAllowedUpdates(bot.AllowedUpdates{"message", "channel_post"}),
		bot.WithNotAsyncHandlers(), // serialize ingestion; SQLite is single-writer anyway
		bot.WithHTTPClient(time.Minute, httpClient),
		bot.WithCheckInitTimeout(30 * time.Second),
		bot.WithErrorsHandler(func(err error) { b.log.Printf("telegram: %v", err) }),
	}
	if base := os.Getenv("TELEGRAM_API_URL"); base != "" {
		// Escape hatch for firewalled networks: point at a Bot API mirror or
		// local bot-api-server, e.g. http://localhost:8081
		opts = append(opts, bot.WithServerURL(strings.TrimRight(base, "/")))
		b.log.Printf("telegram: using custom API URL %s", base)
	}

	api, err := botNewWithRetry(token, opts, b.log)
	if err != nil {
		return nil, err
	}
	b.api = api
	return b, nil
}

// botNewWithRetry calls bot.New (which does the getMe handshake) with backoff.
func botNewWithRetry(token string, opts []bot.Option, logger *log.Logger) (*bot.Bot, error) {
	delays := []time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second, 20 * time.Second}
	var lastErr error
	for attempt := 0; ; attempt++ {
		api, err := bot.New(token, opts...)
		if err == nil {
			if attempt > 0 {
				logger.Printf("telegram: connected on attempt %d", attempt+1)
			}
			return api, nil
		}
		lastErr = err
		if attempt >= len(delays) {
			break
		}
		logger.Printf("telegram: connect failed (%v); retrying in %s...", err, delays[attempt])
		time.Sleep(delays[attempt])
	}
	return nil, fmt.Errorf("create bot after %d attempts: %w", len(delays)+1, lastErr)
}

// Run blocks, long-polling Telegram until ctx is canceled.
func (b *Bot) Run(ctx context.Context) {
	b.log.Printf("telegram: long polling started")
	b.api.Start(ctx)
}

// handleUpdate is the entry point for every Telegram update.
func (b *Bot) handleUpdate(ctx context.Context, update *models.Update) {
	msg := update.ChannelPost
	if msg == nil {
		msg = update.Message // also accepts documents posted in groups
	}
	if msg == nil || msg.Document == nil {
		return
	}
	if err := b.ingest(ctx, msg); err != nil {
		b.log.Printf("telegram: ingest message %d in chat %d: %v", msg.ID, msg.Chat.ID, err)
	}
}

// ingest archives one document post and applies the tagging rules.
func (b *Bot) ingest(ctx context.Context, msg *models.Message) error {
	doc := msg.Document

	fileName := doc.FileName
	if fileName == "" {
		fileName = fmt.Sprintf("document-%d", msg.ID)
	}
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(fileName)), ".")
	mimeType := doc.MimeType
	if mimeType == "" {
		mimeType = mime.TypeByExtension("." + ext)
	}

	link := b.messageLink(ctx, msg)

	uploadedAt := time.Unix(int64(msg.Date), 0)
	stored := store.Document{
		FileName:    fileName,
		MimeType:    mimeType,
		Extension:   ext,
		FileSize:    doc.FileSize,
		ChatID:      msg.Chat.ID,
		MessageID:   msg.ID,
		MessageLink: link,
		UploadedAt:  uploadedAt,
	}

	id, created, err := b.store.UpsertDocument(stored)
	if err != nil {
		return fmt.Errorf("store document: %w", err)
	}
	if !created {
		return nil // duplicate delivery (e.g. bot restart overlap); already archived
	}
	stored.ID = id

	added, err := b.engine.ApplyRulesToDocument(stored)
	if err != nil {
		b.log.Printf("rules: document %q archived but tagging failed: %v", fileName, err)
	}
	if len(added) > 0 {
		b.log.Printf("archived %q (%s, %d bytes) -> +%d tag(s) [msg %d]",
			fileName, mimeType, doc.FileSize, len(added), msg.ID)
	} else {
		b.log.Printf("archived %q (%s, %d bytes) [msg %d]", fileName, mimeType, doc.FileSize, msg.ID)
	}
	return nil
}

// messageLink builds the t.me link for a channel/group message:
//   - public channel (username known): https://t.me/<username>/<id>
//   - private channel: https://t.me/c/<internal-id>/<id> (opens for members)
func (b *Bot) messageLink(ctx context.Context, msg *models.Message) string {
	chat := msg.Chat
	if chat.Username != "" {
		b.cacheUsername(chat.ID, chat.Username)
		return fmt.Sprintf("https://t.me/%s/%d", chat.Username, msg.ID)
	}
	if username := b.cachedUsername(chat.ID); username != "" {
		return fmt.Sprintf("https://t.me/%s/%d", username, msg.ID)
	}
	// Username missing from the payload: ask Telegram once and cache it.
	full, err := b.api.GetChat(ctx, &bot.GetChatParams{ChatID: chat.ID})
	if err == nil && full.Username != "" {
		b.cacheUsername(chat.ID, full.Username)
		return fmt.Sprintf("https://t.me/%s/%d", full.Username, msg.ID)
	}
	// Private channel fallback: strip the -100 prefix from the chat id.
	internal := strings.TrimPrefix(fmt.Sprint(chat.ID), "-100")
	return fmt.Sprintf("https://t.me/c/%s/%d", internal, msg.ID)
}

func (b *Bot) cacheUsername(chatID int64, username string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.usernames[chatID] = username
}

func (b *Bot) cachedUsername(chatID int64) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.usernames[chatID]
}

// Me returns the bot's own username (for startup logging / README hints).
func (b *Bot) Me(ctx context.Context) string {
	u, err := b.api.GetMe(ctx)
	if err != nil {
		return "unknown"
	}
	return "@" + u.Username
}
