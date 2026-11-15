// Package server exposes the memory store as an MCP server.
// It contains no SQL: every handler delegates to internal/store.
package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Morditux/llm-memory/internal/briefing"
	"github.com/Morditux/llm-memory/internal/store"
)

// Name is the MCP implementation name advertised to clients.
const Name = "llm-memory"

// Instructions summarises the expected usage for the connected LLM.
const Instructions = `Persistent memory shared with other AI assistants (Claude Desktop, Claude Code, VS Code…). What you store here is read by the next assistant the user works with.
- At the start of every task, call get_context (with the project namespace if known) before answering.
- Before asking the user for context, search it with recall.
- Store with remember: decisions and their reason, lasting user preferences, stable facts about the project. Not transient details, not file contents already in the repository.
- When a memory becomes wrong, fix it with update_memory or replace it (remember with supersedes).
- When the user stops or switches tools, or at the end of a long task, call handoff with the state, next steps and open questions.
- Never store secrets (passwords, API keys, tokens).`

// Options configures the server.
type Options struct {
	Version          string
	DefaultNamespace string       // used when a tool call omits namespace; default "global"
	Source           string       // forced author name; empty = MCP clientInfo.name
	Logger           *slog.Logger // nil = discard
	// Searcher replaces the full-text engine of recall (semantic search,
	// phase 6). nil = store.Search, the v1 behaviour.
	Searcher Searcher
}

// Searcher runs a recall query.
type Searcher interface {
	Search(ctx context.Context, q store.SearchQuery) ([]store.SearchResult, error)
}

type srv struct {
	store     *store.Store
	search    Searcher
	semantic  bool
	defaultNS string
	source0   string
	log       *slog.Logger
}

// New builds the MCP server with its eight tools.
func New(st *store.Store, opts Options) (*mcp.Server, error) {
	s := &srv{store: st, search: opts.Searcher, semantic: opts.Searcher != nil,
		defaultNS: opts.DefaultNamespace, source0: strings.TrimSpace(opts.Source), log: opts.Logger}
	if s.search == nil {
		s.search = st
	}
	if s.defaultNS == "" {
		s.defaultNS = store.DefaultNamespace
	}
	ns, err := store.NormalizeNamespace(s.defaultNS)
	if err != nil {
		return nil, fmt.Errorf("default namespace: %w", err)
	}
	s.defaultNS = ns
	if s.log == nil {
		s.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.Version == "" {
		opts.Version = "dev"
	}

	server := mcp.NewServer(
		&mcp.Implementation{Name: Name, Version: opts.Version},
		&mcp.ServerOptions{Instructions: Instructions},
	)
	if err := s.register(server); err != nil {
		return nil, err
	}
	return server, nil
}

func ptr[T any](v T) *T { return &v }

func readOnly(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true, OpenWorldHint: ptr(false)}
}

func writes(title string, destructive bool) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, DestructiveHint: ptr(destructive), OpenWorldHint: ptr(false)}
}

