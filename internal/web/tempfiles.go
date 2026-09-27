package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"

	"teledoc/internal/store"
	"teledoc/internal/telegram"
)

// Telegram's Bot API refuses getFile downloads for files larger than 20 MiB
// (https://core.telegram.org/bots/api#getfile), so only smaller documents get
// the temp-download path; bigger ones redirect to the t.me message.
const telegramDownloadCap = 20 << 20

// tempEntry is one document currently available under /temp. Files are
// per-document (scoped by id, so same-named files never collide) and stay
// until their window closes; a repeat click inside the window re-serves the
// same file without re-fetching and without extending the deadline.
type tempEntry struct {
	path      string    // temp file on disk
	name      string    // original file name, shown to the browser
	mime      string    // stored MIME type, may be empty
	size      int64     // byte size, for logging and ServeContent validation
	expiresAt time.Time // window start = moment the file finished downloading
}

// url is the browser-visible /temp/<docID>/<file name> address of the entry.
func (e tempEntry) url(docID int64) string {
	return "/temp/" + strconv.FormatInt(docID, 10) + "/" + url.PathEscape(e.name)
}

// tempManager owns the temp download directory and every file handed out
// from it. The directory itself is user-mappable (TEMP_DIR; mounted at /temp
// in docker-compose).
type tempManager struct {
	dir string        // temp download location
	ttl time.Duration // window length, from TEMP_FILE_TTL_MINUTES
	log *log.Logger

	mu    sync.Mutex
	files map[int64]tempEntry // document id -> live temp file
}

func newTempManager(dir string, ttl time.Duration, logger *log.Logger) *tempManager {
	if logger == nil {
		logger = log.Default()
	}
	return &tempManager{dir: dir, ttl: ttl, log: logger, files: map[int64]tempEntry{}}
}
// Start creates the temp directory if missing and launches the background
// sweeper that deletes files whose window has closed. It runs until ctx is
// canceled. Leftovers from a previous run (a process killed mid-window cannot
// sweep behind itself) are removed by the orphan sweep below — scoped to
// files carrying the teledoc- prefix, since the directory may be a
// user-mapped volume holding unrelated data.
func (m *tempManager) Start(ctx context.Context) {
	if err := m.ensureDir(); err != nil {
		// Every download click will fail with a flash message; keep serving
		// the rest of the UI.
		return
	}
	go func() {
		// The sweep cadence is the worst-case grace period: a file disappears
		// at most ~30s after its window closes, and expired entries already
		// 404 on request before the sweeper reaches them.
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.sweep(time.Now())
				m.sweepOrphans(time.Now())
			}
		}
	}()
}

// sweep deletes every file past its expiry, logging each deletion. Deletion
// bookkeeping happens under the mutex; the log lines are collected and
// emitted AFTER releasing it — a blocked log write inside the critical
// section would freeze every future download click (they take the same
// mutex before deciding to re-fetch), which is exactly the stuck-logging
// incident this file once shipped with.
func (m *tempManager) sweep(now time.Time) {
	type deleted struct {
		id   int64
		e    tempEntry
	}
	var deletions []deleted
	m.mu.Lock()
	for id, e := range m.files {
		if now.After(e.expiresAt) {
			_ = os.Remove(e.path)
			delete(m.files, id)
			deletions = append(deletions, deleted{id: id, e: e})
		}
	}
	m.mu.Unlock()
	for _, d := range deletions {
		m.log.Printf("web: temp window closed for document %d (%q); deleted %s",
			d.id, d.e.name, filepath.Base(d.e.path))
	}
}

// tempFilePrefix marks every file this manager creates: final downloads are
// "teledoc-<doc id>-<file name>", in-flight fetches ".teledoc-part-<id>-<n>".
// The prefix lets the orphan sweep clean up after a crash without ever
// touching unrelated files in a user-mapped directory.
const tempFilePrefix = "teledoc-"

