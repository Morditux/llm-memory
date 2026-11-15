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

	// 5. Create via POST /api/memories
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/memories",
		strings.NewReader(`{"namespace":"demo","kind":"decision","title":"Use SQLite WAL","content":"SQLite single-binary deployment with WAL"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/memories = %d: %s", rec.Code, rec.Body.String())
	}
	var created store.Memory
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	// 6. Update via PUT /api/memories/{id}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, fmt.Sprintf("/api/memories/%d", created.ID),
		strings.NewReader(`{"importance":3,"tags":["db","sqlite"]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /api/memories/%d = %d: %s", created.ID, rec.Code, rec.Body.String())
	}

	// 7. Similar & Supersede
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/memories/%d/similar", created.ID), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET similar = %d: %s", rec.Code, rec.Body.String())
	}
	var sims []store.Similar
	if err := json.Unmarshal(rec.Body.Bytes(), &sims); err != nil {
		t.Fatal(err)
	}
	if len(sims) == 0 || sims[0].ID != m.ID {
		t.Fatalf("expected similar memory #%d, got %+v", m.ID, sims)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/memories/%d/supersede", created.ID),
		strings.NewReader(fmt.Sprintf(`{"target_id":%d}`, m.ID))))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST supersede = %d: %s", rec.Code, rec.Body.String())
	}
	gotOld, err := st.Get(ctx, m.ID)
	if err != nil || gotOld.ArchivedAt == nil {
		t.Fatalf("expected #%d to be archived after supersede, got %+v, %v", m.ID, gotOld, err)
	}

	// 8. GC endpoint
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/gc", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/gc = %d: %s", rec.Code, rec.Body.String())
	}
}
