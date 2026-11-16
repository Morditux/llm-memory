package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Morditux/llm-memory/internal/store"
)

// connect starts a server on st and a client named clientName, connected by
// in-memory transports.
func connect(t *testing.T, st *store.Store, opts Options, clientName string) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	srv, err := New(st, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	serverT, clientT := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: clientName, Version: "1.0"}, nil)
	cs, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() {
		_ = cs.Close()
		_ = ss.Wait()
	})
	return cs
}

func openStore(t *testing.T, path string) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func newSession(t *testing.T, clientName string) *mcp.ClientSession {
	t.Helper()
	st := openStore(t, filepath.Join(t.TempDir(), "mem.db"))
	return connect(t, st, Options{Version: "test"}, clientName)
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: protocol error: %v", name, err)
	}
	return res
}

// callOK calls a tool, fails on IsError, and decodes the structured output.
func callOK[T any](t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) T {
	t.Helper()
	res := call(t, cs, name, args)
	if res.IsError {
		t.Fatalf("%s(%v): tool error: %s", name, args, text(res))
	}
	var out T
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("%s: decode %s: %v", name, b, err)
	}
	return out
}

func callErr(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any, wantSub ...string) {
	t.Helper()
	res := call(t, cs, name, args)
	if !res.IsError {
		t.Fatalf("%s(%v): expected IsError, got %s", name, args, text(res))
	}
	msg := text(res)
	for _, w := range wantSub {
		if !strings.Contains(msg, w) {
			t.Errorf("%s error %q should contain %q", name, msg, w)
		}
	}
}

func text(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func briefingText(t *testing.T, cs *mcp.ClientSession, args map[string]any) string {
	t.Helper()
	res := call(t, cs, "get_context", args)
	if res.IsError {
		t.Fatalf("get_context: %s", text(res))
	}
	return text(res)
}

func TestListTools(t *testing.T) {
	cs := newSession(t, "client-a")
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	byName := map[string]*mcp.Tool{}
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		byName[tool.Name] = tool
	}
	slices.Sort(names)
	want := []string{"forget", "get_context", "get_memory", "handoff", "list_namespaces", "recall", "remember", "update_memory"}
	if !slices.Equal(names, want) {
		t.Fatalf("tools = %v, want %v", names, want)
	}

	for _, tool := range res.Tools {
		if tool.Description == "" {
			t.Errorf("%s: empty description", tool.Name)
		}
		b, _ := json.Marshal(tool.InputSchema)
		var sch map[string]any
		if err := json.Unmarshal(b, &sch); err != nil || sch["type"] != "object" {
			t.Errorf("%s: input schema is not an object: %s", tool.Name, b)
		}
		props, _ := sch["properties"].(map[string]any)
		for pname, p := range props {
			if d, _ := p.(map[string]any)["description"].(string); d == "" {
				t.Errorf("%s.%s: missing description", tool.Name, pname)
			}
		}
		if tool.Name != "get_context" && tool.OutputSchema == nil {
			t.Errorf("%s: missing output schema", tool.Name)
		}
	}

	for _, n := range []string{"get_context", "recall", "get_memory", "list_namespaces"} {
		if a := byName[n].Annotations; a == nil || !a.ReadOnlyHint {
			t.Errorf("%s should be readOnly", n)
		}
	}
	for _, n := range []string{"remember", "update_memory", "forget", "handoff"} {
		if a := byName[n].Annotations; a == nil || a.ReadOnlyHint {
			t.Errorf("%s should not be readOnly", n)
		}
	}
	if a := byName["forget"].Annotations; a.DestructiveHint == nil || *a.DestructiveHint {
		t.Error("forget should have destructiveHint=false")
	}

	b, _ := json.Marshal(byName["remember"].InputSchema)
	s := string(b)
	if !strings.Contains(s, `"decision"`) || strings.Contains(s, `"handoff"`) {
		t.Errorf("remember kind enum wrong: %s", s)
	}
	var req struct {
		Required []string `json:"required"`
	}
	_ = json.Unmarshal(b, &req)
	slices.Sort(req.Required)
	if !slices.Equal(req.Required, []string{"content", "kind"}) {
		t.Errorf("remember required = %v", req.Required)
	}
}

