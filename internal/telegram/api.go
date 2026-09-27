package telegram

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-telegram/bot"
)

// API is what the web server needs from the bot client. It keeps the web
// package decoupled from the concrete client (unit tests stub this).
type API interface {
	DownloadInfo(ctx context.Context, fileID string) (url string, size int64, err error)
}

// Compile-time proof that *Bot satisfies the interface.
var _ API = (*Bot)(nil)

// Client is a lightweight API-only client (no polling, no ingestion, no
// getMe handshake): the web server's temp downloads use it so the bot's
// long-polling client is not shared across concerns.
type Client struct {
	api *bot.Bot
}

// Compile-time proof that *Client satisfies the interface too.
var _ API = (*Client)(nil)

// DownloadInfo implements API (delegates to the shared helper below).
func (c *Client) DownloadInfo(ctx context.Context, fileID string) (url string, size int64, err error) {
	return downloadInfo(c.api, ctx, fileID)
}

// NewClient builds a Client for callers that only need method calls — the
// web server uses it for temp downloads. WithSkipGetMe means construction
// never talks to the network: the web UI starts (and downloads fail with a
// clear error) even while Telegram is unreachable, matching the bot's
// retry-with-backoff startup instead of crashing the whole process.
func NewClient(token string, logger *log.Logger) *Client {
	if logger == nil {
		logger = log.Default()
	}
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment, // honors HTTP(S)_PROXY vars
		DialContext:         dialer.DialContext,
		TLSHandshakeTimeout: 15 * time.Second,
		// getFile responses are tiny; the file download itself streams through
		// the same client. 10 minutes is a generous ceiling for 20 MiB.
		ResponseHeaderTimeout: 60 * time.Second,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          4,
		IdleConnTimeout:       90 * time.Second,
	}
	httpClient := &telegramHTTPClient{http: &http.Client{Timeout: 10 * time.Minute, Transport: transport}}
	opts := []bot.Option{
		bot.WithSkipGetMe(),
		bot.WithHTTPClient(time.Minute, httpClient),
		bot.WithErrorsHandler(func(err error) { logger.Printf("telegram-api: %v", err) }),
	}
	if base := os.Getenv("TELEGRAM_API_URL"); base != "" {
		// Same escape hatch as the bot client: point at a Bot API mirror or
		// local bot-api-server.
		opts = append(opts, bot.WithServerURL(strings.TrimRight(base, "/")))
		logger.Printf("telegram-api: using custom API URL %s", base)
	}
	api, err := bot.New(token, opts...)
	if err != nil {
		// bot.New with WithSkipGetMe only fails on an empty token, which the
		// caller validates first; log-and-continue keeps this non-fatal.
		logger.Printf("telegram-api: client creation failed: %v", err)
	}
	return &Client{api: api}
}

// downloadInfo is the shared getFile + download-URL resolution used by both
// the polling bot and the lightweight client. It resolves a stored Telegram
// file id into a direct download URL plus the file's reported size, for the
// web UI's temp-download feature. The URL embeds the bot token (as Telegram
// requires) and expires after a while, so it must only be used server-side,
// never exposed to browsers.
func downloadInfo(api *bot.Bot, ctx context.Context, fileID string) (url string, size int64, err error) {
	if api == nil {
		return "", 0, fmt.Errorf("download: telegram client unavailable")
	}
	if fileID == "" {
		return "", 0, fmt.Errorf("download: empty telegram file id")
	}
	f, err := api.GetFile(ctx, &bot.GetFileParams{FileID: fileID})
	if err != nil {
		return "", 0, fmt.Errorf("download: get file: %w", err)
	}
	if f == nil || f.FilePath == "" {
		return "", 0, fmt.Errorf("download: telegram returned no file path")
	}
	return api.FileDownloadLink(f), f.FileSize, nil
}

// DownloadInfo resolves a stored Telegram file id into a direct download URL
// plus the file's reported size (see downloadInfo).
func (b *Bot) DownloadInfo(ctx context.Context, fileID string) (url string, size int64, err error) {
	return downloadInfo(b.api, ctx, fileID)
}
