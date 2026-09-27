package telegram

import (
	"context"
	"io"
	"log"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"

	"teledoc/internal/rules"
	"teledoc/internal/store"
)

func newIngestTestBot(t *testing.T) *Bot {
	t.Helper()
	st, err := store.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &Bot{
		store:     st,
		engine:    rules.New(st, nil),
		log:       log.New(io.Discard, "", 0),
		usernames: map[int64]string{},
	}
}

func ingestDoc(t *testing.T, b *Bot, chatID int64, msgID int, name string, size int64, fileID, uniqueID string) (store.Document, bool) {
	t.Helper()
	b.usernames[chatID] = "testchannel" // message-link builder never hits the API
	msg := models.Message{
		ID:  msgID,
		Chat: models.Chat{ID: chatID, Username: "testchannel"},
		Document: &models.Document{
			FileName:     name,
			FileSize:     size,
			FileID:       fileID,
			FileUniqueID: uniqueID,
		},
	}
	status, err := b.ingest(context.Background(), &msg)
	// A re-send of a file name already archived is flagged as a duplicate
	// (the 👀 reaction) but is still archived and healed like any delivery.
	if err != nil || (status != ingestArchived && status != ingestDuplicateFileName) {
		t.Fatalf("ingest: status=%v err=%v", status, err)
	}
	doc, err := b.store.DocumentByMessage(chatID, msgID)
	if err != nil {
		t.Fatal(err)
	}
	return doc, true
}

// Regression for the mass re-forward incident: builds before the temp-download
// feature ingested documents WITHOUT storing Telegram's file_id, and the first
// cut of the feature repeated that bug. Telegram file ids only reach the bot
// through deliveries, so the fix stores the id on every ingest AND backfills
// older rows of the same file (same chat, same name, same size) — so one
// re-send heals the pre-migration row and the bugged duplicate row together.
func TestIngestStoresFileID(t *testing.T) {
	b := newIngestTestBot(t)
	doc, _ := ingestDoc(t, b, -100, 1, "manual.pdf", 1234, "FID-1", "UNIQ-1")
	if doc.FileID != "FID-1" {
		t.Fatalf("ingest did not store the telegram file id: %q", doc.FileID)
	}
	if doc.FileUniqueID != "UNIQ-1" {
		t.Fatalf("ingest did not store the file unique id: %q", doc.FileUniqueID)
	}
}

// A legacy row (archived before file ids were stored, file id empty) is
// healed when the same file is delivered again — and the healing must not
// touch rows that already have an id, nor rows of other chats / other names /
// other sizes.
func TestIngestBackfillsLegacyRowFileID(t *testing.T) {
	b := newIngestTestBot(t)

	// Seed a legacy row the way the old build would have: no file id.
	legacy, _, err := b.store.UpsertDocument(store.Document{
		FileName: "report.pdf", Extension: "pdf", FileSize: 500,
		ChatID: -100, MessageID: 10, MessageLink: "l", UploadedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// A different file that must NOT be healed by the delivery below.
	other, _, err := b.store.UpsertDocument(store.Document{
		FileName: "other.pdf", Extension: "pdf", FileSize: 500,
		ChatID: -100, MessageID: 11, MessageLink: "l", UploadedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Same name, but a different size: must not match either.
	wrongSize, _, err := b.store.UpsertDocument(store.Document{
		FileName: "report.pdf", Extension: "pdf", FileSize: 999,
		ChatID: -100, MessageID: 12, MessageLink: "l", UploadedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// The user re-sends the file; the fresh delivery carries a new file id.
	ingestDoc(t, b, -100, 20, "report.pdf", 500, "FID-new", "UNIQ-new")

	legacyDoc, err := b.store.DocumentByID(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if legacyDoc.FileID != "FID-new" {
		t.Fatalf("legacy row was not healed: file id %q", legacyDoc.FileID)
	}
	if legacyDoc.FileUniqueID != "UNIQ-new" {
		t.Fatalf("legacy row unique id not healed: %q", legacyDoc.FileUniqueID)
	}

	otherDoc, err := b.store.DocumentByID(other)
	if err != nil {
		t.Fatal(err)
	}
	if otherDoc.FileID != "" {
		t.Fatalf("unrelated row must not be healed, got %q", otherDoc.FileID)
	}
	wrongSizeDoc, err := b.store.DocumentByID(wrongSize)
	if err != nil {
		t.Fatal(err)
	}
	if wrongSizeDoc.FileID != "" {
		t.Fatalf("wrong-size row must not be healed, got %q", wrongSizeDoc.FileID)
	}
}

// The backfill never overwrites a live file id: a delivery of a file whose
// name+size matches an already-healed row leaves that row's id untouched.
func TestIngestBackfillNeverOverwrites(t *testing.T) {
	b := newIngestTestBot(t)

	// First delivery stores its id...
	ingestDoc(t, b, -100, 30, "doc.pdf", 100, "FID-first", "UNIQ-first")

	// ...a second delivery of the same name+size (e.g. a re-upload) creates a
	// second row with its own id; the first row's id must remain FID-first.
	ingestDoc(t, b, -100, 31, "doc.pdf", 100, "FID-second", "UNIQ-second")

	first, err := b.store.DocumentByMessage(-100, 30)
	if err != nil {
		t.Fatal(err)
	}
	if first.FileID != "FID-first" {
		t.Fatalf("live file id was overwritten: %q", first.FileID)
	}
	second, err := b.store.DocumentByMessage(-100, 31)
	if err != nil {
		t.Fatal(err)
	}
	if second.FileID != "FID-second" {
		t.Fatalf("new row file id missing: %q", second.FileID)
	}
}

// A duplicate delivery of the same message (bot restart overlap) also runs
// the backfill, so a row the buggy build left empty gets healed even without
// a fresh message id.
func TestRedeliveryBackfillsOwnRow(t *testing.T) {
	b := newIngestTestBot(t)

	// Buggy-build row: file id empty despite the delivery carrying one.
	broken, _, err := b.store.UpsertDocument(store.Document{
		FileName: "again.pdf", Extension: "pdf", FileSize: 77,
		ChatID: -100, MessageID: 40, MessageLink: "l", UploadedAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// The same message is delivered again (idempotent redelivery path).
	b.usernames[-100] = "testchannel"
	msg := models.Message{
		ID:       40,
		Chat:     models.Chat{ID: -100, Username: "testchannel"},
		Document: &models.Document{FileName: "again.pdf", FileSize: 77, FileID: "FID-again", FileUniqueID: "UNIQ-again"},
	}
	status, err := b.ingest(context.Background(), &msg)
	if err != nil || status != ingestAlreadyArchived {
		t.Fatalf("redelivery: status=%v err=%v", status, err)
	}

	doc, err := b.store.DocumentByID(broken)
	if err != nil {
		t.Fatal(err)
	}
	if doc.FileID != "FID-again" {
		t.Fatalf("redelivery did not heal the row: %q", doc.FileID)
	}
}
