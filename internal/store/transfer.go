package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// FormatVersion is the version of the JSON export format.
const FormatVersion = 1

// Dump is the JSON export of a database (or of one namespace).
type Dump struct {
	FormatVersion int             `json:"format_version"`
	SchemaVersion int             `json:"schema_version"`
	ExportedAt    time.Time       `json:"exported_at"`
	Namespaces    []DumpNamespace `json:"namespaces"`
	Memories      []DumpMemory    `json:"memories"`
}

// DumpNamespace is an exported namespace.
type DumpNamespace struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
}

// DumpMemory is an exported memory, archived ones included.
type DumpMemory struct {
	Memory
	History []DumpHistory `json:"history,omitempty"`
}

// DumpHistory is an exported history line. Snapshot ids refer to the
// exporting database.
type DumpHistory struct {
	Action    Action          `json:"action"`
	Snapshot  json.RawMessage `json:"snapshot"`
	ChangedBy string          `json:"changed_by"`
	ChangedAt time.Time       `json:"changed_at"`
}

// ExportOptions restricts Export.
type ExportOptions struct {
	Namespace   string // empty = everything; must exist otherwise
	WithHistory bool
}

// Export returns every memory (archived included) and namespace, ordered by id.
func (s *Store) Export(ctx context.Context, opt ExportOptions) (Dump, error) {
	d := Dump{FormatVersion: FormatVersion, ExportedAt: time.Now().UTC(), Namespaces: []DumpNamespace{}, Memories: []DumpMemory{}}
	var err error
	if d.SchemaVersion, err = s.SchemaVersion(ctx); err != nil {
		return Dump{}, err
	}
	ns := ""
	if opt.Namespace != "" {
		if ns, err = s.RequireNamespace(ctx, opt.Namespace); err != nil {
			return Dump{}, err
		}
	}

	nsQuery, nsArgs := "SELECT name, description, created_at FROM namespaces ORDER BY name", []any{}
	if ns != "" {
		nsQuery, nsArgs = "SELECT name, description, created_at FROM namespaces WHERE name = ?", []any{ns}
	}
	rows, err := s.db.QueryContext(ctx, nsQuery, nsArgs...)
	if err != nil {
		return Dump{}, fmt.Errorf("export namespaces: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var n DumpNamespace
		var created string
		if err := rows.Scan(&n.Name, &n.Description, &created); err != nil {
			return Dump{}, fmt.Errorf("export namespaces: %w", err)
		}
		if n.CreatedAt, err = parseTime(created); err != nil {
			return Dump{}, err
		}
		d.Namespaces = append(d.Namespaces, n)
	}
	if err := rows.Err(); err != nil {
		return Dump{}, fmt.Errorf("export namespaces: %w", err)
	}

	q, args := "SELECT "+memoryColumns+" FROM memories ORDER BY id", []any{}
	if ns != "" {
		q, args = "SELECT "+memoryColumns+" FROM memories WHERE namespace = ? ORDER BY id", []any{ns}
	}
	ms, err := queryMemories(ctx, s.db, q, args...)
	if err != nil {
		return Dump{}, err
	}
	for _, m := range ms {
		dm := DumpMemory{Memory: m}
		if opt.WithHistory {
			hs, err := s.History(ctx, m.ID)
			if err != nil {
				return Dump{}, err
			}
			for _, h := range hs {
				dm.History = append(dm.History, DumpHistory{
					Action: h.Action, Snapshot: json.RawMessage(h.Snapshot), ChangedBy: h.ChangedBy, ChangedAt: h.ChangedAt,
				})
			}
		}
		d.Memories = append(d.Memories, dm)
	}
	return d, nil
}

// errDryRun rolls back the import transaction in dry-run mode.
var errDryRun = errors.New("dry run")

// ImportReport summarises an import.
type ImportReport struct {
	DryRun            bool    `json:"dry_run"`
	NamespacesCreated int     `json:"namespaces_created"`
	Imported          int     `json:"imported"`
	Skipped           int     `json:"skipped"`      // exact duplicates (namespace + kind + content)
	ImportedIDs       []int64 `json:"imported_ids"` // new ids, in dump order
}

// Import loads a dump in one transaction. Memories keep their timestamps,
// authors and archive state but get new ids. Exact duplicates (same
// namespace, kind and content as an existing memory, archived or not) are
// skipped. Without history in the dump, a "create" history line by `by` is
// written for each imported memory. With dryRun, everything is checked and
// counted, then rolled back.
func (s *Store) Import(ctx context.Context, d Dump, dryRun bool, by string) (ImportReport, error) {
	rep := ImportReport{DryRun: dryRun, ImportedIDs: []int64{}}
	if d.FormatVersion != FormatVersion {
		return rep, &ValidationError{Field: "format_version", Msg: fmt.Sprintf("is %d, this binary reads version %d", d.FormatVersion, FormatVersion)}
	}
	by = normalizeActor(by)

	err := s.withTx(ctx, func(tx *sql.Tx) error {
		for _, n := range d.Namespaces {
			name, err := NormalizeNamespace(n.Name)
			if err != nil {
				return fmt.Errorf("namespace %q: %w", n.Name, err)
			}
			res, err := tx.ExecContext(ctx,
				"INSERT OR IGNORE INTO namespaces(name, description, created_at) VALUES (?, ?, ?)",
				name, n.Description, FormatTime(n.CreatedAt))
			if err != nil {
				return fmt.Errorf("import namespace %q: %w", name, err)
			}
			if c, _ := res.RowsAffected(); c > 0 {
				rep.NamespacesCreated++
			} else if n.Description != "" {
				if _, err := tx.ExecContext(ctx,
					"UPDATE namespaces SET description = ? WHERE name = ? AND description = ''", n.Description, name); err != nil {
					return fmt.Errorf("import namespace %q: %w", name, err)
				}
			}
		}
		for i, m := range d.Memories {
			id, skipped, err := importMemory(ctx, tx, m, by)
			if err != nil {
				return fmt.Errorf("memory %d (id %d in the dump): %w", i+1, m.ID, err)
			}
			if skipped {
				rep.Skipped++
				continue
			}
			rep.Imported++
			rep.ImportedIDs = append(rep.ImportedIDs, id)
		}
		if dryRun {
			return errDryRun
		}
		return nil
	})
	if err != nil && !errors.Is(err, errDryRun) {
		return ImportReport{}, err
	}
	return rep, nil
}

func importMemory(ctx context.Context, tx *sql.Tx, m DumpMemory, by string) (int64, bool, error) {
	ns, err := NormalizeNamespace(m.Namespace)
	if err != nil {
		return 0, false, err
	}
	if err := validateKind(m.Kind); err != nil {
		return 0, false, err
	}
	content, err := normalizeContent(m.Content)
	if err != nil {
		return 0, false, err
	}
	title, err := normalizeTitle(m.Title)
	if err != nil {
		return 0, false, err
	}
	imp := m.Importance
	if imp == 0 {
		imp = DefaultImportance
	}
	if err := validateImportance(imp); err != nil {
		return 0, false, err
	}
	var dup int
	err = tx.QueryRowContext(ctx,
		"SELECT count(*) FROM memories WHERE namespace = ? AND kind = ? AND content = ?",
		ns, string(m.Kind), content).Scan(&dup)
	if err != nil {
		return 0, false, fmt.Errorf("check duplicate: %w", err)
	}
	if dup > 0 {
		return 0, true, nil
	}
	if err := ensureNamespace(ctx, tx, ns); err != nil {
		return 0, false, err
	}
	created, updated := m.CreatedAt, m.UpdatedAt
	if created.IsZero() {
		created = time.Now()
	}
	if updated.IsZero() {
		updated = created
	}
	var archived any
	if m.ArchivedAt != nil {
		archived = FormatTime(*m.ArchivedAt)
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO memories(namespace, kind, title, content, tags, importance, source,
		                      created_at, updated_at, updated_by, archived_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ns, string(m.Kind), title, content, joinTags(m.Tags), imp, normalizeActor(m.Source),
		FormatTime(created), FormatTime(updated), normalizeActor(m.UpdatedBy), archived)
	if err != nil {
		return 0, false, fmt.Errorf("insert: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, false, fmt.Errorf("insert: %w", err)
	}
	if len(m.History) == 0 {
		_, err := writeHistory(ctx, tx, id, ActionCreate, by, "imported")
		return id, false, err
	}
	for _, h := range m.History {
		switch h.Action {
		case ActionCreate, ActionUpdate, ActionArchive, ActionRestore:
		default:
			return 0, false, &ValidationError{Field: "history.action", Msg: fmt.Sprintf("unknown action %q", h.Action)}
		}
		if !json.Valid(h.Snapshot) {
			return 0, false, &ValidationError{Field: "history.snapshot", Msg: "is not valid JSON"}
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO memory_history(memory_id, action, snapshot, changed_by, changed_at) VALUES (?, ?, ?, ?, ?)",
			id, string(h.Action), string(h.Snapshot), normalizeActor(h.ChangedBy), FormatTime(h.ChangedAt)); err != nil {
			return 0, false, fmt.Errorf("insert history: %w", err)
		}
	}
	return id, false, nil
}
