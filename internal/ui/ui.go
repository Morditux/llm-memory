// Package ui serves a lightweight, self-contained local web explorer for llm-memory.
package ui

import (
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/Morditux/llm-memory/internal/explorer"
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
type Searcher = explorer.Searcher

// NewHandler returns the HTTP handler for the local web UI and JSON API.
func NewHandler(st *store.Store, search Searcher) http.Handler {
	return NewHandlerWithOptions(st, search, Options{})
}

// NewHandlerWithOptions returns the HTTP handler for the web UI and JSON API.
func NewHandlerWithOptions(st *store.Store, search Searcher, opts Options) http.Handler {
	ex := explorer.New(st, search, explorer.DefaultActor)
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
		stats, err := ex.Stats(r.Context())
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, stats)
	})
	mux.HandleFunc("GET /api/namespaces", func(w http.ResponseWriter, r *http.Request) {
		nss, err := ex.Namespaces(r.Context())
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
		norm, desc, err := ex.SetNamespaceDescription(r.Context(), name, body.Description)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, map[string]string{"name": norm, "description": desc})
	})
	mux.HandleFunc("POST /api/namespaces/{name}/rename", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		var body struct {
			NewName string `json:"new_name"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		ns, err := ex.RenameNamespace(r.Context(), name, body.NewName)
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
		moved, err := ex.MergeNamespace(r.Context(), name, body.Target)
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
		n, err := ex.ArchiveNamespace(r.Context(), name, body.Reason)
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
		ms, err := ex.List(r.Context(), f)
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
		res, err := ex.Search(r.Context(), sq)
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
		d, err := ex.Detail(r.Context(), id)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, d)
	})
	mux.HandleFunc("POST /api/memories/{id}/archive", func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseID(w, r)
		if !ok {
			return
		}
		m, err := ex.Archive(r.Context(), id)
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
		m, err := ex.Restore(r.Context(), id)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, m)
	})
	mux.HandleFunc("POST /api/memories", func(w http.ResponseWriter, r *http.Request) {
		var body explorer.CreateRequest
		if !decodeJSON(w, r, &body) {
			return
		}
		m, err := ex.Create(r.Context(), body)
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
		var body explorer.UpdateRequest
		if !decodeJSON(w, r, &body) {
			return
		}
		m, err := ex.Update(r.Context(), id, body)
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
		sims, err := ex.Similar(r.Context(), id)
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
		m, err := ex.Supersede(r.Context(), id, body.TargetID)
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
		links, err := ex.AddLink(r.Context(), id, body.TargetID, body.Kind)
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
		links, err := ex.RemoveLink(r.Context(), id, body.TargetID, body.Kind)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, links)
	})
	mux.HandleFunc("POST /api/gc", func(w http.ResponseWriter, r *http.Request) {
		n, err := ex.GC(r.Context())
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