func TestScenario(t *testing.T) {
	cs := newSession(t, "client-a")

	// remember → the client name is recorded as source.
	r := callOK[RememberOutput](t, cs, "remember", map[string]any{
		"content": "PostgreSQL plutôt que MySQL, pour le support JSONB.", "kind": "decision",
		"namespace": "Demo", "title": "Base de données", "tags": []string{"db"},
	})
	id := r.Memory.ID
	if r.Memory.Namespace != "demo" || r.Memory.Source != "client-a" || len(r.PossibleDuplicates) != 0 {
		t.Fatalf("remember: %+v", r)
	}

	// recall (accents ignored, prefix).
	rc := callOK[RecallOutput](t, cs, "recall", map[string]any{"query": "base donnees postg"})
	if len(rc.Results) != 1 || rc.Results[0].ID != id || !strings.Contains(rc.Results[0].Snippet, "**") {
		t.Fatalf("recall: %+v", rc)
	}

	// update_memory.
	u := callOK[UpdateMemoryOutput](t, cs, "update_memory", map[string]any{"id": id, "importance": 3, "title": "SGBD"})
	if u.Memory.Importance != 3 || u.Memory.Title != "SGBD" || !slices.Equal(u.Changed, []string{"importance", "title"}) {
		t.Fatalf("update: %+v", u)
	}

	// get_context shows it under Essentiel.
	b := briefingText(t, cs, map[string]any{"namespace": "demo"})
	if !strings.Contains(b, fmt.Sprintf("#%d [decision] **SGBD**", id)) {
		t.Fatalf("briefing missing memory:\n%s", b)
	}

	// get_memory with history.
	g := callOK[GetMemoryOutput](t, cs, "get_memory", map[string]any{"id": id, "with_history": true})
	if len(g.History) != 2 || g.History[0].Action != "create" || g.History[1].Title != "SGBD" || g.History[0].ChangedBy != "client-a" {
		t.Fatalf("history: %+v", g.History)
	}

	// forget → gone from recall and get_context.
	f := callOK[ForgetOutput](t, cs, "forget", map[string]any{"id": id, "reason": "test"})
	if !f.Archived || !strings.Contains(f.Message, "restore") {
		t.Fatalf("forget: %+v", f)
	}
	if rc := callOK[RecallOutput](t, cs, "recall", map[string]any{"query": "postgresql"}); len(rc.Results) != 0 || rc.Hint == "" {
		t.Fatalf("recall after forget: %+v", rc)
	}
	if b := briefingText(t, cs, map[string]any{"namespace": "demo"}); strings.Contains(b, "SGBD") {
		t.Fatalf("forgotten memory in briefing:\n%s", b)
	}
	g = callOK[GetMemoryOutput](t, cs, "get_memory", map[string]any{"id": id, "with_history": true})
	if g.Memory.ArchivedAt == "" || g.History[2].Reason != "test" {
		t.Fatalf("archived memory: %+v", g)
	}

	// list_namespaces.
	ln := callOK[ListNamespacesOutput](t, cs, "list_namespaces", nil)
	if ln.Default != "global" || len(ln.Namespaces) != 2 || ln.Namespaces[0].Name != "demo" || ln.Namespaces[0].ActiveCount != 0 {
		t.Fatalf("list_namespaces: %+v", ln)
	}
}

