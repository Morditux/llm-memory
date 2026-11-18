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
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Morditux/llm-memory/internal/bootstrap"
	"github.com/Morditux/llm-memory/internal/config"
	"github.com/Morditux/llm-memory/internal/semantic"
	"github.com/Morditux/llm-memory/internal/server"
	"github.com/Morditux/llm-memory/internal/store"
	"github.com/Morditux/llm-memory/internal/ui"
)

const (
	cliDate      = "2006-01-02 15:04"
	summaryRunes = 60
)

// filterFlags are shared by list and search.
type filterFlags struct {
	kinds    string
	tags     string
	archived bool
	asJSON   bool
	limit    int
}

func registerFilters(fs *flag.FlagSet, f *filterFlags, defLimit int, archivedHelp string) {
	fs.StringVar(&f.kinds, "kind", "", "comma-separated kinds: "+kindNames())
	fs.StringVar(&f.tags, "tag", "", "comma-separated tags (all must match)")
	fs.BoolVar(&f.archived, "archived", false, archivedHelp)
	fs.BoolVar(&f.asJSON, "json", false, "output as JSON")
	fs.IntVar(&f.limit, "limit", defLimit, "maximum number of rows")
}

func kindNames() string {
	n := make([]string, len(store.Kinds))
	for i, k := range store.Kinds {
		n[i] = string(k)
	}
	return strings.Join(n, ", ")
}

