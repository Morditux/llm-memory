package core

import (
	"context"
	"fmt"
	"strings"

	"github.com/Morditux/llm-memory/internal/store"
)

// NamespaceArchiveReason is the reason recorded when a whole namespace is
// archived from the GUI.
const NamespaceArchiveReason = "archived from desktop GUI"

// isGlobal reports whether name designates the protected global namespace.
func isGlobal(name string) bool {
	norm, err := store.NormalizeNamespace(name)
	return err == nil && norm == store.DefaultNamespace
}

// selectNamespace makes name the selected namespace filter and resets the
// pagination, so that the next list load shows its first page.
func (c *Controller) selectNamespace(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.filters.Namespace = name
	c.filters.Offset = 0
}

// SetNamespaceDescription sets the description of namespace name, announces
// it and reloads the namespaces only, like the web explorer.
func (c *Controller) SetNamespaceDescription(ctx context.Context, name, desc string) error {
	if _, _, err := c.backend.SetNamespaceDescription(ctx, name, desc); err != nil {
		return c.bannerErr(err)
	}
	c.banner(fmt.Sprintf("Updated description for \"%s\"", name), false)
	return c.LoadNamespaces(ctx)
}

// RenameNamespace renames namespace name. The selected namespace becomes the
// normalized new name, then everything is reloaded. The global namespace is
// refused.
func (c *Controller) RenameNamespace(ctx context.Context, name, newName string) error {
	if isGlobal(name) {
		return c.bannerErr(fmt.Errorf("cannot rename the %s namespace", store.DefaultNamespace))
	}
	got, err := c.backend.RenameNamespace(ctx, name, newName)
	if err != nil {
		return c.bannerErr(err)
	}
	c.selectNamespace(got)
	c.banner(fmt.Sprintf("Renamed namespace to \"%s\"", got), false)
	c.refresh(ctx, c.SelectedID())
	return nil
}

// NormalizeNamespaceInput returns the canonical namespace name when s is valid,
// or trimmed s so invalid inputs still reach the backend for validation.
func NormalizeNamespaceInput(s string) string {
	if norm, err := store.NormalizeNamespace(s); err == nil {
		return norm
	}
	return strings.TrimSpace(s)
}

// MergeNamespace moves every memory of name into target (normalized when valid).
// The selected namespace becomes the target, then everything is reloaded. The
// global namespace is refused as a source.
func (c *Controller) MergeNamespace(ctx context.Context, name, target string) error {
	if isGlobal(name) {
		return c.bannerErr(fmt.Errorf("cannot merge the %s namespace", store.DefaultNamespace))
	}
	target = NormalizeNamespaceInput(target)
	moved, err := c.backend.MergeNamespace(ctx, name, target)
	if err != nil {
		return c.bannerErr(err)
	}
	c.selectNamespace(target)
	c.banner(fmt.Sprintf("Merged \"%s\" into \"%s\" (%d moved)", name, target, moved), false)
	c.refresh(ctx, c.SelectedID())
	return nil
}

// ArchiveNamespace archives every active memory of name with
// NamespaceArchiveReason and reloads everything. The global namespace is
// refused.
func (c *Controller) ArchiveNamespace(ctx context.Context, name string) error {
	if isGlobal(name) {
		return c.bannerErr(fmt.Errorf("cannot archive the %s namespace", store.DefaultNamespace))
	}
	n, err := c.backend.ArchiveNamespace(ctx, name, NamespaceArchiveReason)
	if err != nil {
		return c.bannerErr(err)
	}
	c.banner(fmt.Sprintf("Archived %d memory(ies) in \"%s\"", n, name), false)
	c.refresh(ctx, c.SelectedID())
	return nil
}
