// Package telegram runs the long-polling bot that ingests documents posted
// in the channel. Bots cannot read history, so every document uploaded to
// the channel *after* the bot is added as admin arrives as a channel_post
// update and gets archived here.
package telegram

import (
	"context"
	"database/sql"
	"errors"
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
	"unicode/utf16"

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
		Proxy:               http.ProxyFromEnvironment, // honors HTTP(S)_PROXY vars
		DialContext:         dialer.DialContext,
		TLSHandshakeTimeout: 15 * time.Second,
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
		bot.WithAllowedUpdates(bot.AllowedUpdates{"message", "channel_post", "edited_message", "edited_channel_post"}),
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

// Status reactions set on the document message itself, so the channel doubles
// as an archive status indicator. Telegram restricts bot reactions to a fixed
// emoji list; the first-choice glyphs (✅ / ⚠️ / 🔁) are not on it, so the
// closest allowed equivalents are used
// (https://core.telegram.org/bots/api#available-reactions).
const (
	reactArchived  = "👍" // document archived successfully
	reactTagFailed = "🤔" // archived, but tagging failed — needs attention
	reactDuplicate = "👀" // a file with this name was already archived before
)

// hashtagEntities extracts #hashtag texts from a message caption using
// Telegram's own hashtag entities, so parsing matches exactly what clients
// highlight. Entity offsets/lengths are measured in UTF-16 code units, which
// differ from byte offsets whenever the caption contains emoji or other
// non-BMP characters; the caption is converted to UTF-16 first and hashtag
// spans are then decoded back to Go strings.
func hashtagEntities(caption string, entities []models.MessageEntity) []string {
	if caption == "" {
		return nil
	}
	units := utf16.Encode([]rune(caption))
	var out []string
	seen := map[string]bool{}
	for _, ent := range entities {
		if ent.Type != models.MessageEntityTypeHashtag {
			continue
		}
		from, to := ent.Offset, ent.Offset+ent.Length
		if from < 0 || to > len(units) || from >= to {
			continue
		}
		ht := string(utf16.Decode(units[from:to]))
		if !seen[ht] {
			seen[ht] = true
			out = append(out, ht)
		}
	}
	return out
}

// handleUpdate is the entry point for every Telegram update.
func (b *Bot) handleUpdate(ctx context.Context, update *models.Update) {
	// Caption edits re-run tagging (add-only) on the archived document.
	switch {
	case update.EditedChannelPost != nil:
		b.handleEdit(ctx, update.EditedChannelPost)
		return
	case update.EditedMessage != nil:
		b.handleEdit(ctx, update.EditedMessage)
		return
	}
	msg := update.ChannelPost
	if msg == nil {
		msg = update.Message // also accepts documents posted in groups
	}
	if msg == nil || msg.Document == nil {
		return
	}

	status, err := b.ingest(ctx, msg)
	if err != nil {
		b.log.Printf("telegram: ingest message %d in chat %d: %v", msg.ID, msg.Chat.ID, err)
		// Archiving or tagging failed: flag the message so the channel
		// itself shows that it needs attention.
		b.react(ctx, msg.Chat.ID, msg.ID, reactTagFailed)
		return
	}
	switch status {
	case ingestDuplicateFileName:
		b.react(ctx, msg.Chat.ID, msg.ID, reactDuplicate)
	case ingestArchived:
		b.react(ctx, msg.Chat.ID, msg.ID, reactArchived)
	default: // ingestAlreadyArchived: redelivery of an archived message;
		// its reaction was already set on first delivery.
	}
}

// ingestStatus tells handleUpdate which reaction the document message earned.
type ingestStatus int

const (
	ingestArchived          ingestStatus = iota // archived (zero or more tags applied)
	ingestDuplicateFileName                     // archived, but a file with the same name was archived before
	ingestAlreadyArchived                       // duplicate delivery of an already-archived message
)

// ingest archives one document post and applies the tagging rules. A non-nil
// error means archiving or tagging failed.
func (b *Bot) ingest(ctx context.Context, msg *models.Message) (ingestStatus, error) {
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

	// Duplicate-filename check runs before the insert and excludes this very
	// message, so a reupload is flagged while a redelivery of the same
	// message is not.
	dup, err := b.store.HasDocumentWithFileName(msg.Chat.ID, fileName, msg.ID)
	if err != nil {
		// The duplicate flag is cosmetic; never block archiving on it.
		b.log.Printf("telegram: duplicate check for %q: %v", fileName, err)
	}

	id, created, err := b.store.UpsertDocument(stored)
	if err != nil {
		return ingestArchived, fmt.Errorf("store document: %w", err)
	}
	if !created {
		return ingestAlreadyArchived, nil // duplicate delivery (e.g. bot restart overlap); already archived
	}
	stored.ID = id

	// Caption hashtags tag the document directly, bypassing the rules engine
	// entirely (Telegram-native tagging). A failure here fails the whole
	// tagging step, so the channel shows the needs-attention reaction.
	tagErr := b.applyCaptionHashtags(stored.ID, msg)

	added, err := b.engine.ApplyRulesToDocument(stored)
	if tagErr == nil {
		tagErr = err
	}
	if tagErr != nil {
		return ingestArchived, fmt.Errorf("tag %q: %w", fileName, tagErr)
	}

	if len(added) > 0 {
		b.log.Printf("archived %q (%s, %d bytes) -> +%d tag(s) [msg %d]",
			fileName, mimeType, doc.FileSize, len(added), msg.ID)
	} else {
		b.log.Printf("archived %q (%s, %d bytes) [msg %d]", fileName, mimeType, doc.FileSize, msg.ID)
	}
	if dup {
		b.log.Printf("telegram: %q was already archived from this channel (duplicate reupload)", fileName)
		return ingestDuplicateFileName, nil
	}
	return ingestArchived, nil
}

// handleEdit re-runs caption-hashtag tagging when a document post's caption
// is edited (adds a hashtag, fixes a typo). Tagging is strictly add-only: an
// edit never removes a tag — removal is a web-UI action. Edits to non-
// document posts, and edits of documents that are not in the archive (posted
// before the bot joined, or deleted via the web UI — deletion stays
// authoritative), are ignored. On success the existing status reaction is
// left untouched; on failure it flips to the needs-attention reaction.
func (b *Bot) handleEdit(ctx context.Context, msg *models.Message) {
	if msg == nil || msg.Document == nil {
		return // caption edits on non-document posts never affect tags
	}
	doc, err := b.store.DocumentByMessage(msg.Chat.ID, msg.ID)
	if errors.Is(err, sql.ErrNoRows) {
		b.log.Printf("telegram: edit of message %d in chat %d: not archived, ignoring", msg.ID, msg.Chat.ID)
		return
	}
	if err != nil {
		b.log.Printf("telegram: edit lookup message %d in chat %d: %v", msg.ID, msg.Chat.ID, err)
		b.react(ctx, msg.Chat.ID, msg.ID, reactTagFailed)
		return
	}
	if err := b.applyCaptionHashtags(doc.ID, msg); err != nil {
		b.log.Printf("telegram: re-tag message %d in chat %d: %v", msg.ID, msg.Chat.ID, err)
		b.react(ctx, msg.Chat.ID, msg.ID, reactTagFailed)
	}
}

// applyCaptionHashtags applies a message's caption #hashtags to a document,
// bypassing the rules engine entirely (Telegram-native tagging). Each tag is
// resolved case-insensitively — an existing tag wins whatever its casing,
// otherwise a new tag is created (lowercased). Idempotent: re-applying a tag
// the document already has is a no-op, which makes caption edits add-only by
// construction. Returns nil when the caption carries no hashtags.
func (b *Bot) applyCaptionHashtags(docID int64, msg *models.Message) error {
	hts := hashtagEntities(msg.Caption, msg.CaptionEntities)
	if len(hts) == 0 {
		return nil
	}
	b.log.Printf("telegram: caption hashtags: %s", strings.Join(hts, " "))
	for _, ht := range hts {
		name := strings.TrimPrefix(ht, "#")
		if name == "" {
			continue
		}
		tagID, err := b.store.EnsureTag(name)
		if err != nil {
			return fmt.Errorf("ensure caption tag %q: %w", name, err)
		}
		if _, err := b.store.AddTagToDocument(docID, tagID); err != nil {
			return fmt.Errorf("apply caption tag %q: %w", name, err)
		}
	}
	return nil
}

// react sets a single emoji reaction on the document message. Failures are
// logged and swallowed: a status reaction must never break ingestion.
func (b *Bot) react(ctx context.Context, chatID int64, messageID int, emoji string) {
	_, err := b.api.SetMessageReaction(ctx, &bot.SetMessageReactionParams{
		ChatID:    chatID,
		MessageID: messageID,
		Reaction: []models.ReactionType{{
			Type:              models.ReactionTypeTypeEmoji,
			ReactionTypeEmoji: &models.ReactionTypeEmoji{Emoji: emoji},
		}},
	})
	if err != nil {
		b.log.Printf("telegram: set reaction %q on message %d in chat %d: %v", emoji, messageID, chatID, err)
	}
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
