package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// queryer is satisfied by *sql.DB and *sql.Tx.
type queryer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

const memoryColumns = `id, namespace, kind, title, content, tags, importance, source,
	created_at, updated_at, updated_by, archived_at`

type scanner interface{ Scan(dest ...any) error }

func scanMemory(sc scanner) (Memory, error) {
	var (
		m                Memory
		kind, tags       string
		created, updated string
		archived         sql.NullString
	)
	if err := sc.Scan(&m.ID, &m.Namespace, &kind, &m.Title, &m.Content, &tags, &m.Importance,
		&m.Source, &created, &updated, &m.UpdatedBy, &archived); err != nil {
		return Memory{}, err
	}
	m.Kind = Kind(kind)
	m.Tags = splitTags(tags)
	var err error
	if m.CreatedAt, err = parseTime(created); err != nil {
		return Memory{}, err
	}
	if m.UpdatedAt, err = parseTime(updated); err != nil {
		return Memory{}, err
	}
	if archived.Valid {
		t, err := parseTime(archived.String)
		if err != nil {
			return Memory{}, err
		}
		m.ArchivedAt = &t
	}
	return m, nil
}

func getMemory(ctx context.Context, q queryer, id int64) (Memory, error) {
	m, err := scanMemory(q.QueryRowContext(ctx, "SELECT "+memoryColumns+" FROM memories WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Memory{}, &NotFoundError{ID: id}
	}
	if err != nil {
		return Memory{}, fmt.Errorf("get memory #%d: %w", id, err)
	}
	return m, nil
}

// snapshot is the JSON stored in memory_history: the row after the action,
// plus an optional free-text reason (archive).
type snapshot struct {
	Memory
	Reason string `json:"reason,omitempty"`
}

// writeHistory reloads the memory inside tx and records it.
func writeHistory(ctx context.Context, tx *sql.Tx, id int64, action Action, by, reason string) (Memory, error) {
	m, err := getMemory(ctx, tx, id)
	if err != nil {
		return Memory{}, err
	}
	b, err := json.Marshal(snapshot{Memory: m, Reason: strings.TrimSpace(reason)})
	if err != nil {
		return Memory{}, fmt.Errorf("encode snapshot: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO memory_history(memory_id, action, snapshot, changed_by) VALUES (?, ?, ?, ?)",
		id, string(action), string(b), by); err != nil {
		return Memory{}, fmt.Errorf("write history for #%d: %w", id, err)
	}
	return m, nil
}

// NewMemory is the input of Create.
type NewMemory struct {
	Namespace            string // created if missing; default "global"
	NamespaceDescription string // optional description for the namespace
	Kind                 Kind
	Title                string
	Content              string
	Tags                 []string
	Importance           int     // 0 means default (2)
	Source               string  // author; "unknown" if empty
	Supersedes           []int64 // memories archived in the same transaction
}

// Create inserts a memory, creating its namespace if needed, and archives
// the memories listed in Supersedes, all in one transaction. A new handoff
// archives the previous active handoff(s) of its namespace: there is at most
// one active handoff per namespace.
func (s *Store) Create(ctx context.Context, in NewMemory) (Memory, error) {
	m, _, err := s.CreateDetailed(ctx, in)
	return m, err
}

// CreateDetailed is Create, also returning the ids archived by the call
// (Supersedes and replaced handoffs).
func (s *Store) CreateDetailed(ctx context.Context, in NewMemory) (Memory, []int64, error) {
	ns := in.Namespace
	if strings.TrimSpace(ns) == "" {
		ns = DefaultNamespace
	}
	ns, err := NormalizeNamespace(ns)
	if err != nil {
		return Memory{}, nil, err
	}
	if err := validateKind(in.Kind); err != nil {
		return Memory{}, nil, err
	}
	content, err := normalizeContent(in.Content)
	if err != nil {
		return Memory{}, nil, err
	}
	if err := checkNoSecrets("content", content); err != nil {
		return Memory{}, nil, err
	}
	title, err := normalizeTitle(in.Title)
	if err != nil {
		return Memory{}, nil, err
	}
	if err := checkNoSecrets("title", title); err != nil {
		return Memory{}, nil, err
	}
	imp := in.Importance
	if imp == 0 {
		imp = DefaultImportance
	}
	if err := validateImportance(imp); err != nil {
		return Memory{}, nil, err
	}
	src := normalizeActor(in.Source)

	var out Memory
	archived := []int64{}
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		if err := ensureNamespace(ctx, tx, ns); err != nil {
			return err
		}
		if desc := strings.TrimSpace(in.NamespaceDescription); desc != "" {
			if _, err := tx.ExecContext(ctx, "UPDATE namespaces SET description = ? WHERE name = ?", desc, ns); err != nil {
				return fmt.Errorf("describe namespace %q: %w", ns, err)
			}
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO memories(namespace, kind, title, content, tags, importance, source, updated_by)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			ns, string(in.Kind), title, content, joinTags(in.Tags), imp, src, src)
		if err != nil {
			return fmt.Errorf("insert memory: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("insert memory: %w", err)
		}
		if out, err = writeHistory(ctx, tx, id, ActionCreate, src, ""); err != nil {
			return err
		}
		for _, old := range in.Supersedes {
			if old == id {
				continue
			}
			prev, err := archive(ctx, tx, old, src, fmt.Sprintf("superseded by #%d", id), true)
			if err != nil {
				return err
			}
			if prev.changed {
				archived = append(archived, old)
			}
		}
		if in.Kind != KindHandoff {
			return nil
		}
		replaced, err := archiveOtherHandoffs(ctx, tx, ns, id, src)
		if err != nil {
			return err
		}
		archived = append(archived, replaced...)
		return nil
	})
	if err != nil {
		return Memory{}, nil, err
	}
	return out, archived, nil
}

// Get returns a memory, archived or not.
func (s *Store) Get(ctx context.Context, id int64) (Memory, error) {
	return getMemory(ctx, s.db, id)
}

// Patch lists the fields to change; nil means unchanged.
type Patch struct {
	Content    *string
	Title      *string
	Tags       *[]string
	Importance *int
	Kind       *Kind
	Namespace  *string // created if missing
}

func (p Patch) empty() bool {
	return p.Content == nil && p.Title == nil && p.Tags == nil && p.Importance == nil &&
		p.Kind == nil && p.Namespace == nil
}

// Update modifies the given fields of an active memory. The previous version
// remains available in the history.
func (s *Store) Update(ctx context.Context, id int64, p Patch, by string) (Memory, error) {
	if p.empty() {
		return Memory{}, &ValidationError{Field: "update", Msg: "no field to change: provide at least one of content, title, tags, importance, kind, namespace"}
	}
	// Column names come from this fixed list, never from user input.
	var sets []string
	var args []any
	if p.Content != nil {
		c, err := normalizeContent(*p.Content)
		if err != nil {
			return Memory{}, err
		}
		if err := checkNoSecrets("content", c); err != nil {
			return Memory{}, err
		}
		sets, args = append(sets, "content = ?"), append(args, c)
	}
	if p.Title != nil {
		t, err := normalizeTitle(*p.Title)
		if err != nil {
			return Memory{}, err
		}
		if err := checkNoSecrets("title", t); err != nil {
			return Memory{}, err
		}
		sets, args = append(sets, "title = ?"), append(args, t)
	}
	if p.Tags != nil {
		sets, args = append(sets, "tags = ?"), append(args, joinTags(*p.Tags))
	}
	if p.Importance != nil {
		if err := validateImportance(*p.Importance); err != nil {
			return Memory{}, err
		}
		sets, args = append(sets, "importance = ?"), append(args, *p.Importance)
	}
	if p.Kind != nil {
		if err := validateKind(*p.Kind); err != nil {
			return Memory{}, err
		}
		sets, args = append(sets, "kind = ?"), append(args, string(*p.Kind))
	}
	var ns string
	if p.Namespace != nil {
		var err error
		if ns, err = NormalizeNamespace(*p.Namespace); err != nil {
			return Memory{}, err
		}
		sets, args = append(sets, "namespace = ?"), append(args, ns)
	}
	by = normalizeActor(by)
	sets = append(sets, "updated_at = "+sqlNow, "updated_by = ?")
	args = append(args, by, id)

	var out Memory
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		cur, err := getMemory(ctx, tx, id)
		if err != nil {
			return err
		}
		if cur.Archived() {
			return &StateError{ID: id, Msg: "is archived; restore it first (llm-memory restore) or create a new memory"}
		}
		if p.Namespace != nil {
			if err := ensureNamespace(ctx, tx, ns); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, "UPDATE memories SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...); err != nil {
			return fmt.Errorf("update memory #%d: %w", id, err)
		}
		targetNS := cur.Namespace
		if p.Namespace != nil {
			targetNS = ns
		}
		targetKind := cur.Kind
		if p.Kind != nil {
			targetKind = *p.Kind
		}
		if targetKind == KindHandoff {
			if _, err := archiveOtherHandoffs(ctx, tx, targetNS, id, by); err != nil {
				return err
			}
		}
		out, err = writeHistory(ctx, tx, id, ActionUpdate, by, "")
		return err
	})
	if err != nil {
		return Memory{}, err
	}
	return out, nil
}