func TestHandoffBetweenClients(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mem.db")

	// Session A (Claude Desktop) records a decision and a handoff.
	stA := openStore(t, path)
	a := connect(t, stA, Options{}, "claude-desktop")
	callOK[RememberOutput](t, a, "remember", map[string]any{
		"content": "PostgreSQL plutôt que MySQL, pour le support JSONB.", "kind": "decision", "namespace": "demo",
	})
	h1 := callOK[HandoffOutput](t, a, "handoff", map[string]any{"summary": "Premier jet", "namespace": "demo"})
	h2 := callOK[HandoffOutput](t, a, "handoff", map[string]any{
		"summary":        "Schéma prêt.\nReste l'API.",
		"next_steps":     []string{"écrire /orders", " ", "tests"},
		"open_questions": []string{"GraphQL ?"},
		"namespace":      "demo",
	})
	if !slices.Equal(h2.Replaced, []int64{h1.Memory.ID}) {
		t.Fatalf("second handoff should replace the first: %+v", h2)
	}
	if h2.Memory.Title != "Handoff: Schéma prêt." || h2.Memory.Kind != "handoff" {
		t.Errorf("handoff memory: %+v", h2.Memory)
	}
	for _, want := range []string{"## Summary\nSchéma prêt.", "## Next steps\n- écrire /orders\n- tests", "## Open questions\n- GraphQL ?"} {
		if !strings.Contains(h2.Memory.Content, want) {
			t.Errorf("handoff content missing %q:\n%s", want, h2.Memory.Content)
		}
	}
	_ = stA.Close()

	// Session B (VS Code), new process on the same file.
	stB := openStore(t, path)
	b := connect(t, stB, Options{DefaultNamespace: "demo"}, "vscode")
	brief := briefingText(t, b, nil) // default namespace from the server options
	for _, want := range []string{`namespace "demo"`, "Latest handoff", "claude-desktop", "Reste l'API", "PostgreSQL plutôt que MySQL"} {
		if !strings.Contains(brief, want) {
			t.Errorf("briefing in session B missing %q:\n%s", want, brief)
		}
	}
	if strings.Contains(brief, "Premier jet") {
		t.Error("replaced handoff still shown")
	}
	r := callOK[RememberOutput](t, b, "remember", map[string]any{"content": "API en Go", "kind": "fact"})
	if r.Memory.Source != "vscode" || r.Memory.Namespace != "demo" {
		t.Errorf("session B memory: %+v", r.Memory)
	}
}

func TestForcedSource(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "mem.db"))
	cs := connect(t, st, Options{Source: "claude-code"}, "some-client")
	r := callOK[RememberOutput](t, cs, "remember", map[string]any{"content": "x", "kind": "note"})
	if r.Memory.Source != "claude-code" {
		t.Errorf("source = %q, want forced claude-code", r.Memory.Source)
	}
}

func TestPossibleDuplicates(t *testing.T) {
	cs := newSession(t, "a")
	first := callOK[RememberOutput](t, cs, "remember", map[string]any{
		"content": "Le déploiement se fait avec Docker Compose sur un VPS Hetzner.", "kind": "fact", "namespace": "demo",
	})
	dup := callOK[RememberOutput](t, cs, "remember", map[string]any{
		"content": "Déploiement : Docker Compose sur VPS Hetzner.", "kind": "fact", "namespace": "demo",
	})
	if len(dup.PossibleDuplicates) != 1 || dup.PossibleDuplicates[0].ID != first.Memory.ID || dup.Hint == "" {
		t.Fatalf("expected duplicate warning: %+v", dup)
	}
	// Superseding the old one: no duplicate warning about it, and it is archived.
	repl := callOK[RememberOutput](t, cs, "remember", map[string]any{
		"content": "Le déploiement se fait avec Docker Compose sur un VPS Hetzner, via Ansible.", "kind": "fact",
		"namespace": "demo", "supersedes": []int64{first.Memory.ID, dup.Memory.ID},
	})
	if len(repl.PossibleDuplicates) != 0 || len(repl.Superseded) != 2 {
		t.Fatalf("supersedes: %+v", repl)
	}
}

