package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Morditux/llm-memory/internal/config"
	"github.com/Morditux/llm-memory/internal/server"
	"github.com/Morditux/llm-memory/internal/store"
)

const (
	cliDate      = "2006-01-02 15:04"
	summaryRunes = 60
)

// filterFlags are shared by list and search.
type filterFlags struct {
	kinds    string
	archived bool
	limit    int
}

func registerFilters(fs *flag.FlagSet, f *filterFlags, defLimit int, archivedHelp string) {
	fs.StringVar(&f.kinds, "kind", "", "comma-separated kinds: "+kindNames())
	fs.BoolVar(&f.archived, "archived", false, archivedHelp)
	fs.IntVar(&f.limit, "limit", defLimit, "maximum number of rows")
}

func kindNames() string {
	n := make([]string, len(store.Kinds))
	for i, k := range store.Kinds {
		n[i] = string(k)
	}
	return strings.Join(n, ", ")
}

func parseKinds(s string) []store.Kind {
	var out []store.Kind
	for _, k := range strings.Split(s, ",") {
		if k = strings.TrimSpace(k); k != "" {
			out = append(out, store.Kind(k))
		}
	}
	return out
}

// namespaceFilter returns the namespace to filter on: only when --namespace
// is given explicitly (the env default is meant for serve and context).
func namespaceFilter(fs *flag.FlagSet, cfg config.Config) string {
	if isSet(fs, "namespace") {
		return cfg.Namespace
	}
	return ""
}

func summary(m store.Memory) string {
	s := m.Title
	if s == "" {
		s = strings.Join(strings.Fields(m.Content), " ")
	}
	if r := []rune(s); len(r) > summaryRunes {
		s = string(r[:summaryRunes-1]) + "…"
	}
	return s
}

func kindLabel(m store.Memory) string {
	if m.Archived() {
		return string(m.Kind) + " (archived)"
	}
	return string(m.Kind)
}

func parseID(s string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimPrefix(s, "#"), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("invalid memory id %q", s)
	}
	return id, nil
}

// ---- list -------------------------------------------------------------------

func (a app) cmdList(ctx context.Context, args []string) error {
	fs := a.newFlagSet("list")
	flags := config.Register(fs)
	var f filterFlags
	registerFilters(fs, &f, 0, "include archived memories")
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	st, cfg, err := a.openStore(ctx, flags)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	filter := store.ListFilter{Namespace: namespaceFilter(fs, cfg), Kinds: parseKinds(f.kinds), Limit: f.limit}
	if f.archived {
		filter.Archived = store.IncludeArchived
	}
	ms, err := st.List(ctx, filter)
	if err != nil {
		return err
	}
	if len(ms) == 0 {
		fmt.Fprintln(a.stdout, "No memories.")
		return nil
	}
	tw := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAMESPACE\tKIND\tTITLE / CONTENT\tSOURCE\tUPDATED (UTC)")
	for _, m := range ms {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", m.ID, m.Namespace, kindLabel(m), summary(m), m.Source, m.UpdatedAt.Format(cliDate))
	}
	return tw.Flush()
}

// ---- show -------------------------------------------------------------------

func (a app) cmdShow(ctx context.Context, args []string) error {
	fs := a.newFlagSet("show")
	flags := config.Register(fs)
	pos, err := parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	id, err := parseID(pos[0])
	if err != nil {
		return err
	}
	st, _, err := a.openStore(ctx, flags)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	m, err := st.Get(ctx, id)
	if err != nil {
		return err
	}
	hs, err := st.History(ctx, id)
	if err != nil {
		return err
	}
	w := a.stdout
	fmt.Fprintf(w, "#%d  %s  [%s]\n", m.ID, m.Namespace, kindLabel(m))
	if m.Title != "" {
		fmt.Fprintf(w, "Title:      %s\n", m.Title)
	}
	if len(m.Tags) > 0 {
		fmt.Fprintf(w, "Tags:       %s\n", strings.Join(m.Tags, " "))
	}
	fmt.Fprintf(w, "Importance: %d\n", m.Importance)
	fmt.Fprintf(w, "Created:    %s by %s\n", m.CreatedAt.Format(cliDate), m.Source)
	fmt.Fprintf(w, "Updated:    %s by %s\n", m.UpdatedAt.Format(cliDate), m.UpdatedBy)
	if m.ArchivedAt != nil {
		fmt.Fprintf(w, "Archived:   %s (restore with: llm-memory restore %d)\n", m.ArchivedAt.Format(cliDate), m.ID)
	}
	fmt.Fprintf(w, "\n%s\n\nHistory:\n", m.Content)
	for _, h := range hs {
		snap, reason, err := h.Decode()
		if err != nil {
			return err
		}
		line := fmt.Sprintf("  %s  %-7s  by %s", h.ChangedAt.Format(cliDate), h.Action, h.ChangedBy)
		if reason != "" {
			line += " — " + reason
		}
		if h.Action == store.ActionCreate || h.Action == store.ActionUpdate {
			line += "\n      " + summary(snap)
		}
		fmt.Fprintln(w, line)
	}
	return nil
}

