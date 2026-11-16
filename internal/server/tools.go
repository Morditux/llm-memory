package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Morditux/llm-memory/internal/briefing"
	"github.com/Morditux/llm-memory/internal/store"
)

// ---- Shared views -----------------------------------------------------------

// MemoryView is a memory as returned to the LLM.
type MemoryView struct {
	ID         int64    `json:"id"`
	Namespace  string   `json:"namespace"`
	Kind       string   `json:"kind"`
	Title      string   `json:"title,omitempty"`
	Content    string   `json:"content"`
	Tags       []string `json:"tags,omitempty"`
	Importance int      `json:"importance"`
	Source     string   `json:"source" jsonschema:"Client that created the memory."`
	CreatedAt  string   `json:"created_at"`
	UpdatedAt  string   `json:"updated_at"`
	UpdatedBy  string   `json:"updated_by" jsonschema:"Client that last changed the memory."`
	ArchivedAt string   `json:"archived_at,omitempty" jsonschema:"Set when the memory is archived (forgotten)."`
	ExpiresAt  string   `json:"expires_at,omitempty" jsonschema:"Optional expiration timestamp (RFC 3339)."`
}

func viewOf(m store.Memory) MemoryView {
	v := MemoryView{
		ID: m.ID, Namespace: m.Namespace, Kind: string(m.Kind), Title: m.Title, Content: m.Content,
		Tags: m.Tags, Importance: m.Importance, Source: m.Source,
		CreatedAt: ts(m.CreatedAt), UpdatedAt: ts(m.UpdatedAt), UpdatedBy: m.UpdatedBy,
	}
	if len(v.Tags) == 0 {
		v.Tags = nil
	}
	if m.ArchivedAt != nil {
		v.ArchivedAt = ts(*m.ArchivedAt)
	}
	if m.ExpiresAt != nil {
		v.ExpiresAt = ts(*m.ExpiresAt)
	}
	return v
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// ---- get_context ------------------------------------------------------------

// GetContextInput is the input of get_context.
type GetContextInput struct {
	Namespace string `json:"namespace,omitempty" jsonschema:"Project namespace (e.g. the repository name). Omit to use the server default."`
	MaxChars  int    `json:"max_chars,omitempty" jsonschema:"Character budget of the briefing. Default 6000."`
	Focus     string `json:"focus,omitempty" jsonschema:"Optional topic or query to surface relevant memories in the briefing."`
}

func (s *srv) getContext(ctx context.Context, req *mcp.CallToolRequest, in GetContextInput) (*mcp.CallToolResult, any, error) {
	ns := s.namespace(in.Namespace)
	text, err := briefing.Build(ctx, s.store, briefing.Options{Namespace: ns, MaxChars: in.MaxChars, Focus: in.Focus, Searcher: s.search})
	if err != nil {
		return nil, nil, s.toolError(ctx, req, "get_context", err)
	}
	s.logCall(ctx, req, "get_context", "namespace", ns)
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
}

// ---- remember ---------------------------------------------------------------

// RememberInput is the input of remember.
type RememberInput struct {
	Content    string   `json:"content" jsonschema:"The memory: one self-contained fact, decision (with its reason) or preference, understandable without this conversation. Max 8000 characters. Never secrets."`
	Kind       string   `json:"kind" jsonschema:"preference = lasting user preference; fact = stable fact about the project; decision = choice made and why; task_state = progress of an ongoing task; note = anything else. Use the handoff tool for handoffs."`
	Namespace  string   `json:"namespace,omitempty" jsonschema:"Project namespace; created if new (lowercase, spaces become dashes). Use global for cross-project preferences. Omit for the server default."`
	Title      string   `json:"title,omitempty" jsonschema:"One-line summary shown in briefings and lists. Recommended."`
	Tags       []string `json:"tags,omitempty" jsonschema:"Lowercase keywords that help recall."`
	Importance int      `json:"importance,omitempty" jsonschema:"1 = low, 2 = normal (default), 3 = essential: always shown by get_context. Use 3 sparingly."`
	Supersedes []int64  `json:"supersedes,omitempty" jsonschema:"IDs of memories this one replaces; they are archived in the same operation."`
	RelatedTo  []int64  `json:"related_to,omitempty" jsonschema:"IDs of related memories to link (relates_to)."`
	DependsOn  []int64  `json:"depends_on,omitempty" jsonschema:"IDs of memories this one depends on (depends_on)."`
	ExpiresIn  string   `json:"expires_in,omitempty" jsonschema:"Optional TTL duration (e.g. 24h, 7d) after which the memory expires."`
	ExpiresAt  string   `json:"expires_at,omitempty" jsonschema:"Optional RFC 3339 expiration timestamp."`
}

// DuplicateView is a probable duplicate reported by remember.
type DuplicateView struct {
	ID         int64   `json:"id"`
	Kind       string  `json:"kind"`
	Title      string  `json:"title,omitempty"`
	Snippet    string  `json:"snippet"`
	Similarity float64 `json:"similarity" jsonschema:"Word overlap between 0 and 1."`
}

// RememberOutput is the output of remember.
type RememberOutput struct {
	Memory             MemoryView      `json:"memory"`
	Superseded         []int64         `json:"superseded,omitempty" jsonschema:"IDs archived because this memory replaces them."`
	PossibleDuplicates []DuplicateView `json:"possible_duplicates,omitempty" jsonschema:"Existing memories that look similar. The new memory was created anyway."`
	Hint               string          `json:"hint,omitempty"`
}

func (s *srv) remember(ctx context.Context, req *mcp.CallToolRequest, in RememberInput) (*mcp.CallToolResult, RememberOutput, error) {
	ns := s.namespace(in.Namespace)
	src := s.source(req)

	exp, err := parseExpiration(in.ExpiresIn, in.ExpiresAt)
	if err != nil {
		return nil, RememberOutput{}, s.toolError(ctx, req, "remember", err)
	}

	// Duplicates are searched before insertion, among active memories of the
	// target namespace, ignoring the ones being superseded.
	queryText := strings.TrimSpace(in.Title + " " + in.Content)
	sims, err := s.store.FindSimilar(ctx, queryText, ns, 3, 0)
	if err != nil {
		var ve *store.ValidationError
		if !errors.As(err, &ve) { // invalid namespace is reported by Create below
			return nil, RememberOutput{}, s.toolError(ctx, req, "remember", err)
		}
	}
	if sf, ok := s.search.(interface {
		FindSimilar(ctx context.Context, text, namespace string, minScore float64, limit int) ([]store.Similar, error)
	}); ok && s.semantic {
		if sems, serr := sf.FindSimilar(ctx, queryText, ns, 0, 3); serr == nil {
			sims = mergeSimilar(sims, sems, 3)
		}
	}
	var links []store.LinkRef
	for _, id := range in.RelatedTo {
		links = append(links, store.LinkRef{TargetID: id, Kind: store.LinkRelatesTo})
	}
	for _, id := range in.DependsOn {
		links = append(links, store.LinkRef{TargetID: id, Kind: store.LinkDependsOn})
	}
	m, archived, err := s.store.CreateDetailed(ctx, store.NewMemory{
		Namespace: ns, Kind: store.Kind(in.Kind), Title: in.Title, Content: in.Content,
		Tags: in.Tags, Importance: in.Importance, Source: src, Supersedes: in.Supersedes,
		Links: links, ExpiresAt: exp,
	})
	if err != nil {
		return nil, RememberOutput{}, s.toolError(ctx, req, "remember", err)
	}
	out := RememberOutput{Memory: viewOf(m)}
	if len(archived) > 0 {
		out.Superseded = archived
	}
	superseded := map[int64]bool{}
	for _, id := range in.Supersedes {
		superseded[id] = true
	}
	for _, d := range sims {
		if !superseded[d.ID] {
			out.PossibleDuplicates = append(out.PossibleDuplicates, DuplicateView{
				ID: d.ID, Kind: string(d.Kind), Title: d.Title, Snippet: d.Snippet, Similarity: d.Similarity,
			})
		}
	}
	if len(out.PossibleDuplicates) > 0 {
		out.Hint = fmt.Sprintf("Memory #%d was created, but similar memories exist. If one says the same thing, "+
			"call forget on #%d; if an older one is now wrong, update_memory it or forget it.", m.ID, m.ID)
	}
	s.logCall(ctx, req, "remember", "id", m.ID, "namespace", m.Namespace, "kind", m.Kind)
	return nil, out, nil
}

// ---- recall -----------------------------------------------------------------

// RecallInput is the input of recall.
type RecallInput struct {
	Query           string   `json:"query" jsonschema:"Plain words to look for (no special syntax; accents and case are ignored, words match as prefixes). Empty returns the most recent memories."`
	Namespace       string   `json:"namespace,omitempty" jsonschema:"Restrict to one namespace. Omit to search all namespaces."`
	Kinds           []string `json:"kinds,omitempty" jsonschema:"Restrict to these kinds."`
	Tags            []string `json:"tags,omitempty" jsonschema:"Restrict to memories having all of these tags."`
	Limit           int      `json:"limit,omitempty" jsonschema:"Maximum number of results. Default 10, maximum 50."`
	IncludeArchived bool     `json:"include_archived,omitempty" jsonschema:"Also search forgotten (archived) memories."`
}

// ResultView is one recall hit.
type ResultView struct {
	ID        int64    `json:"id"`
	Kind      string   `json:"kind"`
	Namespace string   `json:"namespace"`
	Title     string   `json:"title,omitempty"`
	Snippet   string   `json:"snippet" jsonschema:"Excerpt with matched words in **bold**. Call get_memory for the full text."`
	Tags      []string `json:"tags,omitempty"`
	Source    string   `json:"source"`
	UpdatedAt string   `json:"updated_at"`
	Archived  bool     `json:"archived,omitempty"`
}

// RecallOutput is the output of recall.
type RecallOutput struct {
	Results []ResultView `json:"results"`
	Hint    string       `json:"hint,omitempty"`
}

func (s *srv) recall(ctx context.Context, req *mcp.CallToolRequest, in RecallInput) (*mcp.CallToolResult, RecallOutput, error) {
	res, err := s.search.Search(ctx, store.SearchQuery{
		Query: in.Query, Namespace: in.Namespace, Kinds: kinds(in.Kinds), Tags: in.Tags,
		IncludeArchived: in.IncludeArchived, Limit: in.Limit,
	})
	if err != nil {
		return nil, RecallOutput{}, s.toolError(ctx, req, "recall", err)
	}
	out := RecallOutput{Results: make([]ResultView, len(res))}
	for i, r := range res {
		v := ResultView{
			ID: r.ID, Kind: string(r.Kind), Namespace: r.Namespace, Title: r.Title, Snippet: r.Snippet,
			Tags: r.Tags, Source: r.Source, UpdatedAt: ts(r.UpdatedAt), Archived: r.Archived(),
		}
		if len(v.Tags) == 0 {
			v.Tags = nil
		}
		out.Results[i] = v
	}
	if len(res) == 0 {
		hint := "Nothing found. Try fewer or different words"
		if in.Namespace != "" {
			hint += ", or omit namespace to search everywhere"
		}
		out.Hint = hint + "."
	}
	s.logCall(ctx, req, "recall", "results", len(res))
	return nil, out, nil
}

// ---- get_memory -------------------------------------------------------------

// GetMemoryInput is the input of get_memory.
type GetMemoryInput struct {
	ID          int64 `json:"id" jsonschema:"Memory ID (the number after # in briefings and results)."`
	WithHistory bool  `json:"with_history,omitempty" jsonschema:"Also return every past version and who changed it."`
}

// HistoryView is one past version of a memory.
type HistoryView struct {
	Action    string `json:"action" jsonschema:"create, update, archive or restore."`
	ChangedBy string `json:"changed_by"`
	ChangedAt string `json:"changed_at"`
	Title     string `json:"title,omitempty"`
	Content   string `json:"content" jsonschema:"Content after this action."`
	Reason    string `json:"reason,omitempty"`
}

// LinkView is one directed link attached to a memory.
type LinkView struct {
	Direction  string `json:"direction" jsonschema:"outgoing (this -> target) or incoming (source -> this)."`
	Kind       string `json:"kind" jsonschema:"supersedes, relates_to or depends_on."`
	ID         int64  `json:"id"`
	MemoryKind string `json:"memory_kind"`
	Namespace  string `json:"namespace"`
	Title      string `json:"title,omitempty"`
	Archived   bool   `json:"archived,omitempty"`
}

// GetMemoryOutput is the output of get_memory.
type GetMemoryOutput struct {
	Memory  MemoryView    `json:"memory"`
	History []HistoryView `json:"history,omitempty" jsonschema:"Oldest first."`
	Links   []LinkView    `json:"links,omitempty" jsonschema:"Relationships with other memories (supersedes, relates_to, depends_on)."`
}

func (s *srv) getMemory(ctx context.Context, req *mcp.CallToolRequest, in GetMemoryInput) (*mcp.CallToolResult, GetMemoryOutput, error) {
	m, err := s.store.Get(ctx, in.ID)
	if err != nil {
		return nil, GetMemoryOutput{}, s.toolError(ctx, req, "get_memory", err)
	}
	out := GetMemoryOutput{Memory: viewOf(m)}
	ls, err := s.store.Links(ctx, in.ID)
	if err != nil {
		return nil, GetMemoryOutput{}, s.toolError(ctx, req, "get_memory", err)
	}
	for _, l := range ls {
		out.Links = append(out.Links, LinkView{
			Direction:  l.Direction,
			Kind:       string(l.Kind),
			ID:         l.ID,
			MemoryKind: string(l.MemoryKind),
			Namespace:  l.Namespace,
			Title:      l.Title,
			Archived:   l.Archived,
		})
	}
	if in.WithHistory {
		hs, err := s.store.History(ctx, in.ID)
		if err != nil {
			return nil, GetMemoryOutput{}, s.toolError(ctx, req, "get_memory", err)
		}
		for _, h := range hs {
			snap, reason, err := h.Decode()
			if err != nil {
				return nil, GetMemoryOutput{}, s.toolError(ctx, req, "get_memory", err)
			}
			out.History = append(out.History, HistoryView{
				Action: string(h.Action), ChangedBy: h.ChangedBy, ChangedAt: ts(h.ChangedAt),
				Title: snap.Title, Content: snap.Content, Reason: reason,
			})
		}
	}
	s.logCall(ctx, req, "get_memory", "id", in.ID)
	return nil, out, nil
}

// ---- update_memory ----------------------------------------------------------

// LinkInput identifies a directed link to add or remove on a memory.
type LinkInput struct {
	TargetID int64  `json:"target_id" jsonschema:"Target memory ID."`
	Kind     string `json:"kind" jsonschema:"Relationship kind: relates_to, depends_on or supersedes."`
}

// UpdateMemoryInput is the input of update_memory. Omitted fields are unchanged.
type UpdateMemoryInput struct {
	ID           int64       `json:"id" jsonschema:"Memory ID to change."`
	Content      *string     `json:"content,omitempty" jsonschema:"New full content (replaces the old one; the old version stays in history)."`
	Title        *string     `json:"title,omitempty" jsonschema:"New one-line title; empty string removes it."`
	Tags         *[]string   `json:"tags,omitempty" jsonschema:"New complete tag list (replaces the old one)."`
	Importance   *int        `json:"importance,omitempty" jsonschema:"1 = low, 2 = normal, 3 = always shown by get_context."`
	Kind         *string     `json:"kind,omitempty" jsonschema:"New kind."`
	Namespace    *string     `json:"namespace,omitempty" jsonschema:"Move to this namespace (created if new)."`
	ExpiresIn    string      `json:"expires_in,omitempty" jsonschema:"New TTL duration (e.g. 24h, 7d)."`
	ExpiresAt    string      `json:"expires_at,omitempty" jsonschema:"New RFC 3339 expiration timestamp."`
	ClearExpires bool        `json:"clear_expires,omitempty" jsonschema:"Remove any expiration timestamp."`
	AddLinks     []LinkInput `json:"add_links,omitempty" jsonschema:"Links to add from this memory to other memories."`
	RemoveLinks  []LinkInput `json:"remove_links,omitempty" jsonschema:"Links to remove from this memory."`
}

// UpdateMemoryOutput is the output of update_memory.
type UpdateMemoryOutput struct {
	Memory  MemoryView `json:"memory"`
	Changed []string   `json:"changed" jsonschema:"Fields that were provided."`
}

func (s *srv) updateMemory(ctx context.Context, req *mcp.CallToolRequest, in UpdateMemoryInput) (*mcp.CallToolResult, UpdateMemoryOutput, error) {
	p := store.Patch{Content: in.Content, Title: in.Title, Tags: in.Tags, Importance: in.Importance, Namespace: in.Namespace, ClearExpiresAt: in.ClearExpires}
	exp, err := parseExpiration(in.ExpiresIn, in.ExpiresAt)
	if err != nil {
		return nil, UpdateMemoryOutput{}, s.toolError(ctx, req, "update_memory", err)
	}
	if exp != nil {
		p.ExpiresAt = exp
	}
	changed := []string{}
	for name, set := range map[string]bool{
		"content": in.Content != nil, "title": in.Title != nil, "tags": in.Tags != nil,
		"importance": in.Importance != nil, "kind": in.Kind != nil, "namespace": in.Namespace != nil,
		"expires_at": exp != nil || in.ClearExpires,
		"links":      len(in.AddLinks) > 0 || len(in.RemoveLinks) > 0,
	} {
		if set {
			changed = append(changed, name)
		}
	}
	slices.Sort(changed)
	if in.Kind != nil {
		k := store.Kind(*in.Kind)
		p.Kind = &k
	}
	hasFieldPatch := in.Content != nil || in.Title != nil || in.Tags != nil ||
		in.Importance != nil || in.Kind != nil || in.Namespace != nil ||
		exp != nil || in.ClearExpires

	var m store.Memory
	if hasFieldPatch || (len(in.AddLinks) == 0 && len(in.RemoveLinks) == 0) {
		m, err = s.store.Update(ctx, in.ID, p, s.source(req))
		if err != nil {
			return nil, UpdateMemoryOutput{}, s.toolError(ctx, req, "update_memory", err)
		}
	} else {
		m, err = s.store.Get(ctx, in.ID)
		if err != nil {
			return nil, UpdateMemoryOutput{}, s.toolError(ctx, req, "update_memory", err)
		}
		if m.Archived() {
			return nil, UpdateMemoryOutput{}, s.toolError(ctx, req, "update_memory", &store.StateError{ID: in.ID, Msg: "is archived; restore it first (llm-memory restore) or create a new memory"})
		}
	}
	for _, l := range in.AddLinks {
		if err := s.store.AddLink(ctx, in.ID, l.TargetID, store.LinkKind(l.Kind)); err != nil {
			return nil, UpdateMemoryOutput{}, s.toolError(ctx, req, "update_memory", err)
		}
	}
	for _, l := range in.RemoveLinks {
		if err := s.store.RemoveLink(ctx, in.ID, l.TargetID, store.LinkKind(l.Kind)); err != nil {
			return nil, UpdateMemoryOutput{}, s.toolError(ctx, req, "update_memory", err)
		}
	}
	s.logCall(ctx, req, "update_memory", "id", in.ID, "changed", changed)
	return nil, UpdateMemoryOutput{Memory: viewOf(m), Changed: changed}, nil
}

// ---- forget -----------------------------------------------------------------

// ForgetInput is the input of forget.
type ForgetInput struct {
	ID     int64  `json:"id" jsonschema:"Memory ID to archive."`
	Reason string `json:"reason,omitempty" jsonschema:"Why it is forgotten (wrong, obsolete, duplicate of #N…). Kept in history."`
}

// ForgetOutput is the output of forget.
type ForgetOutput struct {
	ID       int64  `json:"id"`
	Archived bool   `json:"archived"`
	Message  string `json:"message"`
}

func (s *srv) forget(ctx context.Context, req *mcp.CallToolRequest, in ForgetInput) (*mcp.CallToolResult, ForgetOutput, error) {
	if _, err := s.store.Archive(ctx, in.ID, s.source(req), in.Reason); err != nil {
		return nil, ForgetOutput{}, s.toolError(ctx, req, "forget", err)
	}
	s.logCall(ctx, req, "forget", "id", in.ID)
	return nil, ForgetOutput{
		ID: in.ID, Archived: true,
		Message: fmt.Sprintf("Memory #%d archived: hidden from get_context and recall. A human can restore it with `llm-memory restore %d`.", in.ID, in.ID),
	}, nil
}

// ---- handoff ----------------------------------------------------------------

// HandoffInput is the input of handoff.
type HandoffInput struct {
	Summary              string   `json:"summary" jsonschema:"Where the work stands: what was done, what was decided, current state. Written for another assistant with no access to this conversation."`
	NextSteps            []string `json:"next_steps,omitempty" jsonschema:"Concrete next actions, in order."`
	OpenQuestions        []string `json:"open_questions,omitempty" jsonschema:"Questions still waiting for the user or unresolved."`
	Namespace            string   `json:"namespace,omitempty" jsonschema:"Project namespace. Omit for the server default."`
	NamespaceDescription string   `json:"namespace_description,omitempty" jsonschema:"Optional one-line description of the project namespace."`
	Supersedes           []int64  `json:"supersedes,omitempty" jsonschema:"IDs of completed task_state or obsolete memories to archive along with this handoff."`
}

// HandoffOutput is the output of handoff.
type HandoffOutput struct {
	Memory   MemoryView `json:"memory"`
	Replaced []int64    `json:"replaced,omitempty" jsonschema:"Previous handoff(s) of the namespace and superseded memories, now archived."`
}

const maxHandoffTitle = 80

// FormatHandoff builds the title and Markdown body of a handoff memory.
func FormatHandoff(summary string, next, questions []string) (string, string) {
	return handoffTitle(summary), handoffContent(summary, next, questions)
}

func (s *srv) handoff(ctx context.Context, req *mcp.CallToolRequest, in HandoffInput) (*mcp.CallToolResult, HandoffOutput, error) {
	summary := strings.TrimSpace(in.Summary)
	if summary == "" {
		return nil, HandoffOutput{}, errors.New("summary must not be empty: describe where the work stands")
	}
	title, content := FormatHandoff(summary, in.NextSteps, in.OpenQuestions)
	m, archived, err := s.store.CreateDetailed(ctx, store.NewMemory{
		Namespace:            s.namespace(in.Namespace),
		NamespaceDescription: in.NamespaceDescription,
		Kind:                 store.KindHandoff,
		Title:                title,
		Content:              content,
		Importance:           store.DefaultImportance,
		Source:               s.source(req),
		Supersedes:           in.Supersedes,
	})
	if err != nil {
		return nil, HandoffOutput{}, s.toolError(ctx, req, "handoff", err)
	}
	out := HandoffOutput{Memory: viewOf(m)}
	if len(archived) > 0 {
		out.Replaced = archived
	}
	s.logCall(ctx, req, "handoff", "id", m.ID, "namespace", m.Namespace)
	return nil, out, nil
}

func handoffTitle(summary string) string {
	first, _, _ := strings.Cut(summary, "\n")
	first = strings.TrimLeft(strings.TrimSpace(first), "#-* ")
	if utf8.RuneCountInString(first) > maxHandoffTitle {
		first = string([]rune(first)[:maxHandoffTitle-1]) + "…"
	}
	return "Handoff: " + first
}

func handoffContent(summary string, next, questions []string) string {
	var b strings.Builder
	b.WriteString("## Summary\n")
	b.WriteString(summary)
	b.WriteString("\n")
	list := func(title string, items []string) {
		var kept []string
		for _, it := range items {
			if it = strings.TrimSpace(it); it != "" {
				kept = append(kept, it)
			}
		}
		if len(kept) == 0 {
			return
		}
		b.WriteString("\n## " + title + "\n")
		for _, it := range kept {
			b.WriteString("- " + it + "\n")
		}
	}
	list("Next steps", next)
	list("Open questions", questions)
	return strings.TrimRight(b.String(), "\n")
}

// ---- list_namespaces --------------------------------------------------------

// ListNamespacesInput is the (empty) input of list_namespaces.
type ListNamespacesInput struct{}

// NamespaceView describes one namespace.
type NamespaceView struct {
	Name         string `json:"name"`
	Description  string `json:"description,omitempty"`
	ActiveCount  int    `json:"active_count" jsonschema:"Number of active (not archived) memories."`
	LastActivity string `json:"last_activity,omitempty"`
}

// ListNamespacesOutput is the output of list_namespaces.
type ListNamespacesOutput struct {
	Default    string          `json:"default" jsonschema:"Namespace used when a tool call omits it."`
	Namespaces []NamespaceView `json:"namespaces"`
}

func (s *srv) listNamespaces(ctx context.Context, req *mcp.CallToolRequest, _ ListNamespacesInput) (*mcp.CallToolResult, ListNamespacesOutput, error) {
	ns, err := s.store.ListNamespaces(ctx)
	if err != nil {
		return nil, ListNamespacesOutput{}, s.toolError(ctx, req, "list_namespaces", err)
	}
	out := ListNamespacesOutput{Default: s.defaultNS, Namespaces: make([]NamespaceView, len(ns))}
	for i, n := range ns {
		v := NamespaceView{Name: n.Name, Description: n.Description, ActiveCount: n.ActiveCount}
		if n.LastActivity != nil {
			v.LastActivity = ts(*n.LastActivity)
		}
		out.Namespaces[i] = v
	}
	s.logCall(ctx, req, "list_namespaces")
	return nil, out, nil
}

// ---- Helpers ----------------------------------------------------------------

func kinds(in []string) []store.Kind {
	out := make([]store.Kind, len(in))
	for i, k := range in {
		out[i] = store.Kind(k)
	}
	return out
}

// toolError turns a store error into an actionable message for the LLM.
// Returned as a plain error, the SDK reports it as a tool result with
// IsError set, not as a protocol error.
func (s *srv) toolError(ctx context.Context, req *mcp.CallToolRequest, tool string, err error) error {
	var (
		nf  *store.NotFoundError
		nsE *store.NamespaceNotFoundError
		ve  *store.ValidationError
		se  *store.StateError
	)
	switch {
	case errors.As(err, &nf):
		return fmt.Errorf("memory #%d does not exist. Use recall to find the right id", nf.ID)
	case errors.As(err, &nsE):
		return fmt.Errorf("namespace %q does not exist. Existing namespaces: %s. Call list_namespaces, or omit namespace",
			nsE.Name, strings.Join(nsE.Existing, ", "))
	case errors.As(err, &ve):
		return fmt.Errorf("invalid %s: %s", ve.Field, ve.Msg)
	case errors.As(err, &se):
		return se
	default:
		s.log.ErrorContext(ctx, "tool failed", "tool", tool, "source", s.source(req), "err", err)
		return fmt.Errorf("internal error in %s: %v. Retry once; if it fails again, tell the user the memory store is unavailable", tool, err) //nolint:errorlint // message for the LLM; the cause is logged, not unwrapped
	}
}

func (s *srv) logCall(ctx context.Context, req *mcp.CallToolRequest, tool string, attrs ...any) {
	s.log.DebugContext(ctx, "tool call", append([]any{"tool", tool, "source", s.source(req)}, attrs...)...)
}

// ---- Schemas ----------------------------------------------------------------

// inputSchema infers the schema of T, then lets patch add enums and bounds
// that struct tags cannot express.
func inputSchema[T any](patch func(props map[string]*jsonschema.Schema)) (*jsonschema.Schema, error) {
	sch, err := jsonschema.For[T](nil)
	if err != nil {
		return nil, fmt.Errorf("schema for %T: %w", *new(T), err)
	}
	if patch != nil {
		patch(sch.Properties)
	}
	return sch, nil
}

func enumOf(kinds []store.Kind) []any {
	out := make([]any, len(kinds))
	for i, k := range kinds {
		out[i] = string(k)
	}
	return out
}

func bounds(s *jsonschema.Schema, lo, hi float64) {
	s.Minimum, s.Maximum = &lo, &hi
}

// writableKinds excludes handoff: handoffs go through the handoff tool, which
// keeps a single active handoff per namespace.
func writableKinds() []store.Kind {
	var out []store.Kind
	for _, k := range store.Kinds {
		if k != store.KindHandoff {
			out = append(out, k)
		}
	}
	return out
}

// parseExpiration parses either expiresIn (e.g. "24h", "7d") or expiresAt (RFC 3339).
func parseExpiration(expiresIn, expiresAt string) (*time.Time, error) {
	in := strings.TrimSpace(expiresIn)
	at := strings.TrimSpace(expiresAt)
	if in != "" && at != "" {
		return nil, &store.ValidationError{Field: "expires", Msg: "provide either expires_in or expires_at, not both"}
	}
	if in != "" {
		if strings.HasSuffix(in, "d") {
			days, err := strconv.Atoi(strings.TrimSuffix(in, "d"))
			if err != nil || days <= 0 {
				return nil, &store.ValidationError{Field: "expires_in", Msg: fmt.Sprintf("invalid duration %q", expiresIn)}
			}
			t := time.Now().UTC().Add(time.Duration(days) * 24 * time.Hour)
			return &t, nil
		}
		d, err := time.ParseDuration(in)
		if err != nil || d <= 0 {
			return nil, &store.ValidationError{Field: "expires_in", Msg: fmt.Sprintf("invalid duration %q", expiresIn)}
		}
		t := time.Now().UTC().Add(d)
		return &t, nil
	}
	if at != "" {
		t, err := time.Parse(time.RFC3339, at)
		if err != nil {
			return nil, &store.ValidationError{Field: "expires_at", Msg: fmt.Sprintf("invalid RFC 3339 timestamp %q", expiresAt)}
		}
		utc := t.UTC()
		return &utc, nil
	}
	return nil, nil
}

// mergeSimilar combines FTS and semantic duplicate candidates, keeping the
// highest similarity score per memory ID, up to limit.
func mergeSimilar(fts, sem []store.Similar, limit int) []store.Similar {
	if len(sem) == 0 {
		return fts
	}
	byID := make(map[int64]store.Similar, len(fts)+len(sem))
	for _, s := range fts {
		byID[s.ID] = s
	}
	for _, s := range sem {
		if cur, ok := byID[s.ID]; !ok || s.Similarity > cur.Similarity {
			byID[s.ID] = s
		}
	}
	out := make([]store.Similar, 0, len(byID))
	for _, s := range byID {
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b store.Similar) int {
		if a.Similarity > b.Similarity {
			return -1
		}
		if a.Similarity < b.Similarity {
			return 1
		}
		if a.ID > b.ID {
			return -1
		}
		if a.ID < b.ID {
			return 1
		}
		return 0
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}
