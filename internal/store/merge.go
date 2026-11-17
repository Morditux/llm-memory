package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// MergeOptions tunes Merge.
type MergeOptions struct {
	// NoContentMatch disables the first-merge rule that pairs a memory
	// unknown by uuid with a local memory of the same namespace, kind and
	// content that is not itself in the dump.
	NoContentMatch bool
}

// MergeChange describes one memory whose local and incoming versions differ.
type MergeChange struct {
	ID     int64  `json:"id"` // local id
	UUID   string `json:"uuid"`
	Title  string `json:"title"`
	Result string `json:"result"` // "updated" (incoming won) or "kept_local"
}

// MergeReport summarises a merge.
type MergeReport struct {
	DryRun            bool          `json:"dry_run"`
	NamespacesCreated int           `json:"namespaces_created"`
	Inserted          int           `json:"inserted"`
	Updated           int           `json:"updated"`    // incoming version was newer and replaced the local one
	KeptLocal         int           `json:"kept_local"` // local version was newer (or won the tie) and was kept
	Unchanged         int           `json:"unchanged"`
	TwinsMatched      int           `json:"twins_matched"` // paired by content instead of uuid (first merge)
	LinksAdded        int           `json:"links_added"`
	LinksSkipped      int           `json:"links_skipped"` // already present, or an end is unknown
	HistoryAdded      int           `json:"history_added"`
	Changes           []MergeChange `json:"changes"`
}

