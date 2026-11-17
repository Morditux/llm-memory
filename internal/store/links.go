package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

func validateLinkKind(k LinkKind) error {
	if !k.Valid() {
		names := make([]string, len(LinkKinds))
		for i, v := range LinkKinds {
			names[i] = string(v)
		}
		return &ValidationError{Field: "link_kind", Msg: fmt.Sprintf("%q is not valid; use one of %s", k, strings.Join(names, ", "))}
	}
	return nil
}

func insertLink(ctx context.Context, q queryer, srcID, dstID int64, kind LinkKind) (bool, error) {
	if err := validateLinkKind(kind); err != nil {
		return false, err
	}
	if srcID <= 0 || dstID <= 0 {
		return false, &ValidationError{Field: "link", Msg: "memory ids must be positive"}
	}
	if srcID == dstID {
		return false, &ValidationError{Field: "link", Msg: "cannot link a memory to itself"}
	}
	if _, err := getMemory(ctx, q, srcID); err != nil {
		return false, err
	}
	if _, err := getMemory(ctx, q, dstID); err != nil {
		return false, err
	}
	res, err := q.ExecContext(ctx,
		"INSERT OR IGNORE INTO memory_links(src_id, dst_id, kind) VALUES (?, ?, ?)",
		srcID, dstID, string(kind))
	if err != nil {
		return false, fmt.Errorf("create link #%d -> #%d (%s): %w", srcID, dstID, kind, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("create link #%d -> #%d (%s): %w", srcID, dstID, kind, err)
	}
	return n > 0, nil
}

func touchAndRecordLinkHistory(ctx context.Context, tx *sql.Tx, srcID int64, by, reason string) error {
	if _, err := tx.ExecContext(ctx,
		"UPDATE memories SET updated_at = "+sqlNow+", updated_by = ? WHERE id = ?",
		by, srcID); err != nil {
		return fmt.Errorf("update memory #%d for link history: %w", srcID, err)
	}
	_, err := writeHistory(ctx, tx, srcID, ActionUpdate, by, reason)
	return err
}

// AddLink records a directed relationship srcID -> dstID of the given kind
// and records an update history entry on srcID if the link was newly created.
func (s *Store) AddLink(ctx context.Context, srcID, dstID int64, kind LinkKind, by string) error {
	by = normalizeActor(by)
	return s.withTx(ctx, func(tx *sql.Tx) error {
		inserted, err := insertLink(ctx, tx, srcID, dstID, kind)
		if err != nil || !inserted {
			return err
		}
		return touchAndRecordLinkHistory(ctx, tx, srcID, by, fmt.Sprintf("added link %s -> #%d", kind, dstID))
	})
}

// RemoveLink deletes a directed relationship srcID -> dstID of the given kind
// and records an update history entry on srcID. Supersedes links record archival
// provenance and cannot be removed.
func (s *Store) RemoveLink(ctx context.Context, srcID, dstID int64, kind LinkKind, by string) error {
	if err := validateLinkKind(kind); err != nil {
		return err
	}
	if kind == LinkSupersedes {
		return &ValidationError{Field: "link_kind", Msg: "supersedes links record archival provenance and cannot be removed"}
	}
	by = normalizeActor(by)
	return s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			"DELETE FROM memory_links WHERE src_id = ? AND dst_id = ? AND kind = ?",
			srcID, dstID, string(kind))
		if err != nil {
			return fmt.Errorf("remove link #%d -> #%d (%s): %w", srcID, dstID, kind, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("remove link: %w", err)
		}
		if n == 0 {
			return &ValidationError{Field: "link", Msg: fmt.Sprintf("no %s link from #%d to #%d", kind, srcID, dstID)}
		}
		return touchAndRecordLinkHistory(ctx, tx, srcID, by, fmt.Sprintf("removed link %s -> #%d", kind, dstID))
	})
}

// Supersede archives targetID as superseded by active memory srcID and records
// a 'supersedes' link in memory_links in the same transaction.
func (s *Store) Supersede(ctx context.Context, srcID, targetID int64, by string) (Memory, error) {
	if srcID == targetID {
		return Memory{}, &ValidationError{Field: "target_id", Msg: "cannot supersede self"}
	}
	by = normalizeActor(by)
	var out Memory
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		src, err := getMemory(ctx, tx, srcID)
		if err != nil {
			return err
		}
		if src.Archived() {
			return &StateError{ID: srcID, Msg: "is archived"}
		}
		res, err := archive(ctx, tx, targetID, by, fmt.Sprintf("superseded by #%d", srcID), false)
		if err != nil {
			return err
		}
		out = res.Memory
		_, err = insertLink(ctx, tx, srcID, targetID, LinkSupersedes)
		return err
	})
	return out, err
}

// Links returns all outgoing and incoming links for memory id.
func (s *Store) Links(ctx context.Context, id int64) ([]LinkView, error) {
	if _, err := getMemory(ctx, s.db, id); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT 'outgoing' AS direction, l.kind AS link_kind, m.id AS mem_id, m.kind AS mem_kind, m.namespace, m.title, m.archived_at IS NOT NULL, l.created_at AS linked_at
		FROM memory_links l
		JOIN memories m ON m.id = l.dst_id
		WHERE l.src_id = ?
		UNION ALL
		SELECT 'incoming' AS direction, l.kind AS link_kind, m.id AS mem_id, m.kind AS mem_kind, m.namespace, m.title, m.archived_at IS NOT NULL, l.created_at AS linked_at
		FROM memory_links l
		JOIN memories m ON m.id = l.src_id
		WHERE l.dst_id = ?
		ORDER BY linked_at DESC, mem_id DESC`, id, id)
	if err != nil {
		return nil, fmt.Errorf("links for #%d: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	out := []LinkView{}
	for rows.Next() {
		var (
			v       LinkView
			kind    string
			memKind string
			created string
		)
		if err := rows.Scan(&v.Direction, &kind, &v.ID, &memKind, &v.Namespace, &v.Title, &v.Archived, &created); err != nil {
			return nil, fmt.Errorf("links for #%d: %w", id, err)
		}
		v.Kind = LinkKind(kind)
		v.MemoryKind = Kind(memKind)
		if v.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("links for #%d: %w", id, err)
	}
	return out, nil
}
