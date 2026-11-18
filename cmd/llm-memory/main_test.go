package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
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
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(dump); fi.Mode().Perm() != 0o600 {
			t.Errorf("export file mode %o", fi.Mode().Perm())
		}
	}
	stdoutDump := cliOK(t, "export", "--history", "--db", db)
	var d store.Dump
	if err := json.Unmarshal([]byte(stdoutDump), &d); err != nil || d.FormatVersion != 2 || d.Origin == "" || len(d.Memories) != 4 {
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

	// Merge: into an empty base, then again (no-op), by uuid.
	mdb := filepath.Join(dir, "merge.db")
	if out := cliOK(t, "import", "--merge", "--dry-run", dump, "--db", mdb); !strings.Contains(out, "Dry run (nothing written): Merged: 4 inserted, 0 updated") {
		t.Errorf("merge dry run: %q", out)
	}
	if out := cliOK(t, "import", "--merge", dump, "--db", mdb); !strings.Contains(out, "Merged: 4 inserted, 0 updated, 0 kept local (newer here), 0 unchanged") {
		t.Errorf("merge: %q", out)
	}
	if out := cliOK(t, "import", "--merge", dump, "--db", mdb); !strings.Contains(out, "Merged: 0 inserted, 0 updated, 0 kept local (newer here), 4 unchanged") || !strings.Contains(out, "0 history line(s) added") {
		t.Errorf("second merge: %q", out)
	}
	if a, b := cliOK(t, "list", "--archived", "--db", db), cliOK(t, "list", "--archived", "--db", mdb); a != b {
		t.Errorf("listings differ after merge\n%s\n%s", a, b)
	}
	v1file := filepath.Join(dir, "v1.json")
	if r := cli(t, "", "export", "--format-version", "1", "-o", v1file, "--db", db); r.err != nil {
		t.Fatal(r.err)
	}
	if r := cli(t, "", "import", "--merge", v1file, "--db", mdb); r.err == nil || !strings.Contains(r.err.Error(), "merge needs a version 2 export") {
		t.Errorf("merge of a v1 file: %+v", r)
	}

	// Format version 1, for older releases: no origin, still importable here.
	var v1 store.Dump
	if err := json.Unmarshal([]byte(cliOK(t, "export", "--format-version", "1", "--db", db)), &v1); err != nil || v1.FormatVersion != 1 || v1.Origin != "" {
		t.Errorf("v1 export: %v %+v", err, v1)
	}
	if r := cli(t, "", "export", "--format-version", "7", "--db", db); r.err == nil || !strings.Contains(r.err.Error(), "cannot export version 7") {
		t.Errorf("export v7: %+v", r)
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

func TestCLIWriteCommandsAndJSON(t *testing.T) {
	db := filepath.Join(t.TempDir(), "mem.db")

	// remember requires --kind and positional content.
	if r := cli(t, "", "remember", "--db", db, "missing kind"); !errors.Is(r.err, errUsage) {
		t.Errorf("remember without kind: %+v", r)
	}
	if r := cli(t, "", "remember", "--db", db, "--kind", "handoff", "x"); r.err == nil {
		t.Error("remember --kind handoff should fail")
	}

	out := cliOK(t, "remember", "--db", db, "--namespace", "demo", "--kind", "decision",
		"--title", "DB choice", "--tag", "db, postgres", "--importance", "3", "Use PostgreSQL for JSONB.")
	if !strings.Contains(out, "Created #1 (demo, decision): DB choice") {
		t.Fatalf("remember output: %q", out)
	}

	// remember from stdin with --json and --supersedes.
	r := cli(t, "Use SQLite in WAL mode.", "remember", "--db", db, "--namespace", "demo",
		"--kind", "decision", "--tag", "db,sqlite", "--supersedes", "#1", "--json", "-")
	if r.err != nil {
		t.Fatalf("remember stdin json: %v\n%s", r.err, r.errOut)
	}
	var remJSON struct {
		Memory     store.Memory `json:"memory"`
		Superseded []int64      `json:"superseded"`
	}
	if err := json.Unmarshal([]byte(r.out), &remJSON); err != nil {
		t.Fatalf("remember json decode: %v\n%s", err, r.out)
	}
	if remJSON.Memory.ID != 2 || len(remJSON.Superseded) != 1 || remJSON.Superseded[0] != 1 {
		t.Fatalf("remember json = %+v", remJSON)
	}

	// handoff with --next, --questions, --description, --supersedes, --json.
	out = cliOK(t, "handoff", "--db", db, "--namespace", "demo", "--description", "Demo repo",
		"--next", "write docs, run tests", "--questions", "release date?", "--json", "Core features done.")
	var hoJSON struct {
		Memory store.Memory `json:"memory"`
	}
	if err := json.Unmarshal([]byte(out), &hoJSON); err != nil {
		t.Fatalf("handoff json decode: %v\n%s", err, out)
	}
	if hoJSON.Memory.Kind != store.KindHandoff || !strings.Contains(hoJSON.Memory.Content, "- write docs") {
		t.Fatalf("handoff json = %+v", hoJSON)
	}

	// --tag filter and --json on list, search, show, stats, context.
	listOut := cliOK(t, "list", "--db", db, "--tag", "sqlite", "--json")
	var listRes []store.Memory
	if err := json.Unmarshal([]byte(listOut), &listRes); err != nil || len(listRes) != 1 || listRes[0].ID != 2 {
		t.Fatalf("list --tag --json: %v %+v", err, listRes)
	}

	searchOut := cliOK(t, "search", "--db", db, "--tag", "sqlite", "--json", "WAL")
	var searchRes []store.SearchResult
	if err := json.Unmarshal([]byte(searchOut), &searchRes); err != nil || len(searchRes) != 1 || searchRes[0].ID != 2 {
		t.Fatalf("search --tag --json: %v %+v", err, searchRes)
	}

	showOut := cliOK(t, "show", "--db", db, "--json", "2")
	var showRes struct {
		Memory  store.Memory         `json:"memory"`
		History []store.HistoryEntry `json:"history"`
	}
	if err := json.Unmarshal([]byte(showOut), &showRes); err != nil || showRes.Memory.ID != 2 || len(showRes.History) != 1 {
		t.Fatalf("show --json: %v %+v", err, showRes)
	}

	statsOut := cliOK(t, "stats", "--db", db, "--json")
	var statsRes store.Stats
	if err := json.Unmarshal([]byte(statsOut), &statsRes); err != nil || statsRes.Active != 2 || statsRes.Archived != 1 {
		t.Fatalf("stats --json: %v %+v", err, statsRes)
	}

	ctxOut := cliOK(t, "context", "--db", db, "--namespace", "demo", "--json")
	var ctxRes struct {
		Namespace string `json:"namespace"`
		Briefing  string `json:"briefing"`
	}
	if err := json.Unmarshal([]byte(ctxOut), &ctxRes); err != nil || ctxRes.Namespace != "demo" || !strings.Contains(ctxRes.Briefing, "Core features done.") {
		t.Fatalf("context --json: %v %+v", err, ctxRes)
	}

	// forget with --reason and --json.
	forgetOut := cliOK(t, "forget", "--db", db, "--reason", "done", "--json", "2")
	var forgetRes []store.Memory
	if err := json.Unmarshal([]byte(forgetOut), &forgetRes); err != nil || len(forgetRes) != 1 || !forgetRes[0].Archived() {
		t.Fatalf("forget --json: %v %+v", err, forgetRes)
	}

	// Empty list/search with --json returns [] not null.
	if emptyList := cliOK(t, "list", "--db", db, "--tag", "nope", "--json"); strings.TrimSpace(emptyList) != "[]" {
		t.Errorf("empty list --json = %q", emptyList)
	}
	if emptySearch := cliOK(t, "search", "--db", db, "--tag", "nope", "--json", "WAL"); strings.TrimSpace(emptySearch) != "[]" {
		t.Errorf("empty search --json = %q", emptySearch)
	}
}

func TestCLIAxes1And2(t *testing.T) {
	db := filepath.Join(t.TempDir(), "mem.db")

	// remember with --expires-in and namespace commands.
	cliOK(t, "remember", "--db", db, "--namespace", "alpha", "--kind", "note", "--expires-in", "7d", "temporary note")
	cliOK(t, "remember", "--db", db, "--namespace", "alpha", "--kind", "fact", "persistent fact")

	nsOut := cliOK(t, "namespace", "list", "--db", db)
	if !strings.Contains(nsOut, "alpha") {
		t.Errorf("namespace list missing alpha:\n%s", nsOut)
	}
	cliOK(t, "namespace", "rename", "--db", db, "alpha", "beta")
	cliOK(t, "namespace", "merge", "--db", db, "beta", "gamma")
	cliOK(t, "namespace", "archive", "--db", db, "gamma")

	// context --focus.
	cliOK(t, "remember", "--db", db, "--namespace", "proj", "--kind", "note", "--title", "Redis", "Redis runs on 6379")
	ctxOut := cliOK(t, "context", "--db", db, "--namespace", "proj", "--focus", "redis")
	if !strings.Contains(ctxOut, `Relevant to "redis"`) {
		t.Errorf("context --focus missing relevant section:\n%s", ctxOut)
	}

	// gc, doctor, setup.
	gcOut := cliOK(t, "gc", "--db", db)
	if !strings.Contains(gcOut, "Archived 0 expired") {
		t.Errorf("gc output = %q", gcOut)
	}
	docOut := cliOK(t, "doctor", "--db", db)
	if !strings.Contains(docOut, "Integrity:") || !strings.Contains(docOut, "ok (SQLite + FTS5)") {
		t.Errorf("doctor output:\n%s", docOut)
	}
	setupOut := cliOK(t, "setup", "--db", db, "antigravity")
	if !strings.Contains(setupOut, "mcpServers") {
		t.Errorf("setup antigravity output:\n%s", setupOut)
	}
	setupCC := cliOK(t, "setup", "--db", db, "claude-code")
	if !strings.Contains(setupCC, "claude mcp add --scope user llm-memory") {
		t.Errorf("setup claude-code output:\n%s", setupCC)
	}
}

func TestCLIBackup(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "mem.db")
	cliOK(t, "remember", "--db", db, "--kind", "fact", "important backup fact")

	dest := filepath.Join(dir, "manual.db")
	out := cliOK(t, "backup", "--db", db, "--json", dest)
	var res struct {
		Path     string `json:"path"`
		Bytes    int64  `json:"bytes"`
		Verified bool   `json:"verified"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("unmarshal backup json: %v\n%s", err, out)
	}
	if res.Path != dest || res.Bytes <= 0 || !res.Verified {
		t.Errorf("unexpected backup json: %+v", res)
	}

	// Verify data is present in the backed-up database.
	listOut := cliOK(t, "list", "--db", dest)
	if !strings.Contains(listOut, "important backup fact") {
		t.Errorf("backup missing memory:\n%s", listOut)
	}
}

func TestCLISetupAndDoctor(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "mem.db")
	cliOK(t, "remember", "--db", db, "--kind", "fact", "setup test fact")

	// doctor --json
	docJSON := cliOK(t, "doctor", "--db", db, "--json")
	var rep doctorReport
	if err := json.Unmarshal([]byte(docJSON), &rep); err != nil {
		t.Fatalf("unmarshal doctor json: %v\n%s", err, docJSON)
	}
	if !rep.OK || rep.Active != 1 || rep.Database != db || rep.LatestSchema == 0 {
		t.Errorf("unexpected doctor report: %+v", rep)
	}

	// setup codex (TOML)
	codexOut := cliOK(t, "setup", "--db", db, "--namespace", "proj", "codex")
	if !strings.Contains(codexOut, "[mcp_servers.llm-memory]") ||
		!strings.Contains(codexOut, `command = "`) ||
		!strings.Contains(codexOut, `"--namespace", "proj"`) {
		t.Errorf("unexpected codex setup output:\n%s", codexOut)
	}

	// setup opencode (JSON)
	ocOut := cliOK(t, "setup", "--db", db, "--ollama", "opencode")
	var ocCfg struct {
		Schema string `json:"$schema"`
		MCP    map[string]struct {
			Type    string   `json:"type"`
			Command []string `json:"command"`
			Enabled bool     `json:"enabled"`
		} `json:"mcp"`
	}
	if err := json.Unmarshal([]byte(ocOut), &ocCfg); err != nil {
		t.Fatalf("unmarshal opencode json: %v\n%s", err, ocOut)
	}
	entry, ok := ocCfg.MCP["llm-memory"]
	if !ok || entry.Type != "local" || !entry.Enabled || len(entry.Command) < 4 {
		t.Fatalf("unexpected opencode entry: %+v", ocCfg)
	}
	joined := strings.Join(entry.Command, " ")
	if !strings.Contains(joined, defaultOllamaURL) || !strings.Contains(joined, defaultOllamaModel) {
		t.Errorf("opencode --ollama missing embed flags: %v", entry.Command)
	}

	// setup vscode
	vsOut := cliOK(t, "setup", "--db", db, "vscode")
	if !strings.Contains(vsOut, `"servers"`) || !strings.Contains(vsOut, `"stdio"`) {
		t.Errorf("unexpected vscode setup output:\n%s", vsOut)
	}

	// setup unknown client returns usage error
	var stderr bytes.Buffer
	err := run(context.Background(), []string{"setup", "unknown-client"}, app{
		stdin: strings.NewReader(""), stdout: &bytes.Buffer{}, stderr: &stderr,
		env: config.Env{Getenv: func(string) string { return "" }, UserHomeDir: func() (string, error) { return dir, nil }},
	})
	if !errors.Is(err, errUsage) {
		t.Errorf("expected errUsage for unknown client, got %v", err)
	}
}

func TestCLISetupWrite(t *testing.T) {
	home := t.TempDir()
	db := filepath.Join(home, "mem.db")
	runWithHome := func(args ...string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		err := run(context.Background(), args, app{
			stdin:  strings.NewReader(""),
			stdout: &stdout,
			stderr: &stderr,
			env: config.Env{
				Getenv:      func(string) string { return "" },
				UserHomeDir: func() (string, error) { return home, nil },
			},
		})
		if err != nil {
			t.Fatalf("run %v: %v\nstderr: %s", args, err, stderr.String())
		}
		if !strings.Contains(stdout.String(), "Updated ") {
			t.Errorf("expected Updated message, got %q", stdout.String())
		}
	}

	// Antigravity: preserves existing servers in ~/.gemini/config/mcp_config.json.
	agyPath := filepath.Join(home, ".gemini", "config", "mcp_config.json")
	if err := os.MkdirAll(filepath.Dir(agyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agyPath, []byte(`{"mcpServers":{"other":{"command":"echo"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	runWithHome("setup", "--db", db, "--write", "antigravity")
	agyData, err := os.ReadFile(agyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(agyData), `"other"`) || !strings.Contains(string(agyData), `"llm-memory"`) {
		t.Errorf("antigravity config not merged properly:\n%s", agyData)
	}

	// Claude Code: writes ~/.claude.json.
	runWithHome("setup", "--db", db, "--write", "claude-code")
	ccData, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ccData), `"mcpServers"`) || !strings.Contains(string(ccData), `"llm-memory"`) {
		t.Errorf("claude-code config unexpected:\n%s", ccData)
	}

	// OpenCode: writes ~/.config/opencode/opencode.json.
	runWithHome("setup", "--db", db, "--write", "--ollama", "opencode")
	ocData, err := os.ReadFile(filepath.Join(home, ".config", "opencode", "opencode.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ocData), `"type": "local"`) || !strings.Contains(string(ocData), defaultOllamaModel) {
		t.Errorf("opencode config unexpected:\n%s", ocData)
	}

	// Codex: appends then updates in-place without clobbering other tables.
	codexPath := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(codexPath), 0o755); err != nil {
		t.Fatal(err)
	}
	initialTOML := "[mcp_servers.other]\ncommand = \"echo\"\n\n[mcp_servers.llm-memory]\ncommand = \"old\"\nargs = [\"serve\"]\n\n[tui]\ntheme = \"dark\"\n"
	if err := os.WriteFile(codexPath, []byte(initialTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	runWithHome("setup", "--db", db, "--namespace", "work", "--write", "codex")
	codexData, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	codexStr := string(codexData)
	if !strings.Contains(codexStr, "[mcp_servers.other]") ||
		!strings.Contains(codexStr, "[tui]") ||
		strings.Contains(codexStr, `command = "old"`) ||
		!strings.Contains(codexStr, `"--namespace", "work"`) {
		t.Errorf("codex config not updated cleanly:\n%s", codexStr)
	}

	// VS Code with --config override.
	vsPath := filepath.Join(home, "proj", ".vscode", "mcp.json")
	runWithHome("setup", "--db", db, "--write", "--config", vsPath, "vscode")
	vsData, err := os.ReadFile(vsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(vsData), `"servers"`) || !strings.Contains(string(vsData), `"stdio"`) {
		t.Errorf("vscode config unexpected:\n%s", vsData)
	}
}

func TestHookCommand(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "mem.db")
	proj := filepath.Join(dir, "myproj")
	if err := os.MkdirAll(filepath.Join(proj, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	cliOK(t, "remember", "--db", db, "--namespace", "myproj", "--kind", "decision", "hooks pick the project namespace")

	// Claude Code / Codex: namespace detected from the payload cwd.
	for _, client := range []string{"claude-code", "codex"} {
		r := cli(t, `{"cwd":"`+filepath.ToSlash(proj)+`","source":"startup"}`, "hook", "--db", db, "--namespace", "auto", client)
		if r.err != nil {
			t.Fatalf("%s: %v\n%s", client, r.err, r.errOut)
		}
		var out struct {
			HookSpecificOutput struct {
				HookEventName     string `json:"hookEventName"`
				AdditionalContext string `json:"additionalContext"`
			} `json:"hookSpecificOutput"`
		}
		if err := json.Unmarshal([]byte(r.out), &out); err != nil {
			t.Fatalf("%s: invalid JSON %v\n%s", client, err, r.out)
		}
		ctx := out.HookSpecificOutput.AdditionalContext
		if out.HookSpecificOutput.HookEventName != "SessionStart" ||
			!strings.Contains(ctx, `namespace "myproj"`) || !strings.Contains(ctx, "hooks pick the project namespace") {
			t.Errorf("%s output:\n%s", client, r.out)
		}
	}

	// Antigravity: workspacePaths and injectSteps.
	r := cli(t, `{"workspacePaths":["`+filepath.ToSlash(proj)+`"]}`, "hook", "--db", db, "--namespace", "auto", "antigravity")
	if r.err != nil {
		t.Fatal(r.err)
	}
	var agy struct {
		InjectSteps []struct {
			SystemMessage struct {
				SystemMessage string `json:"systemMessage"`
			} `json:"systemMessage"`
		} `json:"injectSteps"`
	}
	if err := json.Unmarshal([]byte(r.out), &agy); err != nil || len(agy.InjectSteps) != 1 ||
		!strings.Contains(agy.InjectSteps[0].SystemMessage.SystemMessage, `namespace "myproj"`) {
		t.Errorf("antigravity output (%v):\n%s", err, r.out)
	}

	// Invalid payload: still succeeds, falls back to the default namespace.
	r = cli(t, "not json", "hook", "--db", db, "claude-code")
	if r.err != nil || !strings.Contains(r.out, `namespace \"global\"`) {
		t.Errorf("invalid payload: err=%v out=%s", r.err, r.out)
	}

	// Broken database: exit status 0, nothing on stdout, error on stderr.
	r = cli(t, "", "hook", "--db", dir, "claude-code")
	if r.err != nil || r.out != "" || !strings.Contains(r.errOut, "llm-memory hook:") {
		t.Errorf("broken db: err=%v out=%q stderr=%q", r.err, r.out, r.errOut)
	}

	// Unsupported client.
	if r := cli(t, "", "hook", "vscode"); !errors.Is(r.err, errUsage) {
		t.Errorf("vscode hook: err=%v", r.err)
	}
}

func TestCLISetupHooks(t *testing.T) {
	home := t.TempDir()
	db := filepath.Join(home, "mem.db")
	runHome := func(args ...string) result {
		t.Helper()
		var stdout, stderr bytes.Buffer
		err := run(context.Background(), args, app{
			stdin: strings.NewReader(""), stdout: &stdout, stderr: &stderr,
			env: config.Env{
				Getenv:      func(string) string { return "" },
				UserHomeDir: func() (string, error) { return home, nil },
			},
		})
		return result{stdout.String(), stderr.String(), err}
	}

	// Print mode.
	r := runHome("setup", "--hooks", "--db", db, "claude-code")
	if r.err != nil || !strings.Contains(r.out, `"SessionStart"`) || !strings.Contains(r.out, "hook claude-code") ||
		!strings.Contains(r.out, "--namespace auto") {
		t.Errorf("print claude-code hooks: err=%v\n%s", r.err, r.out)
	}

	// Claude Code: keeps other settings and hooks, idempotent.
	ccPath := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(ccPath), 0o755); err != nil {
		t.Fatal(err)
	}
	initial := `{"model":"opus","hooks":{"SessionStart":[{"matcher":"startup","hooks":[{"type":"command","command":"echo hi"}]}],"Stop":[]}}`
	if err := os.WriteFile(ccPath, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if r := runHome("setup", "--hooks", "--write", "--db", db, "--namespace", "work", "claude-code"); r.err != nil {
			t.Fatalf("write claude-code: %v\n%s", r.err, r.errOut)
		}
	}
	data, err := os.ReadFile(ccPath)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, `"model": "opus"`) || !strings.Contains(s, "echo hi") || !strings.Contains(s, `"Stop"`) ||
		strings.Count(s, "hook claude-code") != 1 || !strings.Contains(s, "--namespace work") {
		t.Errorf("claude-code settings:\n%s", s)
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(ccPath); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("settings mode not preserved: %v %v", fi.Mode(), err)
		}
	}

	// Codex: ~/.codex/hooks.json, trust reminder.
	r = runHome("setup", "--hooks", "--write", "--db", db, "codex")
	if r.err != nil || !strings.Contains(r.errOut, "/hooks") {
		t.Fatalf("write codex: %v\n%s", r.err, r.errOut)
	}
	if data, err := os.ReadFile(filepath.Join(home, ".codex", "hooks.json")); err != nil || !strings.Contains(string(data), "hook codex") {
		t.Errorf("codex hooks.json (%v):\n%s", err, data)
	}

	// Antigravity: named entry in ~/.gemini/config/hooks.json, others kept.
	agyPath := filepath.Join(home, ".gemini", "config", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(agyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agyPath, []byte(`{"guard":{"PreToolUse":[]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := runHome("setup", "--hooks", "--write", "--db", db, "antigravity"); r.err != nil {
		t.Fatal(r.err)
	}
	data, _ = os.ReadFile(agyPath)
	if !strings.Contains(string(data), `"guard"`) || !strings.Contains(string(data), "hook antigravity") {
		t.Errorf("antigravity hooks.json:\n%s", data)
	}

	// Unsupported client and malformed config.
	if r := runHome("setup", "--hooks", "opencode"); !errors.Is(r.err, errUsage) {
		t.Errorf("opencode hooks: err=%v", r.err)
	}
	bad := filepath.Join(home, "bad.json")
	if err := os.WriteFile(bad, []byte(`{"hooks":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := runHome("setup", "--hooks", "--write", "--config", bad, "claude-code"); r.err == nil {
		t.Error("expected error on malformed hooks section")
	}
	// hook --write: same patch as setup --hooks --write, replaces the previous
	// handler and passes --max-chars/--focus through.
	r = runHome("hook", "--write", "--db", db, "--namespace", "work", "--max-chars", "3000", "--focus", "auth flow", "claude-code")
	if r.err != nil || !strings.Contains(r.out, "Updated "+ccPath) {
		t.Fatalf("hook --write: %v\n%s%s", r.err, r.out, r.errOut)
	}
	data, _ = os.ReadFile(ccPath)
	s = string(data)
	if strings.Count(s, "hook claude-code") != 1 || !strings.Contains(s, "echo hi") ||
		!strings.Contains(s, "--max-chars 3000") || (runtime.GOOS != "windows" && !strings.Contains(s, "--focus 'auth flow'")) {
		t.Errorf("hook --write settings:\n%s", s)
	}
	alt := filepath.Join(home, "alt", "hooks.json")
	if r := runHome("hook", "--write", "--config", alt, "--db", db, "codex"); r.err != nil {
		t.Fatalf("hook --write --config: %v", r.err)
	}
	if data, err := os.ReadFile(alt); err != nil || !strings.Contains(string(data), "--namespace auto") {
		t.Errorf("hook --write --config (%v):\n%s", err, data)
	}
	if r := runHome("hook", "--config", alt, "claude-code"); !errors.Is(r.err, errUsage) {
		t.Errorf("hook --config without --write: err=%v", r.err)
	}
	if r := runHome("hook", "--write", "opencode"); !errors.Is(r.err, errUsage) {
		t.Errorf("hook --write opencode: err=%v", r.err)
	}
}

func TestShellQuote(t *testing.T) {
	if got := shellQuote("/usr/bin/llm-memory"); got != "/usr/bin/llm-memory" {
		t.Errorf("plain path quoted: %s", got)
	}
	if runtime.GOOS != "windows" {
		if got := shellQuote("/my dir/it's"); got != `'/my dir/it'\''s'` {
			t.Errorf("got %s", got)
		}
	}
}

func TestCLIUpdateAndLinks(t *testing.T) {
	db := filepath.Join(t.TempDir(), "mem.db")
	r1 := cliOK(t, "remember", "--db", db, "--namespace", "demo", "--kind", "decision", "--title", "DB", "--json", "Use MySQL")
	var cr1 struct {
		Memory store.Memory `json:"memory"`
	}
	if err := json.Unmarshal([]byte(r1), &cr1); err != nil {
		t.Fatal(err)
	}
	r2 := cliOK(t, "remember", "--db", db, "--namespace", "demo", "--kind", "fact", "--title", "Cache", "--json", "Use Redis")
	var cr2 struct {
		Memory store.Memory `json:"memory"`
	}
	if err := json.Unmarshal([]byte(r2), &cr2); err != nil {
		t.Fatal(err)
	}

	upOut := cliOK(t, "update", "--db", db, "--title", "DB v2", "--tag", "db,pg", "--importance", "3",
		"--expires-in", "24h", "--add-link", fmt.Sprintf("relates_to:%d", cr2.Memory.ID), "--json",
		fmt.Sprintf("%d", cr1.Memory.ID), "Use PostgreSQL")
	var up struct {
		Memory store.Memory     `json:"memory"`
		Links  []store.LinkView `json:"links"`
	}
	if err := json.Unmarshal([]byte(upOut), &up); err != nil {
		t.Fatal(err)
	}
	if up.Memory.Title != "DB v2" || up.Memory.Content != "Use PostgreSQL" || up.Memory.Importance != 3 ||
		up.Memory.ExpiresAt == nil || len(up.Links) != 1 || up.Links[0].ID != cr2.Memory.ID {
		t.Fatalf("unexpected update output: %+v", up)
	}

	showOut := cliOK(t, "show", "--db", db, fmt.Sprintf("%d", cr1.Memory.ID))
	if !strings.Contains(showOut, "Links:") || !strings.Contains(showOut, "relates_to") {
		t.Errorf("show missing links:\n%s", showOut)
	}

	cliOK(t, "update", "--db", db, "--clear-expires", "--remove-link", fmt.Sprintf("relates_to:%d", cr2.Memory.ID),
		fmt.Sprintf("%d", cr1.Memory.ID))
	showJSON := cliOK(t, "show", "--db", db, "--json", fmt.Sprintf("%d", cr1.Memory.ID))
	var shown struct {
		Memory store.Memory     `json:"memory"`
		Links  []store.LinkView `json:"links"`
	}
	if err := json.Unmarshal([]byte(showJSON), &shown); err != nil {
		t.Fatal(err)
	}
	if shown.Memory.ExpiresAt != nil || len(shown.Links) != 0 {
		t.Errorf("expected cleared expiry and links, got %+v", shown)
	}
}

func TestMultiEventHooksAndRemoveAndDoctorChecks(t *testing.T) {
	home := t.TempDir()
	db := filepath.Join(home, "mem.db")
	proj := filepath.Join(home, "myproj")
	if err := os.MkdirAll(filepath.Join(proj, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	runHome := func(stdin string, args ...string) result {
		t.Helper()
		var stdout, stderr bytes.Buffer
		err := run(context.Background(), args, app{
			stdin: strings.NewReader(stdin), stdout: &stdout, stderr: &stderr,
			env: config.Env{
				Getenv:      func(string) string { return "" },
				UserHomeDir: func() (string, error) { return home, nil },
			},
		})
		return result{stdout.String(), stderr.String(), err}
	}

	// Create an active task_state so PreCompact / Stop surface it.
	if r := runHome("", "remember", "--db", db, "--namespace", "myproj", "--kind", "task_state", "Finish refactoring hooks"); r.err != nil {
		t.Fatal(r.err)
	}

	// PreCompact & Stop hook outputs.
	r := runHome(`{"cwd":"`+filepath.ToSlash(proj)+`"}`, "hook", "--db", db, "--namespace", "auto", "--event", "PreCompact", "claude-code")
	if r.err != nil || !strings.Contains(r.out, `"hookEventName": "PreCompact"`) || !strings.Contains(r.out, "Finish refactoring hooks") {
		t.Fatalf("PreCompact hook: err=%v\n%s", r.err, r.out)
	}
	r = runHome(`{"cwd":"`+filepath.ToSlash(proj)+`"}`, "hook", "--db", db, "--namespace", "auto", "--event", "Stop", "claude-code")
	if r.err != nil || !strings.Contains(r.out, `"systemMessage"`) || !strings.Contains(r.out, "Finish refactoring hooks") {
		t.Fatalf("Stop hook: err=%v\n%s", r.err, r.out)
	}

	// Install multiple hook events, then remove.
	if r := runHome("", "setup", "--hooks", "--events", "SessionStart,PreCompact,Stop", "--write", "--db", db, "claude-code"); r.err != nil {
		t.Fatal(r.err)
	}
	ccSettings := filepath.Join(home, ".claude", "settings.json")
	data, _ := os.ReadFile(ccSettings)
	if !strings.Contains(string(data), `"PreCompact"`) || !strings.Contains(string(data), `"Stop"`) {
		t.Fatalf("multi-event hooks missing:\n%s", data)
	}
	if r := runHome("", "hook", "--remove", "claude-code"); r.err != nil {
		t.Fatal(r.err)
	}
	data, _ = os.ReadFile(ccSettings)
	if strings.Contains(string(data), "llm-memory") {
		t.Errorf("hook --remove left llm-memory in settings:\n%s", data)
	}

	// Install MCP configs for claude-code and codex, plus a broken binary path, then check doctor & --remove.
	if r := runHome("", "setup", "--write", "--db", db, "codex"); r.err != nil {
		t.Fatal(r.err)
	}
	agyCfg := filepath.Join(home, ".gemini", "config", "mcp_config.json")
	if err := os.MkdirAll(filepath.Dir(agyCfg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agyCfg, []byte(`{"mcpServers":{"llm-memory":{"command":"/nonexistent/llm-memory","args":["serve"]}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	doc := runHome("", "doctor", "--db", db, "--json")
	if doc.err != nil {
		t.Fatalf("doctor: %v\n%s", doc.err, doc.errOut)
	}
	var rep doctorReport
	if err := json.Unmarshal([]byte(doc.out), &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Clients) < 2 {
		t.Fatalf("expected client checks in doctor, got %+v", rep.Clients)
	}
	var sawBroken bool
	for _, w := range rep.Warnings {
		if strings.Contains(w, "/nonexistent/llm-memory") {
			sawBroken = true
		}
	}
	if !sawBroken {
		t.Errorf("expected missing binary warning in doctor, got warnings=%v", rep.Warnings)
	}

	if r := runHome("", "setup", "--remove", "codex"); r.err != nil {
		t.Fatal(r.err)
	}
	codexData, _ := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if strings.Contains(string(codexData), "llm-memory") {
		t.Errorf("setup --remove codex left entry:\n%s", codexData)
	}
}

func TestCompletionCommand(t *testing.T) {
	for _, sh := range []string{"bash", "zsh", "fish"} {
		out := cliOK(t, "completion", sh)
		if !strings.Contains(out, "llm-memory") {
			t.Errorf("%s completion unexpected:\n%s", sh, out)
		}
	}
	if r := cli(t, "", "completion", "powershell"); !errors.Is(r.err, errUsage) {
		t.Errorf("expected errUsage for unsupported shell, got %v", r.err)
	}
}

func TestCLIModel2VecSemanticAndSetup(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "mem.db")
	modelDir := filepath.Join(dir, "local-m2v")
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Build a minimal local Model2Vec model where "sgbd" and "postgres" have identical vectors.
	if err := os.WriteFile(filepath.Join(modelDir, "config.json"), []byte(`{"model_type":"model2vec","normalize":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "tokenizer.json"), []byte(`{
		"normalizer":{"type":"BertNormalizer","lowercase":true},
		"pre_tokenizer":{"type":"BertPreTokenizer"},
		"model":{"type":"WordPiece","unk_token":"[UNK]","vocab":{"[UNK]":0,"sgbd":1,"postgres":2,"redis":3}}
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	header := `{"embeddings":{"dtype":"F32","shape":[4,2],"data_offsets":[0,32]}}`
	var lenBuf [8]byte
	binary.LittleEndian.PutUint64(lenBuf[:], uint64(len(header)))
	payload := make([]byte, 32)
	putF32 := func(idx int, v float32) {
		binary.LittleEndian.PutUint32(payload[4*idx:], math.Float32bits(v))
	}
	putF32(2, 1) // sgbd -> [1, 0]
	putF32(4, 1) // postgres -> [1, 0]
	putF32(7, 1) // redis -> [0, 1]
	sf := append(append(lenBuf[:], []byte(header)...), payload...)
	if err := os.WriteFile(filepath.Join(modelDir, "model.safetensors"), sf, 0o644); err != nil {
		t.Fatal(err)
	}

	// setup --model2vec
	ocOut := cliOK(t, "setup", "--db", db, "--model2vec", "opencode")
	if !strings.Contains(ocOut, `"--embed-provider"`) || !strings.Contains(ocOut, config.DefaultModel2VecModel) {
		t.Errorf("setup --model2vec output missing flags:\n%s", ocOut)
	}
	if r := cli(t, "", "setup", "--db", db, "--model2vec", "--ollama", "opencode"); !errors.Is(r.err, errUsage) {
		t.Errorf("expected errUsage combining --model2vec and --ollama, got %+v", r)
	}

	// Create two memories, reindex with local Model2Vec, and search by synonym.
	cliOK(t, "remember", "--db", db, "--kind", "decision", "--title", "DB", "postgres")
	cliOK(t, "remember", "--db", db, "--kind", "fact", "--title", "Cache", "redis")

	reOut := cliOK(t, "reindex", "--db", db, "--embed-provider", "model2vec", "--embed-model", modelDir)
	if !strings.Contains(reOut, "Indexed 2 memories") {
		t.Errorf("reindex = %q", reOut)
	}

	searchOut := cliOK(t, "search", "--db", db, "--embed-provider", "model2vec", "--embed-model", modelDir, "--json", "sgbd")
	var hits []store.SearchResult
	if err := json.Unmarshal([]byte(searchOut), &hits); err != nil {
		t.Fatalf("search json: %v\n%s", err, searchOut)
	}
	if len(hits) != 1 || hits[0].Content != "postgres" {
		t.Errorf("semantic search for sgbd = %+v, want [postgres]", hits)
	}

	// doctor --json with cached local model2vec
	docJSON := cliOK(t, "doctor", "--db", db, "--embed-provider", "model2vec", "--embed-model", modelDir, "--json")
	var rep doctorReport
	if err := json.Unmarshal([]byte(docJSON), &rep); err != nil {
		t.Fatal(err)
	}
	if !rep.OK || rep.EmbeddingsProvider != "model2vec" || !strings.HasPrefix(rep.Embeddings, "ok (model2vec") {
		t.Errorf("unexpected doctor model2vec report: %+v", rep)
	}

	// doctor --json with uncached HF model reports warning without hitting the network.
	docUncached := cliOK(t, "doctor", "--db", db, "--embed-provider", "model2vec", "--embed-model", "minishlab/not-downloaded", "--embed-cache-dir", filepath.Join(dir, "empty-cache"), "--json")
	var repUncached doctorReport
	if err := json.Unmarshal([]byte(docUncached), &repUncached); err != nil {
		t.Fatal(err)
	}
	if !repUncached.OK || !strings.HasPrefix(repUncached.Embeddings, "not cached") || len(repUncached.Warnings) == 0 {
		t.Errorf("unexpected doctor uncached model2vec report: %+v", repUncached)
	}
}
