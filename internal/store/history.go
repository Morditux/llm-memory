package store

import (
	"context"
	"encoding/json"
	"fmt"
)

// Decode returns the memory as it was after this action, and the optional
// reason recorded with it (archive).
func (h HistoryEntry) Decode() (Memory, string, error) {
	var snap snapshot
	if err := json.Unmarshal([]byte(h.Snapshot), &snap); err != nil {
		return Memory{}, "", fmt.Errorf("decode history #%d: %w", h.ID, err)
	}
	return snap.Memory, snap.Reason, nil
}

// History returns the history of a memory, oldest first.
func (s *Store) History(ctx context.Context, id int64) ([]HistoryEntry, error) {
	if _, err := getMemory(ctx, s.db, id); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, memory_id, action, snapshot, changed_by, changed_at
		 FROM memory_history WHERE memory_id = ? ORDER BY changed_at, id`, id)
	if err != nil {
		return nil, fmt.Errorf("history of #%d: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	out := []HistoryEntry{}
	for rows.Next() {
		var (
			h       HistoryEntry
			action  string
			changed string
		)
		if err := rows.Scan(&h.ID, &h.MemoryID, &action, &h.Snapshot, &h.ChangedBy, &changed); err != nil {
			return nil, fmt.Errorf("history of #%d: %w", id, err)
		}
		h.Action = Action(action)
		if h.ChangedAt, err = parseTime(changed); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("history of #%d: %w", id, err)
	}
	return out, nil
}