// ---- search -----------------------------------------------------------------

func (a app) cmdSearch(ctx context.Context, args []string) error {
	fs := a.newFlagSet("search")
	flags := config.Register(fs)
	var f filterFlags
	registerFilters(fs, &f, store.DefaultSearchLimit, "also search archived memories")
	pos, err := parse(fs, args, 0, -1)
	if err != nil {
		return err
	}
	st, cfg, err := a.openStore(ctx, flags)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	var engine server.Searcher = st
	logger := slog.New(slog.NewTextHandler(a.stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	if e, err := newEngine(st, cfg, logger); err != nil {
		return err
	} else if e != nil {
		engine = e
	}
	res, err := engine.Search(ctx, store.SearchQuery{
		Query: strings.Join(pos, " "), Namespace: namespaceFilter(fs, cfg), Kinds: parseKinds(f.kinds),
		IncludeArchived: f.archived, Limit: f.limit,
	})
	if err != nil {
		return err
	}
	if len(res) == 0 {
		fmt.Fprintln(a.stdout, "No results.")
		return nil
	}
	for _, r := range res {
		title := r.Title
		if title != "" {
			title = " " + title
		}
		fmt.Fprintf(a.stdout, "#%d  %s  [%s]%s  (%s, %s)\n    %s\n",
			r.ID, r.Namespace, kindLabel(r.Memory), title, r.Source, r.UpdatedAt.Format(cliDate), r.Snippet)
	}
	return nil
}

// ---- restore ----------------------------------------------------------------

func (a app) cmdRestore(ctx context.Context, args []string) error {
	fs := a.newFlagSet("restore")
	flags := config.Register(fs)
	pos, err := parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	id, err := parseID(pos[0])
	if err != nil {
		return err
	}
	st, _, err := a.openStore(ctx, flags)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	m, err := st.Restore(ctx, id, cliSource)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "Restored #%d (%s, %s): %s\n", m.ID, m.Namespace, m.Kind, summary(m))
	return nil
}

// ---- purge ------------------------------------------------------------------

func parseDate(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid date %q: use YYYY-MM-DD (UTC) or RFC 3339", s)
	}
	return t, nil
}

func (a app) cmdPurge(ctx context.Context, args []string) error {
	fs := a.newFlagSet("purge")
	flags := config.Register(fs)
	before := fs.String("archived-before", "", "delete memories archived before this date (YYYY-MM-DD, UTC, or RFC 3339)")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	if *before == "" {
		fmt.Fprintln(a.stderr, "purge requires --archived-before")
		return errUsage
	}
	t, err := parseDate(*before)
	if err != nil {
		return err
	}
	st, _, err := a.openStore(ctx, flags)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	n, err := st.CountArchivedBefore(ctx, t)
	if err != nil {
		return err
	}
	if n == 0 {
		fmt.Fprintf(a.stdout, "No memory archived before %s.\n", t.UTC().Format(cliDate))
		return nil
	}
	if !*yes {
		fmt.Fprintf(a.stdout, "Permanently delete %d archived memor%s and their history? [y/N] ", n, plural(n, "y", "ies"))
		answer, _ := bufio.NewReader(a.stdin).ReadString('\n')
		if ans := strings.ToLower(strings.TrimSpace(answer)); ans != "y" && ans != "yes" {
			fmt.Fprintln(a.stdout, "Aborted.")
			return nil
		}
	}
	deleted, err := st.Purge(ctx, t)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "Deleted %d memor%s.\n", deleted, plural(deleted, "y", "ies"))
	return nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// ---- export / import --------------------------------------------------------