func TestToolErrors(t *testing.T) {
	cs := newSession(t, "a")
	callErr(t, cs, "get_memory", map[string]any{"id": 999}, "#999 does not exist", "recall")
	callErr(t, cs, "update_memory", map[string]any{"id": 999, "title": "x"}, "does not exist")
	callErr(t, cs, "forget", map[string]any{"id": 999}, "does not exist")
	callErr(t, cs, "recall", map[string]any{"query": "x", "namespace": "nowhere"}, "nowhere", "global", "list_namespaces")
	// Unknown valid namespace in get_context returns an empty briefing without error; invalid name returns error.
	if b := briefingText(t, cs, map[string]any{"namespace": "nowhere"}); !strings.Contains(b, `namespace "nowhere"`) || !strings.Contains(b, "0 active memories") {
		t.Errorf("get_context on uncreated namespace: %s", b)
	}
	callErr(t, cs, "get_context", map[string]any{"namespace": "-bad"}, "namespace")
	callErr(t, cs, "remember", map[string]any{"content": "x", "kind": "opinion"})               // schema enum
	callErr(t, cs, "remember", map[string]any{"content": "x", "kind": "handoff"})               // handoff tool only
	callErr(t, cs, "remember", map[string]any{"kind": "fact"})                                  // missing content
	callErr(t, cs, "remember", map[string]any{"content": "   ", "kind": "fact"}, "content")     // blank
	callErr(t, cs, "remember", map[string]any{"content": "x", "kind": "fact", "importance": 7}) // bounds
	callErr(t, cs, "remember", map[string]any{"content": "x", "kind": "fact", "namespace": "-x"}, "namespace")
	callErr(t, cs, "remember", map[string]any{"content": strings.Repeat("a", 8001), "kind": "fact"})
	callErr(t, cs, "remember", map[string]any{"content": "key: sk-proj-" + strings.Repeat("a", 28), "kind": "fact"}, "secret")
	callErr(t, cs, "handoff", map[string]any{"summary": "  "}, "summary")

	m := callOK[RememberOutput](t, cs, "remember", map[string]any{"content": "x", "kind": "fact"})
	callErr(t, cs, "update_memory", map[string]any{"id": m.Memory.ID}, "no field to change")
	callErr(t, cs, "update_memory", map[string]any{"id": m.Memory.ID, "namespace": "-bad"}, "namespace")
	callErr(t, cs, "update_memory", map[string]any{"id": m.Memory.ID, "content": "sk-proj-" + strings.Repeat("a", 28)}, "secret")
	moved := callOK[UpdateMemoryOutput](t, cs, "update_memory", map[string]any{"id": m.Memory.ID, "namespace": "nowhere"})
	if moved.Memory.Namespace != "nowhere" {
		t.Errorf("update_memory should create namespace: %+v", moved)
	}
	callOK[ForgetOutput](t, cs, "forget", map[string]any{"id": m.Memory.ID})
	callErr(t, cs, "forget", map[string]any{"id": m.Memory.ID}, "already archived")
	callErr(t, cs, "update_memory", map[string]any{"id": m.Memory.ID, "title": "y"}, "archived")
}

func TestEmptyBaseBriefing(t *testing.T) {
	cs := newSession(t, "a")
	b := briefingText(t, cs, nil)
	if !strings.Contains(b, "No memory stored yet") {
		t.Errorf("welcome message missing:\n%s", b)
	}
	rc := callOK[RecallOutput](t, cs, "recall", map[string]any{"query": ""})
	if rc.Results == nil || len(rc.Results) != 0 {
		t.Errorf("empty recall: %+v", rc)
	}
}

func TestNewRejectsBadDefaultNamespace(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "mem.db"))
	if _, err := New(st, Options{DefaultNamespace: "-bad"}); err == nil {
		t.Error("expected error")
	}
	if _, err := New(st, Options{DefaultNamespace: "Mon Projet"}); err != nil {
		t.Errorf("normalisable namespace rejected: %v", err)
	}
}

func TestHandoffTitle(t *testing.T) {
	if got := handoffTitle("## Summary\nx"); got != "Handoff: Summary" {
		t.Errorf("got %q", got)
	}
	long := strings.Repeat("é", 100)
	if got := handoffTitle(long); len([]rune(got)) != len([]rune("Handoff: "))+maxHandoffTitle {
		t.Errorf("title not cut: %q", got)
	}
}

// meaningSearcher stands for the semantic engine: it finds memories whose
// content contains a synonym of the query.
type meaningSearcher struct{ st *store.Store }

func (m meaningSearcher) Search(ctx context.Context, q store.SearchQuery) ([]store.SearchResult, error) {
	if strings.EqualFold(q.Query, "sgbd") {
		q.Query = "postgresql"
	}
	return m.st.Search(ctx, q)
}

func TestRecallUsesSearcher(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "mem.db"))
	v1 := connect(t, st, Options{}, "a")
	callOK[RememberOutput](t, v1, "remember", map[string]any{"content": "PostgreSQL pour JSONB", "kind": "decision"})
	if rc := callOK[RecallOutput](t, v1, "recall", map[string]any{"query": "SGBD"}); len(rc.Results) != 0 {
		t.Fatalf("v1 recall found %+v", rc)
	}
	sem := connect(t, st, Options{Searcher: meaningSearcher{st}}, "b")
	if rc := callOK[RecallOutput](t, sem, "recall", map[string]any{"query": "SGBD"}); len(rc.Results) != 1 {
		t.Fatalf("semantic recall: %+v", rc)
	}
	desc := func(cs *mcp.ClientSession) string {
		res, _ := cs.ListTools(context.Background(), nil)
		for _, tool := range res.Tools {
			if tool.Name == "recall" {
				return tool.Description
			}
		}
		return ""
	}
	if strings.Contains(desc(v1), "Semantic") || !strings.Contains(desc(sem), "Semantic search is enabled") {
		t.Errorf("recall descriptions:\nv1: %s\nsemantic: %s", desc(v1), desc(sem))
	}
}

