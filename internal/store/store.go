// Package store implements the SQLite persistence layer: documents ingested
// from the Telegram channel, tags, and Papra-style tagging rules.
package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

// Document is one file posted in the Telegram channel. File bytes stay in
// Telegram; we only keep metadata plus the message link.
type Document struct {
	ID          int64
	FileName    string
	MimeType    string
	Extension   string
	FileSize    int64
	ChatID      int64
	MessageID   int
	MessageLink string
	UploadedAt  time.Time
}

// DocumentWithTags is a document plus its tags (for rendering).
type DocumentWithTags struct {
	Document
	Tags []Tag
}

// Tag is a colored label that can be attached to documents.
type Tag struct {
	ID    int64
	Name  string
	Color string
	Count int64 // number of documents carrying this tag (only set by ListTags)
}

// Condition is one rule condition, mirroring Papra's model:
// field + operator + value + per-condition case sensitivity.
type Condition struct {
	Field         string // "name" | "extension" | "mime_type"
	Operator      string // equal | not_equal | contains | not_contains | starts_with | ends_with
	Value         string
	CaseSensitive bool
}

// Rule is a tagging rule: a set of conditions and the tags to apply on match.
type Rule struct {
	ID         int64
	Name       string
	MatchMode  string // "all" | "any"
	Enabled    bool
	Conditions []Condition
	TagIDs     []int64
}

const schema = `
CREATE TABLE IF NOT EXISTS documents (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	file_name   TEXT NOT NULL,
	mime_type   TEXT NOT NULL DEFAULT '',
	extension   TEXT NOT NULL DEFAULT '',
	file_size   INTEGER NOT NULL DEFAULT 0,
	chat_id     INTEGER NOT NULL,
	message_id  INTEGER NOT NULL,
	message_link TEXT NOT NULL,
	uploaded_at INTEGER NOT NULL,
	UNIQUE(chat_id, message_id)
);
CREATE INDEX IF NOT EXISTS idx_documents_uploaded ON documents(uploaded_at DESC);

CREATE TABLE IF NOT EXISTS tags (
	id    INTEGER PRIMARY KEY AUTOINCREMENT,
	name  TEXT NOT NULL UNIQUE,
	color TEXT NOT NULL DEFAULT '#8b5cf6'
);

CREATE TABLE IF NOT EXISTS document_tags (
	document_id INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
	tag_id      INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
	PRIMARY KEY (document_id, tag_id)
);

CREATE TABLE IF NOT EXISTS rules (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	name       TEXT NOT NULL,
	match_mode TEXT NOT NULL DEFAULT 'all' CHECK (match_mode IN ('all','any')),
	enabled    INTEGER NOT NULL DEFAULT 1,
	created_at INTEGER NOT NULL DEFAULT (unixepoch())
);

CREATE TABLE IF NOT EXISTS rule_conditions (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	rule_id        INTEGER NOT NULL REFERENCES rules(id) ON DELETE CASCADE,
	field          TEXT NOT NULL,
	operator       TEXT NOT NULL,
	value          TEXT NOT NULL,
	case_sensitive INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS rule_actions (
	rule_id INTEGER NOT NULL REFERENCES rules(id) ON DELETE CASCADE,
	tag_id  INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
	PRIMARY KEY (rule_id, tag_id)
);
`

