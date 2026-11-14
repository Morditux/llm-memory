package store

import (
	"context"
	"strings"
	"testing"
)

type seed struct {
	ns, title, content string
	kind               Kind
	tags               []string
}

// corpus: 30 memories, French and English, across three namespaces.
var corpus = []seed{
	{"demo", "Base de données", "Décision : PostgreSQL plutôt que MySQL, pour le support JSONB.", KindDecision, []string{"db", "postgres"}},
	{"demo", "", "Les migrations sont appliquées au démarrage via PRAGMA user_version.", KindFact, []string{"db"}},
	{"demo", "Préférence de style", "L'utilisateur préfère des réponses courtes, en français.", KindPreference, nil},
	{"demo", "", "Le déploiement se fait avec Docker Compose sur un VPS Hetzner.", KindFact, []string{"infra"}},
	{"demo", "Cache", "Redis est utilisé comme cache de sessions, TTL 30 minutes.", KindFact, []string{"cache"}},
	{"demo", "", "Tâche en cours : écrire les tests d'intégration de l'API REST.", KindTaskState, nil},
	{"demo", "Authentification", "Choix de JWT signés en EdDSA, rotation des clés tous les 90 jours.", KindDecision, []string{"auth", "sécurité"}},
	{"demo", "", "La CI tourne sur GitHub Actions avec un cache des modules Go.", KindFact, []string{"ci"}},
	{"demo", "Front-end", "Le front-end utilise htmx et des templates Go, pas de SPA.", KindDecision, []string{"web"}},
	{"demo", "", "Les logs sont structurés en JSON avec slog, niveau info en production.", KindFact, nil},
	{"demo", "Économie", "Le budget mensuel d'hébergement ne doit pas dépasser 50 euros.", KindNote, []string{"coût"}},
	{"demo", "", "Énoncé du problème : les requêtes lentes viennent d'un index manquant sur orders(created_at).", KindNote, []string{"perf"}},
	{"global", "Language", "The user prefers concise answers and code examples in Go.", KindPreference, nil},
	{"global", "", "Never commit secrets; use environment variables or a vault.", KindPreference, []string{"security"}},
	{"global", "Editor", "Primary editor is VS Code with the Go extension.", KindFact, nil},
	{"global", "", "Timezone is Europe/Paris; dates in ISO 8601.", KindPreference, nil},
	{"global", "Shell", "The user works on Linux with bash and tmux.", KindFact, nil},
	{"global", "", "Prefer table-driven tests and t.TempDir() for files.", KindPreference, []string{"testing"}},
	{"api", "Rate limiting", "Rate limiting uses a token bucket: 100 requests per minute per API key.", KindDecision, []string{"api"}},
	{"api", "", "Pagination is cursor-based with an opaque base64 cursor.", KindDecision, nil},
	{"api", "Errors", "Errors follow RFC 9457 problem+json responses.", KindDecision, nil},
	{"api", "", "The OpenAPI spec lives in api/openapi.yaml and is the source of truth.", KindFact, []string{"spec"}},
	{"api", "Versioning", "Breaking changes require a new /v2 prefix; v1 is supported for 12 months.", KindDecision, nil},
	{"api", "", "Webhooks are signed with HMAC-SHA256 in the X-Signature header.", KindFact, []string{"security"}},
	{"api", "Handoff", "Next step: implement idempotency keys for POST /orders (C++ client needs it).", KindHandoff, nil},
	{"api", "", "Open question: should we expose GraphQL alongside REST?", KindNote, nil},
	{"api", "Database", "The API reads from a PostgreSQL read replica for list endpoints.", KindFact, []string{"db", "postgres"}},
	{"api", "", "Load tests with k6 show p99 latency of 120 ms at 500 rps.", KindFact, []string{"perf"}},
	{"api", "Deprecated", "Legacy SOAP endpoint was removed in March.", KindNote, nil},
	{"api", "", "Retry policy: exponential backoff with jitter, max 5 attempts.", KindDecision, nil},
}