func writableKindNames() string {
	var n []string
	for _, k := range store.Kinds {
		if k != store.KindHandoff {
			n = append(n, string(k))
		}
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

func parseTags(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return store.NormalizeTags(strings.Split(s, ","))
}

func parseIDList(s string) ([]int64, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var ids []int64
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := parseID(part)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func parseItems(s string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' }) {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func readPositionalOrStdin(pos []string, r io.Reader) (string, error) {
	if len(pos) == 1 && pos[0] == "-" {
		b, err := io.ReadAll(r)
		if err != nil {
			return "", fmt.Errorf("read stdin: %w", err)
		}
		return string(b), nil
	}
	return strings.Join(pos, " "), nil
}

func cliActor(cfg config.Config) string {
	if cfg.Source != "" {
		return cfg.Source
	}
	return cliSource
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("encode json: %w", err)
	}
	return nil
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
	filter := store.ListFilter{
		Namespace: namespaceFilter(fs, cfg),
		Kinds:     parseKinds(f.kinds),
		Tags:      parseTags(f.tags),
		Limit:     f.limit,
	}
	if f.archived {
		filter.Archived = store.IncludeArchived
	}
	ms, err := st.List(ctx, filter)
	if err != nil {
		return err
	}
	if f.asJSON {
		return writeJSON(a.stdout, ms)
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
	asJSON := fs.Bool("json", false, "output as JSON")
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
	links, err := st.Links(ctx, id)
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(a.stdout, struct {
			Memory  store.Memory         `json:"memory"`
			History []store.HistoryEntry `json:"history"`
			Links   []store.LinkView     `json:"links,omitempty"`
		}{Memory: m, History: hs, Links: links})
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
	if len(links) > 0 {
		fmt.Fprintln(w, "Links:")
		for _, l := range links {
			dir := "->"
			if l.Direction == "incoming" {
				dir = "<-"
			}
			arch := ""
			if l.Archived {
				arch = " (archived)"
			}
			title := ""
			if l.Title != "" {
				title = " " + l.Title
			}
			fmt.Fprintf(w, "  %s %-10s #%d [%s]%s%s\n", dir, l.Kind, l.ID, l.MemoryKind, title, arch)
		}
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
	if e, err := bootstrap.NewEngine(st, cfg, logger); err != nil {
		return err
	} else if e != nil {
		engine = e
	}
	res, err := engine.Search(ctx, store.SearchQuery{
		Query:           strings.Join(pos, " "),
		Namespace:       namespaceFilter(fs, cfg),
		Kinds:           parseKinds(f.kinds),
		Tags:            parseTags(f.tags),
		IncludeArchived: f.archived,
		Limit:           f.limit,
	})
	if err != nil {
		return err
	}
	if f.asJSON {
		return writeJSON(a.stdout, res)
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
	version := fs.Int("format-version", store.FormatVersion, "export format version; use 1 for a file that older llm-memory releases can import")
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	st, cfg, err := a.openStore(ctx, flags)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	d, err := st.Export(ctx, store.ExportOptions{Namespace: namespaceFilter(fs, cfg), WithHistory: *withHistory, FormatVersion: *version})
	if err != nil {
		return err
	}
	if d.DroppedLinks > 0 {
		fmt.Fprintf(a.stderr, "Note: %d link(s) to memories outside this export were left out.\n", d.DroppedLinks)
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
	merge := fs.Bool("merge", false, "merge a format 2 export by memory uuid (newest version wins, links and history are added) instead of skipping duplicates")
	noContentMatch := fs.Bool("no-content-match", false, "with --merge: do not pair memories of identical content that have different uuids")
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
	prefix := ""
	if *dryRun {
		prefix = "Dry run (nothing written): "
	}
	if *merge {
		mr, err := st.Merge(ctx, d, store.MergeOptions{NoContentMatch: *noContentMatch}, *dryRun, cliSource)
		if err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "%sMerged: %d inserted, %d updated, %d kept local (newer here), %d unchanged, %d namespace(s) created.\n",
			prefix, mr.Inserted, mr.Updated, mr.KeptLocal, mr.Unchanged, mr.NamespacesCreated)
		fmt.Fprintf(a.stdout, "%s%d link(s) added, %d history line(s) added", prefix, mr.LinksAdded, mr.HistoryAdded)
		if mr.TwinsMatched > 0 {
			fmt.Fprintf(a.stdout, ", %d memory(ies) paired by identical content", mr.TwinsMatched)
		}
		fmt.Fprintln(a.stdout, ".")
		for _, c := range mr.Changes {
			fmt.Fprintf(a.stdout, "  #%d %s: %s\n", c.ID, c.Result, c.Title)
		}
		return nil
	}
	rep, err := st.Import(ctx, d, *dryRun, cliSource)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "%s%d imported, %d skipped (exact duplicates), %d namespace(s) created.\n",
		prefix, rep.Imported, rep.Skipped, rep.NamespacesCreated)
	if len(d.Links) > 0 {
		fmt.Fprintf(a.stdout, "%s%d link(s) imported, %d skipped.\n", prefix, rep.LinksImported, rep.LinksSkipped)
	}
	return nil
}

// ---- stats ------------------------------------------------------------------

func (a app) cmdStats(ctx context.Context, args []string) error {
	fs := a.newFlagSet("stats")
	flags := config.Register(fs)
	asJSON := fs.Bool("json", false, "output as JSON")
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
	if *asJSON {
		return writeJSON(a.stdout, s)
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
	engine, err := bootstrap.NewEngine(st, cfg, logger)
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

// ---- remember / forget / handoff -------------------------------------------

func (a app) cmdRemember(ctx context.Context, args []string) error {
	fs := a.newFlagSet("remember")
	flags := config.Register(fs)
	kind := fs.String("kind", "", "memory kind: "+writableKindNames())
	title := fs.String("title", "", "one-line summary")
	tags := fs.String("tag", "", "comma-separated tags")
	importance := fs.Int("importance", store.DefaultImportance, "1 (low), 2 (normal), 3 (essential)")
	supersedes := fs.String("supersedes", "", "comma-separated IDs to archive")
	expiresIn := fs.String("expires-in", "", "optional TTL duration (e.g. 24h, 7d)")
	asJSON := fs.Bool("json", false, "output as JSON")
	pos, err := parse(fs, args, 1, -1)
	if err != nil {
		return err
	}
	if strings.TrimSpace(*kind) == "" {
		fmt.Fprintln(a.stderr, "remember requires --kind")
		return errUsage
	}
	if store.Kind(*kind) == store.KindHandoff {
		return errors.New("use `llm-memory handoff` for handoffs")
	}
	exp, err := parseCLIExpiration(*expiresIn)
	if err != nil {
		return err
	}
	supIDs, err := parseIDList(*supersedes)
	if err != nil {
		return err
	}
	content, err := readPositionalOrStdin(pos, a.stdin)
	if err != nil {
		return err
	}
	st, cfg, err := a.openStore(ctx, flags)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	m, archived, err := st.CreateDetailed(ctx, store.NewMemory{
		Namespace:  cfg.Namespace,
		Kind:       store.Kind(*kind),
		Title:      *title,
		Content:    content,
		Tags:       parseTags(*tags),
		Importance: *importance,
		Source:     cliActor(cfg),
		Supersedes: supIDs,
		ExpiresAt:  exp,
	})
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(a.stdout, struct {
			Memory     store.Memory `json:"memory"`
			Superseded []int64      `json:"superseded,omitempty"`
		}{Memory: m, Superseded: archived})
	}
	fmt.Fprintf(a.stdout, "Created #%d (%s, %s): %s\n", m.ID, m.Namespace, m.Kind, summary(m))
	return nil
}

func parseLinkSpecs(raw string) ([]store.LinkRef, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var out []store.LinkRef
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kindStr, idStr, ok := strings.Cut(part, ":")
		if !ok {
			return nil, fmt.Errorf("invalid link spec %q (expected kind:id, e.g. relates_to:42)", part)
		}
		k := store.LinkKind(strings.TrimSpace(kindStr))
		if !k.Valid() {
			return nil, fmt.Errorf("invalid link kind %q (expected supersedes, relates_to or depends_on)", kindStr)
		}
		id, err := parseID(strings.TrimSpace(idStr))
		if err != nil {
			return nil, err
		}
		out = append(out, store.LinkRef{TargetID: id, Kind: k})
	}
	return out, nil
}

func (a app) cmdUpdate(ctx context.Context, args []string) error {
	fs := a.newFlagSet("update")
	flags := config.Register(fs)
	kind := fs.String("kind", "", "new kind: "+writableKindNames())
	title := fs.String("title", "", "new one-line summary (empty string clears)")
	contentFlag := fs.String("content", "", "new content")
	tagsFlag := fs.String("tag", "", "new comma-separated tags (replaces existing)")
	importance := fs.Int("importance", 0, "1 (low), 2 (normal), 3 (essential)")
	expiresIn := fs.String("expires-in", "", "new TTL duration (e.g. 24h, 7d)")
	clearExpires := fs.Bool("clear-expires", false, "remove expiration timestamp")
	addLink := fs.String("add-link", "", "comma-separated links to add (kind:id, e.g. relates_to:5)")
	removeLink := fs.String("remove-link", "", "comma-separated links to remove (kind:id)")
	asJSON := fs.Bool("json", false, "output as JSON")
	pos, err := parse(fs, args, 1, -1)
	if err != nil {
		return err
	}
	id, err := parseID(pos[0])
	if err != nil {
		return err
	}
	var patch store.Patch
	if isSet(fs, "kind") {
		k := store.Kind(*kind)
		if k == store.KindHandoff {
			return errors.New("use `llm-memory handoff` for handoffs")
		}
		patch.Kind = &k
	}
	if isSet(fs, "title") {
		patch.Title = title
	}
	if isSet(fs, "content") {
		patch.Content = contentFlag
	} else if len(pos) > 1 {
		c, err := readPositionalOrStdin(pos[1:], a.stdin)
		if err != nil {
			return err
		}
		patch.Content = &c
	}
	if isSet(fs, "tag") {
		t := parseTags(*tagsFlag)
		patch.Tags = &t
	}
	if isSet(fs, "importance") {
		patch.Importance = importance
	}
	if isSet(fs, "namespace") {
		nsVal := fs.Lookup("namespace").Value.String()
		patch.Namespace = &nsVal
	}
	if *clearExpires {
		patch.ClearExpiresAt = true
	} else if isSet(fs, "expires-in") {
		exp, err := parseCLIExpiration(*expiresIn)
		if err != nil {
			return err
		}
		patch.ExpiresAt = exp
	}
	adds, err := parseLinkSpecs(*addLink)
	if err != nil {
		return err
	}
	rems, err := parseLinkSpecs(*removeLink)
	if err != nil {
		return err
	}

	hasFieldPatch := patch.Content != nil || patch.Title != nil || patch.Tags != nil ||
		patch.Importance != nil || patch.Kind != nil || patch.Namespace != nil ||
		patch.ExpiresAt != nil || patch.ClearExpiresAt
	if !hasFieldPatch && len(adds) == 0 && len(rems) == 0 {
		fmt.Fprintln(a.stderr, "update requires at least one field flag, content argument, or link change")
		return errUsage
	}

	st, cfg, err := a.openStore(ctx, flags)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	if patch.Namespace != nil {
		*patch.Namespace = cfg.Namespace
	}
	var m store.Memory
	if hasFieldPatch {
		m, err = st.Update(ctx, id, patch, cliActor(cfg))
		if err != nil {
			return err
		}
	} else {
		m, err = st.Get(ctx, id)
		if err != nil {
			return err
		}
		if m.Archived() {
			return &store.StateError{ID: id, Msg: "is archived; restore it first (llm-memory restore)"}
		}
	}
	actor := cliActor(cfg)
	for _, l := range adds {
		if err := st.AddLink(ctx, id, l.TargetID, l.Kind, actor); err != nil {
			return err
		}
	}
	for _, l := range rems {
		if err := st.RemoveLink(ctx, id, l.TargetID, l.Kind, actor); err != nil {
			return err
		}
	}
	if len(adds) > 0 || len(rems) > 0 {
		if m, err = st.Get(ctx, id); err != nil {
			return err
		}
	}
	if *asJSON {
		links, err := st.Links(ctx, id)
		if err != nil {
			return err
		}
		return writeJSON(a.stdout, struct {
			Memory store.Memory     `json:"memory"`
			Links  []store.LinkView `json:"links,omitempty"`
		}{Memory: m, Links: links})
	}
	fmt.Fprintf(a.stdout, "Updated #%d (%s, %s): %s\n", m.ID, m.Namespace, m.Kind, summary(m))
	return nil
}

func (a app) cmdForget(ctx context.Context, args []string) error {
	fs := a.newFlagSet("forget")
	flags := config.Register(fs)
	reason := fs.String("reason", "", "why the memory is archived")
	asJSON := fs.Bool("json", false, "output as JSON")
	pos, err := parse(fs, args, 1, -1)
	if err != nil {
		return err
	}
	ids := make([]int64, len(pos))
	for i, p := range pos {
		id, err := parseID(p)
		if err != nil {
			return err
		}
		ids[i] = id
	}
	st, cfg, err := a.openStore(ctx, flags)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	actor := cliActor(cfg)
	archived := make([]store.Memory, 0, len(ids))
	for _, id := range ids {
		m, err := st.Archive(ctx, id, actor, *reason)
		if err != nil {
			return err
		}
		archived = append(archived, m)
	}
	if *asJSON {
		return writeJSON(a.stdout, archived)
	}
	for _, m := range archived {
		fmt.Fprintf(a.stdout, "Archived #%d (%s, %s): %s\n", m.ID, m.Namespace, m.Kind, summary(m))
	}
	return nil
}

func (a app) cmdHandoff(ctx context.Context, args []string) error {
	fs := a.newFlagSet("handoff")
	flags := config.Register(fs)
	next := fs.String("next", "", "comma- or newline-separated next steps")
	questions := fs.String("questions", "", "comma- or newline-separated open questions")
	supersedes := fs.String("supersedes", "", "comma-separated IDs to archive")
	desc := fs.String("description", "", "optional namespace description")
	asJSON := fs.Bool("json", false, "output as JSON")
	pos, err := parse(fs, args, 1, -1)
	if err != nil {
		return err
	}
	supIDs, err := parseIDList(*supersedes)
	if err != nil {
		return err
	}
	rawSummary, err := readPositionalOrStdin(pos, a.stdin)
	if err != nil {
		return err
	}
	sum := strings.TrimSpace(rawSummary)
	if sum == "" {
		return errors.New("summary must not be empty")
	}
	st, cfg, err := a.openStore(ctx, flags)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	title, content := server.FormatHandoff(sum, parseItems(*next), parseItems(*questions))
	m, replaced, err := st.CreateDetailed(ctx, store.NewMemory{
		Namespace:            cfg.Namespace,
		NamespaceDescription: *desc,
		Kind:                 store.KindHandoff,
		Title:                title,
		Content:              content,
		Importance:           store.DefaultImportance,
		Source:               cliActor(cfg),
		Supersedes:           supIDs,
	})
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(a.stdout, struct {
			Memory   store.Memory `json:"memory"`
			Replaced []int64      `json:"replaced,omitempty"`
		}{Memory: m, Replaced: replaced})
	}
	fmt.Fprintf(a.stdout, "Handoff #%d (%s): %s\n", m.ID, m.Namespace, summary(m))
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

func parseCLIExpiration(s string) (*time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	if strings.HasSuffix(s, "d") {
		days, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil || days <= 0 {
			return nil, fmt.Errorf("invalid --expires-in duration %q", s)
		}
		t := time.Now().UTC().Add(time.Duration(days) * 24 * time.Hour)
		return &t, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return nil, fmt.Errorf("invalid --expires-in duration %q", s)
	}
	t := time.Now().UTC().Add(d)
	return &t, nil
}

// ---- namespace / gc / doctor / setup / ui ----------------------------------

func (a app) cmdNamespace(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(a.stderr, "Usage: llm-memory namespace <list|rename|merge|archive> [args]")
		return errUsage
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		fs := a.newFlagSet("namespace list")
		flags := config.Register(fs)
		asJSON := fs.Bool("json", false, "output as JSON")
		if _, err := parse(fs, rest, 0, 0); err != nil {
			return err
		}
		st, _, err := a.openStore(ctx, flags)
		if err != nil {
			return err
		}
		defer func() { _ = st.Close() }()
		nss, err := st.ListNamespaces(ctx)
		if err != nil {
			return err
		}
		if *asJSON {
			return writeJSON(a.stdout, nss)
		}
		tw := tabwriter.NewWriter(a.stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tACTIVE\tLAST ACTIVITY\tDESCRIPTION")
		for _, n := range nss {
			last := "-"
			if n.LastActivity != nil {
				last = n.LastActivity.Local().Format(cliDate)
			}
			fmt.Fprintf(tw, "%s\t%d\t%s\t%s\n", n.Name, n.ActiveCount, last, n.Description)
		}
		return tw.Flush()
	case "rename":
		fs := a.newFlagSet("namespace rename")
		flags := config.Register(fs)
		pos, err := parse(fs, rest, 2, 2)
		if err != nil {
			return err
		}
		st, cfg, err := a.openStore(ctx, flags)
		if err != nil {
			return err
		}
		defer func() { _ = st.Close() }()
		ns, err := st.RenameNamespace(ctx, pos[0], pos[1], cliActor(cfg))
		if err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "Renamed namespace %q to %q\n", pos[0], ns)
		return nil
	case "merge":
		fs := a.newFlagSet("namespace merge")
		flags := config.Register(fs)
		pos, err := parse(fs, rest, 2, 2)
		if err != nil {
			return err
		}
		st, cfg, err := a.openStore(ctx, flags)
		if err != nil {
			return err
		}
		defer func() { _ = st.Close() }()
		moved, err := st.MergeNamespace(ctx, pos[0], pos[1], cliActor(cfg))
		if err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "Merged %q into %q (%d memories moved)\n", pos[0], pos[1], moved)
		return nil
	case "archive":
		fs := a.newFlagSet("namespace archive")
		flags := config.Register(fs)
		reason := fs.String("reason", "", "archive reason recorded in history")
		pos, err := parse(fs, rest, 1, 1)
		if err != nil {
			return err
		}
		st, cfg, err := a.openStore(ctx, flags)
		if err != nil {
			return err
		}
		defer func() { _ = st.Close() }()
		n, err := st.ArchiveNamespace(ctx, pos[0], cliActor(cfg), *reason)
		if err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "Archived %d memories in namespace %q\n", n, pos[0])
		return nil
	default:
		fmt.Fprintf(a.stderr, "unknown namespace subcommand %q (expected list, rename, merge or archive)\n", sub)
		return errUsage
	}
}

func (a app) cmdGC(ctx context.Context, args []string) error {
	fs := a.newFlagSet("gc")
	flags := config.Register(fs)
	asJSON := fs.Bool("json", false, "output as JSON")
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	st, cfg, err := a.openStore(ctx, flags)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	n, err := st.ArchiveExpired(ctx, time.Now().UTC(), cliActor(cfg))
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(a.stdout, map[string]int64{"archived": n})
	}
	fmt.Fprintf(a.stdout, "Archived %d expired memories\n", n)
	return nil
}

