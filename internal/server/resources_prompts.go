package server

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Morditux/llm-memory/internal/briefing"
	"github.com/Morditux/llm-memory/internal/store"
)

const (
	contextResourceURI         = "memory://context"
	contextResourceURITemplate = "memory://context/{namespace}"
)

func (s *srv) registerResourcesAndPrompts(server *mcp.Server) {
	server.AddResource(&mcp.Resource{
		URI:         contextResourceURI,
		Name:        "context",
		Title:       "Session briefing (default namespace)",
		Description: "Markdown briefing for the default namespace.",
		MIMEType:    "text/markdown",
	}, s.readContextResource)

	server.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: contextResourceURITemplate,
		Name:        "namespace_context",
		Title:       "Session briefing by namespace",
		Description: "Markdown briefing for a project namespace.",
		MIMEType:    "text/markdown",
	}, s.readContextResource)

	server.AddPrompt(&mcp.Prompt{
		Name:        "briefing",
		Title:       "Session briefing",
		Description: "Load the shared memory briefing for a project namespace.",
		Arguments: []*mcp.PromptArgument{
			{
				Name:        "namespace",
				Description: "Project namespace (omit for the server default).",
			},
		},
	}, s.promptBriefing)

	server.AddPrompt(&mcp.Prompt{
		Name:        "handoff",
		Title:       "Session handoff",
		Description: "Summarise current progress, next steps and open questions into shared memory.",
		Arguments: []*mcp.PromptArgument{
			{
				Name:        "namespace",
				Description: "Project namespace (omit for the server default).",
			},
		},
	}, s.promptHandoff)
}

func (s *srv) readContextResource(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	uri := ""
	if req != nil && req.Params != nil {
		uri = req.Params.URI
	}
	var ns string
	switch {
	case uri == contextResourceURI:
		ns = s.defaultNS
	case strings.HasPrefix(uri, contextResourceURI+"/"):
		raw, err := url.PathUnescape(strings.TrimPrefix(uri, contextResourceURI+"/"))
		if err != nil {
			return nil, mcp.ResourceNotFoundError(uri)
		}
		norm, err := store.NormalizeNamespace(raw)
		if err != nil {
			return nil, mcp.ResourceNotFoundError(uri)
		}
		ns = norm
	default:
		return nil, mcp.ResourceNotFoundError(uri)
	}
	text, err := briefing.Build(ctx, s.store, briefing.Options{Namespace: ns})
	if err != nil {
		return nil, err
	}
	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{
			{
				URI:      uri,
				MIMEType: "text/markdown",
				Text:     text,
			},
		},
	}, nil
}

func (s *srv) promptBriefing(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	var rawNS string
	if req != nil && req.Params != nil {
		rawNS = req.Params.Arguments["namespace"]
	}
	ns := s.namespace(rawNS)
	text, err := briefing.Build(ctx, s.store, briefing.Options{Namespace: ns})
	if err != nil {
		return nil, err
	}
	normNS, _ := store.NormalizeNamespace(ns)
	return &mcp.GetPromptResult{
		Description: fmt.Sprintf("Shared memory briefing for namespace %q.", normNS),
		Messages: []*mcp.PromptMessage{
			{
				Role: mcp.Role("user"),
				Content: &mcp.TextContent{
					Text: text + "\n\nUse this shared memory context for the task. " +
						"Store durable decisions or facts with `remember`, fix wrong memories with `update_memory`, " +
						"and leave a `handoff` before ending or switching tools.",
				},
			},
		},
	}, nil
}

func (s *srv) promptHandoff(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	var rawNS string
	if req != nil && req.Params != nil {
		rawNS = req.Params.Arguments["namespace"]
	}
	ns, err := store.NormalizeNamespace(s.namespace(rawNS))
	if err != nil {
		return nil, err
	}
	return &mcp.GetPromptResult{
		Description: fmt.Sprintf("Prompt to write a session handoff for namespace %q.", ns),
		Messages: []*mcp.PromptMessage{
			{
				Role: mcp.Role("user"),
				Content: &mcp.TextContent{
					Text: fmt.Sprintf("Call the `handoff` tool for namespace %q with a concise `summary` of current progress, "+
						"concrete `next_steps`, and any `open_questions`. If any `task_state` memories in %q are now completed, "+
						"pass their IDs in `supersedes` to archive them.", ns, ns),
				},
			},
		},
	}, nil
}
