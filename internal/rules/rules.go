// Package rules implements the tagging-rule engine, mirroring Papra's
// server-side semantics (papra-hq/papra, apps/papra-server
// src/modules/tagging-rules):
//
//   - Operators: equal, not_equal, contains, not_contains, starts_with, ends_with
//   - Each condition carries its own case-sensitivity flag.
//   - A rule has a match mode: "all" (default) or "any".
//   - A rule with no conditions matches every document.
//   - A condition that errors (unknown field/operator) evaluates to false and
//     never breaks ingestion.
//   - Actions apply one or more tags; tag application is idempotent.
//   - "Run now" re-applies a rule to every archived document (Papra's
//     "Apply to existing documents") and reports stats.
//
// Fields are Papra's "name" plus cheap Telegram metadata: "extension" and
// "mime_type" (Papra's "content" field needs text extraction and is out of
// scope here).
package rules

import (
	"log"
	"strings"

	"tgarchive/internal/store"
)

// Field getters: map a condition field name to the document property.
func fieldValue(doc store.Document, field string) (string, bool) {
	switch field {
	case "name":
		return doc.FileName, true
	case "extension":
		return doc.Extension, true
	case "mime_type":
		return doc.MimeType, true
	default:
		return "", false
	}
}

// validateOperator implements one Papra operator comparison.
func validateOperator(operator, conditionValue, fieldValue string, caseSensitive bool) (bool, bool) {
	// Second return is false when the operator is unknown -> condition fails
	// (mirrors Papra: a validator error logs and counts as non-match).
	cv, fv := conditionValue, fieldValue
	if !caseSensitive {
		cv, fv = strings.ToLower(cv), strings.ToLower(fv)
	}
	switch operator {
	case "equal":
		return fv == cv, true
	case "not_equal":
		return fv != cv, true
	case "contains":
		return strings.Contains(fv, cv), true
	case "not_contains":
		return !strings.Contains(fv, cv), true
	case "starts_with":
		return strings.HasPrefix(fv, cv), true
	case "ends_with":
		return strings.HasSuffix(fv, cv), true
	default:
		return false, false
	}
}

// Matches reports whether the rule's conditions match the document. A rule
// with no conditions matches everything (Papra semantics).
func Matches(rule store.Rule, doc store.Document) bool {
	if len(rule.Conditions) == 0 {
		return true
	}
	anyMode := rule.MatchMode == "any"
	for _, c := range rule.Conditions {
		ok := false
		if fv, known := fieldValue(doc, c.Field); known {
			// Unknown fields/operators leave ok=false: the condition fails but
			// never panics or aborts the rule (Papra logs and moves on).
			ok, _ = validateOperator(c.Operator, c.Value, fv, c.CaseSensitive)
		}
		if anyMode && ok {
			return true
		}
		if !anyMode && !ok {
			return false
		}
	}
	// "any": nothing matched -> false. "all": nothing failed -> true.
	return !anyMode
}

// Engine applies rules to documents and persists the resulting tags.
type Engine struct {
	Store *store.Store
	Log   *log.Logger
}

// New creates an Engine.
func New(st *store.Store, logger *log.Logger) *Engine {
	if logger == nil {
		logger = log.Default()
	}
	return &Engine{Store: st, Log: logger}
}

// ApplyRulesToDocument runs every enabled rule against one document and
// applies the tags of matching rules. Idempotent. Returns the tag ids that
// were newly added.
func (e *Engine) ApplyRulesToDocument(doc store.Document) ([]int64, error) {
	rules, err := e.Store.EnabledRules()
	if err != nil {
		return nil, err
	}
	var added []int64
	seen := map[int64]bool{}
	for _, r := range rules {
		if !Matches(r, doc) {
			continue
		}
		for _, tagID := range r.TagIDs {
			if seen[tagID] {
				continue
			}
			seen[tagID] = true
			isNew, err := e.Store.AddTagToDocument(doc.ID, tagID)
			if err != nil {
				// A faulty rule must never break ingestion (Papra swallows
				// tag-apply errors too); record and continue.
				e.Log.Printf("rules: apply tag %d to doc %d: %v", tagID, doc.ID, err)
				continue
			}
			if isNew {
				added = append(added, tagID)
			}
		}
	}
	return added, nil
}

// RunStats reports what a "Run now" pass did, like Papra's background task.
type RunStats struct {
	Processed int
	Tagged    int
	Errors    int
}

// RunRuleOverAllDocuments re-applies one rule to every archived document
// (Papra's "Apply to existing documents") and returns stats.
func (e *Engine) RunRuleOverAllDocuments(ruleID int64) (RunStats, error) {
	var target *store.Rule
	all, err := e.Store.ListRules()
	if err != nil {
		return RunStats{}, err
	}
	for i := range all {
		if all[i].ID == ruleID {
			target = &all[i]
			break
		}
	}
	if target == nil {
		return RunStats{}, ErrRuleNotFound
	}
	docs, err := e.Store.AllDocuments()
	if err != nil {
		return RunStats{}, err
	}
	stats := RunStats{}
	for _, doc := range docs {
		stats.Processed++
		if !Matches(*target, doc) {
			continue
		}
		anyNew := false
		errCount := 0
		for _, tagID := range target.TagIDs {
			isNew, err := e.Store.AddTagToDocument(doc.ID, tagID)
			if err != nil {
				errCount++
				e.Log.Printf("rules: run-now tag %d on doc %d: %v", tagID, doc.ID, err)
				continue
			}
			if isNew {
				anyNew = true
			}
		}
		stats.Errors += errCount
		if anyNew {
			stats.Tagged++
		}
	}
	return stats, nil
}

// ErrRuleNotFound is returned by RunRuleOverAllDocuments for unknown rules.
var ErrRuleNotFound = errRuleNotFound{}

type errRuleNotFound struct{}

func (errRuleNotFound) Error() string { return "tagging rule not found" }
