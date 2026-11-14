package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Morditux/llm-memory/internal/config"
	"github.com/Morditux/llm-memory/internal/store"
)

var update = flag.Bool("update", false, "rewrite testdata/*.golden")

var testEnv = config.Env{
	Getenv:      func(string) string { return "" },
	UserHomeDir: func() (string, error) { return "/home/test", nil },
}

type result struct {
	out, errOut string
	err         error
}

// cli runs the command with the given stdin.
func cli(t *testing.T, stdin string, args ...string) result {
	t.Helper()
	var out, errOut bytes.Buffer
	err := run(context.Background(), args, app{stdin: strings.NewReader(stdin), stdout: &out, stderr: &errOut, env: testEnv})
	return result{out.String(), errOut.String(), err}
}

func cliOK(t *testing.T, args ...string) string {
	t.Helper()
	r := cli(t, "", args...)
	if r.err != nil {
		t.Fatalf("%v: %v\nstderr: %s", args, r.err, r.errOut)
	}
	return r.out
}

var sizeRe = regexp.MustCompile(`Size: +[0-9.]+ [KMG]?i?B`)

func golden(t *testing.T, name, got, dbPath string) {
	t.Helper()
	got = strings.ReplaceAll(got, dbPath, "<DB>")
	got = sizeRe.ReplaceAllString(got, "Size:     <SIZE>")
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden (run go test ./cmd/llm-memory -update): %v", err)
	}
	if got != string(want) {
		t.Errorf("%s mismatch (run go test ./cmd/llm-memory -update to accept)\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

// seeded creates a database with fixed timestamps (loaded through Import,
// so that golden files are deterministic) and returns its path.
func seeded(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "mem.db")
	st, err := store.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	at := func(s string) time.Time {
		tm, err := time.Parse("2006-01-02 15:04", s)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	hist := func(m store.Memory, action store.Action, by string, when time.Time, reason string) store.DumpHistory {
		snap := map[string]any{"id": m.ID, "title": m.Title, "content": m.Content, "kind": m.Kind}
		if reason != "" {
			snap["reason"] = reason
		}
		b, _ := json.Marshal(snap)
		return store.DumpHistory{Action: action, Snapshot: b, ChangedBy: by, ChangedAt: when}
	}
	archived := at("2026-01-12 10:00")
	m1 := store.Memory{ID: 1, Namespace: "demo", Kind: store.KindDecision, Title: "BDD", Content: "MySQL", Importance: 2,
		Source: "claude-desktop", CreatedAt: at("2026-01-10 09:00"), UpdatedAt: archived, UpdatedBy: "vscode", ArchivedAt: &archived}
	m2 := store.Memory{ID: 2, Namespace: "demo", Kind: store.KindDecision, Title: "BDD", Content: "PostgreSQL plutôt que MySQL, pour le support JSONB.",
		Tags: []string{"db"}, Importance: 3, Source: "vscode", CreatedAt: archived, UpdatedAt: archived, UpdatedBy: "vscode"}
	m3 := store.Memory{ID: 3, Namespace: "demo", Kind: store.KindFact, Content: "Le déploiement se fait avec Docker Compose sur un VPS Hetzner, derrière Caddy pour le TLS.",
		Importance: 2, Source: "claude-code", CreatedAt: at("2026-01-13 08:30"), UpdatedAt: at("2026-01-13 08:30"), UpdatedBy: "claude-code"}
	m4 := store.Memory{ID: 4, Namespace: "global", Kind: store.KindPreference, Content: "Réponses courtes, en français.",
		Importance: 2, Source: "claude-desktop", CreatedAt: at("2026-01-05 18:00"), UpdatedAt: at("2026-01-05 18:00"), UpdatedBy: "claude-desktop"}
	d := store.Dump{FormatVersion: store.FormatVersion, Memories: []store.DumpMemory{
		{Memory: m1, History: []store.DumpHistory{
			hist(m1, store.ActionCreate, "claude-desktop", m1.CreatedAt, ""),
			hist(m1, store.ActionArchive, "vscode", archived, "superseded by #2"),
		}},
		{Memory: m2, History: []store.DumpHistory{hist(m2, store.ActionCreate, "vscode", archived, "")}},
		{Memory: m3, History: []store.DumpHistory{hist(m3, store.ActionCreate, "claude-code", m3.CreatedAt, "")}},
		{Memory: m4, History: []store.DumpHistory{hist(m4, store.ActionCreate, "claude-desktop", m4.CreatedAt, "")}},
	}}
	if _, err := st.Import(ctx, d, false, "test"); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestVersionAndHelp(t *testing.T) {
	if out := cliOK(t, "version"); !strings.HasPrefix(out, "llm-memory ") || !strings.Contains(out, fmt.Sprintf("schema %d", store.LatestSchemaVersion())) {
		t.Errorf("version: %q", out)
	}
	if out := cliOK(t, "help"); !strings.Contains(out, "llm-memory purge") {
		t.Errorf("help: %q", out)
	}
	// -h prints flag help and does not run the command (no database opened).
	r := cli(t, "", "stats", "-h", "--db", "/nonexistent/dir/x.db")
	if !errors.Is(r.err, errHelp) || !strings.Contains(r.errOut, "Usage of llm-memory stats") {
		t.Errorf("stats -h: %+v", r)
	}
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"frobnicate"}, {"show"}, {"show", "1", "2"}, {"restore"}, {"import"}, {"version", "extra"},
		{"purge"}, {"list", "--bogus"},
	} {
		r := cli(t, "", args...)
		if !errors.Is(r.err, errUsage) {
			t.Errorf("%v: err = %v, want usage error", args, r.err)
		}
		if r.out != "" {
			t.Errorf("%v: stdout must stay empty, got %q", args, r.out)
		}
	}
	if r := cli(t, "", "--log-level", "loud"); r.err == nil || r.out != "" {
		t.Errorf("serve with bad flag: %+v", r)
	}
	if r := cli(t, "", "show", "abc", "--db", filepath.Join(t.TempDir(), "x.db")); r.err == nil || !strings.Contains(r.err.Error(), "invalid memory id") {
		t.Errorf("show abc: %+v", r)
	}
}

func TestGoldenCLI(t *testing.T) {
	db := seeded(t)
	golden(t, "list", cliOK(t, "list", "--db", db), db)
	golden(t, "list_archived", cliOK(t, "list", "--archived", "--namespace", "demo", "--db", db), db)
	golden(t, "list_kind", cliOK(t, "list", "--kind", "fact,preference", "--db", db), db)
	golden(t, "show", cliOK(t, "show", "1", "--db", db), db) // flag after positional
	golden(t, "search", cliOK(t, "search", "--db", db, "postgres", "jsonb"), db)
	golden(t, "stats", cliOK(t, "stats", "--db", db), db)
	golden(t, "context", cliOK(t, "context", "--db", db, "--namespace", "demo"), db)
}

func TestListEmptyAndErrors(t *testing.T) {
	db := filepath.Join(t.TempDir(), "mem.db")
	if out := cliOK(t, "list", "--db", db); out != "No memories.\n" {
		t.Errorf("empty list: %q", out)
	}
	if out := cliOK(t, "search", "--db", db, "anything"); out != "No results.\n" {
		t.Errorf("empty search: %q", out)
	}
	if r := cli(t, "", "list", "--db", db, "--namespace", "nope"); r.err == nil || !strings.Contains(r.err.Error(), "global") {
		t.Errorf("unknown namespace: %+v", r)
	}
	if r := cli(t, "", "show", "42", "--db", db); r.err == nil || !strings.Contains(r.err.Error(), "#42 does not exist") {
		t.Errorf("show unknown: %+v", r)
	}
}

func TestRestore(t *testing.T) {
	db := seeded(t)
	out := cliOK(t, "restore", "#1", "--db", db)
	if !strings.Contains(out, "Restored #1") {
		t.Errorf("restore: %q", out)
	}
	if r := cli(t, "", "restore", "1", "--db", db); r.err == nil || !strings.Contains(r.err.Error(), "not archived") {
		t.Errorf("restore twice: %+v", r)
	}
	if show := cliOK(t, "show", "1", "--db", db); !strings.Contains(show, "restore  by cli") {
		t.Errorf("history should record cli as author:\n%s", show)
	}
}

func TestPurge(t *testing.T) {
	db := seeded(t)
	future := time.Now().Add(48 * time.Hour).UTC().Format("2006-01-02")

	if out := cliOK(t, "purge", "--archived-before", "2000-01-01", "--db", db); !strings.Contains(out, "No memory archived") {
		t.Errorf("nothing to purge: %q", out)
	}
	// Refused confirmation.
	r := cli(t, "n\n", "purge", "--archived-before", future, "--db", db)
	if r.err != nil || !strings.Contains(r.out, "Permanently delete 1 archived memory") || !strings.Contains(r.out, "Aborted") {
		t.Fatalf("refused purge: %+v", r)
	}
	// Accepted confirmation.
	r = cli(t, "yes\n", "purge", "--archived-before", future, "--db", db)
	if r.err != nil || !strings.Contains(r.out, "Deleted 1 memory") {
		t.Fatalf("confirmed purge: %+v", r)
	}
	if r := cli(t, "", "show", "1", "--db", db); r.err == nil {
		t.Error("purged memory still exists")
	}
	if r := cli(t, "", "purge", "--archived-before", "yesterday", "--db", db); r.err == nil {
		t.Error("invalid date accepted")
	}
	if out := cliOK(t, "purge", "--archived-before", future, "--yes", "--db", db); !strings.Contains(out, "No memory archived") {
		t.Errorf("--yes: %q", out)
	}
}

func TestExportImportCLI(t *testing.T) {
	db := seeded(t)
	dir := t.TempDir()
	dump := filepath.Join(dir, "dump.json")

	// To a file (mode 0600) and to stdout.
	r := cli(t, "", "export", "--history", "-o", dump, "--db", db)
	if r.err != nil || !strings.Contains(r.errOut, "Exported 4 memories") {
		t.Fatalf("export -o: %+v", r)
	}
	if fi, _ := os.Stat(dump); fi.Mode().Perm() != 0o600 {
		t.Errorf("export file mode %o", fi.Mode().Perm())
	}
	stdoutDump := cliOK(t, "export", "--history", "--db", db)
	var d store.Dump
	if err := json.Unmarshal([]byte(stdoutDump), &d); err != nil || d.FormatVersion != 1 || len(d.Memories) != 4 {
		t.Fatalf("stdout export: %v %+v", err, d)
	}
	if ns := cliOK(t, "export", "--namespace", "demo", "--db", db); strings.Contains(ns, "Réponses courtes") {
		t.Error("namespace export contains global memory")
	}

	// Dry run into an empty base, then real import, then idempotence.
	dst := filepath.Join(dir, "dst.db")
	if out := cliOK(t, "import", "--dry-run", dump, "--db", dst); !strings.Contains(out, "Dry run (nothing written): 4 imported") {
		t.Errorf("dry run: %q", out)
	}
	if out := cliOK(t, "list", "--db", dst); out != "No memories.\n" {
		t.Errorf("dry run wrote: %q", out)
	}
	if out := cliOK(t, "import", dump, "--db", dst); !strings.Contains(out, "4 imported, 0 skipped") {
		t.Errorf("import: %q", out)
	}
	r = cli(t, stdoutDump, "import", "-", "--db", dst) // from stdin
	if r.err != nil || !strings.Contains(r.out, "0 imported, 4 skipped") {
		t.Errorf("re-import: %+v", r)
	}
	// Same listing on both sides.
	if a, b := cliOK(t, "list", "--archived", "--db", db), cliOK(t, "list", "--archived", "--db", dst); a != b {
		t.Errorf("listings differ after import\n--- source ---\n%s\n--- destination ---\n%s", a, b)
	}

	bad := filepath.Join(dir, "bad.json")
	_ = os.WriteFile(bad, []byte(`{"hello": 1}`), 0o600)
	if r := cli(t, "", "import", bad, "--db", dst); r.err == nil || !strings.Contains(r.err.Error(), "not a llm-memory export") {
		t.Errorf("bad import: %+v", r)
	}
}

// mcpSession drives `serve` through pipes, as an MCP client process would.
type mcpSession struct {
	t      *testing.T
	in     *io.PipeWriter
	out    *bufio.Scanner
	done   chan result
	nextID int
}

func startServe(t *testing.T, db, clientName string, extra ...string) *mcpSession {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	s := &mcpSession{t: t, in: inW, out: bufio.NewScanner(outR), done: make(chan result, 1)}
	s.out.Buffer(make([]byte, 1<<20), 1<<20)
	go func() {
		var errOut bytes.Buffer
		args := append([]string{"serve", "--db", db}, extra...)
		err := run(context.Background(), args, app{stdin: inR, stdout: outW, stderr: &errOut, env: testEnv})
		_ = outW.Close()
		s.done <- result{errOut: errOut.String(), err: err}
	}()
	s.request("initialize", map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": clientName, "version": "1"},
	})
	s.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	return s
}