// Merge folds a format 2 dump into the database, so that merging A into B and
// B into A leaves both with the same memories.
//
// Memories are matched by uuid. A memory unknown by uuid is paired with a local
// memory of the same namespace, kind and content when there is one that the
// dump does not already account for (two machines that stored the same fact
// independently); both then settle on the smaller uuid. Otherwise it is
// inserted with its uuid, timestamps and authors.
//
// When both sides have the memory, the version with the later updated_at wins
// as a whole row; on equal timestamps the larger canonical form wins, which
// does not depend on which side runs the merge. A replaced local version stays
// in the local history. History lines of the dump are added when absent, and
// links are added as a union, so neither is ever removed. Merging never bumps
// updated_at itself, otherwise two databases would never agree. Local memories
// that the dump does not mention are left alone. Embeddings are not
// transferred: a changed text drops the stale vector and it is rebuilt later.
//
// With dryRun everything is computed, then rolled back.
func (s *Store) Merge(ctx context.Context, d Dump, opt MergeOptions, dryRun bool, by string) (MergeReport, error) {
	rep := MergeReport{DryRun: dryRun, Changes: []MergeChange{}}
	if d.FormatVersion != FormatVersion {
		return rep, &ValidationError{Field: "format_version", Msg: fmt.Sprintf("merge needs a version %d export (it carries the memory uuids), got version %d; re-export with a current llm-memory", FormatVersion, d.FormatVersion)}
	}
	if err := validateDumpIdentities(d); err != nil {
		return rep, err
	}
	inDump := make(map[string]bool, len(d.Memories))
	for i, m := range d.Memories {
		if m.UUID == "" {
			return rep, &ValidationError{Field: "memories.uuid", Msg: fmt.Sprintf("memory %d has no uuid", i+1)}
		}
		inDump[m.UUID] = true
	}
	by = normalizeActor(by)
	origin := d.Origin
	if len(origin) > 8 {
		origin = origin[:8]
	}

	err := s.withTx(ctx, func(tx *sql.Tx) error {
		if err := importNamespaces(ctx, tx, d.Namespaces, &rep.NamespacesCreated); err != nil {
			return err
		}
		local := make(map[string]int64, len(d.Memories)) // dump uuid -> local id
		claimed := map[int64]bool{}                      // local rows already paired in this merge
		for i, m := range d.Memories {
			f, err := normalizeDumpMemory(m)
			if err != nil {
				return fmt.Errorf("memory %d (id %d in the dump): %w", i+1, m.ID, err)
			}
			for _, h := range m.History {
				if err := validateDumpHistory(h); err != nil {
					return fmt.Errorf("memory %d (id %d in the dump): %w", i+1, m.ID, err)
				}
			}
			id, twin, found, err := findLocal(ctx, tx, m.UUID, f, inDump, claimed, opt.NoContentMatch)
			if err != nil {
				return fmt.Errorf("memory %d (id %d in the dump): %w", i+1, m.ID, err)
			}
			if !found {
				if id, err = insertMemoryRow(ctx, tx, f, m.UUID); err != nil {
					return fmt.Errorf("memory %d (id %d in the dump): %w", i+1, m.ID, err)
				}
				local[m.UUID], claimed[id] = id, true
				rep.Inserted++
				if len(m.History) == 0 {
					if _, err := writeHistory(ctx, tx, id, ActionCreate, by, "merged"); err != nil {
						return err
					}
				} else if rep.HistoryAdded, err = mergeHistory(ctx, tx, id, m.History, rep.HistoryAdded); err != nil {
					return err
				}
				continue
			}
			local[m.UUID], claimed[id] = id, true
			cur, err := getMemory(ctx, tx, id)
			if err != nil {
				return err
			}
			if twin {
				rep.TwinsMatched++
				if m.UUID < cur.UUID {
					if _, err := tx.ExecContext(ctx, "UPDATE memories SET uuid = ? WHERE id = ?", m.UUID, id); err != nil {
						return fmt.Errorf("adopt uuid of memory %d: %w", i+1, err)
					}
				}
			}
			if f.created.Before(cur.CreatedAt) {
				if _, err := tx.ExecContext(ctx, "UPDATE memories SET created_at = ? WHERE id = ?", FormatTime(f.created), id); err != nil {
					return fmt.Errorf("merge created_at of memory %d: %w", i+1, err)
				}
			}
			cf := fieldsOfMemory(cur)
			switch {
			case canonical(f) == canonical(cf):
				rep.Unchanged++
			case incomingWins(f, cf):
				if err := ensureNamespace(ctx, tx, f.ns); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx,
					`UPDATE memories SET namespace = ?, kind = ?, title = ?, content = ?, tags = ?, importance = ?,
					        source = ?, updated_at = ?, updated_by = ?, archived_at = ?, expires_at = ? WHERE id = ?`,
					f.ns, f.kind, f.title, f.content, f.tags, f.importance, f.source,
					FormatTime(f.updated), f.updatedBy, optTime(f.archived), optTime(f.expires), id); err != nil {
					return fmt.Errorf("merge memory %d: %w", i+1, err)
				}
				if _, err := writeHistory(ctx, tx, id, ActionUpdate, by, "merged: newer version from "+origin); err != nil {
					return err
				}
				rep.Updated++
				rep.Changes = append(rep.Changes, MergeChange{ID: id, UUID: m.UUID, Title: changeLabel(f.title, f.content), Result: "updated"})
			default:
				rep.KeptLocal++
				rep.Changes = append(rep.Changes, MergeChange{ID: id, UUID: m.UUID, Title: changeLabel(cf.title, cf.content), Result: "kept_local"})
			}
			if rep.HistoryAdded, err = mergeHistory(ctx, tx, id, m.History, rep.HistoryAdded); err != nil {
				return err
			}
		}

		for i, l := range d.Links {
			src, err := resolveUUID(ctx, tx, local, l.Src)
			if err != nil {
				return err
			}
			dst, err := resolveUUID(ctx, tx, local, l.Dst)
			if err != nil {
				return err
			}
			if src == 0 || dst == 0 || src == dst {
				rep.LinksSkipped++
				continue
			}
			created := l.CreatedAt
			if created.IsZero() {
				created = d.ExportedAt
			}
			res, err := tx.ExecContext(ctx,
				"INSERT OR IGNORE INTO memory_links(src_id, dst_id, kind, created_at) VALUES (?, ?, ?, ?)",
				src, dst, string(l.Kind), FormatTime(created))
			if err != nil {
				return fmt.Errorf("link %d (%s -> %s): %w", i+1, l.Src, l.Dst, err)
			}
			if n, _ := res.RowsAffected(); n > 0 {
				rep.LinksAdded++
			} else {
				rep.LinksSkipped++
			}
		}
		if dryRun {
			return errDryRun
		}
		return nil
	})
	if err != nil && !errors.Is(err, errDryRun) {
		return MergeReport{}, err
	}
	return rep, nil
}

