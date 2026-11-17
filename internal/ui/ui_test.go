package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Morditux/llm-memory/internal/store"
)

var csrfMetaRE = regexp.MustCompile(`<meta name="csrf-token" content="([0-9a-f]+)"`)

func extractCSRFToken(t *testing.T, h http.Handler) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8765/", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d: %s", rec.Code, rec.Body.String())
	}
	m := csrfMetaRE.FindStringSubmatch(rec.Body.String())
	if len(m) != 2 || m[1] == "" {
		t.Fatalf("missing csrf-token meta tag in index.html")
	}
	return m[1]
}

func uiReq(method, path, body, token string) *http.Request {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, "http://127.0.0.1:8765"+path, r)
	if method != http.MethodGet && method != http.MethodHead {
		req.Header.Set("X-CSRF-Token", token)
		req.Header.Set("Origin", "http://127.0.0.1:8765")
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
	}
	return req
}

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
	tok := extractCSRFToken(t, h)

	// 1. Index HTML (token embedded, placeholder replaced, no-store cache)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, uiReq(http.MethodGet, "/", "", tok))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "llm-memory") || strings.Contains(rec.Body.String(), "__CSRF_TOKEN__") {
		t.Fatalf("GET / = %d", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}

	// 2. Stats & Namespaces & Memories & Search
	for _, path := range []string{"/api/stats", "/api/namespaces", "/api/memories?namespace=demo", "/api/search?q=SQLite"} {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, uiReq(http.MethodGet, path, "", tok))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", path, rec.Code, rec.Body.String())
		}
	}

	// 3. Detail
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, uiReq(http.MethodGet, fmt.Sprintf("/api/memories/%d", m.ID), "", tok))
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
	h.ServeHTTP(rec, uiReq(http.MethodPost, fmt.Sprintf("/api/memories/%d/archive", m.ID), "", tok))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST archive = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, uiReq(http.MethodPost, fmt.Sprintf("/api/memories/%d/restore", m.ID), "", tok))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST restore = %d", rec.Code)
	}

	// 5. Create via POST /api/memories
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, uiReq(http.MethodPost, "/api/memories",
		`{"namespace":"demo","kind":"decision","title":"Use SQLite WAL","content":"SQLite single-binary deployment with WAL"}`, tok))
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/memories = %d: %s", rec.Code, rec.Body.String())
	}
	var created store.Memory
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	// 6. Update via PUT /api/memories/{id}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, uiReq(http.MethodPut, fmt.Sprintf("/api/memories/%d", created.ID),
		`{"importance":3,"tags":["db","sqlite"]}`, tok))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /api/memories/%d = %d: %s", created.ID, rec.Code, rec.Body.String())
	}

	// 7. Similar & Supersede
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, uiReq(http.MethodGet, fmt.Sprintf("/api/memories/%d/similar", created.ID), "", tok))
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
	h.ServeHTTP(rec, uiReq(http.MethodPost, fmt.Sprintf("/api/memories/%d/supersede", created.ID),
		fmt.Sprintf(`{"target_id":%d}`, m.ID), tok))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST supersede = %d: %s", rec.Code, rec.Body.String())
	}
	gotOld, err := st.Get(ctx, m.ID)
	if err != nil || gotOld.ArchivedAt == nil {
		t.Fatalf("expected #%d to be archived after supersede, got %+v, %v", m.ID, gotOld, err)
	}

	// 8. GC endpoint
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, uiReq(http.MethodPost, "/api/gc", "", tok))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/gc = %d: %s", rec.Code, rec.Body.String())
	}

	// 9. Links API & Pagination & Tag filter & Namespace management
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, uiReq(http.MethodPost, fmt.Sprintf("/api/memories/%d/links", created.ID),
		fmt.Sprintf(`{"target_id":%d,"kind":"relates_to"}`, m.ID), tok))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST link = %d: %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, uiReq(http.MethodPost, fmt.Sprintf("/api/memories/%d/links/remove", created.ID),
		fmt.Sprintf(`{"target_id":%d,"kind":"relates_to"}`, m.ID), tok))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST remove link = %d: %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, uiReq(http.MethodPost, fmt.Sprintf("/api/memories/%d/links/remove", created.ID),
		fmt.Sprintf(`{"target_id":%d,"kind":"supersedes"}`, m.ID), tok))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST remove supersedes link = %d, want 400: %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, uiReq(http.MethodGet, "/api/memories?namespace=demo&tag=sqlite&limit=1&offset=0", "", tok))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "SQLite") {
		t.Fatalf("GET paginated tag memories = %d: %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, uiReq(http.MethodPut, "/api/namespaces/demo", `{"description":"Demo project"}`, tok))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT namespace = %d: %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, uiReq(http.MethodPost, "/api/namespaces/demo/rename", `{"new_name":"demo2"}`, tok))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST rename namespace = %d: %s", rec.Code, rec.Body.String())
	}
}