func (s *mcpSession) send(msg map[string]any) {
	b, _ := json.Marshal(msg)
	if _, err := s.in.Write(append(b, '\n')); err != nil {
		s.t.Fatalf("write: %v", err)
	}
}

func (s *mcpSession) request(method string, params map[string]any) map[string]any {
	s.t.Helper()
	s.nextID++
	s.send(map[string]any{"jsonrpc": "2.0", "id": s.nextID, "method": method, "params": params})
	for s.out.Scan() {
		var msg map[string]any
		if err := json.Unmarshal(s.out.Bytes(), &msg); err != nil {
			s.t.Fatalf("stdout is not pure JSON-RPC: %q", s.out.Text())
		}
		if id, ok := msg["id"].(float64); ok && int(id) == s.nextID {
			if e, ok := msg["error"]; ok {
				s.t.Fatalf("%s: protocol error %v", method, e)
			}
			return msg["result"].(map[string]any)
		}
	}
	s.t.Fatalf("%s: no response (%v)", method, s.out.Err())
	return nil
}

func (s *mcpSession) tool(name string, args map[string]any) map[string]any {
	s.t.Helper()
	res := s.request("tools/call", map[string]any{"name": name, "arguments": args})
	if isErr, _ := res["isError"].(bool); isErr {
		s.t.Fatalf("%s: tool error %v", name, res["content"])
	}
	return res
}

