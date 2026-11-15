// Package ui serves a lightweight, self-contained local web explorer for llm-memory.
package ui

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Morditux/llm-memory/internal/store"
)

//go:embed static/*
var staticFS embed.FS

// Searcher abstracts store.Search and semantic.Engine.Search.
type Searcher interface {
	Search(ctx context.Context, q store.SearchQuery) ([]store.SearchResult, error)
}

// NewHandler returns the HTTP handler for the local web UI and JSON API.
func NewHandler(st *store.Store, search Searcher) http.Handler {
	if search == nil {
		search = st
	}
	sub, _ := fs.Sub(staticFS, "static")
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(sub)))
	mux.HandleFunc("GET /api/stats", func(w http.ResponseWriter, r *http.Request) {
		stats, err := st.Stats(r.Context())
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, stats)
	})
	mux.HandleFunc("GET /api/namespaces", func(w http.ResponseWriter, r *http.Request) {
		nss, err := st.ListNamespaces(r.Context())
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, nss)
	})
	mux.HandleFunc("GET /api/memories", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f := store.ListFilter{
			Namespace: q.Get("namespace"),
			Archived:  parseArchiveFilter(q.Get("archived")),
			Limit:     parseLimit(q.Get("limit"), 100),
		}
		if k := strings.TrimSpace(q.Get("kind")); k != "" {
			f.Kinds = []store.Kind{store.Kind(k)}
		}
		ms, err := st.List(r.Context(), f)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, ms)
	})
	mux.HandleFunc("GET /api/search", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		sq := store.SearchQuery{
			Query:           q.Get("q"),
			Namespace:       q.Get("namespace"),
			IncludeArchived: q.Get("archived") != "active" && q.Get("archived") != "",
			Limit:           parseLimit(q.Get("limit"), 50),
		}
		if k := strings.TrimSpace(q.Get("kind")); k != "" {
			sq.Kinds = []store.Kind{store.Kind(k)}
		}
		res, err := search.Search(r.Context(), sq)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, res)
	})
	mux.HandleFunc("GET /api/memories/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseID(w, r)
		if !ok {
			return
		}
		m, err := st.Get(r.Context(), id)
		if err != nil {
			writeErr(w, err)
			return
		}
		hist, err := st.History(r.Context(), id)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, map[string]any{"memory": m, "history": hist})
	})
	mux.HandleFunc("POST /api/memories/{id}/archive", func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseID(w, r)
		if !ok {
			return
		}
		m, err := st.Archive(r.Context(), id, "ui", "archived from web UI")
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, m)
	})
	mux.HandleFunc("POST /api/memories/{id}/restore", func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseID(w, r)
		if !ok {
			return
		}
		m, err := st.Restore(r.Context(), id, "ui")
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, m)
	})
	mux.HandleFunc("POST /api/memories", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Namespace  string     `json:"namespace"`
			Kind       store.Kind `json:"kind"`
			Title      string     `json:"title"`
			Content    string     `json:"content"`
			Tags       []string   `json:"tags"`
			Importance int        `json:"importance"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		m, err := st.Create(r.Context(), store.NewMemory{
			Namespace:  body.Namespace,
			Kind:       body.Kind,
			Title:      body.Title,
			Content:    body.Content,
			Tags:       body.Tags,
			Importance: body.Importance,
			Source:     "ui",
		})
		if err != nil {
			writeErr(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(m)
	})
	mux.HandleFunc("PUT /api/memories/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseID(w, r)
		if !ok {
			return
		}
		var body struct {
			Kind       *store.Kind `json:"kind"`
			Title      *string     `json:"title"`
			Content    *string     `json:"content"`
			Tags       *[]string   `json:"tags"`
			Importance *int        `json:"importance"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		m, err := st.Update(r.Context(), id, store.Patch{
			Kind:       body.Kind,
			Title:      body.Title,
			Content:    body.Content,
			Tags:       body.Tags,
			Importance: body.Importance,
		}, "ui")
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, m)
	})
	mux.HandleFunc("GET /api/memories/{id}/similar", func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseID(w, r)
		if !ok {
			return
		}
		m, err := st.Get(r.Context(), id)
		if err != nil {
			writeErr(w, err)
			return
		}
		q := strings.TrimSpace(m.Title + " " + m.Content)
		sims, err := st.FindSimilar(r.Context(), q, m.Namespace, 5, id)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, sims)
	})
	mux.HandleFunc("POST /api/memories/{id}/supersede", func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseID(w, r)
		if !ok {
			return
		}
		var body struct {
			TargetID int64 `json:"target_id"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		if body.TargetID <= 0 {
			http.Error(w, "invalid target_id", http.StatusBadRequest)
			return
		}
		if body.TargetID == id {
			http.Error(w, "cannot supersede self", http.StatusBadRequest)
			return
		}
		src, err := st.Get(r.Context(), id)
		if err != nil {
			writeErr(w, err)
			return
		}
		if src.ArchivedAt != nil {
			http.Error(w, fmt.Sprintf("memory #%d is archived", id), http.StatusConflict)
			return
		}
		m, err := st.Archive(r.Context(), body.TargetID, "ui", fmt.Sprintf("superseded by #%d", id))
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, m)
	})
	mux.HandleFunc("POST /api/gc", func(w http.ResponseWriter, r *http.Request) {
		n, err := st.ArchiveExpired(r.Context(), time.Now().UTC(), "ui")
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, map[string]int64{"archived": n})
	})
	return mux
}

func parseID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return 0, false
	}
	return id, true
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		http.Error(w, "invalid json: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

func parseArchiveFilter(s string) store.ArchiveFilter {
	switch s {
	case "archived":
		return store.ArchivedOnly
	case "all":
		return store.IncludeArchived
	default:
		return store.ActiveOnly
	}
}

func parseLimit(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return n
	}
	return def
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	var nf *store.NotFoundError
	var nsf *store.NamespaceNotFoundError
	var ve *store.ValidationError
	var se *store.StateError
	switch {
	case errors.As(err, &nf), errors.As(err, &nsf):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.As(err, &ve):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.As(err, &se):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