// sweepOrphans deletes prefix-matched files older than one TTL that the
// registry no longer knows about — e.g. files left behind when the process
// died mid-window. Their URLs died with the old process anyway. Like sweep,
// log lines are collected under the mutex and emitted after releasing it.
func (m *tempManager) sweepOrphans(now time.Time) {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return
	}
	var removed []string
	m.mu.Lock()
	for _, de := range entries {
		name := de.Name()
		if !strings.HasPrefix(name, tempFilePrefix) && !strings.HasPrefix(name, "."+tempFilePrefix+"part-") {
			continue
		}
		live := false
		for _, e := range m.files {
			if filepath.Base(e.path) == name {
				live = true
				break
			}
		}
		if live {
			continue
		}
		info, err := de.Info()
		if err != nil || now.Sub(info.ModTime()) <= m.ttl {
			continue // maybe still being registered or downloaded
		}
		_ = os.Remove(filepath.Join(m.dir, name))
		removed = append(removed, name)
	}
	m.mu.Unlock()
	for _, name := range removed {
		m.log.Printf("web: removed orphaned temp file %s (no live window; left over from a previous run or crashed fetch)", name)
	}
}

// get returns the live entry for a document, or nil when the window has
// closed (or never opened).
func (m *tempManager) get(docID int64, now time.Time) *tempEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.files[docID]
	if !ok || now.After(e.expiresAt) {
		return nil
	}
	return &e
}

// fetch ensures the document's bytes are available in the temp directory: it
// re-serves an entry whose window is still open, otherwise it downloads the
// file from Telegram (getFile + the file download URL) and only then starts
// its TTL window. An error means nothing was made available and the click
// should fail (or fall back to the t.me link for errTooLarge).
func (m *tempManager) fetch(ctx context.Context, api telegram.API, doc store.Document) (tempEntry, error) {
	now := time.Now()
	if e := m.get(doc.ID, now); e != nil {
		m.log.Printf("web: download re-served from the still-open temp window for document %d (%q), expires in %s",
			doc.ID, e.name, time.Until(e.expiresAt).Round(time.Second))
		return *e, nil
	}
	m.log.Printf("web: download triggered for document %d (%q, %s, file id present=%v); fetching from Telegram",
		doc.ID, tempDisplayName(doc), humanSize(doc.FileSize), doc.FileID != "")

	if err := m.ensureDir(); err != nil {
		return tempEntry{}, err
	}

	// Download into a private staging file first, then rename into place: a
	// failed fetch never leaves a half-written file at the final path, and
	// two racing clicks converge — the winner's rename overwrites the
	// loser's (same final name), the registry keeps one live entry.
	staging := filepath.Join(m.dir, fmt.Sprintf(".teledoc-part-%d-%d", doc.ID, now.UnixNano()))
	if err := m.download(ctx, api, doc, staging); err != nil {
		_ = os.Remove(staging)
		return tempEntry{}, err
	}
	final := filepath.Join(m.dir, tempDiskName(doc))
	if err := os.Rename(staging, final); err != nil {
		_ = os.Remove(staging)
		return tempEntry{}, fmt.Errorf("move temp file into place: %w", err)
	}

	entry := tempEntry{
		path:      final,
		name:      tempDisplayName(doc),
		mime:      doc.MimeType,
		size:      doc.FileSize,
		expiresAt: now.Add(m.ttl),
	}
	m.mu.Lock()
	m.files[doc.ID] = entry
	m.mu.Unlock()
	m.log.Printf("web: downloaded document %d (%q, %s) into %s; available until %s",
		doc.ID, entry.name, humanSize(doc.FileSize), filepath.Base(final), entry.expiresAt.Format("15:04:05"))
	return entry, nil
}

// downloadWaitBudget bounds the getFile retry loop below: a browser is
// waiting on the download click, so retried calls must give up within this
// budget and fail with a clear error instead of hanging for a full
// flood-control window (which can be tens of seconds).
const downloadWaitBudget = 10 * time.Second