func TestRecallTagsAndHandoffSupersedesDescription(t *testing.T) {
	cs := newSession(t, "a")
	task := callOK[RememberOutput](t, cs, "remember", map[string]any{
		"content": "Implémenter les filtres de recherche", "kind": "task_state", "namespace": "demo", "tags": []string{"search", "v2"},
	})
	_ = callOK[RememberOutput](t, cs, "remember", map[string]any{
		"content": "Déploiement Docker", "kind": "fact", "namespace": "demo", "tags": []string{"infra"},
	})

	rc := callOK[RecallOutput](t, cs, "recall", map[string]any{"query": "", "tags": []string{"search", "v2"}})
	if len(rc.Results) != 1 || rc.Results[0].ID != task.Memory.ID {
		t.Fatalf("recall by tags: %+v", rc)
	}

	h := callOK[HandoffOutput](t, cs, "handoff", map[string]any{
		"summary":               "Filtres terminés.",
		"namespace":             "demo",
		"namespace_description": "Projet de démonstration",
		"supersedes":            []int64{task.Memory.ID},
	})
	if !slices.Equal(h.Replaced, []int64{task.Memory.ID}) {
		t.Fatalf("handoff supersedes: %+v", h)
	}
	g := callOK[GetMemoryOutput](t, cs, "get_memory", map[string]any{"id": task.Memory.ID})
	if g.Memory.ArchivedAt == "" {
		t.Errorf("task_state #%d should be archived", task.Memory.ID)
	}
	ln := callOK[ListNamespacesOutput](t, cs, "list_namespaces", nil)
	var foundDesc string
	for _, n := range ln.Namespaces {
		if n.Name == "demo" {
			foundDesc = n.Description
		}
	}
	if foundDesc != "Projet de démonstration" {
		t.Errorf("namespace description = %q", foundDesc)
	}
}

func TestResourcesAndPrompts(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "mem.db"))
	cs := connect(t, st, Options{DefaultNamespace: "global"}, "client-a")
	ctx := context.Background()

	callOK[RememberOutput](t, cs, "remember", map[string]any{
		"content": "Préférer Go pour les CLI.", "kind": "preference", "namespace": "global",
	})
	callOK[RememberOutput](t, cs, "remember", map[string]any{
		"content": "Base SQLite en mode WAL.", "kind": "decision", "namespace": "demo",
	})

	// ListResources & ListResourceTemplates
	lr, err := cs.ListResources(ctx, nil)
	if err != nil || len(lr.Resources) != 1 || lr.Resources[0].URI != contextResourceURI {
		t.Fatalf("ListResources = %+v, %v", lr, err)
	}
	lt, err := cs.ListResourceTemplates(ctx, nil)
	if err != nil || len(lt.ResourceTemplates) != 1 || lt.ResourceTemplates[0].URITemplate != contextResourceURITemplate {
		t.Fatalf("ListResourceTemplates = %+v, %v", lt, err)
	}

	// Read default resource & templated resource
	rDef, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: "memory://context"})
	if err != nil || len(rDef.Contents) != 1 || !strings.Contains(rDef.Contents[0].Text, "Préférer Go") {
		t.Fatalf("ReadResource default = %+v, %v", rDef, err)
	}
	rDemo, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: "memory://context/demo"})
	if err != nil || len(rDemo.Contents) != 1 || !strings.Contains(rDemo.Contents[0].Text, "Base SQLite") {
		t.Fatalf("ReadResource demo = %+v, %v", rDemo, err)
	}
	if _, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: "memory://context/-bad"}); err == nil {
		t.Error("expected error reading invalid namespace resource")
	}

	// ListPrompts & GetPrompt
	lp, err := cs.ListPrompts(ctx, nil)
	if err != nil || len(lp.Prompts) != 2 {
		t.Fatalf("ListPrompts = %+v, %v", lp, err)
	}
	pb, err := cs.GetPrompt(ctx, &mcp.GetPromptParams{Name: "briefing", Arguments: map[string]string{"namespace": "demo"}})
	if err != nil || len(pb.Messages) != 1 {
		t.Fatalf("GetPrompt briefing = %+v, %v", pb, err)
	}
	if tc, ok := pb.Messages[0].Content.(*mcp.TextContent); !ok || !strings.Contains(tc.Text, "Base SQLite") {
		t.Errorf("briefing prompt content = %#v", pb.Messages[0].Content)
	}

	ph, err := cs.GetPrompt(ctx, &mcp.GetPromptParams{Name: "handoff", Arguments: map[string]string{"namespace": "demo"}})
	if err != nil || len(ph.Messages) != 1 {
		t.Fatalf("GetPrompt handoff = %+v, %v", ph, err)
	}
	if tc, ok := ph.Messages[0].Content.(*mcp.TextContent); !ok || !strings.Contains(tc.Text, "supersedes") || !strings.Contains(tc.Text, `"demo"`) {
		t.Errorf("handoff prompt content = %#v", ph.Messages[0].Content)
	}
}