// Archive hides a memory from searches and briefings. It is reversible with
// Restore. reason is kept in the history snapshot.
func (s *Store) Archive(ctx context.Context, id int64, by, reason string) (Memory, error) {
	var out Memory
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := archive(ctx, tx, id, normalizeActor(by), reason, false)
		out = res.Memory
		return err
	})
	return out, err
}

// archiveResult tells whether archive actually changed the memory.
type archiveResult struct {
	Memory
	changed bool
}

// archive marks id archived inside tx. If lenient, an already archived memory
// is left untouched instead of returning a StateError.
func archive(ctx context.Context, tx *sql.Tx, id int64, by, reason string, lenient bool) (archiveResult, error) {
	cur, err := getMemory(ctx, tx, id)
	if err != nil {
		return archiveResult{}, err
	}
	if cur.Archived() {
		if lenient {
			return archiveResult{Memory: cur}, nil
		}
		return archiveResult{}, &StateError{ID: id, Msg: "is already archived"}
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE memories SET archived_at = "+sqlNow+", updated_at = "+sqlNow+", updated_by = ? WHERE id = ?",
		by, id); err != nil {
		return archiveResult{}, fmt.Errorf("archive memory #%d: %w", id, err)
	}
	m, err := writeHistory(ctx, tx, id, ActionArchive, by, reason)
	return archiveResult{Memory: m, changed: true}, err
}