func (s *mcpSession) close() result {
	s.t.Helper()
	_ = s.in.Close() // EOF on stdin: the server must stop cleanly
	select {
	case r := <-s.done:
		return r
	case <-time.After(5 * time.Second):
		s.t.Fatal("serve did not stop after stdin was closed")
		return result{}
	}
}

func TestServePersistenceAcrossSessions(t *testing.T) {
	db := filepath.Join(t.TempDir(), "mem.db")

	// Session A.
	a := startServe(t, db, "client-a")
	a.tool("remember", map[string]any{"content": "PostgreSQL plutôt que MySQL, pour JSONB.", "kind": "decision", "namespace": "demo"})
	a.tool("handoff", map[string]any{"summary": "Schéma prêt, reste l'API.", "namespace": "demo"})
	if r := a.close(); r.err != nil || !strings.Contains(r.errOut, "stopped") {
		t.Fatalf("session A: %+v", r)
	}

	// Session B, another client, forced default namespace.
	b := startServe(t, db, "client-b", "--namespace", "demo")
	res := b.tool("get_context", nil)
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	for _, want := range []string{`namespace "demo"`, "Latest handoff", "client-a", "PostgreSQL plutôt que MySQL"} {
		if !strings.Contains(text, want) {
			t.Errorf("session B briefing missing %q:\n%s", want, text)
		}
	}
	b.tool("remember", map[string]any{"content": "API en Go", "kind": "fact"})
	if r := b.close(); r.err != nil {
		t.Fatalf("session B: %+v", r)
	}

	// The CLI sees both authors.
	out := cliOK(t, "list", "--namespace", "demo", "--db", db)
	for _, src := range []string{"client-a", "client-b"} {
		if !strings.Contains(out, src) {
			t.Errorf("list missing source %s:\n%s", src, out)
		}
	}
}

