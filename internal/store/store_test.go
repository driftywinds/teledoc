package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestHasDocumentWithFileName(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	mk := func(name string, msgID int) {
		t.Helper()
		_, created, err := st.UpsertDocument(Document{
			FileName: name, ChatID: -100, MessageID: msgID, MessageLink: "l", UploadedAt: time.Now(),
		})
		if err != nil || !created {
			t.Fatalf("upsert %q: created=%v err=%v", name, created, err)
		}
	}

	// "Report.PDF" arrives as message 1, "notes.txt" as message 2.
	mk("Report.PDF", 1)
	mk("notes.txt", 2)

	// Reupload under a fresh message id -> duplicate.
	if dup, err := st.HasDocumentWithFileName(-100, "Report.PDF", 3); err != nil || !dup {
		t.Fatalf("expected duplicate for reuploaded name, got dup=%v err=%v", dup, err)
	}
	// Same file name, different case -> still a duplicate.
	if dup, err := st.HasDocumentWithFileName(-100, "report.pdf", 3); err != nil || !dup {
		t.Fatalf("expected case-insensitive duplicate, got dup=%v err=%v", dup, err)
	}
	// Checking the message that itself carries the name must not count.
	if dup, err := st.HasDocumentWithFileName(-100, "notes.txt", 2); err != nil || dup {
		t.Fatalf("expected no duplicate when excluding own message, got dup=%v err=%v", dup, err)
	}
	// Distinct file name -> no duplicate.
	if dup, err := st.HasDocumentWithFileName(-100, "other.doc", 3); err != nil || dup {
		t.Fatalf("expected no duplicate for distinct name, got dup=%v err=%v", dup, err)
	}
	// Same name in a different chat -> no duplicate.
	if dup, err := st.HasDocumentWithFileName(-200, "Report.PDF", 3); err != nil || dup {
		t.Fatalf("expected no duplicate across chats, got dup=%v err=%v", dup, err)
	}
}

// EnsureTag backs caption-hashtag tagging: an existing tag must win over
// creation regardless of the casing it was requested in, and a brand-new tag
// is stored lowercased so the tag list stays canonical.
func TestEnsureTagCaseInsensitive(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	created, err := st.CreateTag("Work")
	if err != nil {
		t.Fatal(err)
	}

	// Any casing of an existing tag resolves to that tag.
	for _, name := range []string{"work", "WORK", "WoRk"} {
		id, err := st.EnsureTag(name)
		if err != nil {
			t.Fatalf("EnsureTag(%q): %v", name, err)
		}
		if id != created.ID {
			t.Fatalf("EnsureTag(%q) = %d, want existing tag %d", name, id, created.ID)
		}
	}

	// A new tag is created lowercased and stays stable across calls.
	id1, err := st.EnsureTag("Invoices")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := st.EnsureTag("INVOICES")
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Fatalf("EnsureTag not stable: %d vs %d", id1, id2)
	}
	var name string
	if err := st.db.QueryRow(`SELECT name FROM tags WHERE id = ?`, id1).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "invoices" {
		t.Fatalf("new tag stored as %q, want %q", name, "invoices")
	}
}

// DocumentByMessage resolves (chat_id, message_id) -> archived document and
// must surface sql.ErrNoRows for messages that are not archived (feature 5
// relies on that sentinel to ignore edits of unarchived documents).
func TestDocumentByMessage(t *testing.T) {
	st, err := New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	id, created, err := st.UpsertDocument(Document{
		FileName: "a.pdf", ChatID: -100, MessageID: 5, MessageLink: "l", UploadedAt: time.Now(),
	})
	if err != nil || !created {
		t.Fatalf("upsert: created=%v err=%v", created, err)
	}

	got, err := st.DocumentByMessage(-100, 5)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != id || got.FileName != "a.pdf" || got.ChatID != -100 || got.MessageID != 5 {
		t.Fatalf("got %+v", got)
	}
	if got.UploadedAt.IsZero() {
		t.Fatal("uploaded_at not populated")
	}

	if _, err := st.DocumentByMessage(-100, 6); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected sql.ErrNoRows for unarchived message, got %v", err)
	}
}