type clientCheck struct {
	Client  string `json:"client"`
	Kind    string `json:"kind"` // "mcp" or "hook"
	Path    string `json:"path"`
	Command string `json:"command"`
	OK      bool   `json:"ok"`
	Warning string `json:"warning,omitempty"`
}

type doctorReport struct {
	OK                bool          `json:"ok"`
	Binary            string        `json:"binary"`
	Database          string        `json:"database"`
	FileBytes         int64         `json:"file_bytes"`
	Permissions       string        `json:"permissions"`
	PermissionsWarn   string        `json:"permissions_warning,omitempty"`
	SchemaVersion     int           `json:"schema_version"`
	LatestSchema      int           `json:"latest_schema"`
	Integrity         string        `json:"integrity"`
	Active            int           `json:"active"`
	Archived          int           `json:"archived"`
	ExpiredPendingGC  int           `json:"expired_pending_gc"`
	Namespace         string        `json:"namespace"`
	Embeddings        string        `json:"embeddings"`
	EmbeddingsURL     string        `json:"embeddings_url,omitempty"`
	EmbeddingsModel   string        `json:"embeddings_model,omitempty"`
	MissingEmbeddings int           `json:"missing_embeddings,omitempty"`
	Clients           []clientCheck `json:"clients,omitempty"`
	Warnings          []string      `json:"warnings,omitempty"`
}