func TestReadOnlyServer(t *testing.T) {
	ctx := context.Background()
	st := openStore(t, filepath.Join(t.TempDir(), "mem.db"))
	if _, err := st.Create(ctx, store.NewMemory{Kind: store.KindFact, Content: "read-only visible"}); err != nil {
		t.Fatal(err)
	}
	cs := connect(t, st, Options{ReadOnly: true}, "reader")
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	names := make([]string, len(tools.Tools))
	for i, tool := range tools.Tools {
		names[i] = tool.Name
	}
	for _, want := range []string{"get_context", "recall", "get_memory", "list_namespaces"} {
		if !slices.Contains(names, want) {
			t.Errorf("missing read tool %q in %v", want, names)
		}
	}
	for _, forbid := range []string{"remember", "update_memory", "forget", "handoff"} {
		if slices.Contains(names, forbid) {
			t.Errorf("write tool %q should not be registered in read-only mode", forbid)
		}
	}
	rc := callOK[RecallOutput](t, cs, "recall", map[string]any{"query": "visible"})
	if len(rc.Results) != 1 {
		t.Errorf("recall in read-only mode = %+v", rc)
	}
}

func TestStreamableHTTP(t *testing.T) {
	ctx := context.Background()
	st := openStore(t, filepath.Join(t.TempDir(), "mem.db"))
	h, err := NewHTTPHandler(st, Options{Version: "test"}, &mcp.StreamableHTTPOptions{Stateless: true})
	if err != nil {
		t.Fatalf("NewHTTPHandler: %v", err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()

	// Liveness probe
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200", resp.StatusCode)
	}

	// Connect an MCP client over Streamable HTTP
	client := mcp.NewClient(&mcp.Implementation{Name: "http-client", Version: "1.0"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatalf("StreamableClientTransport connect: %v", err)
	}
	defer func() { _ = cs.Close() }()

	mem := callOK[RememberOutput](t, cs, "remember", map[string]any{
		"kind":    "fact",
		"content": "stored over streamable HTTP",
	})
	if mem.Memory.Source != "http-client" {
		t.Errorf("source = %q, want http-client", mem.Memory.Source)
	}
	b := briefingText(t, cs, nil)
	if !strings.Contains(b, "stored over streamable HTTP") {
		t.Errorf("briefing missing HTTP memory:\n%s", b)
	}
}

func TestDecisionClaimCaveatSourceSwitchClients(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mem.db")

	const (
		claim    = "Use PostgreSQL 16 with transactional outbox for order state persistence so billing and fulfillment stay atomic."
		caveat   = "Caveat: assumes shard write throughput stays below 4,000 TPS; above that, partition by tenant_id before enabling synchronous cross-region replication."
		sourceID = "Source: ADR-042 / PR #184"
	)
	fullDecision := claim + " " + caveat + " " + sourceID

	// Client A saves a decision (>240 runes) with claim, caveat and source ID, plus a handoff.
	stA := openStore(t, path)
	a := connect(t, stA, Options{}, "claude-desktop")
	dec := callOK[RememberOutput](t, a, "remember", map[string]any{
		"namespace": "orders",
		"kind":      "decision",
		"title":     "Order store engine (ADR-042)",
		"content":   fullDecision,
	})
	callOK[HandoffOutput](t, a, "handoff", map[string]any{
		"namespace":  "orders",
		"summary":    "Recorded order store decision ADR-042 with throughput caveat.",
		"next_steps": []string{"review ADR-042 before implementing partitioning"},
	})
	_ = stA.Close()

	// Client B connects in a new process/client and reads via get_context, recall, and get_memory.
	stB := openStore(t, path)
	b := connect(t, stB, Options{DefaultNamespace: "orders"}, "vscode")

	brief := briefingText(t, b, nil)
	for _, want := range []string{claim, caveat, sourceID, "claude-desktop"} {
		if !strings.Contains(brief, want) {
			t.Errorf("get_context after client switch missing %q:\n%s", want, brief)
		}
	}

	rc := callOK[RecallOutput](t, b, "recall", map[string]any{"query": "ADR-042", "namespace": "orders"})
	if len(rc.Results) == 0 {
		t.Fatalf("recall returned no results for ADR-042")
	}

	got := callOK[GetMemoryOutput](t, b, "get_memory", map[string]any{"id": dec.Memory.ID})
	for _, want := range []string{claim, caveat, sourceID} {
		if !strings.Contains(got.Memory.Content, want) {
			t.Errorf("get_memory missing %q:\n%s", want, got.Memory.Content)
		}
	}
	if got.Memory.Source != "claude-desktop" {
		t.Errorf("source = %q, want claude-desktop", got.Memory.Source)
	}
}

type stubSemanticSearcher struct {
	hit store.Memory
}

func (s *stubSemanticSearcher) Search(_ context.Context, _ store.SearchQuery) ([]store.SearchResult, error) {
	return []store.SearchResult{{Memory: s.hit}}, nil
}

func TestGetContextFocusUsesCustomSearcher(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "mem.db"))
	note, err := st.Create(context.Background(), store.NewMemory{
		Namespace: "demo",
		Kind:      store.KindNote,
		Title:     "Postgres JSONB",
		Content:   "PostgreSQL stores flexible schemas in JSONB",
	})
	if err != nil {
		t.Fatal(err)
	}
	cs := connect(t, st, Options{DefaultNamespace: "demo", Searcher: &stubSemanticSearcher{hit: note}}, "client")
	brief := briefingText(t, cs, map[string]any{"namespace": "demo", "focus": "SGBD"})
	if !strings.Contains(brief, fmt.Sprintf("#%d", note.ID)) {
		t.Errorf("get_context focus did not use custom searcher:\n%s", brief)
	}
}

