package web

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"teledoc/internal/store"
)

// stubAPI fakes the Telegram API: it hands out download URLs on a stand-in
// host and records which file ids were asked for, so tests can assert on
// re-fetches.
type stubAPI struct {
	calls    map[string]int
	fileSize int64
	err      error
	host     string // base URL of the stand-in Telegram file server
}

func (s *stubAPI) DownloadInfo(ctx context.Context, fileID string) (string, int64, error) {
	if s.err != nil {
		return "", 0, s.err
	}
	if s.calls == nil {
		s.calls = map[string]int{}
	}
	s.calls[fileID]++
	return s.host + "/file/" + fileID, s.fileSize, nil
}

// downloadServer stands in for the Telegram file host: it must see the URL
// the stub API handed out and replies with the payload.
func downloadServer(t *testing.T, payload []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/file/") {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newDownloadTestServer wires a Server + store with a stubbed Telegram whose
// download URLs point at the local httptest server instead of Telegram.
func newDownloadTestServer(t *testing.T, api telegramAPI) (*Server, *store.Store) {
	t.Helper()
	st := newTestStore(t)
	s := New(st, nil, "", nil, TempConfig{Dir: t.TempDir(), TTL: time.Minute}, api)
	return s, st
}

// telegramAPI is the interface tempfiles.go consumes; stubAPI satisfies it
// (mirroring telegram.API without importing the bot client here).
type telegramAPI interface {
	DownloadInfo(ctx context.Context, fileID string) (string, int64, error)
}

// seedDocument inserts one archived document and returns it.
func seedDocument(t *testing.T, st *store.Store, name string, size int64) store.Document {
	t.Helper()
	doc, _, err := st.UpsertDocument(store.Document{
		FileName: name, MimeType: "application/pdf", Extension: "pdf",
		FileSize: size, ChatID: 1, MessageID: nextMsgID(),
		MessageLink: "https://t.me/c/1/x", FileID: "FID-" + name,
		UploadedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	d, err := st.DocumentByID(doc)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

var msgIDCounter int

func nextMsgID() int { msgIDCounter++; return msgIDCounter }

// The happy path: download click fetches from Telegram and redirects to the
// temp URL, which serves the exact bytes as an attachment.
func TestDownloadFetchesAndServes(t *testing.T) {
	payload := []byte("%PDF-1.4 fake pdf bytes")
	api := &stubAPI{fileSize: int64(len(payload))}
	// Make the stub's URL resolve against the local stand-in server.
	srv := downloadServer(t, payload)
	api.host = srv.URL

	s, st := newDownloadTestServer(t, api)
	doc := seedDocument(t, st, "report.pdf", int64(len(payload)))
	h := s.Handler()

	// 1. Download click: expect a redirect to /temp/... .
	r := httptest.NewRequest("GET", fmt.Sprintf("/documents/%d/download", doc.ID), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("download click: got %d, want 303", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/temp/"+itoa(doc.ID)+"/") || !strings.HasSuffix(loc, "/report.pdf") {
		t.Fatalf("download click: redirect %q, want /temp/<id>/report.pdf", loc)
	}

	// 2. The temp URL serves the file, auth-guarded, with the right headers.
	r = httptest.NewRequest("GET", loc, nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("temp fetch: got %d, want 200", w.Code)
	}
	if got := w.Body.Bytes(); string(got) != string(payload) {
		t.Fatalf("temp fetch: body %q, want %q", got, payload)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "report.pdf") {
		t.Fatalf("temp fetch: content-disposition %q missing file name", cd)
	}

	// 3. Exactly one Telegram fetch happened.
	if n := api.calls["FID-report.pdf"]; n != 1 {
		t.Fatalf("telegram getFile calls: %d, want 1", n)
	}
}

// A repeat click inside the window re-serves the same file without
// re-fetching from Telegram (the registry answers; the timer is untouched).
func TestDownloadReusesLiveFile(t *testing.T) {
	payload := []byte("hello")
	api := &stubAPI{fileSize: int64(len(payload))}
	api.host = downloadServer(t, payload).URL

	s, st := newDownloadTestServer(t, api)
	doc := seedDocument(t, st, "notes.txt", int64(len(payload)))
	h := s.Handler()

	for i := 0; i < 3; i++ {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", fmt.Sprintf("/documents/%d/download", doc.ID), nil))
		if w.Code != http.StatusSeeOther {
			t.Fatalf("click %d: got %d, want 303", i, w.Code)
		}
		r := httptest.NewRequest("GET", w.Header().Get("Location"), nil)
		w = httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusOK || w.Body.String() != string(payload) {
			t.Fatalf("click %d: temp fetch got %d %q", i, w.Code, w.Body.String())
		}
	}
	if n := api.calls["FID-notes.txt"]; n != 1 {
		t.Fatalf("telegram getFile calls after 3 clicks: %d, want 1", n)
	}
}

// After the TTL expires the temp URL stops serving and the next download
// click fetches the file again (a fresh window).
func TestDownloadAfterExpiryRefetches(t *testing.T) {
	payload := []byte("data")
	api := &stubAPI{fileSize: int64(len(payload))}
	api.host = downloadServer(t, payload).URL

	s, st := newDownloadTestServer(t, api)
	doc := seedDocument(t, st, "doc.bin", int64(len(payload)))
	h := s.Handler()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", fmt.Sprintf("/documents/%d/download", doc.ID), nil))
	loc := w.Header().Get("Location")

	// Force-expire the entry (as the sweeper would) but keep the file on
	// disk for a moment — the URL must still refuse to serve it.
	s.temp.mu.Lock()
	e := s.temp.files[doc.ID]
	s.temp.files[doc.ID] = tempEntry{path: e.path, name: e.name, mime: e.mime, expiresAt: time.Now().Add(-time.Second)}
	s.temp.mu.Unlock()

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", loc, nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("expired temp fetch: got %d, want 404", w.Code)
	}

	// The sweeper deletes the expired file...
	s.temp.sweep(time.Now())
	if _, err := os.Stat(e.path); !os.IsNotExist(err) {
		t.Fatalf("expired file still on disk: %v", err)
	}

	// ...and the next click fetches anew and serves again.
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", fmt.Sprintf("/documents/%d/download", doc.ID), nil))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("second download click: got %d, want 303", w.Code)
	}
	loc = w.Header().Get("Location")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", loc, nil))
	if w.Code != http.StatusOK || w.Body.String() != string(payload) {
		t.Fatalf("re-fetched temp fetch: got %d %q", w.Code, w.Body.String())
	}
	if n := api.calls["FID-doc.bin"]; n != 2 {
		t.Fatalf("telegram getFile calls after expiry: %d, want 2", n)
	}
}

// Documents over the Bot API cap redirect straight to the t.me message,
// and nothing is fetched or stored.
func TestDownloadOversizeRedirectsToTelegram(t *testing.T) {
	api := &stubAPI{}
	s, st := newDownloadTestServer(t, api)
	doc := seedDocument(t, st, "big.zip", telegramDownloadCap+1)
	h := s.Handler()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", fmt.Sprintf("/documents/%d/download", doc.ID), nil))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("oversize download: got %d, want 303", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != doc.MessageLink {
		t.Fatalf("oversize download: redirect %q, want t.me link %q", loc, doc.MessageLink)
	}
	if len(api.calls) != 0 {
		t.Fatalf("oversize download hit telegram: %v", api.calls)
	}
}

// A Telegram-side cap violation (metadata claimed smaller) falls back to the
// t.me link instead of failing the click.
func TestDownloadTelegramCapFallback(t *testing.T) {
	api := &stubAPI{err: errTooLarge{size: telegramDownloadCap + 1}}
	s, st := newDownloadTestServer(t, api)
	doc := seedDocument(t, st, "surprise.bin", 100)
	h := s.Handler()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", fmt.Sprintf("/documents/%d/download", doc.ID), nil))
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != doc.MessageLink {
		t.Fatalf("cap fallback: got %d %q, want 303 to t.me", w.Code, w.Header().Get("Location"))
	}
}

// The temp URL name segment must match the entry — a wrong name cannot fish
// the file out of the directory.
func TestTempURLNameMismatchRejected(t *testing.T) {
	payload := []byte("x")
	api := &stubAPI{fileSize: 1}
	api.host = downloadServer(t, payload).URL

	s, st := newDownloadTestServer(t, api)
	doc := seedDocument(t, st, "secret.pdf", 1)
	h := s.Handler()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", fmt.Sprintf("/documents/%d/download", doc.ID), nil))
	loc := w.Header().Get("Location")

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", loc, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("correct name: got %d, want 200", w.Code)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/temp/"+itoa(doc.ID)+"/wrong-name.pdf", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("wrong name: got %d, want 404", w.Code)
	}
}

// Two archived documents with the same file name must both download: the
// second one's URL and disk file are disambiguated by document id.
func TestDownloadDuplicateFileNames(t *testing.T) {
	payload := []byte("same-name docs")
	api := &stubAPI{fileSize: int64(len(payload))}
	api.host = downloadServer(t, payload).URL

	s, st := newDownloadTestServer(t, api)
	docA := seedDocument(t, st, "report.pdf", int64(len(payload)))
	docB := seedDocument(t, st, "report.pdf", int64(len(payload)))
	h := s.Handler()

	seen := map[string]bool{}
	for _, doc := range []store.Document{docA, docB} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", fmt.Sprintf("/documents/%d/download", doc.ID), nil))
		loc := w.Header().Get("Location")
		if seen[loc] {
			t.Fatalf("duplicate temp URL %q for two documents", loc)
		}
		seen[loc] = true

		w = httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", loc, nil))
		if w.Code != http.StatusOK || w.Body.String() != string(payload) {
			t.Fatalf("doc %d temp fetch: got %d %q", doc.ID, w.Code, w.Body.String())
		}
	}
}