func checkPrivatePerms(dbPath string) (summary, warning string) {
	if runtime.GOOS == "windows" {
		return "n/a (Windows)", ""
	}
	dir := filepath.Dir(dbPath)
	dirFI, dirErr := os.Stat(dir)
	fileFI, fileErr := os.Stat(dbPath)
	var parts []string
	var warns []string
	if dirErr == nil {
		m := dirFI.Mode().Perm()
		parts = append(parts, fmt.Sprintf("dir %04o", m))
		if m&0o077 != 0 {
			warns = append(warns, fmt.Sprintf("directory %s is group/world-accessible (%04o, expected 0700)", dir, m))
		}
	}
	if fileErr == nil {
		m := fileFI.Mode().Perm()
		parts = append(parts, fmt.Sprintf("file %04o", m))
		if m&0o077 != 0 {
			warns = append(warns, fmt.Sprintf("database %s is group/world-accessible (%04o, expected 0600)", dbPath, m))
		}
	}
	if len(parts) == 0 {
		return "unknown", ""
	}
	return strings.Join(parts, ", "), strings.Join(warns, "; ")
}

func (a app) cmdDoctor(ctx context.Context, args []string) error {
	fs := a.newFlagSet("doctor")
	flags := config.Register(fs)
	asJSON := fs.Bool("json", false, "output as JSON")
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	st, cfg, err := a.openStore(ctx, flags)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	bin, err := os.Executable()
	if err != nil || bin == "" {
		bin = "llm-memory"
	}
	sv, err := st.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	stats, err := st.Stats(ctx)
	if err != nil {
		return err
	}
	expired, err := st.CountExpired(ctx, time.Now().UTC())
	if err != nil {
		return err
	}

	permSummary, permWarn := checkPrivatePerms(cfg.DB)
	rep := doctorReport{
		OK:               true,
		Binary:           bin,
		Database:         cfg.DB,
		FileBytes:        stats.FileBytes,
		Permissions:      permSummary,
		PermissionsWarn:  permWarn,
		SchemaVersion:    sv,
		LatestSchema:     store.LatestSchemaVersion(),
		Integrity:        "ok (SQLite + FTS5)",
		Active:           stats.Active,
		Archived:         stats.Archived,
		ExpiredPendingGC: expired,
		Namespace:        cfg.Namespace,
		Embeddings:       "disabled (full-text search only)",
	}
	if permWarn != "" {
		rep.Warnings = append(rep.Warnings, permWarn)
	}
	if expired > 0 {
		rep.Warnings = append(rep.Warnings, fmt.Sprintf("%d expired memories pending gc (run `llm-memory gc`)", expired))
	}
	rep.Clients = checkClientConfigs(a.env)
	for _, c := range rep.Clients {
		if c.Warning != "" {
			rep.Warnings = append(rep.Warnings, c.Warning)
		}
	}

	var checkErr error
	if err := st.IntegrityCheck(ctx); err != nil {
		rep.OK = false
		rep.Integrity = fmt.Sprintf("error (%v)", err)
		checkErr = err
	}

	if cfg.Embed.Enabled() {
		rep.EmbeddingsURL = cfg.Embed.URL
		rep.EmbeddingsModel = cfg.Embed.Model
		missing, mErr := st.CountMissingEmbeddings(ctx, cfg.Embed.Model)
		if mErr == nil {
			rep.MissingEmbeddings = missing
			if missing > 0 {
				rep.Warnings = append(rep.Warnings, fmt.Sprintf("%d memories missing embeddings (run `llm-memory reindex`)", missing))
			}
		}
		client, err := semantic.NewClient(semantic.ClientConfig{
			URL: cfg.Embed.URL, Model: cfg.Embed.Model, APIKey: cfg.Embed.APIKey, Timeout: cfg.Embed.Timeout,
		})
		if err != nil {
			rep.OK = false
			rep.Embeddings = fmt.Sprintf("error (%v)", err)
			if checkErr == nil {
				checkErr = err
			}
		} else if _, err := client.Embed(ctx, []string{"healthcheck"}); err != nil {
			rep.OK = false
			rep.Embeddings = fmt.Sprintf("unreachable (%s, %v)", cfg.Embed.URL, err)
			if checkErr == nil {
				checkErr = err
			}
		} else {
			rep.Embeddings = fmt.Sprintf("ok (%s, model %s)", cfg.Embed.URL, cfg.Embed.Model)
		}
	}

	if *asJSON {
		if err := writeJSON(a.stdout, rep); err != nil {
			return err
		}
		return checkErr
	}

	fmt.Fprintf(a.stdout, "Binary:      %s\n", rep.Binary)
	fmt.Fprintf(a.stdout, "Database:    %s (%s)\n", rep.Database, humanBytes(rep.FileBytes))
	fmt.Fprintf(a.stdout, "Permissions: %s\n", rep.Permissions)
	fmt.Fprintf(a.stdout, "Schema:      v%d (latest v%d)\n", rep.SchemaVersion, rep.LatestSchema)
	fmt.Fprintf(a.stdout, "Integrity:   %s\n", rep.Integrity)
	fmt.Fprintf(a.stdout, "Memories:    %d active, %d archived (%d expired pending gc)\n", rep.Active, rep.Archived, rep.ExpiredPendingGC)
	fmt.Fprintf(a.stdout, "Namespace:   %s\n", rep.Namespace)
	if cfg.Embed.Enabled() {
		fmt.Fprintf(a.stdout, "Embeddings:  %s (%d missing)\n", rep.Embeddings, rep.MissingEmbeddings)
	} else {
		fmt.Fprintf(a.stdout, "Embeddings:  %s\n", rep.Embeddings)
	}
	for _, c := range rep.Clients {
		status := "ok"
		if !c.OK {
			status = "broken"
		}
		fmt.Fprintf(a.stdout, "Client:      %s (%s) -> %s [%s]\n", c.Client, c.Kind, c.Command, status)
	}
	for _, w := range rep.Warnings {
		fmt.Fprintf(a.stdout, "Warning:     %s\n", w)
	}
	return checkErr
}

