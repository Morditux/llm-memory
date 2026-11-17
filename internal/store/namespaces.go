package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ensureNamespace creates ns (already normalised) if it does not exist.
func ensureNamespace(ctx context.Context, q queryer, ns string) error {
	if _, err := q.ExecContext(ctx, "INSERT OR IGNORE INTO namespaces(name) VALUES (?)", ns); err != nil {
		return fmt.Errorf("create namespace %q: %w", ns, err)
	}
	return nil
}

// requireNamespace returns a NamespaceNotFoundError if ns does not exist.
func requireNamespace(ctx context.Context, q queryer, ns string) error {
	var one int
	err := q.QueryRowContext(ctx, "SELECT 1 FROM namespaces WHERE name = ?", ns).Scan(&one)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("look up namespace %q: %w", ns, err)
	}
	names, lerr := namespaceNames(ctx, q)
	if lerr != nil {
		return lerr
	}
	return &NamespaceNotFoundError{Name: ns, Existing: names}
}

func namespaceNames(ctx context.Context, q queryer) ([]string, error) {
	rows, err := q.QueryContext(ctx, "SELECT name FROM namespaces ORDER BY name")
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("list namespaces: %w", err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}
	return names, nil
}

// RequireNamespace normalises name and checks that it exists. It returns the
// normalised name, or a *NamespaceNotFoundError listing existing namespaces.
func (s *Store) RequireNamespace(ctx context.Context, name string) (string, error) {
	ns, err := NormalizeNamespace(name)
	if err != nil {
		return "", err
	}
	return ns, requireNamespace(ctx, s.db, ns)
}

// EnsureNamespace normalises name and creates it if missing. A non-empty
// description replaces the current one.
func (s *Store) EnsureNamespace(ctx context.Context, name, description string) (string, error) {
	ns, err := NormalizeNamespace(name)
	if err != nil {
		return "", err
	}
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		if err := ensureNamespace(ctx, tx, ns); err != nil {
			return err
		}
		if description == "" {
			return nil
		}
		if _, err := tx.ExecContext(ctx, "UPDATE namespaces SET description = ? WHERE name = ?", description, ns); err != nil {
			return fmt.Errorf("describe namespace %q: %w", ns, err)
		}
		return nil
	})
	return ns, err
}