// archiveOtherHandoffs archives any active handoffs in ns other than keepID.
func archiveOtherHandoffs(ctx context.Context, tx *sql.Tx, ns string, keepID int64, by string) ([]int64, error) {
	olds, err := queryMemories(ctx, tx, "SELECT "+memoryColumns+
		" FROM memories WHERE namespace = ? AND kind = 'handoff' AND archived_at IS NULL AND id <> ?", ns, keepID)
	if err != nil {
		return nil, err
	}
	var archived []int64
	for _, o := range olds {
		if _, err := archive(ctx, tx, o.ID, by, fmt.Sprintf("replaced by handoff #%d", keepID), true); err != nil {
			return nil, err
		}
		archived = append(archived, o.ID)
	}
	return archived, nil
}

// Restore un-archives a memory.
func (s *Store) Restore(ctx context.Context, id int64, by string) (Memory, error) {
	by = normalizeActor(by)
	var out Memory
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		cur, err := getMemory(ctx, tx, id)
		if err != nil {
			return err
		}
		if !cur.Archived() {
			return &StateError{ID: id, Msg: "is not archived"}
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE memories SET archived_at = NULL, updated_at = "+sqlNow+", updated_by = ? WHERE id = ?",
			by, id); err != nil {
			return fmt.Errorf("restore memory #%d: %w", id, err)
		}
		if cur.Kind == KindHandoff {
			if _, err := archiveOtherHandoffs(ctx, tx, cur.Namespace, id, by); err != nil {
				return err
			}
		}
		out, err = writeHistory(ctx, tx, id, ActionRestore, by, "")
		return err
	})
	return out, err
}

// ArchiveFilter selects memories by archival state.
type ArchiveFilter int

// Archive filters.
const (
	ActiveOnly ArchiveFilter = iota
	IncludeArchived
	ArchivedOnly
)

// ListFilter restricts List. Zero values mean "no restriction".
type ListFilter struct {
	Namespace string // exact (normalised) name; must exist if set
	Kinds     []Kind
	Tags      []string
	Archived  ArchiveFilter
	Limit     int // 0 = unlimited
	Offset    int
}

// List returns memories ordered by most recent update first.
func (s *Store) List(ctx context.Context, f ListFilter) ([]Memory, error) {
	var where []string
	var args []any
	if f.Namespace != "" {
		ns, err := NormalizeNamespace(f.Namespace)
		if err != nil {
			return nil, err
		}
		if err := requireNamespace(ctx, s.db, ns); err != nil {
			return nil, err
		}
		where, args = append(where, "namespace = ?"), append(args, ns)
	}
	if len(f.Kinds) > 0 {
		ph := make([]string, len(f.Kinds))
		for i, k := range f.Kinds {
			if err := validateKind(k); err != nil {
				return nil, err
			}
			ph[i] = "?"
			args = append(args, string(k))
		}
		where = append(where, "kind IN ("+strings.Join(ph, ", ")+")")
	}
	for _, t := range NormalizeTags(f.Tags) {
		where = append(where, "instr(' ' || tags || ' ', ' ' || ? || ' ') > 0")
		args = append(args, t)
	}
	switch f.Archived {
	case ActiveOnly:
		where = append(where, "archived_at IS NULL")
	case ArchivedOnly:
		where = append(where, "archived_at IS NOT NULL")
	case IncludeArchived:
	}
	q := "SELECT " + memoryColumns + " FROM memories"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY updated_at DESC, id DESC"
	if f.Limit > 0 {
		q += " LIMIT ? OFFSET ?"
		args = append(args, f.Limit, f.Offset)
	}
	return queryMemories(ctx, s.db, q, args...)
}

func queryMemories(ctx context.Context, q queryer, query string, args ...any) ([]Memory, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query memories: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []Memory{}
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, fmt.Errorf("scan memory: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query memories: %w", err)
	}
	return out, nil
}