// findLocal returns the local memory for an incoming uuid, or, failing that, a
// content twin. twin reports the second case.
func findLocal(ctx context.Context, tx *sql.Tx, uid string, f rowFields, inDump map[string]bool, claimed map[int64]bool, noTwin bool) (id int64, twin, found bool, err error) {
	err = tx.QueryRowContext(ctx, "SELECT id FROM memories WHERE uuid = ?", uid).Scan(&id)
	if err == nil {
		return id, false, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, false, fmt.Errorf("look up uuid: %w", err)
	}
	if noTwin {
		return 0, false, false, nil
	}
	rows, err := tx.QueryContext(ctx,
		"SELECT id, uuid FROM memories WHERE namespace = ? AND kind = ? AND content = ? ORDER BY id",
		f.ns, f.kind, f.content)
	if err != nil {
		return 0, false, false, fmt.Errorf("look up twin: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var cid int64
		var cuid sql.NullString
		if err := rows.Scan(&cid, &cuid); err != nil {
			return 0, false, false, fmt.Errorf("look up twin: %w", err)
		}
		// A row the dump itself describes is not a twin, nor is one already
		// paired with another incoming memory.
		if claimed[cid] || inDump[cuid.String] {
			continue
		}
		return cid, true, true, nil
	}
	return 0, false, false, rows.Err()
}

func resolveUUID(ctx context.Context, tx *sql.Tx, local map[string]int64, uid string) (int64, error) {
	if id, ok := local[uid]; ok {
		return id, nil
	}
	var id int64
	err := tx.QueryRowContext(ctx, "SELECT id FROM memories WHERE uuid = ?", uid).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("resolve link end %s: %w", uid, err)
	}
	return id, nil
}

func fieldsOfMemory(m Memory) rowFields {
	return rowFields{
		ns: m.Namespace, kind: string(m.Kind), title: m.Title, content: m.Content, tags: joinTags(m.Tags),
		source: m.Source, updatedBy: m.UpdatedBy, importance: m.Importance,
		created: m.CreatedAt, updated: m.UpdatedAt, archived: m.ArchivedAt, expires: m.ExpiresAt,
	}
}

// canonical is the comparable form of everything a merge reconciles
// (created_at is excluded: it is settled separately, as the earliest).
func canonical(f rowFields) string {
	archived, expires := "", ""
	if f.archived != nil {
		archived = FormatTime(*f.archived)
	}
	if f.expires != nil {
		expires = FormatTime(*f.expires)
	}
	return strings.Join([]string{
		FormatTime(f.updated), f.ns, f.kind, f.title, f.content, f.tags, fmt.Sprint(f.importance),
		f.source, f.updatedBy, archived, expires,
	}, "\x1f")
}

// incomingWins decides between two differing versions of the same memory.
// updated_at comes first in canonical(), so the tie-break is the rest of the
// row, identical whichever database runs the merge.
func incomingWins(in, cur rowFields) bool {
	return canonical(in) > canonical(cur)
}

// historyKey identifies a history line independently of the local ids and
// uuids written inside its snapshot.
func historyKey(action Action, by string, at string, snapshot []byte) string {
	var snap map[string]any
	canon := snapshot
	if json.Unmarshal(snapshot, &snap) == nil {
		delete(snap, "id")
		delete(snap, "uuid")
		if b, err := json.Marshal(snap); err == nil {
			canon = b
		}
	}
	sum := sha256.Sum256(canon)
	return string(action) + "|" + by + "|" + at + "|" + hex.EncodeToString(sum[:])
}

// mergeHistory adds the lines of hs that memory id does not have yet and
// returns the running count of lines added.
func mergeHistory(ctx context.Context, tx *sql.Tx, id int64, hs []DumpHistory, added int) (int, error) {
	if len(hs) == 0 {
		return added, nil
	}
	have := map[string]bool{}
	rows, err := tx.QueryContext(ctx,
		"SELECT action, changed_by, changed_at, snapshot FROM memory_history WHERE memory_id = ?", id)
	if err != nil {
		return added, fmt.Errorf("read history of #%d: %w", id, err)
	}
	for rows.Next() {
		var action, by, at, snap string
		if err := rows.Scan(&action, &by, &at, &snap); err != nil {
			_ = rows.Close()
			return added, fmt.Errorf("read history of #%d: %w", id, err)
		}
		have[historyKey(Action(action), by, at, []byte(snap))] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return added, fmt.Errorf("read history of #%d: %w", id, err)
	}
	_ = rows.Close()
	for _, h := range hs {
		key := historyKey(h.Action, normalizeActor(h.ChangedBy), FormatTime(h.ChangedAt), h.Snapshot)
		if have[key] {
			continue
		}
		if err := insertHistoryRow(ctx, tx, id, h); err != nil {
			return added, err
		}
		have[key] = true
		added++
	}
	return added, nil
}

// changeLabel names a memory in a report: its title, or the start of its
// content when it has none.
func changeLabel(title, content string) string {
	if title != "" {
		return title
	}
	line, _, _ := strings.Cut(strings.TrimSpace(content), "\n")
	if r := []rune(line); len(r) > 60 {
		line = string(r[:60]) + "…"
	}
	return line
}
