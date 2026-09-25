// Package web serves the Papra-style UI: a document archive filterable by
// tags, plus management pages for tags and tagging rules. Pages are rendered
// with html/template; htmx (vendored) handles filtering without reloads.
// Static assets and templates are embedded in the binary.
package web

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"teledoc/internal/rules"
	"teledoc/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

// Server is the web UI.
type Server struct {
	store  *store.Store
	engine *rules.Engine
	log    *log.Logger

	password string // empty = no auth (trusted LAN)

	mu       sync.Mutex
	sessions map[string]time.Time
}

// New creates the web server. When password is non-empty every page (except
// /login and /static) requires a session cookie.
func New(st *store.Store, engine *rules.Engine, password string, logger *log.Logger) *Server {
	if logger == nil {
		logger = log.Default()
	}
	return &Server{
		store:    st,
		engine:   engine,
		log:      logger,
		password: password,
		sessions: map[string]time.Time{},
	}
}

// Handler returns the routed http.Handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /static/", s.handleStatic)

	if s.password != "" {
		mux.HandleFunc("GET /login", s.handleLoginPage)
		mux.HandleFunc("POST /login", s.handleLoginSubmit)
		mux.HandleFunc("POST /logout", s.handleLogout)
	}

	protected := http.NewServeMux()
	protected.HandleFunc("GET /{$}", s.handleDocuments)
	protected.HandleFunc("GET /documents", s.handleDocuments)
	protected.HandleFunc("GET /documents/list", s.handleDocumentList) // htmx partial
	protected.HandleFunc("POST /documents/{id}/tags", s.handleDocumentTag)
	protected.HandleFunc("POST /documents/{id}/delete", s.handleDocumentDelete)
	protected.HandleFunc("POST /documents/bulk-delete", s.handleDocumentsBulkDelete)
	protected.HandleFunc("POST /documents/bulk-tag", s.handleDocumentsBulkTag)
	protected.HandleFunc("GET /tags", s.handleTags)
	protected.HandleFunc("POST /tags", s.handleCreateTag)
	protected.HandleFunc("POST /tags/{id}/delete", s.handleDeleteTag)
	protected.HandleFunc("POST /tags/{id}/color", s.handleSetTagColor)
	protected.HandleFunc("GET /rules", s.handleRules)
	protected.HandleFunc("POST /rules", s.handleCreateRule)
	protected.HandleFunc("POST /rules/{id}/delete", s.handleDeleteRule)
	protected.HandleFunc("POST /rules/{id}/toggle", s.handleRuleToggle)
	protected.HandleFunc("POST /rules/{id}/run", s.handleRuleRun)

	mux.Handle("/", s.withAuth(protected))
	return mux
}

// withAuth guards a handler with the session cookie when a password is set.
func (s *Server) withAuth(next http.Handler) http.Handler {
	if s.password == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.validSession(r) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) validSession(r *http.Request) bool {
	c, err := r.Cookie("teledoc_session")
	if err != nil || c.Value == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.sessions[c.Value]
	if !ok || time.Now().After(exp) {
		delete(s.sessions, c.Value)
		return false
	}
	return true
}

func (s *Server) newSession(w http.ResponseWriter) {
	tok := make([]byte, 32)
	if _, err := rand.Read(tok); err != nil {
		http.Error(w, "session error", http.StatusInternalServerError)
		return
	}
	id := hex.EncodeToString(tok)
	s.mu.Lock()
	s.sessions[id] = time.Now().Add(7 * 24 * time.Hour)
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name:     "teledoc_session",
		Value:    id,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   7 * 24 * 3600,
	})
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if s.validSession(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, pageData{Title: "Sign in", Inner: "page_login", Password: true})
}