func seedCorpus(t *testing.T) (*Store, map[string]int64) {
	t.Helper()
	s := newTestStore(t)
	ids := map[string]int64{}
	for _, c := range corpus {
		m, err := s.Create(context.Background(), NewMemory{Namespace: c.ns, Kind: c.kind, Title: c.title, Content: c.content, Tags: c.tags, Source: "seed"})
		if err != nil {
			t.Fatalf("seed %q: %v", c.content, err)
		}
		ids[string([]rune(c.content)[:20])] = m.ID
	}
	if len(corpus) != 30 {
		t.Fatalf("corpus has %d entries, want 30", len(corpus))
	}
	return s, ids
}

func search(t *testing.T, s *Store, q SearchQuery) []SearchResult {
	t.Helper()
	res, err := s.Search(context.Background(), q)
	if err != nil {
		t.Fatalf("Search(%+v): %v", q, err)
	}
	return res
}

func contentsOf(res []SearchResult) string {
	var b []string
	for _, r := range res {
		b = append(b, r.Content)
	}
	return strings.Join(b, " | ")
}

func TestQueryTerms(t *testing.T) {
	cases := map[string]string{
		`hello world`:              "hello,world",
		`"unbalanced`:              "unbalanced",
		`title:foo -bar (baz) OR*`: "title,foo,bar,baz,OR",
		`C++ né-e e2e`:             "C,né,e,e2e",
		`***  ::: "" ()`:           "",
		`Go go GO`:                 "Go",
		"école":                   "école", // combining accent kept in the word
	}
	for in, want := range cases {
		if got := strings.Join(queryTerms(in, 16), ","); got != want {
			t.Errorf("queryTerms(%q) = %q, want %q", in, got, want)
		}
	}
	if got := buildMatch([]string{"a", "b"}, true, "AND"); got != `"a"* AND "b"*` {
		t.Errorf("buildMatch = %s", got)
	}
	if got := quoteTerm(`x"y`); got != `"x""y"` {
		t.Errorf("quoteTerm = %s", got)
	}
}

func TestSearchAccentsAndPrefixes(t *testing.T) {
	s, _ := seedCorpus(t)
	cases := []struct {
		query string
		want  string // substring that must appear in the first result
	}{
		{"decision postgresql", "PostgreSQL plutôt que MySQL"},
		{"DÉCISION", "PostgreSQL plutôt que MySQL"},
		{"preference style", "réponses courtes"},
		{"economie", "budget mensuel"},
		{"enonce probleme", "index manquant"},
		{"postg", "PostgreSQL"},             // prefix
		{"déploi docker", "Docker Compose"}, // prefix + accent
		{"securite", "EdDSA"},               // tag with accent
		{"idempot", "idempotency keys"},
		{"hmac", "HMAC-SHA256"},
	}
	for _, c := range cases {
		res := search(t, s, SearchQuery{Query: c.query})
		if len(res) == 0 || !strings.Contains(res[0].Content, c.want) {
			t.Errorf("Search(%q): first result %q, want it to contain %q", c.query, contentsOf(res), c.want)
		}
	}
}

func TestSearchNeverFailsOnSpecialInput(t *testing.T) {
	s, _ := seedCorpus(t)
	inputs := []string{
		``, `   `, `"`, `""`, `"""`, `*`, `**`, `:`, `-`, `--`, `(`, `)`, `()`, `((`, `^`, `+`, `.`,
		`"postgres`, `postgres"`, `post*gres`, `*postgres`, `title:postgres`, `content:`, `-postgres`,
		`NOT`, `AND`, `OR`, `NEAR`, `NEAR(a b)`, `a AND`, `OR b`, `NOT NOT`, `postgres NOT mysql`,
		`{title content}: x`, `rowid = 1`, `'; DROP TABLE memories; --`, `\x00`, "\x00null",
		`C++ / C#`, `x-signature`, `HMAC-SHA256`, `RFC 9457`, `p99`, `🙂 émoji`, `日本語`, `Ελληνικά`,
		strings.Repeat("mot ", 500), strings.Repeat(`"(*:-`, 200),
	}
	for _, in := range inputs {
		if _, err := s.Search(context.Background(), SearchQuery{Query: in}); err != nil {
			t.Errorf("Search(%q): %v", in, err)
		}
		if _, err := s.FindSimilar(context.Background(), in, "demo", 3, 0); err != nil {
			t.Errorf("FindSimilar(%q): %v", in, err)
		}
	}
	// Special characters act as separators, so these still find something.
	for _, in := range []string{`"postgres`, `(postgres)`, `title:postgres`, `-postgres`, `x-signature`, `HMAC-SHA256`} {
		if len(search(t, s, SearchQuery{Query: in})) == 0 {
			t.Errorf("Search(%q) found nothing", in)
		}
	}
}

