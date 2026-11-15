package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Morditux/llm-memory/internal/store"
)

func TestUIEndpoints(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "mem.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	m, err := st.Create(ctx, store.NewMemory{
		Namespace: "demo",
		Kind:      store.KindDecision,
		Title:     "Use SQLite",
		Content:   "SQLite keeps deployment single-binary",
	})
	if err != nil {
		t.Fatal(err)
	}

	h := NewHandler(st, nil)

	// 1. Index HTML
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "llm-memory") {
		t.Fatalf("GET / = %d", rec.Code)
	}

	// 2. Stats & Namespaces & Memories & Search
	for _, path := range []string{"/api/stats", "/api/namespaces", "/api/memories?namespace=demo", "/api/search?q=SQLite"} {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", path, rec.Code, rec.Body.String())
		}
	}

	// 3. Detail
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/memories/%d", m.ID), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET detail = %d", rec.Code)
	}
	var detail struct {
		Memory  store.Memory         `json:"memory"`
		History []store.HistoryEntry `json:"history"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Memory.ID != m.ID || len(detail.History) != 1 {
		t.Fatalf("detail = %+v", detail)
	}

	// 4. Archive & Restore
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/memories/%d/archive", m.ID), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST archive = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/memories/%d/restore", m.ID), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST restore = %d", rec.Code)
	}
}
