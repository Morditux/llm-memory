# Instruction block for each client

Without these instructions, LLMs do not call the memory reliably.

Where to put it:

| Client | Location |
| --- | --- |
| Antigravity | `~/.gemini/GEMINI.md` (global) or `GEMINI.md` / `AGENTS.md` in the project |
| Claude Code | `~/.claude/CLAUDE.md` (global) or the project's `CLAUDE.md` |
| Claude Desktop | Custom instructions, or project instructions |
| VS Code / Copilot | `.github/copilot-instructions.md` |
| Internal LLM | System prompt |

```markdown
## Shared memory (llm-memory MCP server)

You share a persistent memory with other AI assistants.

- At the start of every task, call `get_context` with the project namespace (call `list_namespaces` first if unsure, or use `global`) before answering.
- Before asking me for context, search for it with `recall`.
- Immediately store with `remember`: decisions made and their reason, stable project facts (project namespace), and my lasting cross-project preferences (`namespace: "global"`). Do not store transient details or the content of files already in the repository.
- If a memory becomes wrong or obsolete, fix it with `update_memory`, replace it (`remember` with `supersedes`), or archive it with `forget`.
- When I say I am switching tools or stopping, or at the end of a long task, call `handoff` with the state, next steps and open questions.
- Never store secrets (passwords, API keys, tokens).
```