// Temp URLs sit behind the same session auth as the rest of the site: a
// leaked link without a session redirects to /login.
func TestTempURLRequiresAuth(t *testing.T) {
	api := &stubAPI{fileSize: 1}
	st := newTestStore(t)
	s := New(st, nil, "hunter2", nil, TempConfig{Dir: t.TempDir(), TTL: time.Minute}, api)
	h := s.Handler()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/temp/123/whatever.pdf", nil))
	if w.Code != http.StatusSeeOther || !strings.HasSuffix(w.Header().Get("Location"), "/login") {
		t.Fatalf("unauthenticated temp fetch: got %d %q, want redirect to /login",
			w.Code, w.Header().Get("Location"))
	}

	// Log in, then the same URL pattern is reachable (404 for the unknown
	// entry, not a redirect — proving auth passed).
	w = httptest.NewRecorder()
	login := httptest.NewRequest("POST", "/login", strings.NewReader("password=hunter2"))
	login.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(w, login)
	r := httptest.NewRequest("GET", "/temp/123/whatever.pdf", nil)
	for _, c := range w.Result().Cookies() {
		r.AddCookie(c)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("authenticated temp fetch: got %d, want 404", w.Code)
	}
}

// The orphan sweep deletes only teledoc- prefixed files (and part files) the
// registry does not know about — unrelated files in a mapped directory are
// never touched.
func TestSweepOrphansRespectsPrefix(t *testing.T) {
	dir := t.TempDir()
	s := New(nil, nil, "", nil, TempConfig{Dir: dir, TTL: time.Minute}, nil).temp

	// An orphan old enough to sweep, a fresh part file (still downloading),
	// and files the feature never created.
	stale := filepath.Join(dir, "teledoc-9-orphan.pdf")
	if err := os.WriteFile(stale, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(stale, past, past); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(dir, ".teledoc-part-9-1")
	if err := os.WriteFile(fresh, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(dir, "important.txt")
	if err := os.WriteFile(unrelated, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	s.sweepOrphans(time.Now())

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale orphan survived: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh part file was swept: %v", err)
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Errorf("unrelated file was swept: %v", err)
	}
}

// Oversize metadata updates: the download handler trusts stored size for the
// fast path but re-checks the Telegram-reported size before writing (covered
// by errTooLarge above). Also verify the cap constant matches the Bot API.
func TestTelegramCapConstant(t *testing.T) {
	const botAPICap = 20 * 1024 * 1024
	if telegramDownloadCap != botAPICap {
		t.Fatalf("telegramDownloadCap = %d, want %d", telegramDownloadCap, botAPICap)
	}
}

// The full download lifecycle is observable in the log: the click, the
// Telegram fetch, each serve of the file, the reuse of a live window, and
// the deletion when the window closes.
func TestDownloadLifecycleLogs(t *testing.T) {
	payload := []byte("log me")
	api := &stubAPI{fileSize: int64(len(payload))}
	api.host = downloadServer(t, payload).URL

	var buf lockedBuffer
	s, st := newDownloadTestServerWithLogger(t, api, &buf)
	doc := seedDocument(t, st, "logged.pdf", int64(len(payload)))
	h := s.Handler()

	// Click 1: trigger + fetched + redirected.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", fmt.Sprintf("/documents/%d/download", doc.ID), nil))
	loc := w.Header().Get("Location")
	for _, want := range []string{
		"download triggered for document",
		"downloaded document",
		"download click for document",
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("after first click, log missing %q; got:\n%s", want, buf.String())
		}
	}

	// Serve the file: a serving line must appear.
	buf.Reset()
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", loc, nil))
	if !strings.Contains(buf.String(), "serving temp download of document") {
		t.Errorf("temp serve did not log; got:\n%s", buf.String())
	}

	// Click 2 inside the window: reuse, no new fetch.
	buf.Reset()
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", fmt.Sprintf("/documents/%d/download", doc.ID), nil))
	if !strings.Contains(buf.String(), "re-served from the still-open temp window") {
		t.Errorf("reuse click did not log; got:\n%s", buf.String())
	}

	// Expire and sweep: the deletion must be logged.
	buf.Reset()
	s.temp.mu.Lock()
	e := s.temp.files[doc.ID]
	s.temp.files[doc.ID] = tempEntry{path: e.path, name: e.name, mime: e.mime, size: e.size, expiresAt: time.Now().Add(-time.Second)}
	s.temp.mu.Unlock()
	s.temp.sweep(time.Now())
	if !strings.Contains(buf.String(), "temp window closed for document") || !strings.Contains(buf.String(), "deleted") {
		t.Errorf("expiry deletion did not log; got:\n%s", buf.String())
	}
}

// lockedBuffer is a concurrency-safe bytes.Buffer for log capture from the
// sweeper goroutine and request handlers.
type lockedBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *lockedBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

// newDownloadTestServerWithLogger is newDownloadTestServer with the server's
// log output captured into buf.
func newDownloadTestServerWithLogger(t *testing.T, api telegramAPI, buf *lockedBuffer) (*Server, *store.Store) {
	t.Helper()
	st := newTestStore(t)
	s := New(st, nil, "", log.New(buf, "", 0), TempConfig{Dir: t.TempDir(), TTL: time.Minute}, api)
	return s, st
}

// A failing Telegram fetch leaves no staging file behind and surfaces an
// error flash (redirect back to the list, no temp entry).
func TestDownloadFetchFailureCleansUp(t *testing.T) {
	api := &stubAPI{err: errors.New("network down")}
	s, st := newDownloadTestServer(t, api)
	doc := seedDocument(t, st, "fail.pdf", 10)
	h := s.Handler()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", fmt.Sprintf("/documents/%d/download", doc.ID), nil))
	if w.Code != http.StatusSeeOther {
		t.Fatalf("failed fetch: got %d, want 303 back to the list", w.Code)
	}
	if loc := w.Header().Get("Location"); strings.HasPrefix(loc, "/temp/") {
		t.Fatalf("failed fetch redirected to temp URL %q", loc)
	}
	s.temp.mu.Lock()
	live := len(s.temp.files)
	s.temp.mu.Unlock()
	if live != 0 {
		t.Fatalf("failed fetch registered %d temp entries", live)
	}
	entries, _ := os.ReadDir(s.temp.dir)
	if len(entries) != 0 {
		t.Fatalf("failed fetch left %d files behind", len(entries))
	}
}