func TestMemoryLinksTools(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "mem.db"))
	cs := connect(t, st, Options{DefaultNamespace: "demo"}, "client")

	m1 := callOK[RememberOutput](t, cs, "remember", map[string]any{
		"kind":    "decision",
		"title":   "Use MySQL",
		"content": "Initial database choice",
	})
	m2 := callOK[RememberOutput](t, cs, "remember", map[string]any{
		"kind":       "decision",
		"title":      "Use PostgreSQL",
		"content":    "PostgreSQL replaces MySQL",
		"supersedes": []int64{m1.Memory.ID},
	})
	m3 := callOK[RememberOutput](t, cs, "remember", map[string]any{
		"kind":       "fact",
		"title":      "GIN index",
		"content":    "JSONB uses GIN index",
		"depends_on": []int64{m2.Memory.ID},
	})

	// update_memory with add_links only, then remove_links.
	up := callOK[UpdateMemoryOutput](t, cs, "update_memory", map[string]any{
		"id": m3.Memory.ID,
		"add_links": []map[string]any{
			{"target_id": m1.Memory.ID, "kind": "relates_to"},
		},
	})
	if !slices.Contains(up.Changed, "links") {
		t.Errorf("expected links in changed, got %v", up.Changed)
	}

	got3 := callOK[GetMemoryOutput](t, cs, "get_memory", map[string]any{"id": m3.Memory.ID})
	if len(got3.Links) != 2 {
		t.Fatalf("expected 2 links on m3, got %+v", got3.Links)
	}

	callOK[UpdateMemoryOutput](t, cs, "update_memory", map[string]any{
		"id": m3.Memory.ID,
		"remove_links": []map[string]any{
			{"target_id": m1.Memory.ID, "kind": "relates_to"},
		},
	})
	got2 := callOK[GetMemoryOutput](t, cs, "get_memory", map[string]any{"id": m2.Memory.ID})
	if len(got2.Links) != 2 {
		t.Fatalf("expected 2 links on m2 (supersedes m1, incoming depends_on m3), got %+v", got2.Links)
	}
}