func (s *srv) register(server *mcp.Server) error {
	maxChars := float64(briefing.MinMaxChars)
	getContextSchema, err := inputSchema[GetContextInput](func(p map[string]*jsonschema.Schema) {
		bounds(p["max_chars"], maxChars, 100000)
	})
	if err != nil {
		return err
	}
	rememberSchema, err := inputSchema[RememberInput](func(p map[string]*jsonschema.Schema) {
		p["kind"].Enum = enumOf(writableKinds())
		bounds(p["importance"], 1, 3)
		p["content"].MinLength, p["content"].MaxLength = ptr(1), ptr(store.MaxContentRunes)
	})
	if err != nil {
		return err
	}
	recallSchema, err := inputSchema[RecallInput](func(p map[string]*jsonschema.Schema) {
		p["kinds"].Items.Enum = enumOf(store.Kinds)
		bounds(p["limit"], 1, store.MaxSearchLimit)
	})
	if err != nil {
		return err
	}
	getMemorySchema, err := inputSchema[GetMemoryInput](func(p map[string]*jsonschema.Schema) {
		p["id"].Minimum = ptr(1.0)
	})
	if err != nil {
		return err
	}
	updateSchema, err := inputSchema[UpdateMemoryInput](func(p map[string]*jsonschema.Schema) {
		p["id"].Minimum = ptr(1.0)
		p["kind"].Enum = append(enumOf(writableKinds()), nil) // nullable pointer field
		bounds(p["importance"], 1, 3)
	})
	if err != nil {
		return err
	}
	forgetSchema, err := inputSchema[ForgetInput](func(p map[string]*jsonschema.Schema) {
		p["id"].Minimum = ptr(1.0)
	})
	if err != nil {
		return err
	}

	mcp.AddTool(server, &mcp.Tool{
		Name: "get_context",
		Description: "Briefing to call FIRST at the start of every task or session: latest handoff from the previous assistant, " +
			"essential memories, ongoing tasks, preferences, recent decisions and facts of a namespace (Markdown). " +
			"Each item shows its #id for get_memory, update_memory or forget.",
		InputSchema: getContextSchema,
		Annotations: readOnly("Session briefing"),
	}, s.getContext)

	mcp.AddTool(server, &mcp.Tool{
		Name: "remember",
		Description: "Store one durable memory shared with other assistants: a decision and its reason, a lasting user preference, " +
			"or a stable project fact. One idea per call. Returns possible_duplicates: if one already says the same thing, " +
			"forget the new memory or update the old one. Never store secrets.",
		InputSchema: rememberSchema,
		Annotations: writes("Remember", false),
	}, s.remember)

	recallDesc := "Full-text search in the shared memory. Use it before asking the user for context they may have given before. " +
		"Returns ranked excerpts; call get_memory for the full text."
	if s.semantic {
		recallDesc += " Semantic search is enabled: results also match by meaning, not only by words."
	}
	mcp.AddTool(server, &mcp.Tool{
		Name:        "recall",
		Description: recallDesc,
		InputSchema: recallSchema,
		Annotations: readOnly("Search memory"),
	}, s.recall)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_memory",
		Description: "Full content of one memory by id, optionally with its change history.",
		InputSchema: getMemorySchema,
		Annotations: readOnly("Read memory"),
	}, s.getMemory)

	mcp.AddTool(server, &mcp.Tool{
		Name: "update_memory",
		Description: "Correct an active memory: only the fields you provide change; the previous version stays in history. " +
			"To replace a memory by a different statement, prefer remember with supersedes.",
		InputSchema: updateSchema,
		Annotations: writes("Update memory", true),
	}, s.updateMemory)

	mcp.AddTool(server, &mcp.Tool{
		Name: "forget",
		Description: "Archive a memory that is wrong, obsolete or duplicated. It disappears from get_context and recall " +
			"but is kept and can be restored by the user.",
		InputSchema: forgetSchema,
		Annotations: writes("Forget memory", false),
	}, s.forget)

	mcp.AddTool(server, &mcp.Tool{
		Name: "handoff",
		Description: "Leave the work state for the next assistant: call it when the user stops, switches tools, or at the end of a long task. " +
			"Replaces the previous handoff of the namespace; get_context shows it first.",
		Annotations: writes("Hand off", false),
	}, s.handoff)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_namespaces",
		Description: "List namespaces (projects) with their number of active memories and last activity.",
		Annotations: readOnly("List namespaces"),
	}, s.listNamespaces)

	s.registerResourcesAndPrompts(server)
	return nil
}

// namespace returns the namespace of a call, or the server default.
func (s *srv) namespace(in string) string {
	if strings.TrimSpace(in) == "" {
		return s.defaultNS
	}
	return in
}

// source identifies the author: --source, then clientInfo.name, then "unknown".
// The LLM never provides it.
func (s *srv) source(req *mcp.CallToolRequest) string {
	if s.source0 != "" {
		return s.source0
	}
	if req != nil && req.Session != nil {
		if p := req.Session.InitializeParams(); p != nil && p.ClientInfo != nil {
			if n := strings.TrimSpace(p.ClientInfo.Name); n != "" {
				return n
			}
		}
	}
	return store.UnknownSource
}
