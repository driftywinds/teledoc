package web

import (
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"teledoc/internal/rules"
	"teledoc/internal/store"
)

// Verification that all embedded templates parse and key pages render.
func TestTemplatesParseAndRender(t *testing.T) {
	tpl := loadTemplates()
	if tpl == nil {
		t.Fatal("templates failed to load")
	}
	for _, name := range []string{"layout", "fragment", "page_documents", "fragment_grid", "page_tags", "page_rules", "page_login", "head", "foot"} {
		if tpl.Lookup(name) == nil {
			t.Errorf("template %q missing", name)
		}
	}
}

func TestRenderPagesWithoutError(t *testing.T) {
	s := New(nil, nil, "", nil) // store/engine unused on the code paths below
	r := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()

	s.render(w, r, 200, pageData{Title: "t", Inner: "page_documents", Data: documentsPage{}, Password: false})
	if w.Code != 200 {
		t.Errorf("documents page: got %d", w.Code)
	}

	w = httptest.NewRecorder()
	s.render(w, r, 200, pageData{Title: "t", Inner: "page_tags", Data: []any{}, Password: false})
	if w.Code != 200 {
		t.Errorf("tags page: got %d", w.Code)
	}

	w = httptest.NewRecorder()
	s.render(w, r, 200, pageData{Title: "t", Inner: "page_rules", Data: rulesPage{}, Password: false})
	if w.Code != 200 {
		t.Errorf("rules page: got %d", w.Code)
	}

	// htmx fragment path
	r.Header.Set("HX-Request", "true")
	w = httptest.NewRecorder()
	s.render(w, r, 200, pageData{Title: "t", Inner: "fragment_grid", Data: documentsPage{}})
	if w.Code != 200 {
		t.Errorf("fragment: got %d", w.Code)
	}
}

// newTestStore opens a throwaway SQLite store in a temp directory.
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// itoa formats an int64 for form values.
func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// The "Tag selected" bulk action: form parsing (repeated doc_ids/tags values),
// the redirect, and the resulting document_tags rows.
func TestHandleDocumentsBulkTag(t *testing.T) {
	st := newTestStore(t)
	docA, _, err := st.UpsertDocument(store.Document{FileName: "a.pdf", ChatID: 1, MessageID: 1, MessageLink: "l", UploadedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	docB, _, err := st.UpsertDocument(store.Document{FileName: "b.pdf", ChatID: 1, MessageID: 2, MessageLink: "l", UploadedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	tag1, err := st.CreateTag("invoices")
	if err != nil {
		t.Fatal(err)
	}
	tag2, err := st.CreateTag("reports")
	if err != nil {
		t.Fatal(err)
	}

	s := New(st, rules.New(st, nil), "", nil)
	handler := s.Handler()

	form := url.Values{}
	form.Add("doc_ids", itoa(docA))
	form.Add("doc_ids", itoa(docB))
	form.Add("tags", itoa(tag1.ID))
	form.Add("tags", itoa(tag2.ID))
	r := httptest.NewRequest("POST", "/documents/bulk-tag", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != 303 {
		t.Fatalf("expected redirect, got %d", w.Code)
	}
	for _, docID := range []int64{docA, docB} {
		tags, err := st.DocumentTags(docID)
		if err != nil {
			t.Fatal(err)
		}
		if len(tags) != 2 {
			t.Errorf("document %d: got %d tags, want 2", docID, len(tags))
		}
	}

	// Submitting the same selection again is a no-op (idempotent links).
	r = httptest.NewRequest("POST", "/documents/bulk-tag", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 303 {
		t.Fatalf("re-tag: expected redirect, got %d", w.Code)
	}
	tags, err := st.DocumentTags(docA)
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 2 {
		t.Errorf("re-tag duplicated links: got %d tags, want 2", len(tags))
	}

	// Missing selections redirect back with an error flash, no changes.
	r = httptest.NewRequest("POST", "/documents/bulk-tag", strings.NewReader("doc_ids="+itoa(docA)))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 303 {
		t.Fatalf("no-tags submit: expected redirect, got %d", w.Code)
	}
}
