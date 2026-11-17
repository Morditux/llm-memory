package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestMemoryLinksAndSupersede(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	m1, err := s.Create(ctx, NewMemory{Namespace: "demo", Kind: KindDecision, Title: "MySQL", Content: "Use MySQL"})
	if err != nil {
		t.Fatal(err)
	}
	m2, err := s.Create(ctx, NewMemory{
		Namespace:  "demo",
		Kind:       KindDecision,
		Title:      "Postgres",
		Content:    "Use PostgreSQL",
		Supersedes: []int64{m1.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	m3, err := s.Create(ctx, NewMemory{
		Namespace: "demo",
		Kind:      KindFact,
		Title:     "JSONB index",
		Content:   "GIN indexes used on jsonb columns",
		Links:     []LinkRef{{TargetID: m2.ID, Kind: LinkDependsOn}},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Check links on m2: outgoing supersedes->m1, incoming depends_on<-m3.
	links2, err := s.Links(ctx, m2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(links2) != 2 {
		t.Fatalf("m2 links = %+v, want 2", links2)
	}
	var gotOut, gotIn bool
	for _, l := range links2 {
		if l.Direction == "outgoing" && l.Kind == LinkSupersedes && l.ID == m1.ID && l.Archived {
			gotOut = true
		}
		if l.Direction == "incoming" && l.Kind == LinkDependsOn && l.ID == m3.ID && !l.Archived {
			gotIn = true
		}
	}
	if !gotOut || !gotIn {
		t.Fatalf("unexpected links on m2: %+v", links2)
	}

	// AddLink / duplicate AddLink (idempotent, no extra history) / RemoveLink.
	if err := s.AddLink(ctx, m3.ID, m1.ID, LinkRelatesTo, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddLink(ctx, m3.ID, m1.ID, LinkRelatesTo, "alice"); err != nil {
		t.Fatal(err)
	}
	hist3, err := s.History(ctx, m3.ID)
	if err != nil || len(hist3) != 2 || hist3[1].Action != ActionUpdate || hist3[1].ChangedBy != "alice" {
		t.Fatalf("history after AddLink (and duplicate) = %+v, %v", hist3, err)
	}
	if _, reason, err := hist3[1].Decode(); err != nil || reason != fmt.Sprintf("added link relates_to -> #%d", m1.ID) {
		t.Fatalf("AddLink history reason = %q, %v", reason, err)
	}
	if err := s.RemoveLink(ctx, m3.ID, m1.ID, LinkRelatesTo, "bob"); err != nil {
		t.Fatal(err)
	}
	hist3, err = s.History(ctx, m3.ID)
	if err != nil || len(hist3) != 3 || hist3[2].Action != ActionUpdate || hist3[2].ChangedBy != "bob" {
		t.Fatalf("history after RemoveLink = %+v, %v", hist3, err)
	}
	if _, reason, err := hist3[2].Decode(); err != nil || reason != fmt.Sprintf("removed link relates_to -> #%d", m1.ID) {
		t.Fatalf("RemoveLink history reason = %q, %v", reason, err)
	}
	if err := s.RemoveLink(ctx, m3.ID, m1.ID, LinkRelatesTo, "bob"); err == nil {
		t.Fatal("expected error removing absent link")
	}

	// Removing a supersedes link is refused.
	var ve *ValidationError
	if err := s.RemoveLink(ctx, m2.ID, m1.ID, LinkSupersedes, "cli"); !errors.As(err, &ve) {
		t.Errorf("remove supersedes err = %v, want ValidationError", err)
	}

	// Validation errors.
	if err := s.AddLink(ctx, m2.ID, m2.ID, LinkRelatesTo, "cli"); !errors.As(err, &ve) {
		t.Errorf("self link err = %v, want ValidationError", err)
	}
	if err := s.AddLink(ctx, m2.ID, m3.ID, LinkKind("bogus"), "cli"); !errors.As(err, &ve) {
		t.Errorf("bad kind err = %v, want ValidationError", err)
	}
	if err := s.AddLink(ctx, m2.ID, 9999, LinkRelatesTo, "cli"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing target err = %v, want ErrNotFound", err)
	}

	// Supersede archives target and creates supersedes link.
	m4, err := s.Create(ctx, NewMemory{Namespace: "demo", Kind: KindFact, Title: "JSONB v2", Content: "Updated GIN index details"})
	if err != nil {
		t.Fatal(err)
	}
	archivedM3, err := s.Supersede(ctx, m4.ID, m3.ID, "cli")
	if err != nil {
		t.Fatal(err)
	}
	if !archivedM3.Archived() {
		t.Errorf("expected m3 archived after Supersede")
	}
	links4, err := s.Links(ctx, m4.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(links4) != 1 || links4[0].Direction != "outgoing" || links4[0].Kind != LinkSupersedes || links4[0].ID != m3.ID {
		t.Fatalf("links4 = %+v", links4)
	}

	// Handoff replacement also records a supersedes link.
	h1, _ := s.Create(ctx, NewMemory{Namespace: "demo", Kind: KindHandoff, Content: "handoff 1"})
	h2, _ := s.Create(ctx, NewMemory{Namespace: "demo", Kind: KindHandoff, Content: "handoff 2"})
	hLinks, err := s.Links(ctx, h2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(hLinks) != 1 || hLinks[0].ID != h1.ID || hLinks[0].Kind != LinkSupersedes {
		t.Fatalf("handoff links = %+v", hLinks)
	}

	// Purge cascades links.
	if _, err := s.Purge(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	links2After, err := s.Links(ctx, m2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(links2After) != 0 {
		t.Errorf("expected links to archived/purged memories to cascade delete, got %+v", links2After)
	}
}
