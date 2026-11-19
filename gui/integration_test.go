package gui_test

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Morditux/llm-memory/gui/internal/core"
	"github.com/Morditux/llm-memory/internal/explorer"
	"github.com/Morditux/llm-memory/internal/store"
)

// TestMutationsAgainstRealStore drives the controller over a real SQLite store
// and checks the history written for each action.
func TestMutationsAgainstRealStore(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "gui.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	var banners []string
	var details []core.DetailView
	c := core.NewController(explorer.New(st, nil, "gui"), core.Observer{
		Banner: func(msg string, isErr bool) {
			if isErr {
				msg = "ERR " + msg
			}
			banners = append(banners, msg)
		},
		Detail: func(v core.DetailView) { details = append(details, v) },
		Error:  func(err error) { t.Errorf("load error: %v", err) },
	})

	a, err := c.Create(ctx, core.CreateForm{Namespace: " proj ", Kind: "decision", Importance: 3, Title: "Use SQLite", Tags: "db, , go", Content: "because"}.Request())
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.Create(ctx, core.CreateForm{Kind: "note", Importance: 2, Content: "other"}.Request())
	if err != nil {
		t.Fatal(err)
	}
	if c.SelectedID() != b.ID || details[len(details)-1].Memory.Namespace != "global" {
		t.Fatalf("selected = %d, last detail = %+v", c.SelectedID(), details[len(details)-1].Memory)
	}
	if a.Namespace != "proj" || !slices.Equal(a.Tags, []string{"db", "go"}) {
		t.Fatalf("created = %+v", a)
	}

	// Invalid expirations are rejected and reported in the banner.
	err = c.Update(ctx, a.ID, core.EditForm{Namespace: "proj", Kind: "decision", Importance: 3, Title: "x", ExpiresIn: "soon"}.Request())
	if err == nil || banners[len(banners)-1] != "ERR "+err.Error() || !strings.Contains(err.Error(), "soon") {
		t.Fatalf("err = %v, banners = %q", err, banners)
	}
	if _, err := c.Create(ctx, core.CreateForm{Kind: "note", Importance: 2, ExpiresIn: "-3h", Content: "x"}.Request()); err == nil {
		t.Fatal("negative duration accepted")
	}

	if err := c.Update(ctx, a.ID, core.EditForm{
		Namespace: "proj", Kind: "decision", Importance: 3, Title: "Use SQLite WAL", Tags: "db", ExpiresIn: "7d", Content: "because",
	}.Request()); err != nil {
		t.Fatal(err)
	}
	if err := c.Archive(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if v := details[len(details)-1]; v.Memory.ID != a.ID || v.Memory.ArchivedAt == nil {
		t.Fatalf("detail after archive = %+v", v.Memory)
	}
	if err := c.Restore(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.AddLink(ctx, a.ID, b.ID, store.LinkDependsOn); err != nil {
		t.Fatal(err)
	}
	if v := details[len(details)-1]; len(v.Links) != 1 || v.Links[0].ID != b.ID || v.Links[0].Kind != store.LinkDependsOn {
		t.Fatalf("links = %+v", v.Links)
	}

	hist, err := st.History(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	var actions []store.Action
	for _, h := range hist {
		if h.ChangedBy != "gui" {
			t.Errorf("history %d changed_by = %q", h.ID, h.ChangedBy)
		}
		actions = append(actions, h.Action)
	}
	want := []store.Action{store.ActionCreate, store.ActionUpdate, store.ActionArchive, store.ActionRestore, store.ActionUpdate}
	if !slices.Equal(actions, want) {
		t.Fatalf("actions = %v, want %v", actions, want)
	}
	if m, _ := st.Get(ctx, a.ID); m.UpdatedBy != "gui" || m.ExpiresAt == nil || m.Title != "Use SQLite WAL" {
		t.Fatalf("memory = %+v", m)
	}
	if err := c.RemoveLink(ctx, a.ID, b.ID, store.LinkDependsOn); err != nil {
		t.Fatal(err)
	}
	if v := details[len(details)-1]; len(v.Links) != 0 {
		t.Fatalf("links after removal = %+v", v.Links)
	}
}

// TestNamespaceActionsAgainstRealStore renames, merges and archives namespaces
// over a real store and checks counts, selection and history attribution.
func TestNamespaceActionsAgainstRealStore(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "gui.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	var banners []string
	var nss []store.Namespace
	c := core.NewController(explorer.New(st, nil, "gui"), core.Observer{
		Banner: func(msg string, isErr bool) {
			if isErr {
				msg = "ERR " + msg
			}
			banners = append(banners, msg)
		},
		Namespaces: func(ns []store.Namespace) { nss = ns },
		Error:      func(err error) { t.Errorf("load error: %v", err) },
	})
	mk := func(ns, content string) store.Memory {
		t.Helper()
		m, err := c.Create(ctx, core.CreateForm{Namespace: ns, Kind: "note", Importance: 2, Content: content}.Request())
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	a1, a2 := mk("alpha", "one"), mk("alpha", "two")
	b1 := mk("beta", "three")
	count := func(name string) int {
		for _, n := range nss {
			if n.Name == name {
				return n.ActiveCount
			}
		}
		return -1
	}
	lastBanner := func() string { return banners[len(banners)-1] }

	if err := c.SetNamespaceDescription(ctx, "alpha", " The alphas "); err != nil {
		t.Fatal(err)
	}
	if lastBanner() != `Updated description for "alpha"` {
		t.Fatalf("banner = %q", lastBanner())
	}
	for _, n := range nss {
		if n.Name == "alpha" && n.Description != "The alphas" {
			t.Fatalf("description = %q", n.Description)
		}
	}

	c.SelectNamespace("alpha")
	c.SetOffset(50)
	if err := c.RenameNamespace(ctx, "alpha", " Gamma "); err != nil {
		t.Fatal(err)
	}
	if f := c.Filters(); f.Namespace != "gamma" || f.Offset != 0 {
		t.Fatalf("filters = %+v", f)
	}
	if lastBanner() != `Renamed namespace to "gamma"` || count("gamma") != 2 || count("alpha") != -1 {
		t.Fatalf("banner = %q, gamma = %d, alpha = %d", lastBanner(), count("gamma"), count("alpha"))
	}
	if err := c.RenameNamespace(ctx, "gamma", "beta"); err == nil || !strings.HasPrefix(lastBanner(), "ERR ") {
		t.Fatalf("renaming onto an existing namespace: err = %v, banner = %q", err, lastBanner())
	}
	if err := c.RenameNamespace(ctx, "global", "x"); err == nil {
		t.Fatal("global renamed")
	}

	if err := c.MergeNamespace(ctx, "gamma", "  Beta "); err != nil {
		t.Fatal(err)
	}
	if c.Filters().Namespace != "beta" || lastBanner() != `Merged "gamma" into "beta" (2 moved)` ||
		count("beta") != 3 || count("gamma") != -1 {
		t.Fatalf("namespace = %q, banner = %q, beta = %d", c.Filters().Namespace, lastBanner(), count("beta"))
	}

	if err := c.ArchiveNamespace(ctx, "beta"); err != nil {
		t.Fatal(err)
	}
	if lastBanner() != `Archived 3 memory(ies) in "beta"` || count("beta") != 0 {
		t.Fatalf("banner = %q, beta = %d", lastBanner(), count("beta"))
	}

	for _, id := range []int64{a1.ID, a2.ID, b1.ID} {
		hist, err := st.History(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		var actions []store.Action
		for _, h := range hist {
			if h.ChangedBy != "gui" {
				t.Errorf("memory %d: history %d changed_by = %q", id, h.ID, h.ChangedBy)
			}
			actions = append(actions, h.Action)
		}
		want := []store.Action{store.ActionCreate, store.ActionUpdate, store.ActionUpdate, store.ActionArchive}
		if id == b1.ID {
			want = []store.Action{store.ActionCreate, store.ActionArchive} // b1 stays in beta
		}
		if !slices.Equal(actions, want) {
			t.Errorf("memory %d: actions = %v, want %v", id, actions, want)
		}
		m, err := st.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if m.Namespace != "beta" || m.ArchivedAt == nil || m.UpdatedBy != "gui" {
			t.Errorf("memory %d = %+v", id, m)
		}
	}
}