func (a app) cmdExport(ctx context.Context, args []string) error {
	fs := a.newFlagSet("export")
	flags := config.Register(fs)
	withHistory := fs.Bool("history", false, "include the history of each memory")
	outPath := fs.String("o", "", "write to this file instead of stdout")
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	st, cfg, err := a.openStore(ctx, flags)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	d, err := st.Export(ctx, store.ExportOptions{Namespace: namespaceFilter(fs, cfg), WithHistory: *withHistory})
	if err != nil {
		return err
	}
	encode := func(w io.Writer) error {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(d); err != nil {
			return fmt.Errorf("write export: %w", err)
		}
		return nil
	}
	if *outPath == "" {
		return encode(a.stdout)
	}
	// The export may contain personal information: keep it private.
	f, err := os.OpenFile(*outPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", *outPath, err)
	}
	if err := encode(f); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write %s: %w", *outPath, err)
	}
	fmt.Fprintf(a.stderr, "Exported %d memories to %s\n", len(d.Memories), *outPath)
	return nil
}

func (a app) cmdImport(ctx context.Context, args []string) error {
	fs := a.newFlagSet("import")
	flags := config.Register(fs)
	dryRun := fs.Bool("dry-run", false, "check and count without writing")
	pos, err := parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	r := a.stdin
	if pos[0] != "-" {
		f, err := os.Open(pos[0])
		if err != nil {
			return fmt.Errorf("open %s: %w", pos[0], err)
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	var d store.Dump
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return fmt.Errorf("read %s: not a llm-memory export: %w", pos[0], err)
	}
	st, _, err := a.openStore(ctx, flags)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	rep, err := st.Import(ctx, d, *dryRun, cliSource)
	if err != nil {
		return err
	}
	prefix := ""
	if rep.DryRun {
		prefix = "Dry run (nothing written): "
	}
	fmt.Fprintf(a.stdout, "%s%d imported, %d skipped (exact duplicates), %d namespace(s) created.\n",
		prefix, rep.Imported, rep.Skipped, rep.NamespacesCreated)
	return nil
}

// ---- stats ------------------------------------------------------------------

func (a app) cmdStats(ctx context.Context, args []string) error {
	fs := a.newFlagSet("stats")
	flags := config.Register(fs)
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	st, _, err := a.openStore(ctx, flags)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	s, err := st.Stats(ctx)
	if err != nil {
		return err
	}
	w := a.stdout
	fmt.Fprintf(w, "Database: %s\nSize:     %s (schema %d)\nMemories: %d active, %d archived\n",
		s.Path, humanBytes(s.FileBytes), s.SchemaVersion, s.Active, s.Archived)
	for _, g := range []struct {
		title string
		rows  []store.Count
	}{{"namespace", s.ByNamespace}, {"kind", s.ByKind}, {"source", s.BySource}} {
		fmt.Fprintf(w, "\nBy %s:\n", g.title)
		if len(g.rows) == 0 {
			fmt.Fprintln(w, "  (none)")
			continue
		}
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		for _, c := range g.rows {
			fmt.Fprintf(tw, "  %s\t%d active\t%d archived\n", c.Key, c.Active, c.Archived)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	if len(s.Embeddings) > 0 { // semantic search only: v1 output unchanged
		fmt.Fprintln(w, "\nEmbeddings:")
		for _, c := range s.Embeddings {
			fmt.Fprintf(w, "  %s  %d\n", c.Model, c.Count)
		}
	}
	return nil
}

// ---- reindex ----------------------------------------------------------------

func (a app) cmdReindex(ctx context.Context, args []string) error {
	fs := a.newFlagSet("reindex")
	flags := config.Register(fs)
	all := fs.Bool("all", false, "recompute every embedding of the current model, not only the missing ones")
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	st, cfg, err := a.openStore(ctx, flags)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	if !cfg.Embed.Enabled() {
		return errors.New("semantic search is not configured: set --embed-url and --embed-model (or LLM_MEMORY_EMBED_URL and LLM_MEMORY_EMBED_MODEL)")
	}
	logger := slog.New(slog.NewTextHandler(a.stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	engine, err := newEngine(st, cfg, logger)
	if err != nil {
		return err
	}
	total, err := st.CountMissingEmbeddings(ctx, engine.Model())
	if err != nil {
		return err
	}
	if *all {
		if total, err = st.Count(ctx); err != nil {
			return err
		}
	}
	n, err := engine.Reindex(ctx, *all, func(done int) {
		fmt.Fprintf(a.stderr, "  %d/%d\n", done, total)
	})
	if err != nil {
		return fmt.Errorf("reindex stopped after %d memories: %w", n, err)
	}
	fmt.Fprintf(a.stdout, "Indexed %d memor%s with model %s.\n", n, plural(n, "y", "ies"), engine.Model())
	return nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
