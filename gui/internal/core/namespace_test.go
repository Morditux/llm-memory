package core

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestSetNamespaceDescription(t *testing.T) {
	fb := mutFake()
	c, r := setup(fb)
	if err := c.SetNamespaceDescription(context.Background(), "demo", "About demo"); err != nil {
		t.Fatal(err)
	}
	want := MutationCall{Method: "SetNamespaceDescription", Name: "demo", Description: "About demo"}
	if calls := fb.MutationCalls(); len(calls) != 1 || !reflect.DeepEqual(calls[0], want) {
		t.Fatalf("calls = %+v", calls)
	}
	if b := []banner{{`Updated description for "demo"`, false}}; !slices.Equal(r.banners, b) {
		t.Fatalf("banners = %+v", r.banners)
	}
	// Like the web explorer, only the namespaces are reloaded.
	if fb.Calls("Namespaces") != 1 || fb.Calls("Stats") != 0 || fb.Calls("List") != 0 || fb.Calls("Detail") != 0 {
		t.Fatalf("reloads: ns=%d stats=%d list=%d detail=%d",
			fb.Calls("Namespaces"), fb.Calls("Stats"), fb.Calls("List"), fb.Calls("Detail"))
	}
	// The global namespace keeps its description editable.
	if err := c.SetNamespaceDescription(context.Background(), "global", "g"); err != nil {
		t.Fatal(err)
	}
}

func TestRenameNamespaceFollowsNewName(t *testing.T) {
	fb := mutFake()
	fb.RenameValue = "new-name"
	c, r := setup(fb)
	ctx := context.Background()
	c.SelectNamespace("demo")
	c.SetOffset(100)
	c.Select(1)

	if err := c.RenameNamespace(ctx, "demo", " New Name "); err != nil {
		t.Fatal(err)
	}
	want := MutationCall{Method: "RenameNamespace", Name: "demo", NewName: " New Name "}
	if calls := fb.MutationCalls(); len(calls) != 1 || !reflect.DeepEqual(calls[0], want) {
		t.Fatalf("calls = %+v", calls)
	}
	if b := []banner{{`Renamed namespace to "new-name"`, false}}; !slices.Equal(r.banners, b) {
		t.Fatalf("banners = %+v", r.banners)
	}
	if f := c.Filters(); f.Namespace != "new-name" || f.Offset != 0 {
		t.Fatalf("filters = %+v", f)
	}
	assertRefreshed(t, fb, r, 1)
	if got := fb.ListCalls(); len(got) != 1 || got[0].Namespace != "new-name" || got[0].Offset != 0 {
		t.Fatalf("list calls = %+v", got)
	}
	if c.SelectedID() != 1 {
		t.Fatalf("selected = %d", c.SelectedID())
	}
}

func TestMergeNamespaceFollowsTarget(t *testing.T) {
	fb := mutFake()
	fb.MergeValue = 4
	c, r := setup(fb)
	ctx := context.Background()
	c.SelectNamespace("demo")
	c.SetOffset(50)
	c.Select(2)

	if err := c.MergeNamespace(ctx, "demo", "  other "); err != nil {
		t.Fatal(err)
	}
	want := MutationCall{Method: "MergeNamespace", Name: "demo", NewName: "other"}
	if calls := fb.MutationCalls(); len(calls) != 1 || !reflect.DeepEqual(calls[0], want) {
		t.Fatalf("calls = %+v", calls)
	}
	if b := []banner{{`Merged "demo" into "other" (4 moved)`, false}}; !slices.Equal(r.banners, b) {
		t.Fatalf("banners = %+v", r.banners)
	}
	if f := c.Filters(); f.Namespace != "other" || f.Offset != 0 {
		t.Fatalf("filters = %+v", f)
	}
	assertRefreshed(t, fb, r, 1)
	if got := fb.ListCalls(); len(got) != 1 || got[0].Namespace != "other" {
		t.Fatalf("list calls = %+v", got)
	}
}