func TestSearchEmptyQueryListsRecent(t *testing.T) {
	s, _ := seedCorpus(t)
	res := search(t, s, SearchQuery{Query: " *** ", Namespace: "api", Limit: 3})
	if len(res) != 3 {
		t.Fatalf("got %d results", len(res))
	}
	if !strings.HasPrefix(res[0].Content, "Retry policy") { // last inserted in api
		t.Errorf("first = %q", res[0].Content)
	}
	if res[0].Snippet == "" || res[0].Score != 0 {
		t.Errorf("snippet/score = %q/%v", res[0].Snippet, res[0].Score)
	}
}

func TestSearchAndThenOr(t *testing.T) {
	s, _ := seedCorpus(t)
	// AND: both words in the same memory.
	res := search(t, s, SearchQuery{Query: "postgresql replica"})
	if len(res) != 1 || !strings.Contains(res[0].Content, "read replica") {
		t.Errorf("AND: %s", contentsOf(res))
	}
	// No memory has both words: falls back to OR.
	res = search(t, s, SearchQuery{Query: "redis graphql"})
	if len(res) != 2 {
		t.Errorf("OR fallback: %s", contentsOf(res))
	}
	// Nothing at all.
	if res := search(t, s, SearchQuery{Query: "kubernetes"}); len(res) != 0 {
		t.Errorf("unexpected: %s", contentsOf(res))
	}
}

func TestSearchFiltersAndLimits(t *testing.T) {
	s, ids := seedCorpus(t)
	ctx := context.Background()

	res := search(t, s, SearchQuery{Query: "postgresql", Namespace: "API"})
	if len(res) != 1 || res[0].Namespace != "api" {
		t.Errorf("namespace filter: %s", contentsOf(res))
	}
	res = search(t, s, SearchQuery{Query: "postgresql", Kinds: []Kind{KindFact}})
	if len(res) != 1 || res[0].Kind != KindFact {
		t.Errorf("kind filter: %s", contentsOf(res))
	}
	if _, err := s.Search(ctx, SearchQuery{Query: "x", Namespace: "nope"}); err == nil {
		t.Error("unknown namespace accepted")
	}
	if _, err := s.Search(ctx, SearchQuery{Query: "x", Kinds: []Kind{"bogus"}}); err == nil {
		t.Error("unknown kind accepted")
	}

	// Archived excluded unless requested.
	id := ids["Redis est utilisé co"]
	if _, err := s.Archive(ctx, id, "t", ""); err != nil {
		t.Fatal(err)
	}
	if res := search(t, s, SearchQuery{Query: "redis"}); len(res) != 0 {
		t.Errorf("archived returned: %s", contentsOf(res))
	}
	if res := search(t, s, SearchQuery{Query: "redis", IncludeArchived: true}); len(res) != 1 || !res[0].Archived() {
		t.Errorf("include archived: %s", contentsOf(res))
	}

	// Limits: default 10, max 50.
	if res := search(t, s, SearchQuery{}); len(res) != DefaultSearchLimit {
		t.Errorf("default limit: %d", len(res))
	}
	if res := search(t, s, SearchQuery{Limit: 1000, IncludeArchived: true}); len(res) != 30 {
		t.Errorf("capped limit over 30 rows: %d", len(res))
	}
	if clampLimit(1000) != MaxSearchLimit || clampLimit(-1) != DefaultSearchLimit || clampLimit(7) != 7 {
		t.Error("clampLimit")
	}
}

