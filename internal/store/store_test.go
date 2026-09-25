package store

import (
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