type stubUISearcher struct {
	similar []store.Similar
}

func (s *stubUISearcher) Search(_ context.Context, _ store.SearchQuery) ([]store.SearchResult, error) {
	return nil, nil
}

func (s *stubUISearcher) FindSimilar(_ context.Context, _, _ string, _ float64, _ int) ([]store.Similar, error) {
	return s.similar, nil
}

func TestUISemanticSimilar(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "mem.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	m1, _ := st.Create(ctx, store.NewMemory{Namespace: "demo", Kind: store.KindFact, Title: "A", Content: "PostgreSQL database"})
	m2, _ := st.Create(ctx, store.NewMemory{Namespace: "demo", Kind: store.KindFact, Title: "B", Content: "Completely different words"})

	h := NewHandler(st, &stubUISearcher{
		similar: []store.Similar{{ID: m2.ID, Kind: m2.Kind, Title: m2.Title, Snippet: m2.Content, Similarity: 0.91}},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, uiReq(http.MethodGet, fmt.Sprintf("/api/memories/%d/similar", m1.ID), "", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET similar = %d: %s", rec.Code, rec.Body.String())
	}
	var sims []store.Similar
	if err := json.Unmarshal(rec.Body.Bytes(), &sims); err != nil {
		t.Fatal(err)
	}
	if len(sims) != 1 || sims[0].ID != m2.ID {
		t.Fatalf("expected semantic similar #%d, got %+v", m2.ID, sims)
	}
}

func TestUISecurityGuards(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "mem.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	h := NewHandler(st, nil)
	tok := extractCSRFToken(t, h)

	const payload = `{"namespace":"global","kind":"preference","importance":3,"content":"Injected prompt"}`

	// 1. Cross-origin simple POST (text/plain, no CSRF token, evil Origin) is rejected.
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8765/api/memories", strings.NewReader(payload))
	r.Header.Set("Content-Type", "text/plain")
	r.Header.Set("Origin", "https://evil.example")
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin text/plain POST = %d, want 403", rec.Code)
	}

	// 2. Missing or wrong CSRF token is rejected even with same Origin and application/json.
	for _, badTok := range []string{"", "wrong-token"} {
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, uiReq(http.MethodPost, "/api/memories", payload, badTok))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("bad csrf token %q POST = %d, want 403", badTok, rec.Code)
		}
	}

	// 3. Wrong Content-Type is rejected even with valid CSRF token and same Origin.
	rec = httptest.NewRecorder()
	r = uiReq(http.MethodPost, "/api/memories", payload, tok)
	r.Header.Set("Content-Type", "text/plain")
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain with valid token = %d, want 415", rec.Code)
	}

	// 4. Null Origin or Sec-Fetch-Site: cross-site is rejected.
	rec = httptest.NewRecorder()
	r = uiReq(http.MethodPost, "/api/gc", "", tok)
	r.Header.Set("Origin", "null")
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("Origin: null POST = %d, want 403", rec.Code)
	}

	rec = httptest.NewRecorder()
	r = uiReq(http.MethodPost, "/api/gc", "", tok)
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("Sec-Fetch-Site: cross-site POST = %d, want 403", rec.Code)
	}

	// 5. DNS rebinding (non-loopback Host) is rejected on both GET and POST by default.
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		rec = httptest.NewRecorder()
		r = httptest.NewRequest(method, "http://evil.example:8765/api/stats", nil)
		if method == http.MethodPost {
			r = httptest.NewRequest(method, "http://evil.example:8765/api/gc", nil)
			r.Header.Set("X-CSRF-Token", tok)
		}
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("non-loopback Host %s = %d, want 403", method, rec.Code)
		}
	}

	// 6. Verify no memory was created by any of the blocked requests.
	if n, err := st.Count(ctx); err != nil || n != 0 {
		t.Fatalf("expected 0 memories after blocked requests, got %d (%v)", n, err)
	}

	// 7. AllowRemoteHost permits matching non-loopback Host+Origin with valid CSRF token,
	// while still rejecting mismatched Origin.
	remoteH := NewHandlerWithOptions(st, nil, Options{AllowRemoteHost: true})
	remoteTok := extractCSRFToken(t, remoteH)
	rec = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodPost, "http://192.168.1.50:8765/api/gc", nil)
	r.Header.Set("Origin", "http://192.168.1.50:8765")
	r.Header.Set("X-CSRF-Token", remoteTok)
	remoteH.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("AllowRemoteHost valid POST = %d: %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodPost, "http://192.168.1.50:8765/api/gc", nil)
	r.Header.Set("Origin", "http://attacker.example")
	r.Header.Set("X-CSRF-Token", remoteTok)
	remoteH.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("AllowRemoteHost mismatched Origin = %d, want 403", rec.Code)
	}
}