func TestMergeNamespaceNormalizesTarget(t *testing.T) {
	fb := mutFake()
	fb.MergeValue = 2
	c, r := setup(fb)
	ctx := context.Background()
	c.SelectNamespace("alpha")

	if err := c.MergeNamespace(ctx, "alpha", "  My  Target "); err != nil {
		t.Fatal(err)
	}
	want := MutationCall{Method: "MergeNamespace", Name: "alpha", NewName: "my-target"}
	if calls := fb.MutationCalls(); len(calls) != 1 || !reflect.DeepEqual(calls[0], want) {
		t.Fatalf("calls = %+v", calls)
	}
	if b := []banner{{`Merged "alpha" into "my-target" (2 moved)`, false}}; !slices.Equal(r.banners, b) {
		t.Fatalf("banners = %+v", r.banners)
	}
	if got := c.Filters().Namespace; got != "my-target" {
		t.Fatalf("namespace filter = %q, want %q", got, "my-target")
	}
}

func TestArchiveNamespace(t *testing.T) {
	fb := mutFake()
	fb.ArchiveNamespaceValue = 6
	c, r := setup(fb)
	c.SelectNamespace("demo")
	c.Select(1)
	if err := c.ArchiveNamespace(context.Background(), "demo"); err != nil {
		t.Fatal(err)
	}
	want := MutationCall{Method: "ArchiveNamespace", Name: "demo", Reason: "archived from desktop GUI"}
	if calls := fb.MutationCalls(); len(calls) != 1 || !reflect.DeepEqual(calls[0], want) {
		t.Fatalf("calls = %+v", calls)
	}
	if b := []banner{{`Archived 6 memory(ies) in "demo"`, false}}; !slices.Equal(r.banners, b) {
		t.Fatalf("banners = %+v", r.banners)
	}
	assertRefreshed(t, fb, r, 1)
	if c.Filters().Namespace != "demo" {
		t.Fatalf("namespace = %q", c.Filters().Namespace)
	}
}

func TestGlobalNamespaceProtected(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"global", " Global "} {
		fb := mutFake()
		c, r := setup(fb)
		c.SelectNamespace("global")
		errs := []error{
			c.RenameNamespace(ctx, name, "x"),
			c.MergeNamespace(ctx, name, "x"),
			c.ArchiveNamespace(ctx, name),
		}
		for i, err := range errs {
			if err == nil || !strings.Contains(err.Error(), "global") {
				t.Errorf("%q action %d: err = %v", name, i, err)
			}
		}
		if len(fb.MutationCalls()) != 0 || fb.Calls("Stats") != 0 {
			t.Errorf("%q: backend reached: %+v", name, fb.MutationCalls())
		}
		if len(r.banners) != 3 || !r.banners[0].IsErr || r.banners[0].Msg != errs[0].Error() {
			t.Errorf("%q: banners = %+v", name, r.banners)
		}
		if c.Filters().Namespace != "global" {
			t.Errorf("selection changed to %q", c.Filters().Namespace)
		}
	}
}

func TestNamespaceErrorsReachBanner(t *testing.T) {
	ctx := context.Background()
	for _, method := range []string{"SetNamespaceDescription", "RenameNamespace", "MergeNamespace", "ArchiveNamespace"} {
		fb := mutFake()
		boom := errors.New(method + " failed")
		fb.Errs = map[string]error{method: boom}
		c, r := setup(fb)
		c.SelectNamespace("demo")
		var err error
		switch method {
		case "SetNamespaceDescription":
			err = c.SetNamespaceDescription(ctx, "demo", "d")
		case "RenameNamespace":
			err = c.RenameNamespace(ctx, "demo", "x")
		case "MergeNamespace":
			err = c.MergeNamespace(ctx, "demo", "x")
		case "ArchiveNamespace":
			err = c.ArchiveNamespace(ctx, "demo")
		}
		if !errors.Is(err, boom) {
			t.Errorf("%s: err = %v", method, err)
		}
		if b := []banner{{boom.Error(), true}}; !slices.Equal(r.banners, b) {
			t.Errorf("%s: banners = %+v", method, r.banners)
		}
		if fb.Calls("Stats") != 0 || fb.Calls("Namespaces") != 0 || c.Filters().Namespace != "demo" {
			t.Errorf("%s: reloaded or reselected after an error", method)
		}
	}
}