// New opens (creating if needed) the SQLite database at path and runs the
// schema. The DSN enables WAL, a busy timeout and foreign keys.
func New(path string) (*Store, error) {
	dsn := fmt.Sprintf("%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite is happiest with a single writer connection.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// ---------------------------------------------------------------------------
// Documents

// UpsertDocument inserts the document if the (chat_id, message_id) pair is
// new and returns its id plus whether it was newly created.
func (s *Store) UpsertDocument(d Document) (int64, bool, error) {
	res, err := s.db.Exec(`
		INSERT OR IGNORE INTO documents
			(file_name, mime_type, extension, file_size, chat_id, message_id, message_link, uploaded_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		d.FileName, d.MimeType, d.Extension, d.FileSize, d.ChatID, d.MessageID, d.MessageLink, d.UploadedAt.Unix())
	if err != nil {
		return 0, false, fmt.Errorf("upsert document: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		id, err := s.lastInsertID(res)
		return id, true, err
	}
	// Already archived.
	var id int64
	err = s.db.QueryRow(`SELECT id FROM documents WHERE chat_id = ? AND message_id = ?`, d.ChatID, d.MessageID).Scan(&id)
	return id, false, err
}

// HasDocumentWithFileName reports whether a document with the same file name
// (case-insensitive) is already archived from the given chat by a DIFFERENT
// message (excludeMessageID) — used to flag duplicate-filename reuploads.
func (s *Store) HasDocumentWithFileName(chatID int64, fileName string, excludeMessageID int) (bool, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM documents WHERE chat_id = ? AND file_name COLLATE NOCASE = ? AND message_id != ?`,
		chatID, fileName, excludeMessageID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("check duplicate file name: %w", err)
	}
	return n > 0, nil
}

func (s *Store) lastInsertID(res sql.Result) (int64, error) {
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("last insert id: %w", err)
	}
	return id, nil
}

// DeleteDocument removes a document's metadata from the archive. Its tag links
// cascade away via the foreign key. The file on Telegram is NOT deleted — this
// only removes the entry from the app's database.
func (s *Store) DeleteDocument(id int64) error {
	_, err := s.db.Exec(`DELETE FROM documents WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete document: %w", err)
	}
	return nil
} // DocumentFilter selects which documents ListDocuments returns.
type DocumentFilter struct {
	Search   string  // substring match on file name
	TagIDs   []int64 // documents having ANY of these tags
	Untagged bool    // only documents with no tags
	SortBy   string  // "name" | "type" | "date" (default) | "size"
	SortDir  string  // "asc" | "desc" (default)
	Limit    int     // page size; <=0 means no explicit limit (use a sane default at call sites)
	Offset   int     // rows to skip for pagination
}

// buildDocumentWhere assembles the WHERE clause shared by listing and counting.
func buildDocumentWhere(f DocumentFilter) (string, []any) {
	where := []string{"1=1"}
	args := []any{}
	if f.Search != "" {
		where = append(where, "file_name LIKE ? ESCAPE '\\'")
		args = append(args, "%"+likeEscape(f.Search)+"%")
	}
	if len(f.TagIDs) > 0 {
		ph := placeholders(len(f.TagIDs))
		where = append(where, fmt.Sprintf(
			"id IN (SELECT document_id FROM document_tags WHERE tag_id IN (%s))", ph))
		for _, id := range f.TagIDs {
			args = append(args, id)
		}
	}
	if f.Untagged {
		where = append(where, "id NOT IN (SELECT document_id FROM document_tags)")
	}
	return strings.Join(where, " AND "), args
}

// documentOrderBy maps a sort request to a safe ORDER BY expression
// (white-listed; never interpolated from user input).
func documentOrderBy(sortBy, sortDir string) string {
	col := "uploaded_at"
	switch sortBy {
	case "name":
		col = "file_name COLLATE NOCASE" // A-Z case-insensitively
	case "type":
		col = "extension COLLATE NOCASE"
	case "size":
		col = "file_size"
	}
	dir := "DESC"
	if sortDir == "asc" {
		dir = "ASC"
	}
	return col + " " + dir + ", id " + dir // stable tie-breaker
}

// ListDocuments returns documents matching the filter, sorted and paged,
// each with its tags.
func (s *Store) ListDocuments(f DocumentFilter) ([]DocumentWithTags, error) {
	where, args := buildDocumentWhere(f)

	limit := f.Limit
	if limit <= 0 {
		limit = 20
	}
	if f.Offset < 0 {
		f.Offset = 0
	}
	args = append(args, limit, f.Offset)

	rows, err := s.db.Query(`
		SELECT id, file_name, mime_type, extension, file_size, chat_id, message_id, message_link, uploaded_at
		FROM documents
		WHERE `+where+`
		ORDER BY `+documentOrderBy(f.SortBy, f.SortDir)+`
		LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("list documents: %w", err)
	}
	defer rows.Close()

	docs := []DocumentWithTags{}
	for rows.Next() {
		var d DocumentWithTags
		var ts int64
		if err := rows.Scan(&d.ID, &d.FileName, &d.MimeType, &d.Extension, &d.FileSize,
			&d.ChatID, &d.MessageID, &d.MessageLink, &ts); err != nil {
			return nil, err
		}
		d.UploadedAt = time.Unix(ts, 0)
		docs = append(docs, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.loadTagsForDocs(docs); err != nil {
		return nil, err
	}
	return docs, nil
}

// CountDocumentsFiltered counts documents matching the filter (for pagination).
func (s *Store) CountDocumentsFiltered(f DocumentFilter) (int, error) {
	where, args := buildDocumentWhere(f)
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM documents WHERE `+where, args...).Scan(&n)
	return n, err
}

func (s *Store) loadTagsForDocs(docs []DocumentWithTags) error {
	for i := range docs {
		tags, err := s.DocumentTags(docs[i].ID)
		if err != nil {
			return err
		}
		docs[i].Tags = tags
	}
	return nil
}

// DocumentTags returns the tags attached to one document.
func (s *Store) DocumentTags(docID int64) ([]Tag, error) {
	rows, err := s.db.Query(`
		SELECT t.id, t.name, t.color FROM tags t
		JOIN document_tags dt ON dt.tag_id = t.id
		WHERE dt.document_id = ? ORDER BY t.name`, docID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTags(rows)
}

// AllDocuments streams every document (id + metadata needed by the rule
// engine) for re-tagging runs.
func (s *Store) AllDocuments() ([]Document, error) {
	rows, err := s.db.Query(`
		SELECT id, file_name, mime_type, extension, file_size, chat_id, message_id, message_link, uploaded_at
		FROM documents ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var docs []Document
	for rows.Next() {
		var d Document
		var ts int64
		if err := rows.Scan(&d.ID, &d.FileName, &d.MimeType, &d.Extension, &d.FileSize,
			&d.ChatID, &d.MessageID, &d.MessageLink, &ts); err != nil {
			return nil, err
		}
		d.UploadedAt = time.Unix(ts, 0)
		docs = append(docs, d)
	}
	return docs, rows.Err()
}

func likeEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// ---------------------------------------------------------------------------
// Tags

var tagColors = []string{
	"#8b5cf6", "#f59e0b", "#10b981", "#3b82f6", "#ef4444",
	"#14b8a6", "#f97316", "#6366f1", "#ec4899", "#84cc16",
}

// CreateTag adds a tag; the name is unique. Colors cycle through a palette.
func (s *Store) CreateTag(name string) (Tag, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Tag{}, fmt.Errorf("tag name is empty")
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM tags`).Scan(&count); err != nil {
		return Tag{}, err
	}
	color := tagColors[count%len(tagColors)]
	res, err := s.db.Exec(`INSERT OR IGNORE INTO tags (name, color) VALUES (?, ?)`, name, color)
	if err != nil {
		return Tag{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 { // existed already
		row := s.db.QueryRow(`SELECT id, name, color FROM tags WHERE name = ?`, name)
		var t Tag
		return t, row.Scan(&t.ID, &t.Name, &t.Color)
	}
	id, err := s.lastInsertID(res)
	return Tag{ID: id, Name: name, Color: color}, err
}

// DeleteTag removes a tag and its document/rule links (cascades).
func (s *Store) DeleteTag(id int64) error {
	_, err := s.db.Exec(`DELETE FROM tags WHERE id = ?`, id)
	return err
}

// SetTagColor updates the color of a tag. It accepts any non-empty hex-ish
// value; validation and normalization happen at the handler layer.
func (s *Store) SetTagColor(id int64, color string) error {
	if color == "" {
		return fmt.Errorf("tag color is empty")
	}
	_, err := s.db.Exec(`UPDATE tags SET color = ? WHERE id = ?`, color, id)
	return err
}

// ListTags returns all tags with their document counts.
func (s *Store) ListTags() ([]Tag, error) {
	rows, err := s.db.Query(`
		SELECT t.id, t.name, t.color, COUNT(dt.document_id)
		FROM tags t LEFT JOIN document_tags dt ON dt.tag_id = t.id
		GROUP BY t.id ORDER BY t.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tags := []Tag{}
	for rows.Next() {
		var t Tag
		if err := rows.Scan(&t.ID, &t.Name, &t.Color, &t.Count); err != nil {
			return nil, err
		}
		tags = append(tags, t)
	}
	return tags, rows.Err()
}

// EnsureTag returns the id of the tag with the given name, reusing an
// existing tag when one matches case-insensitively (e.g. "Work" is returned
// for a requested "work") and creating a new tag otherwise.
func (s *Store) EnsureTag(name string) (int64, error) {
	var id int64
	err := s.db.QueryRow(`SELECT id FROM tags WHERE name COLLATE NOCASE = ?`, name).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return 0, fmt.Errorf("find tag: %w", err)
	}
	t, err := s.CreateTag(strings.ToLower(name))
	if err != nil {
		return 0, err
	}
	return t.ID, nil
}

// AddTagToDocument links a tag to a document; idempotent. Returns whether a
// new link was created.
func (s *Store) AddTagToDocument(docID, tagID int64) (bool, error) {
	res, err := s.db.Exec(`INSERT OR IGNORE INTO document_tags (document_id, tag_id) VALUES (?, ?)`, docID, tagID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// RemoveTagFromDocument unlinks a tag from a document.
func (s *Store) RemoveTagFromDocument(docID, tagID int64) error {
	_, err := s.db.Exec(`DELETE FROM document_tags WHERE document_id = ? AND tag_id = ?`, docID, tagID)
	return err
}

// ToggleTagsOnDocuments flips tag membership for every (doc, tag) pair in one
// transaction — the "Tag selected" bulk action. A tag is removed from docs
// that have it and added to docs that don't, per document: a doc holding some
// but not all of the chosen tags gains the missing ones and keeps the rest.
// Tags not in tagIDs are untouched. Returns added/removed link counts for the
// confirmation flash.
func (s *Store) ToggleTagsOnDocuments(docIDs, tagIDs []int64) (added, removed int, err error) {
	if len(docIDs) == 0 || len(tagIDs) == 0 {
		return 0, 0, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, fmt.Errorf("toggle tags: %w", err)
	}
	defer tx.Rollback()

	// Existing links between the selected docs and the selected tags.
	args := []any{}
	for _, id := range docIDs {
		args = append(args, id)
	}
	for _, id := range tagIDs {
		args = append(args, id)
	}
	rows, err := tx.Query(
		`SELECT document_id, tag_id FROM document_tags
		 WHERE document_id IN (`+placeholders(len(docIDs))+`)
		   AND tag_id IN (`+placeholders(len(tagIDs))+`)`, args...)
	if err != nil {
		return 0, 0, fmt.Errorf("toggle tags: %w", err)
	}
	has := map[[2]int64]bool{}
	for rows.Next() {
		var d, t int64
		if err := rows.Scan(&d, &t); err != nil {
			rows.Close()
			return 0, 0, fmt.Errorf("toggle tags: %w", err)
		}
		has[[2]int64{d, t}] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, 0, fmt.Errorf("toggle tags: %w", err)
	}
	rows.Close()

	// Flip each pair: present -> remove, absent -> add.
	for _, docID := range docIDs {
		for _, tagID := range tagIDs {
			if has[[2]int64{docID, tagID}] {
				if _, err := tx.Exec(`DELETE FROM document_tags WHERE document_id = ? AND tag_id = ?`, docID, tagID); err != nil {
					return 0, 0, fmt.Errorf("toggle tags: %w", err)
				}
				removed++
			} else {
				if _, err := tx.Exec(`INSERT INTO document_tags (document_id, tag_id) VALUES (?, ?)`, docID, tagID); err != nil {
					return 0, 0, fmt.Errorf("toggle tags: %w", err)
				}
				added++
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("toggle tags: %w", err)
	}
	return added, removed, nil
}

// ---------------------------------------------------------------------------
// Rules

// CreateRule stores a rule with its conditions and tag actions.
func (s *Store) CreateRule(name, matchMode string, enabled bool, conds []Condition, tagIDs []int64) (int64, error) {
	if matchMode != "any" {
		matchMode = "all"
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO rules (name, match_mode, enabled) VALUES (?, ?, ?)`, name, matchMode, boolInt(enabled))
	if err != nil {
		return 0, err
	}
	ruleID, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	for _, c := range conds {
		if _, err := tx.Exec(`INSERT INTO rule_conditions (rule_id, field, operator, value, case_sensitive) VALUES (?, ?, ?, ?, ?)`,
			ruleID, c.Field, c.Operator, c.Value, boolInt(c.CaseSensitive)); err != nil {
			return 0, err
		}
	}
	for _, tagID := range tagIDs {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO rule_actions (rule_id, tag_id) VALUES (?, ?)`, ruleID, tagID); err != nil {
			return 0, err
		}
	}
	return ruleID, tx.Commit()
}

// DeleteRule removes a rule (conditions and actions cascade).
func (s *Store) DeleteRule(id int64) error {
	_, err := s.db.Exec(`DELETE FROM rules WHERE id = ?`, id)
	return err
}

// SetRuleEnabled toggles a rule.
func (s *Store) SetRuleEnabled(id int64, enabled bool) error {
	_, err := s.db.Exec(`UPDATE rules SET enabled = ? WHERE id = ?`, boolInt(enabled), id)
	return err
}

// ListRules returns all rules with conditions and actions.
func (s *Store) ListRules() ([]Rule, error) {
	rows, err := s.db.Query(`SELECT id, name, match_mode, enabled FROM rules ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	rules := []Rule{}
	for rows.Next() {
		var r Rule
		var enabled int
		if err := rows.Scan(&r.ID, &r.Name, &r.MatchMode, &enabled); err != nil {
			return nil, err
		}
		r.Enabled = enabled != 0
		rules = append(rules, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range rules {
		conds, err := s.ruleConditions(rules[i].ID)
		if err != nil {
			return nil, err
		}
		rules[i].Conditions = conds
		tagIDs, err := s.ruleTagIDs(rules[i].ID)
		if err != nil {
			return nil, err
		}
		rules[i].TagIDs = tagIDs
	}
	return rules, nil
}

// EnabledRules returns all enabled rules (what the ingestion pipeline applies).
func (s *Store) EnabledRules() ([]Rule, error) {
	all, err := s.ListRules()
	if err != nil {
		return nil, err
	}
	enabled := []Rule{}
	for _, r := range all {
		if r.Enabled {
			enabled = append(enabled, r)
		}
	}
	return enabled, nil
}

func (s *Store) ruleConditions(ruleID int64) ([]Condition, error) {
	rows, err := s.db.Query(
		`SELECT field, operator, value, case_sensitive FROM rule_conditions WHERE rule_id = ? ORDER BY id`, ruleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	conds := []Condition{}
	for rows.Next() {
		var c Condition
		var cs int
		if err := rows.Scan(&c.Field, &c.Operator, &c.Value, &cs); err != nil {
			return nil, err
		}
		c.CaseSensitive = cs != 0
		conds = append(conds, c)
	}
	return conds, rows.Err()
}

func (s *Store) ruleTagIDs(ruleID int64) ([]int64, error) {
	rows, err := s.db.Query(`SELECT tag_id FROM rule_actions WHERE rule_id = ?`, ruleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// CountDocuments returns the total number of archived documents.
func (s *Store) CountDocuments() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM documents`).Scan(&n)
	return n, err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func scanTags(rows *sql.Rows) ([]Tag, error) {
	tags := []Tag{}
	for rows.Next() {
		var t Tag
		if err := rows.Scan(&t.ID, &t.Name, &t.Color); err != nil {
			return nil, err
		}
		tags = append(tags, t)
	}
	return tags, rows.Err()
}
