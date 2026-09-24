package web

import (
	"net/http/httptest"
	"testing"
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
