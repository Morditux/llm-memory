package store

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Kind is the type of a memory.
type Kind string

// Memory kinds.
const (
	KindPreference Kind = "preference"
	KindFact       Kind = "fact"
	KindDecision   Kind = "decision"
	KindTaskState  Kind = "task_state"
	KindHandoff    Kind = "handoff"
	KindNote       Kind = "note"
)

// Kinds lists every valid kind, in display order.
var Kinds = []Kind{KindPreference, KindFact, KindDecision, KindTaskState, KindHandoff, KindNote}

// Valid reports whether k is a known kind.
func (k Kind) Valid() bool {
	for _, v := range Kinds {
		if k == v {
			return true
		}
	}
	return false
}

// Limits enforced before reaching SQLite.
const (
	MaxContentRunes   = 8000
	MaxTitleRunes     = 200
	MaxNamespaceLen   = 64
	DefaultNamespace  = "global"
	DefaultImportance = 2
	UnknownSource     = "unknown"
)

// Memory is one stored memory.
type Memory struct {
	ID         int64      `json:"id"`
	Namespace  string     `json:"namespace"`
	Kind       Kind       `json:"kind"`
	Title      string     `json:"title"`
	Content    string     `json:"content"`
	Tags       []string   `json:"tags"`
	Importance int        `json:"importance"`
	Source     string     `json:"source"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	UpdatedBy  string     `json:"updated_by"`
	ArchivedAt *time.Time `json:"archived_at,omitempty"`
}

// Archived reports whether the memory is archived.
func (m Memory) Archived() bool { return m.ArchivedAt != nil }

// Action is a history action.
type Action string

// History actions.
const (
	ActionCreate  Action = "create"
	ActionUpdate  Action = "update"
	ActionArchive Action = "archive"
	ActionRestore Action = "restore"
)

// HistoryEntry is one line of memory_history.
type HistoryEntry struct {
	ID        int64     `json:"id"`
	MemoryID  int64     `json:"memory_id"`
	Action    Action    `json:"action"`
	Snapshot  string    `json:"snapshot"` // JSON of the memory after the action
	ChangedBy string    `json:"changed_by"`
	ChangedAt time.Time `json:"changed_at"`
}

// Namespace describes a namespace and its activity.
type Namespace struct {
	Name         string     `json:"name"`
	Description  string     `json:"description"`
	CreatedAt    time.Time  `json:"created_at"`
	ActiveCount  int        `json:"active_count"`
	LastActivity *time.Time `json:"last_activity,omitempty"`
}

// ---- Errors -----------------------------------------------------------------

// ErrNotFound is returned when a memory id does not exist.
var ErrNotFound = errors.New("memory not found")

// NotFoundError carries the missing id; it matches ErrNotFound with errors.Is.
type NotFoundError struct{ ID int64 }

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("memory #%d does not exist", e.ID)
}

// Is makes errors.Is(err, ErrNotFound) true.
func (e *NotFoundError) Is(target error) bool { return target == ErrNotFound }

// ValidationError reports an invalid input value.
type ValidationError struct {
	Field string
	Msg   string
}

func (e *ValidationError) Error() string { return e.Field + ": " + e.Msg }

// NamespaceNotFoundError is returned by operations other than creation when a
// namespace does not exist. Existing lists the known namespaces.
type NamespaceNotFoundError struct {
	Name     string
	Existing []string
}

func (e *NamespaceNotFoundError) Error() string {
	return fmt.Sprintf("namespace %q does not exist; existing namespaces: %s", e.Name, strings.Join(e.Existing, ", "))
}

// StateError reports an operation that conflicts with the memory's state
// (e.g. updating an archived memory).
type StateError struct {
	ID  int64
	Msg string
}

func (e *StateError) Error() string { return fmt.Sprintf("memory #%d %s", e.ID, e.Msg) }

// ---- Normalisation ----------------------------------------------------------

// NormalizeNamespace lowercases, trims and replaces whitespace with '-'.
// The result contains only [a-z0-9_-], starts with [a-z0-9] and is at most
// 64 bytes long.
func NormalizeNamespace(s string) (string, error) {
	n := strings.ToLower(strings.TrimSpace(s))
	n = strings.Join(strings.Fields(n), "-")
	if n == "" {
		return "", &ValidationError{Field: "namespace", Msg: "must not be empty"}
	}
	if len(n) > MaxNamespaceLen {
		return "", &ValidationError{Field: "namespace", Msg: fmt.Sprintf("must be at most %d characters", MaxNamespaceLen)}
	}
	for i, r := range n {
		alnum := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if i == 0 && !alnum {
			return "", &ValidationError{Field: "namespace", Msg: fmt.Sprintf("%q must start with a lowercase letter or digit", n)}
		}
		if !alnum && r != '-' && r != '_' {
			return "", &ValidationError{Field: "namespace", Msg: fmt.Sprintf("%q may only contain a-z, 0-9, '-' and '_'", n)}
		}
	}
	return n, nil
}

// NormalizeTags lowercases, splits on whitespace and commas, strips a leading
// '#', and removes duplicates while keeping order.
func NormalizeTags(tags []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, t := range tags {
		for _, f := range strings.FieldsFunc(strings.ToLower(t), func(r rune) bool {
			return unicode.IsSpace(r) || r == ','
		}) {
			f = strings.TrimLeft(f, "#")
			if f == "" || seen[f] {
				continue
			}
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

func joinTags(tags []string) string { return strings.Join(NormalizeTags(tags), " ") }

func splitTags(s string) []string {
	if s == "" {
		return []string{}
	}
	return strings.Fields(s)
}

func normalizeContent(s string) (string, error) {
	c := strings.TrimSpace(s)
	if c == "" {
		return "", &ValidationError{Field: "content", Msg: "must not be empty"}
	}
	if n := utf8.RuneCountInString(c); n > MaxContentRunes {
		return "", &ValidationError{Field: "content", Msg: fmt.Sprintf("is %d characters long, the maximum is %d: shorten it or split it into several memories", n, MaxContentRunes)}
	}
	return c, nil
}

// normalizeTitle collapses whitespace (titles are single-line).
func normalizeTitle(s string) (string, error) {
	t := strings.Join(strings.Fields(s), " ")
	if n := utf8.RuneCountInString(t); n > MaxTitleRunes {
		return "", &ValidationError{Field: "title", Msg: fmt.Sprintf("is %d characters long, the maximum is %d", n, MaxTitleRunes)}
	}
	return t, nil
}

func validateImportance(i int) error {
	if i < 1 || i > 3 {
		return &ValidationError{Field: "importance", Msg: "must be 1 (low), 2 (normal) or 3 (always in briefing)"}
	}
	return nil
}

func validateKind(k Kind) error {
	if !k.Valid() {
		names := make([]string, len(Kinds))
		for i, v := range Kinds {
			names[i] = string(v)
		}
		return &ValidationError{Field: "kind", Msg: fmt.Sprintf("%q is not valid; use one of %s", k, strings.Join(names, ", "))}
	}
	return nil
}

func normalizeActor(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return UnknownSource
	}
	return s
}

// ---- Time -------------------------------------------------------------------

// timeLayout matches strftime('%Y-%m-%dT%H:%M:%fZ','now').
const timeLayout = "2006-01-02T15:04:05.000Z"

// sqlNow is the SQL expression producing a timestamp in timeLayout.
const sqlNow = "strftime('%Y-%m-%dT%H:%M:%fZ','now')"

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(timeLayout, s)
	if err != nil {
		// Tolerate imported values in RFC 3339.
		if t2, err2 := time.Parse(time.RFC3339Nano, s); err2 == nil {
			return t2.UTC(), nil
		}
		return time.Time{}, fmt.Errorf("parse timestamp %q: %w", s, err)
	}
	return t, nil
}

// FormatTime renders t in the storage format.
func FormatTime(t time.Time) string { return t.UTC().Format(timeLayout) }