// download streams the Telegram file to dst. The size checks guard against
// metadata that slipped past the cap (e.g. a stale stored size or a
// Telegram-side report bigger than the archive row claims).
func (m *tempManager) download(ctx context.Context, api telegram.API, doc store.Document, dst string) error {
	if doc.FileID == "" {
		return errors.New("document has no Telegram file id yet — re-send or forward the file in the channel once and the bot will link it automatically")
	}
	// getFile can hit the same flood control as everything else (e.g. right
	// after a mass import); retry inside a small budget, then fail clearly.
	fileURL, size, err := downloadInfoWithRetry(ctx, api, doc.FileID, downloadWaitBudget)
	if err != nil {
		return err
	}
	if size > telegramDownloadCap {
		return errTooLarge{size: size}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
	if err != nil {
		return fmt.Errorf("build download request: %w", err)
	}
	// A dedicated client with a real timeout: the old code used
	// http.DefaultClient (no timeout), so a stalled Telegram connection hung
	// the handler forever — the click logged "download triggered" and then
	// nothing, and every later click queued behind it.
	client := &http.Client{
		Timeout: 5 * time.Minute, // generous for 20 MiB on a bad link
		Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			TLSHandshakeTimeout: 15 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download from Telegram: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download from Telegram: unexpected status %s", resp.Status)
	}
	f, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	n, copyErr := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if copyErr == nil {
		copyErr = closeErr
	}
	if copyErr == nil && size > 0 && n != size {
		copyErr = fmt.Errorf("download from Telegram: got %d of %d bytes", n, size)
	}
	return copyErr
}

func (m *tempManager) ensureDir() error {
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return fmt.Errorf("create temp download dir: %w", err)
	}
	return nil
}