// fakeEmbeddingsAPI is a tiny OpenAI-compatible endpoint: dimension 0 counts
// database words, dimension 1 everything else. It counts requests.
func fakeEmbeddingsAPI(t *testing.T) (string, *int) {
	t.Helper()
	requests := 0
	dbWords := map[string]bool{"sgbd": true, "postgresql": true, "mysql": true, "jsonb": true, "database": true}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		type item struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		var data []item
		for i, text := range req.Input {
			v := []float32{0, 0}
			for _, word := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return r == ' ' || r == ',' || r == '.' }) {
				if dbWords[word] {
					v[0]++
				} else {
					v[1] += 0.1
				}
			}
			data = append(data, item{i, v})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1/embeddings", &requests
}

func TestSemanticCLI(t *testing.T) {
	db := seeded(t)
	url, requests := fakeEmbeddingsAPI(t)
	embed := []string{"--db", db, "--embed-url", url, "--embed-model", "fake"}

	// Not configured: reindex refuses, search is plain full-text.
	if r := cli(t, "", "reindex", "--db", db); r.err == nil || !strings.Contains(r.err.Error(), "not configured") {
		t.Errorf("reindex without config: %+v", r)
	}
	if out := cliOK(t, "search", "--db", db, "SGBD"); out != "No results.\n" {
		t.Errorf("v1 search found: %q", out)
	}
	if *requests != 0 {
		t.Fatalf("%d requests to the embeddings API without configuration", *requests)
	}
	if r := cli(t, "", "search", "--db", db, "--embed-url", url, "SGBD"); r.err == nil {
		t.Error("--embed-url without --embed-model accepted")
	}

	// Configured.
	r := cli(t, "", append([]string{"reindex"}, embed...)...)
	if r.err != nil || !strings.Contains(r.out, "Indexed 4 memories with model fake") || !strings.Contains(r.errOut, "4/4") {
		t.Fatalf("reindex: %+v", r)
	}
	if out := cliOK(t, append([]string{"reindex"}, embed...)...); !strings.Contains(out, "Indexed 0 memories") {
		t.Errorf("second reindex: %q", out)
	}
	if out := cliOK(t, append([]string{"reindex", "--all"}, embed...)...); !strings.Contains(out, "Indexed 4 memories") {
		t.Errorf("reindex --all: %q", out)
	}
	out := cliOK(t, append([]string{"search", "SGBD"}, embed...)...)
	if !strings.Contains(out, "#2  demo  [decision] BDD") || strings.Contains(out, "#1 ") {
		t.Errorf("semantic search (archived #1 must stay hidden):\n%s", out)
	}
	if stats := cliOK(t, "stats", "--db", db); !strings.Contains(stats, "Embeddings:\n  fake  4") {
		t.Errorf("stats:\n%s", stats)
	}

	// API down: search still answers with full-text results.
	down := []string{"--db", db, "--embed-url", "http://127.0.0.1:1/v1/embeddings", "--embed-model", "fake", "--embed-timeout", "1s"}
	r = cli(t, "", append([]string{"search", "docker"}, down...)...)
	if r.err != nil || !strings.Contains(r.out, "#3") || !strings.Contains(r.errOut, "full-text results only") {
		t.Errorf("search with API down: %+v", r)
	}
}

func TestServeWithSemanticSearch(t *testing.T) {
	db := seeded(t)
	url, _ := fakeEmbeddingsAPI(t)
	s := startServe(t, db, "client", "--embed-url", url, "--embed-model", "fake")
	res := s.tool("recall", map[string]any{"query": "SGBD"})
	sc := res["structuredContent"].(map[string]any)
	results := sc["results"].([]any)
	if len(results) != 1 || results[0].(map[string]any)["id"].(float64) != 2 {
		t.Errorf("semantic recall over serve: %v", sc)
	}
	if r := s.close(); r.err != nil || !strings.Contains(r.errOut, "semantic search enabled") || strings.Contains(r.errOut, "api_key") {
		t.Errorf("serve: %+v", r)
	}
}
