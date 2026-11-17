package store

import (
	"context"
	"math"
	"testing"
	"time"
)

func TestVectorEncoding(t *testing.T) {
	v := []float32{0, 1, -1.5, math.MaxFloat32, float32(math.Inf(-1)), 3.14159}
	got, err := DecodeVector(EncodeVector(v))
	if err != nil || len(got) != len(v) {
		t.Fatalf("round trip: %v, %v", got, err)
	}
	for i := range v {
		if got[i] != v[i] {
			t.Errorf("[%d] = %v, want %v", i, got[i], v[i])
		}
	}
	if _, err := DecodeVector([]byte{1, 2, 3}); err == nil {
		t.Error("odd blob accepted")
	}
}

func TestEmbeddingsStorage(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	a, _ := s.Create(ctx, NewMemory{Namespace: "demo", Kind: KindFact, Title: "T", Content: "alpha", Tags: []string{"x"}})
	b, _ := s.Create(ctx, NewMemory{Namespace: "demo", Kind: KindDecision, Content: "beta"})
	c, _ := s.Create(ctx, NewMemory{Kind: KindNote, Content: "gamma"})

	missing, err := s.MissingEmbeddings(ctx, "m", 0)
	if err != nil || len(missing) != 3 || missing[0].ID != c.ID {
		t.Fatalf("MissingEmbeddings = %+v, %v", missing, err)
	}
	if lim, _ := s.MissingEmbeddings(ctx, "m", 1); len(lim) != 1 {
		t.Errorf("limit: %+v", lim)
	}
	if missing[2].Title != "T" || missing[2].Tags[0] != "x" {
		t.Errorf("source fields: %+v", missing[2])
	}

	// Ids that no longer exist are ignored; empty vectors and models refused.
	if err := s.SetEmbeddings(ctx, "m", []Vector{{ID: a.ID, Values: []float32{1, 0}}, {ID: b.ID, Values: []float32{0, 1}}, {ID: 999, Values: []float32{1, 1}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEmbeddings(ctx, "m", []Vector{{ID: c.ID}}); err == nil {
		t.Error("empty vector accepted")
	}
	if err := s.SetEmbeddings(ctx, " ", []Vector{{ID: c.ID, Values: []float32{1}}}); err == nil {
		t.Error("empty model accepted")
	}
	if n, _ := s.CountMissingEmbeddings(ctx, "m"); n != 1 {
		t.Errorf("missing = %d, want 1", n)
	}
	if n, _ := s.CountMissingEmbeddings(ctx, "other"); n != 3 {
		t.Errorf("other model missing = %d, want 3", n)
	}

	// Filters.
	all, _ := s.Vectors(ctx, "m", VectorFilter{})
	if len(all) != 2 || all[0].ID != a.ID || all[0].Values[0] != 1 {
		t.Errorf("Vectors = %+v", all)
	}
	if v, _ := s.Vectors(ctx, "m", VectorFilter{Kinds: []Kind{KindDecision}}); len(v) != 1 || v[0].ID != b.ID {
		t.Errorf("kind filter: %+v", v)
	}
	if v, _ := s.Vectors(ctx, "m", VectorFilter{Namespace: "global"}); len(v) != 0 {
		t.Errorf("namespace filter: %+v", v)
	}
	if _, err := s.Vectors(ctx, "m", VectorFilter{Namespace: "-bad"}); err == nil {
		t.Error("invalid namespace accepted")
	}
	_, _ = s.Archive(ctx, b.ID, "t", "")
	if v, _ := s.Vectors(ctx, "m", VectorFilter{}); len(v) != 1 {
		t.Errorf("archived vector returned: %+v", v)
	}
	if v, _ := s.Vectors(ctx, "m", VectorFilter{IncludeArchived: true}); len(v) != 2 {
		t.Errorf("include archived: %+v", v)
	}
	// Archiving does not invalidate (text unchanged); a tag change does.
	if n, _ := s.CountMissingEmbeddings(ctx, "m"); n != 1 {
		t.Errorf("archive invalidated the embedding: missing=%d", n)
	}
	tags := []string{"y"}
	_, _ = s.Update(ctx, a.ID, Patch{Tags: &tags}, "t")
	if n, _ := s.CountMissingEmbeddings(ctx, "m"); n != 2 {
		t.Errorf("tag change should invalidate: missing=%d", n)
	}

	// Purge cascades.
	if _, err := s.Purge(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Vectors(ctx, "m", VectorFilter{IncludeArchived: true}); len(v) != 0 {
		t.Errorf("purged memory kept its embedding: %+v", v)
	}

	// Delete by model / all.
	_ = s.SetEmbeddings(ctx, "m", []Vector{{ID: a.ID, Values: []float32{1}}})
	_ = s.SetEmbeddings(ctx, "n", []Vector{{ID: a.ID, Values: []float32{1}}, {ID: c.ID, Values: []float32{1}}})
	st, _ := s.Stats(ctx)
	if len(st.Embeddings) != 2 || st.Embeddings[1] != (ModelCount{"n", 2}) {
		t.Errorf("stats embeddings: %+v", st.Embeddings)
	}
	if n, _ := s.DeleteEmbeddings(ctx, "m"); n != 1 {
		t.Errorf("delete m: %d", n)
	}
	if n, _ := s.DeleteEmbeddings(ctx, ""); n != 2 {
		t.Errorf("delete all: %d", n)
	}
}

func TestMissingEmbeddingsPrioritizesActive(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	active, _ := s.Create(ctx, NewMemory{Kind: KindFact, Content: "active old"})
	arch, _ := s.Create(ctx, NewMemory{Kind: KindFact, Content: "archived newer"})
	if _, err := s.Archive(ctx, arch.ID, "t", ""); err != nil {
		t.Fatal(err)
	}
	missing, err := s.MissingEmbeddings(ctx, "m", 1)
	if err != nil || len(missing) != 1 || missing[0].ID != active.ID {
		t.Fatalf("MissingEmbeddings(limit=1) = %+v, %v; want active #%d first", missing, err, active.ID)
	}
	all, err := s.MissingEmbeddings(ctx, "m", 0)
	if err != nil || len(all) != 2 || all[0].ID != active.ID || all[1].ID != arch.ID {
		t.Fatalf("MissingEmbeddings(all) = %+v, %v", all, err)
	}
}

func TestMissingEmbeddingsFiltered(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	past := time.Now().UTC().Add(-time.Hour)

	demoActive, _ := s.Create(ctx, NewMemory{Namespace: "demo", Kind: KindFact, Content: "demo active"})
	demoArch, _ := s.Create(ctx, NewMemory{Namespace: "demo", Kind: KindFact, Content: "demo archived"})
	_, _ = s.Archive(ctx, demoArch.ID, "t", "")
	demoExp, _ := s.Create(ctx, NewMemory{Namespace: "demo", Kind: KindFact, Content: "demo expired", ExpiresAt: &past})
	apiActive, _ := s.Create(ctx, NewMemory{Namespace: "api", Kind: KindFact, Content: "api active"})

	// Scoped to namespace, active only: skips archived, expired, and other namespaces.
	got, err := s.MissingEmbeddingsFiltered(ctx, "m", MissingFilter{Namespace: "demo"})
	if err != nil || len(got) != 1 || got[0].ID != demoActive.ID {
		t.Fatalf("MissingEmbeddingsFiltered(demo, active) = %+v, %v; want [%d]", got, err, demoActive.ID)
	}

	// IncludeArchived includes archived and expired within the namespace, still skips other namespaces,
	// prioritizing active (non-archived, non-expired) memories first.
	gotAllDemo, err := s.MissingEmbeddingsFiltered(ctx, "m", MissingFilter{Namespace: "demo", IncludeArchived: true})
	if err != nil || len(gotAllDemo) != 3 || gotAllDemo[0].ID != demoActive.ID || gotAllDemo[1].ID != demoExp.ID || gotAllDemo[2].ID != demoArch.ID {
		t.Fatalf("MissingEmbeddingsFiltered(demo, all) = %+v, %v; want [%d, %d, %d]", gotAllDemo, err, demoActive.ID, demoExp.ID, demoArch.ID)
	}

	// All namespaces, active only.
	gotActive, err := s.MissingEmbeddingsFiltered(ctx, "m", MissingFilter{Limit: 10})
	if err != nil || len(gotActive) != 2 || gotActive[0].ID != apiActive.ID || gotActive[1].ID != demoActive.ID {
		t.Fatalf("MissingEmbeddingsFiltered(all ns, active) = %+v, %v", gotActive, err)
	}

	// Invalid namespace returns validation error.
	if _, err := s.MissingEmbeddingsFiltered(ctx, "m", MissingFilter{Namespace: "-bad"}); err == nil {
		t.Error("expected validation error for invalid namespace")
	}
}

func TestSetEmbeddingsSkipsStaleSource(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	m, _ := s.Create(ctx, NewMemory{Kind: KindFact, Title: "Old", Content: "old body", Tags: []string{"a"}})

	missing, err := s.MissingEmbeddings(ctx, "m", 0)
	if err != nil || len(missing) != 1 {
		t.Fatalf("MissingEmbeddings = %+v, %v", missing, err)
	}
	staleSrc := missing[0]

	// Importance-only update leaves title/content/tags intact -> embedding is stored.
	imp := 3
	if _, err := s.Update(ctx, m.ID, Patch{Importance: &imp}, "t"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEmbeddings(ctx, "m", []Vector{{ID: m.ID, Values: []float32{1, 0}, Source: &staleSrc}}); err != nil {
		t.Fatal(err)
	}
	if vecs, _ := s.Vectors(ctx, "m", VectorFilter{}); len(vecs) != 1 || vecs[0].Values[0] != 1 {
		t.Fatalf("expected vector stored after importance-only update, got %+v", vecs)
	}

	// Simulate race: another writer updates content (trigger deletes old vector) and stores a fresh vector;
	// a late SetEmbeddings carrying the old Source must not overwrite the fresh vector.
	newBody := "new body"
	if _, err := s.Update(ctx, m.ID, Patch{Content: &newBody}, "t"); err != nil {
		t.Fatal(err)
	}
	freshMissing, _ := s.MissingEmbeddings(ctx, "m", 0)
	if len(freshMissing) != 1 {
		t.Fatalf("expected missing after update, got %+v", freshMissing)
	}
	if err := s.SetEmbeddings(ctx, "m", []Vector{{ID: m.ID, Values: []float32{0, 1}, Source: &freshMissing[0]}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEmbeddings(ctx, "m", []Vector{{ID: m.ID, Values: []float32{1, 0}, Source: &staleSrc}}); err != nil {
		t.Fatal(err)
	}
	vecs, _ := s.Vectors(ctx, "m", VectorFilter{})
	if len(vecs) != 1 || vecs[0].Values[0] != 0 || vecs[0].Values[1] != 1 {
		t.Fatalf("stale write overwrote fresh vector: %+v", vecs)
	}

	// Same check when no fresh vector exists yet (trigger found nothing to delete).
	newTitle := "New Title"
	if _, err := s.Update(ctx, m.ID, Patch{Title: &newTitle}, "t"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEmbeddings(ctx, "m", []Vector{{ID: m.ID, Values: []float32{1, 0}, Source: &freshMissing[0]}}); err != nil {
		t.Fatal(err)
	}
	if vecs, _ := s.Vectors(ctx, "m", VectorFilter{}); len(vecs) != 0 {
		t.Fatalf("expected stale vector skipped when title changed, got %+v", vecs)
	}
}

func TestGetManyAndHelpers(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	a, _ := s.Create(ctx, NewMemory{Kind: KindFact, Content: "a"})
	b, _ := s.Create(ctx, NewMemory{Kind: KindFact, Content: "b"})
	got, err := s.GetMany(ctx, []int64{a.ID, b.ID, 999})
	if err != nil || len(got) != 2 || got[b.ID].Content != "b" {
		t.Errorf("GetMany = %+v, %v", got, err)
	}
	if got, _ := s.GetMany(ctx, nil); len(got) != 0 {
		t.Errorf("GetMany(nil) = %+v", got)
	}
	if n, _ := s.Count(ctx); n != 2 {
		t.Errorf("Count = %d", n)
	}
	if HasSearchTerms(` "* () `) || !HasSearchTerms("é") {
		t.Error("HasSearchTerms")
	}
	if FirstWords("a b") != "a b" {
		t.Error("FirstWords")
	}
}