// downloadInfoWithRetry wraps one DownloadInfo call with bounded 429
// retries. It waits exactly the retry_after Telegram demands (plus a small
// buffer) as long as the total wait stays within budget, and otherwise
// surfaces the flood error to the caller.
func downloadInfoWithRetry(ctx context.Context, api telegram.API, fileID string, budget time.Duration) (string, int64, error) {
	deadline := time.Now().Add(budget)
	for {
		fileURL, size, err := api.DownloadInfo(ctx, fileID)
		if err == nil {
			return fileURL, size, nil
		}
		var flood *bot.TooManyRequestsError
		if !errors.As(err, &flood) {
			return "", 0, err
		}
		wait := time.Duration(flood.RetryAfter)*time.Second + 500*time.Millisecond
		if time.Now().Add(wait).After(deadline) {
			return "", 0, fmt.Errorf("Telegram is rate limiting this bot (retry after %ds); try again in a minute", flood.RetryAfter)
		}
		select {
		case <-ctx.Done():
			return "", 0, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// errTooLarge marks a file that turned out to be over the Bot API cap; the
// download handler falls back to the t.me link for it.
type errTooLarge struct{ size int64 }

func (e errTooLarge) Error() string {
	return fmt.Sprintf("file is %s, over the %s Bot API download cap", humanSize(e.size), humanSize(telegramDownloadCap))
}

// tempDisplayName is the original file name, shown in the temp URL and as
// the browser's download name. Control characters are stripped so the name
// can never break a Location header; everything else passes through.
func tempDisplayName(doc store.Document) string {
	base := filepath.Base(doc.FileName)
	if base == "" || base == "." || base == string(filepath.Separator) {
		base = fmt.Sprintf("document-%d", doc.ID)
	}
	clean := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, base)
	if clean == "" {
		clean = fmt.Sprintf("document-%d", doc.ID)
	}
	return clean
}

// tempDiskName is the on-disk name: id-prefixed so two archived documents
// with the same file name never collide in the flat temp directory, with
// filesystem-hostile characters (Windows in particular) replaced.
func tempDiskName(doc store.Document) string {
	name := strings.Map(func(r rune) rune {
		if r < 0x20 || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '_'
		}
		return r
	}, tempDisplayName(doc))
	// Trailing dots/spaces are illegal on Windows directories.
	name = strings.TrimRight(name, ". ")
	if name == "" {
		name = "document"
	}
	return fmt.Sprintf("teledoc-%d-%s", doc.ID, name)
}

// ---------------------------------------------------------------------------
// Handlers

// handleDocumentDownload serves the download icon click. Documents over the
// Bot API cap go straight to their t.me message; the rest are fetched from
// Telegram into the temp directory (reusing a file whose window is still
// open) and the browser is redirected to the temp URL, which streams the
// file as an attachment. The temp URL lives under the same session auth as
// every other page, so a leaked link is worthless without a login.
func (s *Server) handleDocumentDownload(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.log.Printf("web: download click for malformed id %q rejected", r.PathValue("id"))
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	doc, err := s.store.DocumentByID(id)
	if errors.Is(err, sql.ErrNoRows) {
		s.log.Printf("web: download click for unknown document %d (not in the archive)", id)
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.log.Printf("web: download lookup for document %d: %v", id, err)
		http.Error(w, "lookup failed", http.StatusInternalServerError)
		return
	}
	if doc.FileSize > telegramDownloadCap {
		s.log.Printf("web: download click for document %d (%q, %s) is over the %s Bot API cap; redirecting to the Telegram message %s",
			doc.ID, doc.FileName, humanSize(doc.FileSize), humanSize(telegramDownloadCap), doc.MessageLink)
		http.Redirect(w, r, doc.MessageLink, http.StatusSeeOther)
		return
	}
	entry, err := s.temp.fetch(r.Context(), s.tg, doc)
	if err != nil {
		var tl errTooLarge
		if errors.As(err, &tl) {
			s.log.Printf("web: download of document %d (%q) fell back to the Telegram message: %v", doc.ID, doc.FileName, err)
			http.Redirect(w, r, doc.MessageLink, http.StatusSeeOther)
			return
		}
		s.log.Printf("web: download of document %d (%q) failed: %v", doc.ID, doc.FileName, err)
		setFlash(w, "Could not prepare the download: "+err.Error())
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.log.Printf("web: download click for document %d (%q) redirected to %s", doc.ID, doc.FileName, entry.url(doc.ID))
	http.Redirect(w, r, entry.url(doc.ID), http.StatusSeeOther)
}

// handleTempFile streams a live temp file as an attachment. The URL's file
// name must match the entry the registry recorded at fetch time, so the id
// segment alone cannot fish arbitrary documents out of the temp directory.
func (s *Server) handleTempFile(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.log.Printf("web: temp request for malformed id %q rejected", r.PathValue("id"))
		http.NotFound(w, r)
		return
	}
	name := r.PathValue("name")
	entry := s.temp.get(id, time.Now())
	if entry == nil || entry.name != name {
		s.log.Printf("web: temp request for /temp/%d/%s rejected (unknown id, mismatched name, or expired window)", id, name)
		http.Error(w, "This download link has expired. Request the file again from the documents page.", http.StatusNotFound)
		return
	}
	f, err := os.Open(entry.path)
	if err != nil {
		s.log.Printf("web: serving temp download of document %d (%q) failed: file missing from the temp directory: %v", id, entry.name, err)
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	if entry.mime != "" {
		w.Header().Set("Content-Type", entry.mime)
	}
	if cd := mime.FormatMediaType("attachment", map[string]string{"filename": entry.name}); cd != "" {
		w.Header().Set("Content-Disposition", cd)
	}
	// ServeContent adds range support (resume) and fills in a content type
	// from the file name when the stored MIME type is empty. The modtime is
	// when the window opened.
	s.log.Printf("web: serving temp download of document %d (%q, %d bytes, window ends %s) to %s",
		id, entry.name, entry.size, entry.expiresAt.Format("15:04:05"), r.RemoteAddr)
	http.ServeContent(w, r, entry.name, entry.expiresAt.Add(-s.temp.ttl), f)
}
