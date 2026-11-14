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
