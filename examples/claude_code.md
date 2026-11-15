# Claude Code

User scope (available in every project):

```bash
claude mcp add --scope user llm-memory -- /absolute/path/llm-memory serve --source claude-code
```

Project scope, with a default namespace (`.mcp.json` file committed at the repository root):

```bash
claude mcp add --scope project llm-memory -- /absolute/path/llm-memory serve --namespace my-project --source claude-code
```

Check: `claude mcp list`, then `/mcp` inside a session.

Optional, recommended: inject the briefing automatically at session start (SessionStart hook in
`~/.claude/settings.json`):

```bash
/absolute/path/llm-memory setup --hooks --write claude-code
```

Then add the block from [instructions_client.md](instructions_client.md) to `~/.claude/CLAUDE.md`.