// ListNamespaces returns every namespace with its number of active memories
// and the timestamp of the last write to any of its memories.
func (s *Store) ListNamespaces(ctx context.Context) ([]Namespace, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT n.name, n.description, n.created_at,
		       COUNT(m.id) FILTER (WHERE m.archived_at IS NULL),
		       MAX(m.updated_at)
		FROM namespaces n
		LEFT JOIN memories m ON m.namespace = n.name
		GROUP BY n.name
		ORDER BY n.name`)
	if err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []Namespace{}
	for rows.Next() {
		var (
			n       Namespace
			created string
			last    sql.NullString
		)
		if err := rows.Scan(&n.Name, &n.Description, &created, &n.ActiveCount, &last); err != nil {
			return nil, fmt.Errorf("list namespaces: %w", err)
		}
		if n.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}
		if last.Valid {
			t, err := parseTime(last.String)
			if err != nil {
				return nil, err
			}
			n.LastActivity = &t
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list namespaces: %w", err)
	}
	return out, nil
}

func recordNamespaceHistory(ctx context.Context, tx *sql.Tx, ids []int64, by, reason string) error {
	for _, id := range ids {
		if _, err := writeHistory(ctx, tx, id, ActionUpdate, by, reason); err != nil {
			return err
		}
	}
	return nil
}

// RenameNamespace renames oldName to newName (updating all memories via ON UPDATE CASCADE)
// and records an update history snapshot for every moved memory.
// The "global" namespace cannot be renamed, and newName must not already exist.
func (s *Store) RenameNamespace(ctx context.Context, oldName, newName, by string) (string, error) {
	oldNS, err := NormalizeNamespace(oldName)
	if err != nil {
		return "", err
	}
	newNS, err := NormalizeNamespace(newName)
	if err != nil {
		return "", err
	}
	if oldNS == DefaultNamespace {
		return "", &ValidationError{Field: "namespace", Msg: "cannot rename the global namespace"}
	}
	if oldNS == newNS {
		return "", &ValidationError{Field: "namespace", Msg: "source and target namespaces are identical"}
	}
	by = normalizeActor(by)
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		if err := requireNamespace(ctx, tx, oldNS); err != nil {
			return err
		}
		var exists int
		err := tx.QueryRowContext(ctx, "SELECT 1 FROM namespaces WHERE name = ?", newNS).Scan(&exists)
		if err == nil {
			return &ValidationError{Field: "namespace", Msg: fmt.Sprintf("namespace %q already exists (use merge instead)", newNS)}
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check namespace %q: %w", newNS, err)
		}
		ids, err := collectIDs(ctx, tx, "SELECT id FROM memories WHERE namespace = ? ORDER BY id", oldNS)
		if err != nil {
			return fmt.Errorf("list memories in %q: %w", oldNS, err)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE namespaces SET name = ? WHERE name = ?", newNS, oldNS); err != nil {
			return fmt.Errorf("rename namespace %q to %q: %w", oldNS, newNS, err)
		}
		if len(ids) > 0 {
			if _, err := tx.ExecContext(ctx,
				"UPDATE memories SET updated_at = "+sqlNow+", updated_by = ? WHERE namespace = ?",
				by, newNS); err != nil {
				return fmt.Errorf("update memories after rename %q to %q: %w", oldNS, newNS, err)
			}
			if err := recordNamespaceHistory(ctx, tx, ids, by, fmt.Sprintf("namespace renamed %q -> %q", oldNS, newNS)); err != nil {
				return err
			}
		}
		return nil
	})
	return newNS, err
}

// MergeNamespace moves all memories from src into dst, records an update
// history snapshot for each moved memory, enforces at most one active handoff
// in dst (keeping the most recent), and removes src.
// The "global" namespace cannot be merged away.
func (s *Store) MergeNamespace(ctx context.Context, src, dst, by string) (int64, error) {
	srcNS, err := NormalizeNamespace(src)
	if err != nil {
		return 0, err
	}
	dstNS, err := NormalizeNamespace(dst)
	if err != nil {
		return 0, err
	}
	if srcNS == DefaultNamespace {
		return 0, &ValidationError{Field: "namespace", Msg: "cannot merge the global namespace"}
	}
	if srcNS == dstNS {
		return 0, &ValidationError{Field: "namespace", Msg: "source and target namespaces are identical"}
	}
	by = normalizeActor(by)
	var moved int64
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		if err := requireNamespace(ctx, tx, srcNS); err != nil {
			return err
		}
		if err := ensureNamespace(ctx, tx, dstNS); err != nil {
			return err
		}
		ids, err := collectIDs(ctx, tx, "SELECT id FROM memories WHERE namespace = ? ORDER BY id", srcNS)
		if err != nil {
			return fmt.Errorf("list memories in %q: %w", srcNS, err)
		}
		res, err := tx.ExecContext(ctx,
			"UPDATE memories SET namespace = ?, updated_at = "+sqlNow+", updated_by = ? WHERE namespace = ?",
			dstNS, by, srcNS)
		if err != nil {
			return fmt.Errorf("move memories from %q to %q: %w", srcNS, dstNS, err)
		}
		moved, _ = res.RowsAffected()
		if err := recordNamespaceHistory(ctx, tx, ids, by, fmt.Sprintf("namespace merged %q -> %q", srcNS, dstNS)); err != nil {
			return err
		}

		var keepID int64
		err = tx.QueryRowContext(ctx,
			"SELECT id FROM memories WHERE namespace = ? AND kind = 'handoff' AND archived_at IS NULL ORDER BY updated_at DESC, id DESC LIMIT 1",
			dstNS).Scan(&keepID)
		if err == nil {
			if _, err := archiveOtherHandoffs(ctx, tx, dstNS, keepID, by); err != nil {
				return err
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check active handoffs in %q: %w", dstNS, err)
		}

		if _, err := tx.ExecContext(ctx, "DELETE FROM namespaces WHERE name = ?", srcNS); err != nil {
			return fmt.Errorf("delete namespace %q: %w", srcNS, err)
		}
		return nil
	})
	return moved, err
}

// ArchiveNamespace archives all active memories in name and returns how many were archived.
func (s *Store) ArchiveNamespace(ctx context.Context, name, by, reason string) (int64, error) {
	ns, err := NormalizeNamespace(name)
	if err != nil {
		return 0, err
	}
	by = normalizeActor(by)
	if reason == "" {
		reason = fmt.Sprintf("namespace %q archived", ns)
	}
	var count int64
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		if err := requireNamespace(ctx, tx, ns); err != nil {
			return err
		}
		ids, err := collectIDs(ctx, tx, "SELECT id FROM memories WHERE namespace = ? AND archived_at IS NULL ORDER BY id", ns)
		if err != nil {
			return fmt.Errorf("list active memories in %q: %w", ns, err)
		}

		for _, id := range ids {
			res, err := archive(ctx, tx, id, by, reason, true)
			if err != nil {
				return err
			}
			if res.changed {
				count++
			}
		}
		return nil
	})
	return count, err
}
