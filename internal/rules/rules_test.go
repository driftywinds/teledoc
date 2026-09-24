package rules

import (
	"errors"
	"testing"

	"tgarchive/internal/store"
)

func doc(name, ext, mime string) store.Document {
	return store.Document{ID: 1, FileName: name, Extension: ext, MimeType: mime}
}

func cond(field, op, value string, cs bool) store.Condition {
	return store.Condition{Field: field, Operator: op, Value: value, CaseSensitive: cs}
}

func TestMatchesContainsCaseInsensitive(t *testing.T) {
	// The user's headline example: "Casio A286 Manual.pdf" + rule name
	// contains "manual" (case-agnostic) -> matches.
	r := store.Rule{MatchMode: "all", Conditions: []store.Condition{cond("name", "contains", "manual", false)}}
	if !Matches(r, doc("Casio A286 Manual.pdf", "pdf", "application/pdf")) {
		t.Fatal("expected case-insensitive contains to match")
	}
	if Matches(r, doc("Casio A286 brochure.pdf", "pdf", "application/pdf")) {
		t.Fatal("expected non-matching filename to fail contains")
	}
}

func TestMatchesCaseSensitive(t *testing.T) {
	r := store.Rule{MatchMode: "all", Conditions: []store.Condition{cond("name", "contains", "Manual", true)}}
	if !Matches(r, doc("User Manual.pdf", "", "")) {
		t.Fatal("expected case-sensitive contains to match exact case")
	}
	if Matches(r, doc("user manual.pdf", "", "")) {
		t.Fatal("expected case-sensitive contains to reject different case")
	}
}

func TestMatchesAllOperators(t *testing.T) {
	d := doc("Casio A286 Manual.pdf", "pdf", "application/pdf")
	cases := []struct {
		op    string
		value string
		want  bool
	}{
		{"equal", "Casio A286 Manual.pdf", true},
		{"equal", "casio a286 manual.pdf", true}, // case-insensitive default makes equal match
		{"equal", "casio a286 manual.PDFX", false},
		{"not_equal", "other.pdf", true},
		{"contains", "a286", true},
		{"not_contains", "brochure", true},
		{"starts_with", "casio", true},
		{"ends_with", ".PDF", true},
		{"starts_with", "manual", false},
		{"ends_with", "manual", false},
	}
	for _, tc := range cases {
		r := store.Rule{MatchMode: "all", Conditions: []store.Condition{cond("name", tc.op, tc.value, false)}}
		if got := Matches(r, d); got != tc.want {
			t.Errorf("operator %q value %q: got %v want %v", tc.op, tc.value, got, tc.want)
		}
	}
}

func TestMatchesExtensionAndMimeFields(t *testing.T) {
	rExt := store.Rule{MatchMode: "all", Conditions: []store.Condition{cond("extension", "equal", "csv", false)}}
	if !Matches(rExt, doc("report.csv", "csv", "text/csv")) {
		t.Fatal("extension field should match")
	}
	rMime := store.Rule{MatchMode: "all", Conditions: []store.Condition{cond("mime_type", "contains", "pdf", false)}}
	if !Matches(rMime, doc("x.bin", "", "application/pdf")) {
		t.Fatal("mime_type field should match")
	}
}

func TestMatchesMatchModes(t *testing.T) {
	// Papra's Quarterly Reports example: name contains Q1 OR Q2 OR Q3 OR Q4.
	r := store.Rule{
		MatchMode: "any",
		Conditions: []store.Condition{
			cond("name", "contains", "Q1", false),
			cond("name", "contains", "Q2", false),
			cond("name", "contains", "Q3", false),
			cond("name", "contains", "Q4", false),
		},
	}
	if !Matches(r, doc("Report Q3 final.pdf", "", "")) {
		t.Fatal("any mode should match on Q3")
	}
	if Matches(r, doc("Report annual.pdf", "", "")) {
		t.Fatal("any mode should not match without any quarter")
	}

	allRule := store.Rule{
		MatchMode: "all",
		Conditions: []store.Condition{
			cond("name", "contains", "invoice", false),
			cond("extension", "equal", "pdf", false),
		},
	}
	if !Matches(allRule, doc("invoice march.pdf", "pdf", "")) {
		t.Fatal("all mode should match when every condition holds")
	}
	if Matches(allRule, doc("invoice march.docx", "docx", "")) {
		t.Fatal("all mode should fail when one condition fails")
	}
}

func TestMatchesNoConditionsMatchesEverything(t *testing.T) {
	r := store.Rule{MatchMode: "all"}
	if !Matches(r, doc("anything.txt", "txt", "text/plain")) {
		t.Fatal("rule with no conditions must match every document (Papra semantics)")
	}
}

func TestMatchesUnknownFieldOrOperatorFailsSafely(t *testing.T) {
	badField := store.Rule{MatchMode: "all", Conditions: []store.Condition{cond("content", "contains", "x", false)}}
	if Matches(badField, doc("a.txt", "txt", "text/plain")) {
		t.Fatal("unknown field must not match")
	}
	badOp := store.Rule{MatchMode: "all", Conditions: []store.Condition{cond("name", "regex", "x", false)}}
	if Matches(badOp, doc("a.txt", "txt", "text/plain")) {
		t.Fatal("unknown operator must not match")
	}
	// But in "any" mode a bad condition must not veto a good one.
	mixed := store.Rule{
		MatchMode:  "any",
		Conditions: []store.Condition{cond("content", "contains", "x", false), cond("name", "contains", "a", false)},
	}
	if !Matches(mixed, doc("a.txt", "txt", "text/plain")) {
		t.Fatal("any mode should still match via the valid condition")
	}
}

func TestErrRuleNotFound(t *testing.T) {
	if !errors.Is(ErrRuleNotFound, ErrRuleNotFound) {
		t.Fatal("sentinel should compare to itself")
	}
}
