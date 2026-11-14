package briefing

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Morditux/llm-memory/internal/store"
)

var update = flag.Bool("update", false, "rewrite testdata/*.golden")

var dateRe = regexp.MustCompile(`\d{4}-\d{2}-\d{2} \d{2}:\d{2} UTC`)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "mem.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mustCreate(t *testing.T, s *store.Store, m store.NewMemory) store.Memory {
	t.Helper()
	got, err := s.Create(context.Background(), m)
	if err != nil {
		t.Fatalf("Create(%+v): %v", m, err)
	}
	return got
}

// loadedStore builds a realistic base: a "demo" project, global preferences
// and a second project.
func loadedStore(t *testing.T) *store.Store {
	t.Helper()
	s := newStore(t)
	ctx := context.Background()
	g := func(kind store.Kind, title, content string, imp int) {
		mustCreate(t, s, store.NewMemory{Namespace: "global", Kind: kind, Title: title, Content: content, Importance: imp, Source: "claude-desktop"})
	}
	d := func(kind store.Kind, title, content string, imp int, src string) store.Memory {
		return mustCreate(t, s, store.NewMemory{Namespace: "demo", Kind: kind, Title: title, Content: content, Importance: imp, Source: src})
	}

	g(store.KindPreference, "Langue", "Répondre en français, de façon concise.", 2)
	g(store.KindPreference, "", "Ne jamais stocker de secrets dans le dépôt.", 3)
	g(store.KindFact, "Éditeur", "VS Code avec l'extension Go.", 2)

	_, _ = s.EnsureNamespace(ctx, "api", "API publique")
	mustCreate(t, s, store.NewMemory{Namespace: "api", Kind: store.KindDecision, Content: "Pagination par curseur opaque.", Source: "vscode"})

	d(store.KindPreference, "Tests", "Tests en table, base dans t.TempDir().", 2, "claude-code")
	d(store.KindFact, "Hébergement", "VPS Hetzner, déploiement Docker Compose.", 2, "claude-code")
	d(store.KindFact, "", "Les migrations s'appliquent au démarrage via PRAGMA user_version.", 2, "claude-code")
	for i := 1; i <= 12; i++ {
		d(store.KindDecision, fmt.Sprintf("Décision %02d", i), fmt.Sprintf("Contenu de la décision numéro %d, avec sa raison.", i), 2, "claude-desktop")
	}
	d(store.KindDecision, "Base de données", "PostgreSQL plutôt que MySQL, pour le support JSONB.", 3, "claude-desktop")
	d(store.KindTaskState, "API REST", "Tests d'intégration écrits pour /orders ; reste /users.\nBloqué par l'absence de fixtures.", 2, "claude-code")
	d(store.KindNote, "", "Idée : exposer GraphQL plus tard.", 1, "vscode")
	long := strings.Repeat("Un fait très long qui doit être coupé dans la liste. ", 10)
	d(store.KindFact, "", long, 2, "vscode")
	old := d(store.KindHandoff, "", "Old handoff.", 2, "vscode")
	if _, err := s.Archive(ctx, old.ID, "vscode", "remplacée"); err != nil {
		t.Fatal(err)
	}
	d(store.KindHandoff, "Passation", "## Summary\nThe schema is in place, search works.\n\n## Next steps\n- write the briefing\n- wire the MCP server\n\n## Open questions\n- duplicate threshold?", 2, "claude-desktop")
	return s
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	got = dateRe.ReplaceAllString(got, "<DATE>")
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
		t.Fatalf("read golden (run go test -update): %v", err)
	}
	if got != string(want) {
		t.Errorf("%s mismatch (run go test ./internal/briefing -update to accept)\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

func build(t *testing.T, s *store.Store, opt Options) string {
	t.Helper()
	out, err := Build(context.Background(), s, opt)
	if err != nil {
		t.Fatalf("Build(%+v): %v", opt, err)
	}
	return out
}

func TestGolden(t *testing.T) {
	empty := newStore(t)
	loaded := loadedStore(t)

	cases := []struct {
		name string
		s    *store.Store
		opt  Options
	}{
		{"empty", empty, Options{}},
		{"loaded_demo", loaded, Options{Namespace: "demo"}},
		{"loaded_global", loaded, Options{}},
		{"no_handoff", loaded, Options{Namespace: "api"}},
		{"budget_1200", loaded, Options{Namespace: "demo", MaxChars: 1200}},
		{"budget_min", loaded, Options{Namespace: "demo", MaxChars: 10}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			golden(t, c.name, build(t, c.s, c.opt))
		})
	}
}

func TestEmptyNamespaceInLoadedBase(t *testing.T) {
	s := loadedStore(t)
	if _, err := s.EnsureNamespace(context.Background(), "vide", ""); err != nil {
		t.Fatal(err)
	}
	out := build(t, s, Options{Namespace: "vide"})
	for _, want := range []string{"0 active memories", "No active memory", "Global preferences", "Ne jamais stocker", "Other namespaces"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestBudgetNeverExceeded(t *testing.T) {
	s := loadedStore(t)
	full := build(t, s, Options{Namespace: "demo", MaxChars: 100000})
	for budget := MinMaxChars; budget <= utf8.RuneCountInString(full)+50; budget += 37 {
		out := build(t, s, Options{Namespace: "demo", MaxChars: budget})
		if n := utf8.RuneCountInString(out); n > budget {
			t.Fatalf("budget %d: output has %d characters", budget, n)
		}
		if !strings.HasPrefix(out, "# Shared memory") {
			t.Fatalf("budget %d: header missing", budget)
		}
	}
}

func TestContentOrderAndDedup(t *testing.T) {
	s := loadedStore(t)
	out := build(t, s, Options{Namespace: "demo"})

	order := []string{"## Latest handoff", "## Essential", "## Ongoing tasks", "## Project preferences",
		"## Global preferences", "## Recent decisions", "## Recent facts", "Other namespaces"}
	pos := -1
	for _, h := range order {
		p := strings.Index(out, h)
		if p < 0 || p < pos {
			t.Fatalf("section %q missing or out of order in:\n%s", h, out)
		}
		pos = p
	}
	// Importance-3 decision appears once (in Essential), not again in decisions.
	if strings.Count(out, "PostgreSQL plutôt que MySQL") != 1 {
		t.Error("importance-3 memory duplicated")
	}
	// Cap of 10 decisions, the 2 oldest announced.
	if strings.Contains(out, "Décision 01") || strings.Contains(out, "Décision 02") || !strings.Contains(out, "Décision 12") {
		t.Error("decision cap or ordering wrong")
	}
	if !strings.Contains(out, "2 more, use recall (kinds: decision)") {
		t.Error("missing omitted-decisions note")
	}
	// Archived handoff is not shown; the active one is shown in full.
	if strings.Contains(out, "Old handoff") || !strings.Contains(out, "duplicate threshold?") {
		t.Error("handoff selection wrong")
	}
	// Facts with a title show only the title.
	if strings.Contains(out, "VPS Hetzner") || !strings.Contains(out, "**Hébergement**") {
		t.Error("fact with title should show the title only")
	}
	// Long lines are cut.
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "- #") && utf8.RuneCountInString(l) > maxLineRunes {
			t.Errorf("line too long (%d): %s", utf8.RuneCountInString(l), l)
		}
	}
}

func TestUnknownNamespace(t *testing.T) {
	s := newStore(t)
	_, err := Build(context.Background(), s, Options{Namespace: "nope"})
	var nf *store.NamespaceNotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("err = %v, want NamespaceNotFoundError", err)
	}
}

func TestHandoffTruncatedOnSmallBudget(t *testing.T) {
	s := newStore(t)
	m := mustCreate(t, s, store.NewMemory{Kind: store.KindHandoff, Content: strings.Repeat("étape ", 300), Source: "a"})
	out := build(t, s, Options{MaxChars: 600})
	if utf8.RuneCountInString(out) > 600 {
		t.Fatalf("over budget: %d", utf8.RuneCountInString(out))
	}
	if !strings.Contains(out, fmt.Sprintf("get_memory #%d", m.ID)) || !strings.Contains(out, "truncated") {
		t.Errorf("truncated handoff should point to get_memory:\n%s", out)
	}
}