func (s *Server) handleLoginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	given := r.PostFormValue("password")
	if subtle.ConstantTimeCompare([]byte(given), []byte(s.password)) != 1 {
		http.Error(w, "Wrong password", http.StatusUnauthorized)
		return
	}
	s.newSession(w)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("teledoc_session"); err == nil {
		s.mu.Lock()
		delete(s.sessions, c.Value)
		s.mu.Unlock()
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// handleStatic serves embedded assets.
func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/static/")
	if strings.Contains(path, "..") {
		http.NotFound(w, r)
		return
	}
	data, err := staticFS.ReadFile("static/" + path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch {
	case strings.HasSuffix(path, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case strings.HasSuffix(path, ".js"):
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	case strings.HasSuffix(path, ".svg"):
		w.Header().Set("Content-Type", "image/svg+xml")
	case strings.HasSuffix(path, ".png"):
		w.Header().Set("Content-Type", "image/png")
	case strings.HasSuffix(path, ".woff2"):
		w.Header().Set("Content-Type", "font/woff2")
	}
	w.Header().Set("Cache-Control", "max-age=300")
	_, _ = w.Write(data)
}

// ---------------------------------------------------------------------------
// Templates

var funcMap = template.FuncMap{
	"fmtSize":  humanSize,
	"fmtDate":  func(t time.Time) string { return t.Format("Jan 2, 2006") },
	"lower":    strings.ToLower,
	"extUpper": func(s string) string { return strings.ToUpper(s) },
	"add":      func(a, b int) int { return a + b },
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

type pageData struct {
	Title    string
	IsHTMX   bool
	Flash    string
	Inner    string
	Data     any
	Authed   bool
	Password bool // whether auth is enabled (shows logout button)
}

var tplCache *template.Template

func loadTemplates() *template.Template {
	if tplCache != nil {
		return tplCache
	}
	t := template.New("base").Funcs(funcMap)
	tplCache = template.Must(t.ParseFS(templateFS, "templates/*.html"))
	return tplCache
}

// render outputs the base layout; htmx requests get only the fragment.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, page pageData) {
	tpl := loadTemplates()
	page.IsHTMX = r.Header.Get("HX-Request") == "true"
	// Authed reflects a valid session on the current request (not merely that
	// auth is enabled), so controls like the logout button only appear once
	// the user has actually signed in.
	page.Authed = s.password != "" && s.validSession(r)
	if page.IsHTMX {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		_ = tpl.ExecuteTemplate(w, "fragment", page)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = tpl.ExecuteTemplate(w, "layout", page)
}

func flashFromCookie(w http.ResponseWriter, r *http.Request) string {
	c, err := r.Cookie("flash")
	if err != nil || c.Value == "" {
		return ""
	}
	http.SetCookie(w, &http.Cookie{Name: "flash", Value: "", Path: "/", MaxAge: -1})
	v, err := url.QueryUnescape(c.Value)
	if err != nil {
		return ""
	}
	return v
}

func setFlash(w http.ResponseWriter, msg string) {
	http.SetCookie(w, &http.Cookie{
		Name: "flash", Value: url.QueryEscape(msg), Path: "/", MaxAge: 30, HttpOnly: true,
	})
}

// ---------------------------------------------------------------------------
// Documents page

type documentsPage struct {
	Tags        []store.Tag
	Documents   []store.DocumentWithTags
	Search      string
	ActiveTags  map[int64]bool
	Untagged    bool
	TotalDocs   int
	SortBy      string
	SortDir     string
	Page        int
	PerPage     int
	TotalPages  int
	ShowFrom    int // 1-based index of first row on the page (for "showing X-Y of Z")
	ShowTo      int
	Q           url.Values // current query, for building sort/page links
	CurrentTime time.Time
}

// defaultSortDir is the direction a column gets on first click.
var defaultSortDir = map[string]string{
	"name": "asc",  // A-Z first
	"type": "asc",  // A-Z first
	"date": "desc", // newest first
	"size": "desc", // largest first
}

func cloneValues(v url.Values) url.Values {
	c := url.Values{}
	for k, vs := range v {
		c[k] = append([]string(nil), vs...)
	}
	return c
}

// SortURL is the querystring for sorting by col, toggling direction when it
// is already the active sort. Sorting resets to page 1.
func (p documentsPage) SortURL(col string) string {
	q := cloneValues(p.Q)
	dir := defaultSortDir[col]
	if p.SortBy == col {
		if p.SortDir == "asc" {
			dir = "desc"
		} else {
			dir = "asc"
		}
	}
	q.Set("sort", col)
	q.Set("order", dir)
	q.Del("page")
	return q.Encode()
}

// PageURL is the querystring for the given page, keeping sort/filter state.
func (p documentsPage) PageURL(page int) string {
	q := cloneValues(p.Q)
	if page <= 1 {
		q.Del("page")
	} else {
		q.Set("page", strconv.Itoa(page))
	}
	return q.Encode()
}

// PerPageURL is the querystring for a different page size (resets to page 1).
func (p documentsPage) PerPageURL(n int) string {
	q := cloneValues(p.Q)
	q.Set("per_page", strconv.Itoa(n))
	q.Del("page")
	return q.Encode()
}

func parseIDs(values []string) []int64 {
	ids := []int64{}
	seen := map[int64]bool{}
	for _, v := range values {
		id, err := strconv.ParseInt(v, 10, 64)
		if err == nil && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids
}

func (s *Server) documentsData(r *http.Request) (documentsPage, error) {
	q := r.URL.Query()
	search := strings.TrimSpace(q.Get("q"))
	tagIDs := parseIDs(q["tag"])
	untagged := q.Get("untagged") == "1"

	sortBy := q.Get("sort")
	switch sortBy {
	case "name", "type", "size", "date":
	default:
		sortBy = "date"
	}
	sortDir := q.Get("order")
	if sortDir != "asc" {
		sortDir = "desc"
	}

	perPage := 10
	switch q.Get("per_page") {
	case "10", "20", "50", "100":
		perPage, _ = strconv.Atoi(q.Get("per_page"))
	}
	total, err := s.store.CountDocumentsFiltered(store.DocumentFilter{
		Search: search, TagIDs: tagIDs, Untagged: untagged,
	})
	if err != nil {
		return documentsPage{}, err
	}
	totalPages := (total + perPage - 1) / perPage
	if totalPages < 1 {
		totalPages = 1
	}
	page := 1
	if p, err := strconv.Atoi(q.Get("page")); err == nil && p > 0 {
		page = p
	}
	if page > totalPages {
		page = totalPages
	}

	f := store.DocumentFilter{
		Search: search, TagIDs: tagIDs, Untagged: untagged,
		SortBy: sortBy, SortDir: sortDir,
		Limit: perPage, Offset: (page - 1) * perPage,
	}
	docs, err := s.store.ListDocuments(f)
	if err != nil {
		return documentsPage{}, err
	}
	tags, err := s.store.ListTags()
	if err != nil {
		return documentsPage{}, err
	}
	active := map[int64]bool{}
	for _, id := range tagIDs {
		active[id] = true
	}
	showFrom := (page-1)*perPage + 1
	if total == 0 {
		showFrom = 0
	}
	showTo := (page-1)*perPage + len(docs)
	return documentsPage{
		Tags: tags, Documents: docs, Search: search,
		ActiveTags: active, Untagged: untagged, TotalDocs: total,
		SortBy: sortBy, SortDir: sortDir,
		Page: page, PerPage: perPage, TotalPages: totalPages,
		ShowFrom: showFrom, ShowTo: showTo,
		Q:           q,
		CurrentTime: time.Now(),
	}, nil
}

func (s *Server) handleDocuments(w http.ResponseWriter, r *http.Request) {
	data, err := s.documentsData(r)
	if err != nil {
		s.render(w, r, http.StatusInternalServerError, pageData{Title: "Error", Flash: err.Error(), Password: s.password != ""})
		return
	}
	s.render(w, r, http.StatusOK, pageData{
		Title: "Documents", Inner: "page_documents", Data: data,
		Flash: flashFromCookie(w, r), Password: s.password != "",
	})
}

// handleDocumentList returns just the grid fragment for htmx filtering.
func (s *Server) handleDocumentList(w http.ResponseWriter, r *http.Request) {
	data, err := s.documentsData(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, http.StatusOK, pageData{Inner: "fragment_grid", Data: data})
}

// handleDocumentTag manually attaches/detaches a tag on a document.
func (s *Server) handleDocumentTag(w http.ResponseWriter, r *http.Request) {
	docID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	_ = r.ParseForm()
	action := r.PostFormValue("action")
	tagID, err := strconv.ParseInt(r.PostFormValue("tag_id"), 10, 64)
	if err != nil {
		http.Error(w, "bad tag", http.StatusBadRequest)
		return
	}
	switch action {
	case "remove":
		err = s.store.RemoveTagFromDocument(docID, tagID)
	default: // add
		_, err = s.store.AddTagToDocument(docID, tagID)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleDocumentDelete removes a document entry from the archive. This only
// deletes the metadata row from the app's DB — it does NOT remove the file
// from the Telegram channel. The delete form carries a hidden confirm token so
// a stale/naive request cannot wipe a row silently.
func (s *Server) handleDocumentDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if r.PostFormValue("confirm") != "1" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if err := s.store.DeleteDocument(id); err != nil {
		setFlash(w, "Could not delete document: "+err.Error())
	} else {
		setFlash(w, "Document deleted from the archive.")
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleDocumentsBulkDelete removes several document entries at once — the
// "Delete selected" toolbar above the table. Same DB-only semantics and
// confirm-token requirement as the single-document delete above; one
// document ID per selected row checkbox, submitted via each checkbox's
// form="bulk-delete-form" attribute.
func (s *Server) handleDocumentsBulkDelete(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if r.PostFormValue("confirm") != "1" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	ids := r.PostForm["doc_ids"]
	if len(ids) == 0 {
		setFlash(w, "No documents were selected.")
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	deleted, failed := 0, 0
	for _, raw := range ids {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			failed++
			continue
		}
		if err := s.store.DeleteDocument(id); err != nil {
			failed++
			continue
		}
		deleted++
	}
	switch {
	case failed == 0:
		setFlash(w, fmt.Sprintf("%d document(s) deleted from the archive.", deleted))
	case deleted == 0:
		setFlash(w, "Could not delete the selected documents.")
	default:
		setFlash(w, fmt.Sprintf("%d document(s) deleted, %d failed.", deleted, failed))
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleDocumentsBulkTag toggles one or more tags across every selected
// document — the "Tag selected" toolbar above the table. Each chosen tag is
// added to selected docs that lack it and removed from those that already
// have it, per document. Tags arrive as one "tags" form value per chosen
// tag, document IDs as one "doc_ids" value per selected row checkbox (the
// checkboxes are owned by the bulk-delete form; app.js copies the checked
// IDs into this form at submit time).
func (s *Server) handleDocumentsBulkTag(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	ids := parseIDs(r.PostForm["doc_ids"])
	tagIDs := parseIDs(r.PostForm["tags"])
	if len(ids) == 0 || len(tagIDs) == 0 {
		setFlash(w, "Select at least one document and one tag.")
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	added, removed, err := s.store.ToggleTagsOnDocuments(ids, tagIDs)
	switch {
	case err != nil:
		setFlash(w, "Could not update tags on the selected documents: "+err.Error())
	case added == 0 && removed == 0:
		setFlash(w, "Nothing to change.")
	case added > 0 && removed > 0:
		setFlash(w, fmt.Sprintf("Tag applied to %d document(s), removed from %d.", added, removed))
	case added > 0:
		setFlash(w, fmt.Sprintf("Tag applied to %d document(s).", added))
	default:
		setFlash(w, fmt.Sprintf("Tag removed from %d document(s).", removed))
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ---------------------------------------------------------------------------
// Tags page

func (s *Server) handleTags(w http.ResponseWriter, r *http.Request) {
	tags, err := s.store.ListTags()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, http.StatusOK, pageData{
		Title: "Tags", Inner: "page_tags", Data: tags,
		Flash: flashFromCookie(w, r), Password: s.password != "",
	})
}

func (s *Server) handleCreateTag(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		setFlash(w, "Tag name cannot be empty.")
	} else if _, err := s.store.CreateTag(name); err != nil {
		setFlash(w, "Could not create tag: "+err.Error())
	} else {
		setFlash(w, "Tag "+name+" created.")
	}
	http.Redirect(w, r, "/tags", http.StatusSeeOther)
}

func (s *Server) handleDeleteTag(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := s.store.DeleteTag(id); err != nil {
		setFlash(w, "Could not delete tag: "+err.Error())
	} else {
		setFlash(w, "Tag deleted.")
	}
	http.Redirect(w, r, "/tags", http.StatusSeeOther)
}

// handleSetTagColor updates a tag's colour from a 6-digit hex value such as
// "#8b5cf6". It validates the input before persisting.
func (s *Server) handleSetTagColor(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	color := normalizeHexColor(strings.TrimSpace(r.PostFormValue("color")))
	if color == "" {
		setFlash(w, "Please enter a valid hex colour (e.g. #8b5cf6).")
		http.Redirect(w, r, "/tags", http.StatusSeeOther)
		return
	}
	if err := s.store.SetTagColor(id, color); err != nil {
		setFlash(w, "Could not update colour: "+err.Error())
	} else {
		setFlash(w, "Tag colour updated.")
	}
	http.Redirect(w, r, "/tags", http.StatusSeeOther)
}

// normalizeHexColor accepts "#rrggbb" or "rrggbb", uppercases the hex digits,
// and returns a canonical "#rrggbb". Returns "" for anything else.
func normalizeHexColor(s string) string {
	s = strings.TrimPrefix(s, "#")
	if len(s) != 6 {
		return ""
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return ""
		}
	}
	return "#" + strings.ToUpper(s)
}

// ---------------------------------------------------------------------------
// Rules page

type rulesPage struct {
	Tags  []store.Tag
	Rules []store.Rule
}

func (s *Server) handleRules(w http.ResponseWriter, r *http.Request) {
	data, err := s.rulesData()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, r, http.StatusOK, pageData{
		Title: "Tagging Rules", Inner: "page_rules", Data: data,
		Flash: flashFromCookie(w, r), Password: s.password != "",
	})
}

func (s *Server) rulesData() (rulesPage, error) {
	tags, err := s.store.ListTags()
	if err != nil {
		return rulesPage{}, err
	}
	rules, err := s.store.ListRules()
	if err != nil {
		return rulesPage{}, err
	}
	return rulesPage{Tags: tags, Rules: rules}, nil
}

func (s *Server) handleCreateRule(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		setFlash(w, "Rule name is required.")
		http.Redirect(w, r, "/rules", http.StatusSeeOther)
		return
	}
	matchMode := r.PostFormValue("match_mode")
	if matchMode != "any" {
		matchMode = "all"
	}

	var conds []store.Condition
	fields := r.PostForm["field"]
	ops := r.PostForm["operator"]
	values := r.PostForm["value"]
	for i := range values {
		if i >= len(fields) || i >= len(ops) {
			break
		}
		value := strings.TrimSpace(values[i])
		if value == "" {
			continue
		}
		cs := r.PostFormValue(fmt.Sprintf("case_%d", i)) == "on"
		conds = append(conds, store.Condition{
			Field: fields[i], Operator: ops[i], Value: value, CaseSensitive: cs,
		})
	}

	tagIDs := parseIDs(r.PostForm["tags"])
	if len(tagIDs) == 0 {
		setFlash(w, "Pick at least one tag for the rule to apply.")
		http.Redirect(w, r, "/rules", http.StatusSeeOther)
		return
	}

	if _, err := s.store.CreateRule(name, matchMode, true, conds, tagIDs); err != nil {
		setFlash(w, "Could not create rule: "+err.Error())
		http.Redirect(w, r, "/rules", http.StatusSeeOther)
		return
	}
	setFlash(w, "Rule "+name+" created. It applies to documents uploaded from now on - use Run now to apply it to existing ones.")
	http.Redirect(w, r, "/rules", http.StatusSeeOther)
}

func (s *Server) handleDeleteRule(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if err := s.store.DeleteRule(id); err != nil {
		setFlash(w, "Could not delete rule: "+err.Error())
	} else {
		setFlash(w, "Rule deleted.")
	}
	http.Redirect(w, r, "/rules", http.StatusSeeOther)
}

func (s *Server) handleRuleToggle(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	rules, err := s.store.ListRules()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, rule := range rules {
		if rule.ID == id {
			if err := s.store.SetRuleEnabled(id, !rule.Enabled); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			break
		}
	}
	http.Redirect(w, r, "/rules", http.StatusSeeOther)
}

// handleRuleRun is Papra's "Apply to existing documents": re-run the rule
// over the whole archive and report what happened.
func (s *Server) handleRuleRun(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	stats, err := s.engine.RunRuleOverAllDocuments(id)
	if err != nil {
		setFlash(w, "Run failed: "+err.Error())
	} else {
		setFlash(w, fmt.Sprintf("Run finished: processed %d documents, tagged %d, errors %d.",
			stats.Processed, stats.Tagged, stats.Errors))
	}
	http.Redirect(w, r, "/rules", http.StatusSeeOther)
}
