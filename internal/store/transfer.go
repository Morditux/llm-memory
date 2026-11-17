package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Export format versions.
//
//   - 1: memories, namespaces and optional history. No identity: ids are local
//     to the exporting database, and older binaries reject unknown fields.
//   - 2: adds the memory uuid, the links between memories (by uuid), and the
//     origin (instance id) of the exporting database.
const (
	FormatVersionV1 = 1
	// FormatVersion is the latest version, written by default.
	FormatVersion = 2
)

// Dump is the JSON export of a database (or of one namespace).
type Dump struct {
	FormatVersion int             `json:"format_version"`
	SchemaVersion int             `json:"schema_version"`
	ExportedAt    time.Time       `json:"exported_at"`
	Origin        string          `json:"origin,omitempty"` // instance id of the exporting database (v2)
	Namespaces    []DumpNamespace `json:"namespaces"`
	Memories      []DumpMemory    `json:"memories"`
	Links         []DumpLink      `json:"links,omitempty"` // v2

	// DroppedLinks counts links left out because one end is not in the
	// export (namespace-scoped export). Not part of the file.
	DroppedLinks int `json:"-"`
}

// DumpLink is an exported link between two memories, identified by uuid.
type DumpLink struct {
	Src       string    `json:"src_uuid"`
	Dst       string    `json:"dst_uuid"`
	Kind      LinkKind  `json:"kind"`
	CreatedAt time.Time `json:"created_at"`
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
	Namespace     string // empty = everything; must exist otherwise
	WithHistory   bool
	FormatVersion int // 0 = latest (FormatVersion); FormatVersionV1 for older binaries
}

// Export returns every memory (archived included) and namespace, ordered by id.
func (s *Store) Export(ctx context.Context, opt ExportOptions) (Dump, error) {
	version := opt.FormatVersion
	if version == 0 {
		version = FormatVersion
	}
	if version != FormatVersionV1 && version != FormatVersion {
		return Dump{}, &ValidationError{Field: "format_version", Msg: fmt.Sprintf("cannot export version %d; use 1 or %d", version, FormatVersion)}
	}
	d := Dump{FormatVersion: version, ExportedAt: time.Now().UTC(), Namespaces: []DumpNamespace{}, Memories: []DumpMemory{}}
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
		if version == FormatVersionV1 {
			// v1 has no uuid and older binaries reject unknown fields.
			m.UUID = ""
		}
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
	if version >= 2 {
		if d.Origin, err = s.InstanceID(ctx); err != nil {
			return Dump{}, err
		}
		if d.Links, d.DroppedLinks, err = s.exportLinks(ctx, d.Memories); err != nil {
			return Dump{}, err
		}
	}
	return d, nil
}

