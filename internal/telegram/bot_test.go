package telegram

import (
	"context"
	"io"
	"log"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/go-telegram/bot/models"

	"teledoc/internal/rules"
	"teledoc/internal/store"
)

func entity(t models.MessageEntityType, offset, length int) models.MessageEntity {
	return models.MessageEntity{Type: t, Offset: offset, Length: length}
}

// Telegram measures entity offsets/lengths in UTF-16 code units, so these
// cases pin down both the emoji (non-BMP, 2 units per rune) and multibyte-BMP
// (é, 2 UTF-8 bytes but 1 unit) traps that break naive byte or rune slicing.
func TestHashtagEntities(t *testing.T) {
	cases := []struct {
		name    string
		caption string
		ents    []models.MessageEntity
		want    []string
	}{
		{
			name:    "ascii caption",
			caption: "quarterly report #work #finance",
			ents: []models.MessageEntity{
				entity(models.MessageEntityTypeHashtag, 17, 5),
				entity(models.MessageEntityTypeHashtag, 23, 8),
			},
			want: []string{"#work", "#finance"},
		},
		{
			name:    "utf16 offsets with emoji",
			caption: "🎉🎉 party files #fun",
			ents:    []models.MessageEntity{entity(models.MessageEntityTypeHashtag, 17, 4)},
			want:    []string{"#fun"},
		},
		{
			name:    "multibyte bmp char",
			caption: "café #work",
			ents:    []models.MessageEntity{entity(models.MessageEntityTypeHashtag, 5, 5)},
			want:    []string{"#work"},
		},
		{
			name:    "case variants stay distinct",
			caption: "#Work #work #WORK",
			ents: []models.MessageEntity{
				entity(models.MessageEntityTypeHashtag, 0, 5),
				entity(models.MessageEntityTypeHashtag, 6, 5),
				entity(models.MessageEntityTypeHashtag, 12, 5),
			},
			want: []string{"#Work", "#work", "#WORK"},
		},
		{
			name:    "exact duplicates collapse",
			caption: "#fun and #fun again",
			ents: []models.MessageEntity{
				entity(models.MessageEntityTypeHashtag, 0, 4),
				entity(models.MessageEntityTypeHashtag, 9, 4),
			},
			want: []string{"#fun"},
		},
		{
			name:    "non-hashtag entities ignored",
			caption: "see example.com #tag",
			ents: []models.MessageEntity{
				entity(models.MessageEntityTypeURL, 4, 11),
				entity(models.MessageEntityTypeHashtag, 16, 4),
			},
			want: []string{"#tag"},
		},
		{
			name:    "no entities means no hashtags",
			caption: "no tags here",
			want:    nil,
		},
		{
			name:    "empty caption",
			caption: "",
			ents:    []models.MessageEntity{entity(models.MessageEntityTypeHashtag, 0, 3)},
			want:    nil,
		},
		{
			name:    "malformed entities are skipped",
			caption: "#ok",
			ents: []models.MessageEntity{
				entity(models.MessageEntityTypeHashtag, 0, 3),  // valid
				entity(models.MessageEntityTypeHashtag, 50, 5), // past end
				entity(models.MessageEntityTypeHashtag, -2, 4), // negative offset
				entity(models.MessageEntityTypeHashtag, 1, 0),  // empty span
			},
			want: []string{"#ok"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := hashtagEntities(tc.caption, tc.ents)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// Feature 5: editing a document post's caption re-runs hashtag tagging on
// the archived document. Tagging is strictly add-only — an edit can never
// remove a tag — and edits of documents that are not in the archive are
// ignored rather than resurrecting them.
func TestHandleEditCaptionRetags(t *testing.T) {
	st, err := store.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	b := &Bot{
		store:     st,
		engine:    rules.New(st, nil),
		log:       log.New(io.Discard, "", 0),
		usernames: map[int64]string{},
	}
	// Username set so ingest's message-link builder never calls the Telegram API.
	chat := models.Chat{ID: -100, Username: "testchannel"}

	// Upload with a captioned hashtag.
	msg := models.Message{
		ID:              7,
		Chat:            chat,
		Document:        &models.Document{FileName: "a.pdf", FileSize: 10},
		Caption:         "report #work",
		CaptionEntities: []models.MessageEntity{entity(models.MessageEntityTypeHashtag, 7, 5)},
	}
	status, err := b.ingest(context.Background(), &msg)
	if err != nil || status != ingestArchived {
		t.Fatalf("ingest: status=%v err=%v", status, err)
	}
	doc, err := st.DocumentByMessage(-100, 7)
	if err != nil {
		t.Fatal(err)
	}

	tagNames := func() map[string]bool {
		t.Helper()
		tags, err := st.DocumentTags(doc.ID)
		if err != nil {
			t.Fatal(err)
		}
		names := map[string]bool{}
		for _, tg := range tags {
			names[tg.Name] = true
		}
		return names
	}
	if got := tagNames(); !reflect.DeepEqual(got, map[string]bool{"work": true}) {
		t.Fatalf("after upload: got %v", got)
	}

	// Edit adds a second hashtag; the first stays applied.
	msg.Caption = "quarterly report #work #finance"
	msg.CaptionEntities = []models.MessageEntity{
		entity(models.MessageEntityTypeHashtag, 17, 5),
		entity(models.MessageEntityTypeHashtag, 23, 8),
	}
	b.handleEdit(context.Background(), &msg)
	if got := tagNames(); !reflect.DeepEqual(got, map[string]bool{"work": true, "finance": true}) {
		t.Fatalf("after edit: got %v", got)
	}

	// Edit that removes the hashtags: tags are add-only and must remain.
	msg.Caption = "quarterly report"
	msg.CaptionEntities = nil
	b.handleEdit(context.Background(), &msg)
	if got := tagNames(); !reflect.DeepEqual(got, map[string]bool{"work": true, "finance": true}) {
		t.Fatalf("after hashtag-removing edit: got %v", got)
	}

	// An edit for a document that is not archived is ignored: no new tag.
	other := models.Message{
		ID:              9,
		Chat:            chat,
		Document:        &models.Document{FileName: "never-archived.pdf"},
		Caption:         "#nope",
		CaptionEntities: []models.MessageEntity{entity(models.MessageEntityTypeHashtag, 0, 5)},
	}
	b.handleEdit(context.Background(), &other)
	all, err := st.ListTags()
	if err != nil {
		t.Fatal(err)
	}
	for _, tg := range all {
		if tg.Name == "nope" {
			t.Fatalf("edit of unarchived document was not ignored: tag %q exists", tg.Name)
		}
	}
}