func checkClientConfigs(env config.Env) []clientCheck {
	var checks []clientCheck
	for _, client := range []string{"antigravity", "claude-desktop", "claude-code", "codex", "opencode"} {
		path, err := clientConfigPath(client, env)
		if err != nil {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var cmd string
		if client == "codex" {
			cmd = extractCodexCommand(string(data))
		} else {
			cmd = extractJSONMCPCommand(client, data)
		}
		if cmd == "" {
			continue
		}
		checks = append(checks, verifyCommand(client, "mcp", path, cmd))
	}
	for _, client := range hookClients {
		path, err := hookConfigPath(client, env)
		if err != nil {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, cmdLine := range extractHookCommands(client, data) {
			bin := firstShellToken(cmdLine)
			checks = append(checks, verifyCommand(client, "hook", path, bin))
		}
	}
	return checks
}

func verifyCommand(client, kind, path, cmd string) clientCheck {
	chk := clientCheck{Client: client, Kind: kind, Path: path, Command: cmd, OK: true}
	if cmd == "" {
		chk.OK = false
		chk.Warning = fmt.Sprintf("%s %s config (%s) has empty command", client, kind, path)
		return chk
	}
	if strings.ContainsAny(cmd, `/\`) {
		fi, err := os.Stat(cmd)
		if err != nil || fi.IsDir() {
			chk.OK = false
			chk.Warning = fmt.Sprintf("%s %s config (%s) points to missing binary %q", client, kind, path, cmd)
		}
		return chk
	}
	if _, err := exec.LookPath(cmd); err != nil {
		chk.OK = false
		chk.Warning = fmt.Sprintf("%s %s config (%s): %q not found in PATH", client, kind, path, cmd)
	}
	return chk
}

func extractJSONMCPCommand(client string, data []byte) string {
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return ""
	}
	key := "mcpServers"
	if client == "opencode" {
		key = "mcp"
	}
	sec, _ := root[key].(map[string]any)
	entry, _ := sec["llm-memory"].(map[string]any)
	if entry == nil {
		return ""
	}
	if s, ok := entry["command"].(string); ok {
		return s
	}
	if arr, ok := entry["command"].([]any); ok && len(arr) > 0 {
		s, _ := arr[0].(string)
		return s
	}
	return ""
}

func extractCodexCommand(content string) string {
	inSection := false
	for _, line := range strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			inSection = trimmed == "[mcp_servers.llm-memory]"
			continue
		}
		if !inSection {
			continue
		}
		if k, v, ok := strings.Cut(trimmed, "="); ok && strings.TrimSpace(k) == "command" {
			val := strings.TrimSpace(v)
			if unq, err := strconv.Unquote(val); err == nil {
				return unq
			}
			return strings.Trim(val, `"'`)
		}
	}
	return ""
}

func extractHookCommands(client string, data []byte) []string {
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil
	}
	var cmds []string
	if client == "antigravity" {
		entry, _ := root["llm-memory"].(map[string]any)
		for _, rawList := range entry {
			list, _ := rawList.([]any)
			for _, item := range list {
				if hm, ok := item.(map[string]any); ok {
					if c, _ := hm["command"].(string); isOurHook(c) {
						cmds = append(cmds, c)
					}
				}
			}
		}
		return cmds
	}
	hooks, _ := root["hooks"].(map[string]any)
	for _, rawGroups := range hooks {
		groups, _ := rawGroups.([]any)
		for _, g := range groups {
			gm, _ := g.(map[string]any)
			hs, _ := gm["hooks"].([]any)
			for _, h := range hs {
				if hm, ok := h.(map[string]any); ok {
					if c, _ := hm["command"].(string); isOurHook(c) {
						cmds = append(cmds, c)
					}
				}
			}
		}
	}
	return cmds
}