// exportLinks returns the links whose two ends are both among ms, ordered by
// (src, dst, kind) so exports are stable, and the number of links dropped
// because one end is outside ms.
func (s *Store) exportLinks(ctx context.Context, ms []DumpMemory) ([]DumpLink, int, error) {
	in := make(map[string]bool, len(ms))
	for _, m := range ms {
		in[m.UUID] = true
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT s.uuid, d.uuid, l.kind, l.created_at
		FROM memory_links l
		JOIN memories s ON s.id = l.src_id
		JOIN memories d ON d.id = l.dst_id
		ORDER BY s.uuid, d.uuid, l.kind`)
	if err != nil {
		return nil, 0, fmt.Errorf("export links: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []DumpLink
	dropped := 0
	for rows.Next() {
		var l DumpLink
		var kind, created string
		if err := rows.Scan(&l.Src, &l.Dst, &kind, &created); err != nil {
			return nil, 0, fmt.Errorf("export links: %w", err)
		}
		l.Kind = LinkKind(kind)
		if l.CreatedAt, err = parseTime(created); err != nil {
			return nil, 0, err
		}
		if !in[l.Src] || !in[l.Dst] {
			dropped++
			continue
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("export links: %w", err)
	}
	return out, dropped, nil
}

// errDryRun rolls back the import transaction in dry-run mode.
var errDryRun = errors.New("dry run")

// ImportReport summarises an import.
type ImportReport struct {
	DryRun            bool    `json:"dry_run"`
	NamespacesCreated int     `json:"namespaces_created"`
	Imported          int     `json:"imported"`
	LinksImported     int     `json:"links_imported"`
	LinksSkipped      int     `json:"links_skipped"` // already present, or an end is missing from the dump
	Skipped           int     `json:"skipped"`       // exact duplicates (namespace + kind + content)
	ImportedIDs       []int64 `json:"imported_ids"`  // new ids, in dump order
}

// Import loads a dump in one transaction. Memories keep their timestamps,
// authors and archive state but get new ids. Exact duplicates (same
// namespace, kind and content as an existing memory, archived or not) are
// skipped. Without history in the dump, a "create" history line by `by` is
// written for each imported memory. With dryRun, everything is checked and
// counted, then rolled back.
func (s *Store) Import(ctx context.Context, d Dump, dryRun bool, by string) (ImportReport, error) {
	rep := ImportReport{DryRun: dryRun, ImportedIDs: []int64{}}
	if d.FormatVersion != FormatVersionV1 && d.FormatVersion != FormatVersion {
		return rep, &ValidationError{Field: "format_version", Msg: fmt.Sprintf("is %d, this binary reads versions 1 to %d", d.FormatVersion, FormatVersion)}
	}
	if err := validateDumpIdentities(d); err != nil {
		return rep, err
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
		// dump uuid -> local id, for imported memories and for the existing
		// duplicates they were skipped for, so links can be restored.
		local := make(map[string]int64, len(d.Memories))
		isNew := make(map[int64]bool, len(d.Memories))
		for i, m := range d.Memories {
			id, skipped, err := importMemory(ctx, tx, m, by)
			if err != nil {
				return fmt.Errorf("memory %d (id %d in the dump): %w", i+1, m.ID, err)
			}
			if m.UUID != "" {
				local[m.UUID] = id
			}
			if skipped {
				rep.Skipped++
				continue
			}
			isNew[id] = true
			rep.Imported++
			rep.ImportedIDs = append(rep.ImportedIDs, id)
		}
		for i, l := range d.Links {
			src, dst := local[l.Src], local[l.Dst]
			if src == 0 || dst == 0 || src == dst {
				rep.LinksSkipped++
				continue
			}
			added, err := insertLink(ctx, tx, src, dst, l.Kind)
			if err != nil {
				return fmt.Errorf("link %d (%s -> %s): %w", i+1, l.Src, l.Dst, err)
			}
			if !added {
				rep.LinksSkipped++
				continue
			}
			rep.LinksImported++
			if !isNew[src] { // an existing memory changed: say so in its history
				if err := touchAndRecordLinkHistory(ctx, tx, src, by, fmt.Sprintf("imported link %s -> #%d", l.Kind, dst)); err != nil {
					return err
				}
			}
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
	var dupID int64
	err = tx.QueryRowContext(ctx,
		"SELECT id FROM memories WHERE namespace = ? AND kind = ? AND content = ? ORDER BY id LIMIT 1",
		ns, string(m.Kind), content).Scan(&dupID)
	if err == nil {
		return dupID, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, fmt.Errorf("check duplicate: %w", err)
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
	var archived, expires any
	if m.ArchivedAt != nil {
		archived = FormatTime(*m.ArchivedAt)
	}
	if m.ExpiresAt != nil {
		expires = FormatTime(*m.ExpiresAt)
	}
	// Phase 1: an imported memory always gets a fresh identity. Keeping the
	// uuid of the dump is part of the merge import.
	uid, err := newUUID()
	if err != nil {
		return 0, false, err
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO memories(namespace, kind, title, content, tags, importance, source,
		                      created_at, updated_at, updated_by, archived_at, expires_at, uuid)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ns, string(m.Kind), title, content, joinTags(m.Tags), imp, normalizeActor(m.Source),
		FormatTime(created), FormatTime(updated), normalizeActor(m.UpdatedBy), archived, expires, uid)
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

// validateDumpIdentities checks the uuids and links of a dump before anything
// is written. Memories may lack a uuid (version 1, or a hand-built dump); when
// present it must be well formed and unique.
func validateDumpIdentities(d Dump) error {
	seen := make(map[string]bool, len(d.Memories))
	for i, m := range d.Memories {
		if m.UUID == "" {
			continue
		}
		if !validUUID(m.UUID) {
			return &ValidationError{Field: "memories.uuid", Msg: fmt.Sprintf("memory %d: %q is not a valid uuid", i+1, m.UUID)}
		}
		if seen[m.UUID] {
			return &ValidationError{Field: "memories.uuid", Msg: fmt.Sprintf("memory %d: uuid %s appears twice", i+1, m.UUID)}
		}
		seen[m.UUID] = true
	}
	for i, l := range d.Links {
		if err := validateLinkKind(l.Kind); err != nil {
			return fmt.Errorf("link %d: %w", i+1, err)
		}
		if !validUUID(l.Src) || !validUUID(l.Dst) {
			return &ValidationError{Field: "links", Msg: fmt.Sprintf("link %d: src_uuid and dst_uuid must be valid uuids", i+1)}
		}
	}
	return nil
}
