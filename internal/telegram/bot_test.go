package telegram

import (
	"reflect"
	"testing"

	"github.com/go-telegram/bot/models"
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