func TestSearchTitleWeightAndSnippet(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	body, _ := s.Create(ctx, NewMemory{Kind: KindFact, Content: "We discussed the cache layer and other topics at length today."})
	titled, _ := s.Create(ctx, NewMemory{Kind: KindFact, Title: "cache", Content: "Uses an in-memory LRU with 10k entries maximum for hot keys."})
	res := search(t, s, SearchQuery{Query: "cache"})
	if len(res) != 2 || res[0].ID != titled.ID || res[1].ID != body.ID {
		t.Fatalf("title should rank first: %+v", res)
	}
	if !strings.Contains(res[1].Snippet, "**cache**") {
		t.Errorf("snippet without markers: %q", res[1].Snippet)
	}
	if res[0].Score >= 0 {
		t.Errorf("bm25 score should be negative, got %v", res[0].Score)
	}
}

func TestFindSimilar(t *testing.T) {
	s, ids := seedCorpus(t)
	ctx := context.Background()
	pg := ids["Décision : PostgreSQ"]

	sim, err := s.FindSimilar(ctx, "On a choisi PostgreSQL plutôt que MySQL pour JSONB", "demo", 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(sim) == 0 || sim[0].ID != pg || sim[0].Similarity < SimilarityThreshold {
		t.Fatalf("expected #%d as duplicate, got %+v", pg, sim)
	}
	if sim[0].Snippet == "" {
		t.Error("missing snippet")
	}

	// Same wording without accents still matches.
	if sim, _ := s.FindSimilar(ctx, "decision postgresql plutot que mysql support jsonb", "demo", 3, 0); len(sim) == 0 || sim[0].ID != pg {
		t.Errorf("unaccented: %+v", sim)
	}
	// Other namespace: not a duplicate.
	if sim, _ := s.FindSimilar(ctx, "PostgreSQL plutôt que MySQL pour JSONB", "api", 3, 0); len(sim) != 0 {
		t.Errorf("cross-namespace duplicate: %+v", sim)
	}
	// Unrelated text sharing one common word: no duplicate.
	if sim, _ := s.FindSimilar(ctx, "Le cache des images est stocké sur S3 avec CloudFront", "demo", 3, 0); len(sim) != 0 {
		t.Errorf("false positive: %+v", sim)
	}
	// excludeID skips the memory itself.
	if sim, _ := s.FindSimilar(ctx, corpus[0].content, "demo", 3, pg); len(sim) != 0 && sim[0].ID == pg {
		t.Errorf("excluded id returned: %+v", sim)
	}
	// Archived memories are not duplicates.
	_, _ = s.Archive(ctx, pg, "t", "")
	if sim, _ := s.FindSimilar(ctx, corpus[0].content, "demo", 3, 0); len(sim) != 0 {
		t.Errorf("archived duplicate: %+v", sim)
	}
	// Unknown namespace (remember creates it later) and n<=0: empty, no error.
	if sim, err := s.FindSimilar(ctx, "anything here", "brand-new", 3, 0); err != nil || len(sim) != 0 {
		t.Errorf("new namespace: %+v, %v", sim, err)
	}
	if sim, err := s.FindSimilar(ctx, "anything", "demo", 0, 0); err != nil || len(sim) != 0 {
		t.Errorf("n=0: %+v, %v", sim, err)
	}
	if _, err := s.FindSimilar(ctx, "x", "-bad", 3, 0); err == nil {
		t.Error("invalid namespace accepted")
	}
}

func TestFindSimilarLongText(t *testing.T) {
	s, _ := seedCorpus(t)
	long := strings.Repeat("Le déploiement se fait avec Docker Compose sur un VPS Hetzner. ", 3) +
		"Ajoutons beaucoup de mots distincts : alpha bravo charlie delta echo foxtrot golf hotel india juliett kilo lima " +
		"mike november oscar papa quebec romeo sierra tango uniform victor whiskey xray yankee zulu."
	if _, err := s.FindSimilar(context.Background(), long, "demo", 3, 0); err != nil {
		t.Fatal(err)
	}
}

func TestFold(t *testing.T) {
	for in, want := range map[string]string{"Économie": "economie", "Cœur": "coeur", "école": "ecole", "Straße": "strasse"} {
		if got := fold(in); got != want {
			t.Errorf("fold(%q) = %q, want %q", in, got, want)
		}
	}
}