func firstShellToken(cmd string) string {
	s := strings.TrimSpace(cmd)
	if s == "" {
		return ""
	}
	if s[0] == '\'' {
		var b strings.Builder
		for i := 1; i < len(s); i++ {
			if s[i] == '\'' {
				if strings.HasPrefix(s[i:], `'\''`) {
					b.WriteByte('\'')
					i += 3
					continue
				}
				return b.String()
			}
			b.WriteByte(s[i])
		}
		return b.String()
	}
	if s[0] == '"' {
		var b strings.Builder
		for i := 1; i < len(s); i++ {
			if s[i] == '\\' && i+1 < len(s) {
				i++
				b.WriteByte(s[i])
				continue
			}
			if s[i] == '"' {
				return b.String()
			}
			b.WriteByte(s[i])
		}
		return b.String()
	}
	tok, _, _ := strings.Cut(s, " ")
	return tok
}

const (
	defaultOllamaURL   = "http://localhost:11434/v1/embeddings"
	defaultOllamaModel = "nomic-embed-text"
)

func formatTOMLStringArray(items []string) string {
	quoted := make([]string, len(items))
	for i, item := range items {
		quoted[i] = strconv.Quote(item)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

func clientConfigPath(client string, env config.Env) (string, error) {
	home, err := env.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	xdgConfig := env.Getenv("XDG_CONFIG_HOME")
	if xdgConfig == "" {
		xdgConfig = filepath.Join(home, ".config")
	}
	switch client {
	case "antigravity":
		return filepath.Join(home, ".gemini", "config", "mcp_config.json"), nil
	case "claude-desktop":
		switch runtime.GOOS {
		case "darwin":
			return filepath.Join(home, "Library", "Application Support", "Claude", "claude_desktop_config.json"), nil
		case "windows":
			appData := env.Getenv("APPDATA")
			if appData == "" {
				appData = filepath.Join(home, "AppData", "Roaming")
			}
			return filepath.Join(appData, "Claude", "claude_desktop_config.json"), nil
		default:
			return filepath.Join(xdgConfig, "Claude", "claude_desktop_config.json"), nil
		}
	case "claude-code":
		return filepath.Join(home, ".claude.json"), nil
	case "codex":
		return filepath.Join(home, ".codex", "config.toml"), nil
	case "opencode":
		return filepath.Join(xdgConfig, "opencode", "opencode.json"), nil
	case "vscode":
		return filepath.Join(".vscode", "mcp.json"), nil
	default:
		return "", fmt.Errorf("unknown client %q", client)
	}
}

func patchJSONConfig(path, sectionKey, serverName string, entry, defaults map[string]any) error {
	root := map[string]any{}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if len(strings.TrimSpace(string(data))) > 0 {
		if err := json.Unmarshal(data, &root); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
	}
	for k, v := range defaults {
		if _, ok := root[k]; !ok {
			root[k] = v
		}
	}
	var section map[string]any
	if raw, ok := root[sectionKey]; ok && raw != nil {
		m, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: %q is not a JSON object", path, sectionKey)
		}
		section = m
	} else {
		section = map[string]any{}
		root[sectionKey] = section
	}
	section[serverName] = entry

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create directory for %s: %w", path, err)
	}
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	out = append(out, '\n')
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func patchCodexTOML(path, block string) error {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	content := strings.ReplaceAll(string(data), "\r\n", "\n")
	var updated string
	if strings.TrimSpace(content) == "" {
		updated = block
	} else {
		lines := strings.Split(content, "\n")
		start := -1
		end := len(lines)
		for i, line := range lines {
			trimmed := strings.TrimSpace(line)
			if start == -1 {
				if trimmed == "[mcp_servers.llm-memory]" {
					start = i
				}
				continue
			}
			if strings.HasPrefix(trimmed, "[") {
				end = i
				break
			}
		}
		if start >= 0 {
			var rebuilt []string
			rebuilt = append(rebuilt, lines[:start]...)
			rebuilt = append(rebuilt, strings.TrimRight(block, "\n"))
			if end < len(lines) {
				if strings.TrimSpace(lines[end]) != "" {
					rebuilt = append(rebuilt, "")
				}
				rebuilt = append(rebuilt, lines[end:]...)
			}
			updated = strings.Join(rebuilt, "\n")
			if !strings.HasSuffix(updated, "\n") {
				updated += "\n"
			}
		} else {
			updated = strings.TrimRight(content, "\n") + "\n\n" + block
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create directory for %s: %w", path, err)
	}
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func removeJSONConfig(path, sectionKey, serverName string) error {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return patchJSONFile(path, func(root map[string]any) error {
		raw, ok := root[sectionKey]
		if !ok || raw == nil {
			return nil
		}
		sec, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("%q is not a JSON object", sectionKey)
		}
		delete(sec, serverName)
		return nil
	})
}

func removeCodexTOML(path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	content := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(content, "\n")
	start := -1
	end := len(lines)
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if start == -1 {
			if trimmed == "[mcp_servers.llm-memory]" {
				start = i
			}
			continue
		}
		if strings.HasPrefix(trimmed, "[") {
			end = i
			break
		}
	}
	if start == -1 {
		return nil
	}
	for end < len(lines) && strings.TrimSpace(lines[end]) == "" {
		end++
	}
	rebuilt := append([]string{}, lines[:start]...)
	rebuilt = append(rebuilt, lines[end:]...)
	updated := strings.TrimRight(strings.Join(rebuilt, "\n"), "\n")
	if updated != "" {
		updated += "\n"
	}
	return os.WriteFile(path, []byte(updated), 0o644)
}

func (a app) cmdSetup(_ context.Context, args []string) error {
	fs := a.newFlagSet("setup")
	flags := config.Register(fs)
	ollama := fs.Bool("ollama", false, "include local Ollama embeddings defaults ("+defaultOllamaModel+")")
	write := fs.Bool("write", false, "patch the client configuration file directly")
	remove := fs.Bool("remove", false, "remove llm-memory from the client configuration file")
	cfgPath := fs.String("config", "", "override target config file path (with --write or --remove)")
	hooks := fs.Bool("hooks", false, "configure client hooks (antigravity, claude-code, codex)")
	eventsFlag := fs.String("events", hookEventSessionStart, "comma-separated hook events with --hooks: "+strings.Join(hookEvents, ", "))
	pos, err := parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	if *write && *remove {
		fmt.Fprintln(a.stderr, "cannot combine --write and --remove")
		return errUsage
	}
	cfg, err := flags.Resolve(a.env)
	if err != nil {
		return err
	}
	if *ollama {
		if cfg.Embed.URL == "" {
			cfg.Embed.URL = defaultOllamaURL
		}
		if cfg.Embed.Model == "" {
			cfg.Embed.Model = defaultOllamaModel
		}
	}
	bin := executablePath()
	client := strings.ToLower(pos[0])
	if *hooks {
		if *remove {
			return a.removeHooks(client, *cfgPath)
		}
		events, err := parseHookEvents(*eventsFlag)
		if err != nil {
			fmt.Fprintln(a.stderr, err)
			return errUsage
		}
		return a.setupHooks(client, bin, cfg.DB, hookNamespace(fs, a.env), events, *write, *cfgPath)
	}
	if *remove {
		target := *cfgPath
		if target == "" {
			target, err = clientConfigPath(client, a.env)
			if err != nil {
				fmt.Fprintf(a.stderr, "unknown client %q (expected antigravity, claude-desktop, claude-code, codex, opencode or vscode)\n", pos[0])
				return errUsage
			}
		}
		switch client {
		case "antigravity", "claude-desktop", "claude-code":
			err = removeJSONConfig(target, "mcpServers", "llm-memory")
		case "vscode":
			err = removeJSONConfig(target, "servers", "llm-memory")
		case "opencode":
			err = removeJSONConfig(target, "mcp", "llm-memory")
		case "codex":
			err = removeCodexTOML(target)
		default:
			fmt.Fprintf(a.stderr, "unknown client %q (expected antigravity, claude-desktop, claude-code, codex, opencode or vscode)\n", pos[0])
			return errUsage
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "Removed llm-memory from %s (%s)\n", target, client)
		return nil
	}

	srvArgs := []string{"serve", "--db", cfg.DB}
	if cfg.Namespace != config.DefaultNamespace {
		srvArgs = append(srvArgs, "--namespace", cfg.Namespace)
	}
	if cfg.Embed.Enabled() {
		srvArgs = append(srvArgs, "--embed-url", cfg.Embed.URL, "--embed-model", cfg.Embed.Model)
	}

	if *write {
		target := *cfgPath
		if target == "" {
			target, err = clientConfigPath(client, a.env)
			if err != nil {
				fmt.Fprintf(a.stderr, "unknown client %q (expected antigravity, claude-desktop, claude-code, codex, opencode or vscode)\n", pos[0])
				return errUsage
			}
		}
		switch client {
		case "antigravity", "claude-desktop", "claude-code":
			entry := map[string]any{"command": bin, "args": srvArgs}
			if err := patchJSONConfig(target, "mcpServers", "llm-memory", entry, nil); err != nil {
				return err
			}
		case "vscode":
			entry := map[string]any{"type": "stdio", "command": bin, "args": srvArgs}
			if err := patchJSONConfig(target, "servers", "llm-memory", entry, nil); err != nil {
				return err
			}
		case "opencode":
			entry := map[string]any{"type": "local", "command": append([]string{bin}, srvArgs...), "enabled": true}
			defaults := map[string]any{"$schema": "https://opencode.ai/config.json"}
			if err := patchJSONConfig(target, "mcp", "llm-memory", entry, defaults); err != nil {
				return err
			}
		case "codex":
			block := fmt.Sprintf("[mcp_servers.llm-memory]\ncommand = %s\nargs = %s\n",
				strconv.Quote(bin), formatTOMLStringArray(srvArgs))
			if err := patchCodexTOML(target, block); err != nil {
				return err
			}
		default:
			fmt.Fprintf(a.stderr, "unknown client %q (expected antigravity, claude-desktop, claude-code, codex, opencode or vscode)\n", pos[0])
			return errUsage
		}
		fmt.Fprintf(a.stdout, "Updated %s (%s)\n", target, client)
		return nil
	}

	switch client {
	case "antigravity", "claude-desktop":
		out := map[string]any{
			"mcpServers": map[string]any{
				"llm-memory": map[string]any{
					"command": bin,
					"args":    srvArgs,
				},
			},
		}
		return writeJSON(a.stdout, out)
	case "vscode":
		out := map[string]any{
			"servers": map[string]any{
				"llm-memory": map[string]any{
					"type":    "stdio",
					"command": bin,
					"args":    srvArgs,
				},
			},
		}
		return writeJSON(a.stdout, out)
	case "claude-code":
		fmt.Fprintf(a.stdout, "claude mcp add --scope user llm-memory -- %s %s\n", bin, strings.Join(srvArgs, " "))
		return nil
	case "codex":
		fmt.Fprintf(a.stdout, "[mcp_servers.llm-memory]\ncommand = %s\nargs = %s\n",
			strconv.Quote(bin), formatTOMLStringArray(srvArgs))
		return nil
	case "opencode":
		cmd := append([]string{bin}, srvArgs...)
		out := map[string]any{
			"$schema": "https://opencode.ai/config.json",
			"mcp": map[string]any{
				"llm-memory": map[string]any{
					"type":    "local",
					"command": cmd,
					"enabled": true,
				},
			},
		}
		return writeJSON(a.stdout, out)
	default:
		fmt.Fprintf(a.stderr, "unknown client %q (expected antigravity, claude-desktop, claude-code, codex, opencode or vscode)\n", pos[0])
		return errUsage
	}
}

func (a app) cmdUI(ctx context.Context, args []string) error {
	fs := a.newFlagSet("ui")
	flags := config.Register(fs)
	addr := fs.String("addr", "127.0.0.1:8765", "listen address")
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	st, cfg, err := a.openStore(ctx, flags)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	logger := slog.New(slog.NewTextHandler(a.stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	eng, err := bootstrap.NewEngine(st, cfg, logger)
	if err != nil {
		return err
	}
	var searcher ui.Searcher = st
	if eng != nil {
		searcher = eng
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", *addr, err)
	}
	allowRemote := !isLoopbackListenAddr(ln.Addr().String())
	if allowRemote {
		logger.Warn("web UI has no user authentication; bind to loopback (127.0.0.1) or place behind an authenticating reverse proxy", "addr", ln.Addr().String())
	}
	fmt.Fprintf(a.stdout, "llm-memory UI listening on http://%s\n", ln.Addr().String())
	srv := &http.Server{
		Handler:           ui.NewHandlerWithOptions(st, searcher, ui.Options{AllowRemoteHost: allowRemote}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (a app) cmdBackup(ctx context.Context, args []string) error {
	fs := a.newFlagSet("backup")
	flags := config.Register(fs)
	keep := fs.Int("keep", 0, "keep only the N most recent auto-named backups (0 = keep all)")
	verify := fs.Bool("verify", true, "verify backup integrity after creation")
	asJSON := fs.Bool("json", false, "output as JSON")
	pos, err := parse(fs, args, 0, 1)
	if err != nil {
		return err
	}
	if *keep < 0 {
		fmt.Fprintln(a.stderr, "--keep must be >= 0")
		return errUsage
	}
	st, _, err := a.openStore(ctx, flags)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	autoNamed := len(pos) == 0
	var dest string
	if autoNamed {
		dest = fmt.Sprintf("%s.backup-%s.db", st.Path(), time.Now().UTC().Format("20060102-150405"))
	} else {
		dest = pos[0]
	}

	size, err := st.Backup(ctx, dest)
	if err != nil {
		return err
	}
	absDest, err := filepath.Abs(dest)
	if err != nil {
		absDest = dest
	}

	if *verify {
		bak, err := store.Open(ctx, absDest)
		if err != nil {
			return fmt.Errorf("verify backup open: %w", err)
		}
		checkErr := bak.IntegrityCheck(ctx)
		_ = bak.Close()
		if checkErr != nil {
			return fmt.Errorf("verify backup integrity: %w", checkErr)
		}
	}

	var pruned []string
	if autoNamed && *keep > 0 {
		matches, _ := filepath.Glob(st.Path() + ".backup-*.db")
		sort.Sort(sort.Reverse(sort.StringSlice(matches)))
		if len(matches) > *keep {
			for _, old := range matches[*keep:] {
				if err := os.Remove(old); err == nil {
					pruned = append(pruned, old)
				}
			}
		}
	}

	if *asJSON {
		return writeJSON(a.stdout, struct {
			Path     string   `json:"path"`
			Bytes    int64    `json:"bytes"`
			Verified bool     `json:"verified"`
			Pruned   []string `json:"pruned,omitempty"`
		}{Path: absDest, Bytes: size, Verified: *verify, Pruned: pruned})
	}
	fmt.Fprintf(a.stdout, "backup written to %s (%d bytes", absDest, size)
	if *verify {
		fmt.Fprint(a.stdout, ", verified")
	}
	fmt.Fprintln(a.stdout, ")")
	if len(pruned) > 0 {
		fmt.Fprintf(a.stdout, "pruned %d old backup(s)\n", len(pruned))
	}
	return nil
}

func (a app) cmdCompletion(_ context.Context, args []string) error {
	fs := a.newFlagSet("completion")
	pos, err := parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	shell := strings.ToLower(pos[0])
	switch shell {
	case "bash":
		_, err = fmt.Fprint(a.stdout, bashCompletion)
		return err
	case "zsh":
		_, err = fmt.Fprint(a.stdout, zshCompletion)
		return err
	case "fish":
		_, err = fmt.Fprint(a.stdout, fishCompletion)
		return err
	default:
		fmt.Fprintf(a.stderr, "unsupported shell %q (expected bash, zsh or fish)\n", pos[0])
		return errUsage
	}
}

const bashCompletion = `_llm_memory() {
  local cur prev cmds
  COMPREPLY=()
  cur="${COMP_WORDS[COMP_CWORD]}"
  prev="${COMP_WORDS[COMP_CWORD-1]}"
  cmds="serve context hook remember update forget handoff list show search restore purge export import stats reindex namespace gc doctor backup setup ui completion version help"
  if [[ ${COMP_CWORD} -eq 1 ]]; then
    COMPREPLY=( $(compgen -W "${cmds}" -- "${cur}") )
    return 0
  fi
  case "${COMP_WORDS[1]}" in
    namespace)
      if [[ ${COMP_CWORD} -eq 2 ]]; then
        COMPREPLY=( $(compgen -W "list rename merge archive" -- "${cur}") )
      fi
      ;;
    setup)
      COMPREPLY=( $(compgen -W "antigravity claude-desktop claude-code codex opencode vscode --hooks --write --remove --ollama --events --config" -- "${cur}") )
      ;;
    hook)
      COMPREPLY=( $(compgen -W "antigravity claude-code codex --event --events --write --remove --max-chars --focus --config" -- "${cur}") )
      ;;
    completion)
      COMPREPLY=( $(compgen -W "bash zsh fish" -- "${cur}") )
      ;;
    *)
      if [[ "${cur}" == -* ]]; then
        COMPREPLY=( $(compgen -W "--db --namespace --source --log-level --json --help" -- "${cur}") )
      fi
      ;;
  esac
}
complete -F _llm_memory llm-memory
`

const zshCompletion = `#compdef llm-memory

_llm-memory() {
  local -a commands
  commands=(
    'serve:start the MCP server (stdio or Streamable HTTP)'
    'context:print the briefing get_context would return'
    'hook:lifecycle hook entry point'
    'remember:create a memory'
    'update:modify an active memory'
    'forget:archive one or more memories'
    'handoff:write a handoff for the next assistant'
    'list:list memories'
    'show:full content, links and history of a memory'
    'search:full-text / semantic search'
    'restore:un-archive a memory'
    'purge:permanently delete archived memories'
    'export:JSON export'
    'import:import a JSON export'
    'stats:database statistics'
    'reindex:compute embeddings'
    'namespace:manage namespaces'
    'gc:archive expired memories'
    'doctor:check database integrity, permissions, embeddings and clients'
    'backup:hot SQLite backup'
    'setup:configure MCP clients or hooks'
    'ui:start local web explorer'
    'completion:print shell completion script'
    'version:version and schema version'
    'help:show usage'
  )
  if (( CURRENT == 2 )); then
    _describe 'command' commands
    return
  fi
  case "${words[2]}" in
    namespace)
      _values 'subcommand' list rename merge archive
      ;;
    setup)
      _values 'client' antigravity claude-desktop claude-code codex opencode vscode
      ;;
    hook)
      _values 'client' antigravity claude-code codex
      ;;
    completion)
      _values 'shell' bash zsh fish
      ;;
  esac
}

_llm-memory "$@"
`

const fishCompletion = `set -l cmds serve context hook remember update forget handoff list show search restore purge export import stats reindex namespace gc doctor backup setup ui completion version help
complete -c llm-memory -f
complete -c llm-memory -n "not __fish_seen_subcommand_from $cmds" -a "$cmds"
complete -c llm-memory -n "__fish_seen_subcommand_from namespace" -a "list rename merge archive"
complete -c llm-memory -n "__fish_seen_subcommand_from setup" -a "antigravity claude-desktop claude-code codex opencode vscode"
complete -c llm-memory -n "__fish_seen_subcommand_from hook" -a "antigravity claude-code codex"
complete -c llm-memory -n "__fish_seen_subcommand_from completion" -a "bash zsh fish"
`
