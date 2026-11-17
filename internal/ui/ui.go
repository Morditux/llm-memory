// Package ui serves a lightweight, self-contained local web explorer for llm-memory.
package ui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Morditux/llm-memory/internal/store"
)

//go:embed static/*
var staticFS embed.FS

// Options configures NewHandlerWithOptions.
type Options struct {
	// AllowRemoteHost permits non-loopback Host headers (when --addr binds a
	// non-loopback interface). Origin, Sec-Fetch-Site, Content-Type and CSRF
	// token checks remain enforced.
	AllowRemoteHost bool
}

// Searcher abstracts store.Search and semantic.Engine.Search.
type Searcher interface {
	Search(ctx context.Context, q store.SearchQuery) ([]store.SearchResult, error)
}

type similarSearcher interface {
	FindSimilar(ctx context.Context, text, namespace string, minScore float64, limit int) ([]store.Similar, error)
}

// NewHandler returns the HTTP handler for the local web UI and JSON API.
func NewHandler(st *store.Store, search Searcher) http.Handler {
	return NewHandlerWithOptions(st, search, Options{})
}

// NewHandlerWithOptions returns the HTTP handler for the web UI and JSON API.
func NewHandlerWithOptions(st *store.Store, search Searcher, opts Options) http.Handler {
	if search == nil {
		search = st
	}
	var tok [32]byte
	_, _ = rand.Read(tok[:])
	csrfToken := hex.EncodeToString(tok[:])

	rawHTML, _ := staticFS.ReadFile("static/index.html")
	indexHTML := []byte(strings.Replace(string(rawHTML), "__CSRF_TOKEN__", csrfToken, 1))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/index.html" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(indexHTML)
	})
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
	mux.HandleFunc("PUT /api/namespaces/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		var body struct {
			Description string `json:"description"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		norm, err := st.RequireNamespace(r.Context(), name)
		if err != nil {
			writeErr(w, err)
			return
		}
		if _, err := st.EnsureNamespace(r.Context(), norm, strings.TrimSpace(body.Description)); err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, map[string]string{"name": norm, "description": strings.TrimSpace(body.Description)})
	})
	mux.HandleFunc("POST /api/namespaces/{name}/rename", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		var body struct {
			NewName string `json:"new_name"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		ns, err := st.RenameNamespace(r.Context(), name, body.NewName, "ui")
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, map[string]string{"name": ns})
	})
	mux.HandleFunc("POST /api/namespaces/{name}/merge", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		var body struct {
			Target string `json:"target"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		moved, err := st.MergeNamespace(r.Context(), name, body.Target, "ui")
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, map[string]any{"moved": moved, "target": body.Target})
	})
	mux.HandleFunc("POST /api/namespaces/{name}/archive", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		var body struct {
			Reason string `json:"reason"`
		}
		if r.ContentLength != 0 && !decodeJSON(w, r, &body) {
			return
		}
		n, err := st.ArchiveNamespace(r.Context(), name, "ui", body.Reason)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, map[string]int64{"archived": n})
	})
	mux.HandleFunc("GET /api/memories", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f := store.ListFilter{
			Namespace: q.Get("namespace"),
			Tags:      parseTagParam(q.Get("tag")),
			Archived:  parseArchiveFilter(q.Get("archived")),
			Limit:     parseLimit(q.Get("limit"), 100),
			Offset:    parseNonNegInt(q.Get("offset"), 0),
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
			Tags:            parseTagParam(q.Get("tag")),
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
		links, err := st.Links(r.Context(), id)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, map[string]any{"memory": m, "history": hist, "links": links})
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
			ExpiresIn  string     `json:"expires_in"`
			ExpiresAt  string     `json:"expires_at"`
			RelatedTo  []int64    `json:"related_to"`
			DependsOn  []int64    `json:"depends_on"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		exp, err := parseExpiration(body.ExpiresIn, body.ExpiresAt)
		if err != nil {
			writeErr(w, err)
			return
		}
		var links []store.LinkRef
		for _, id := range body.RelatedTo {
			links = append(links, store.LinkRef{TargetID: id, Kind: store.LinkRelatesTo})
		}
		for _, id := range body.DependsOn {
			links = append(links, store.LinkRef{TargetID: id, Kind: store.LinkDependsOn})
		}
		m, err := st.Create(r.Context(), store.NewMemory{
			Namespace:  body.Namespace,
			Kind:       body.Kind,
			Title:      body.Title,
			Content:    body.Content,
			Tags:       body.Tags,
			Importance: body.Importance,
			Source:     "ui",
			Links:      links,
			ExpiresAt:  exp,
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
			Namespace    *string     `json:"namespace"`
			Kind         *store.Kind `json:"kind"`
			Title        *string     `json:"title"`
			Content      *string     `json:"content"`
			Tags         *[]string   `json:"tags"`
			Importance   *int        `json:"importance"`
			ExpiresIn    string      `json:"expires_in"`
			ExpiresAt    string      `json:"expires_at"`
			ClearExpires bool        `json:"clear_expires"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		exp, err := parseExpiration(body.ExpiresIn, body.ExpiresAt)
		if err != nil {
			writeErr(w, err)
			return
		}
		m, err := st.Update(r.Context(), id, store.Patch{
			Namespace:      body.Namespace,
			Kind:           body.Kind,
			Title:          body.Title,
			Content:        body.Content,
			Tags:           body.Tags,
			Importance:     body.Importance,
			ExpiresAt:      exp,
			ClearExpiresAt: body.ClearExpires,
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
		if sf, ok := search.(similarSearcher); ok {
			if sems, serr := sf.FindSimilar(r.Context(), q, m.Namespace, 0, 5); serr == nil {
				filtered := sems[:0]
				for _, s := range sems {
					if s.ID != id {
						filtered = append(filtered, s)
					}
				}
				sims = mergeSimilar(sims, filtered, 5)
			}
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
		m, err := st.Supersede(r.Context(), id, body.TargetID, "ui")
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, m)
	})
	mux.HandleFunc("POST /api/memories/{id}/links", func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseID(w, r)
		if !ok {
			return
		}
		var body struct {
			TargetID int64          `json:"target_id"`
			Kind     store.LinkKind `json:"kind"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		if err := st.AddLink(r.Context(), id, body.TargetID, body.Kind, "ui"); err != nil {
			writeErr(w, err)
			return
		}
		links, err := st.Links(r.Context(), id)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, links)
	})
	mux.HandleFunc("POST /api/memories/{id}/links/remove", func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseID(w, r)
		if !ok {
			return
		}
		var body struct {
			TargetID int64          `json:"target_id"`
			Kind     store.LinkKind `json:"kind"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		if err := st.RemoveLink(r.Context(), id, body.TargetID, body.Kind, "ui"); err != nil {
			writeErr(w, err)
			return
		}
		links, err := st.Links(r.Context(), id)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, links)
	})
	mux.HandleFunc("POST /api/gc", func(w http.ResponseWriter, r *http.Request) {
		n, err := st.ArchiveExpired(r.Context(), time.Now().UTC(), "ui")
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, map[string]int64{"archived": n})
	})
	return securityMiddleware(mux, csrfToken, opts)
}

func securityMiddleware(next http.Handler, csrfToken string, opts Options) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")

		if !opts.AllowRemoteHost && !isLoopbackHost(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && !isSameOrigin(origin, r.Host) {
			http.Error(w, "forbidden origin", http.StatusForbidden)
			return
		}
		isMutating := r.Method != http.MethodGet && r.Method != http.MethodHead
		if isMutating || strings.HasPrefix(r.URL.Path, "/api/") {
			if sfs := r.Header.Get("Sec-Fetch-Site"); sfs != "" && sfs != "same-origin" && sfs != "none" {
				http.Error(w, "cross-site request blocked", http.StatusForbidden)
				return
			}
		}
		if isMutating {
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(csrfToken)) != 1 {
				http.Error(w, "invalid or missing csrf token", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func isLoopbackHost(hostport string) bool {
	if hostport == "" {
		return false
	}
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	} else {
		host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func isSameOrigin(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	return strings.EqualFold(u.Host, host)
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
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return false
	}
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

func parseNonNegInt(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil && n >= 0 {
		return n
	}
	return def
}

func parseTagParam(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return store.NormalizeTags(strings.Split(s, ","))
}

func parseExpiration(expiresIn, expiresAt string) (*time.Time, error) {
	in := strings.TrimSpace(expiresIn)
	at := strings.TrimSpace(expiresAt)
	if in != "" && at != "" {
		return nil, &store.ValidationError{Field: "expires", Msg: "provide either expires_in or expires_at, not both"}
	}
	if in != "" {
		if strings.HasSuffix(in, "d") {
			days, err := strconv.Atoi(strings.TrimSuffix(in, "d"))
			if err != nil || days <= 0 {
				return nil, &store.ValidationError{Field: "expires_in", Msg: fmt.Sprintf("invalid duration %q", expiresIn)}
			}
			t := time.Now().UTC().Add(time.Duration(days) * 24 * time.Hour)
			return &t, nil
		}
		d, err := time.ParseDuration(in)
		if err != nil || d <= 0 {
			return nil, &store.ValidationError{Field: "expires_in", Msg: fmt.Sprintf("invalid duration %q", expiresIn)}
		}
		t := time.Now().UTC().Add(d)
		return &t, nil
	}
	if at != "" {
		t, err := time.Parse(time.RFC3339, at)
		if err != nil {
			return nil, &store.ValidationError{Field: "expires_at", Msg: fmt.Sprintf("invalid RFC 3339 timestamp %q", expiresAt)}
		}
		utc := t.UTC()
		return &utc, nil
	}
	return nil, nil
}

func mergeSimilar(fts, sem []store.Similar, limit int) []store.Similar {
	if len(sem) == 0 {
		return fts
	}
	byID := make(map[int64]store.Similar, len(fts)+len(sem))
	for _, s := range fts {
		byID[s.ID] = s
	}
	for _, s := range sem {
		if cur, ok := byID[s.ID]; !ok || s.Similarity > cur.Similarity {
			byID[s.ID] = s
		}
	}
	out := make([]store.Similar, 0, len(byID))
	for _, s := range byID {
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b store.Similar) int {
		if a.Similarity > b.Similarity {
			return -1
		}
		if a.Similarity < b.Similarity {
			return 1
		}
		if a.ID > b.ID {
			return -1
		}
		if a.ID < b.ID {
			return 1
		}
		return 0
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
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
