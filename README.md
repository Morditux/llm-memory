<p align="center">
  <img src="assets/logo.jpg" alt="llm-memory logo" width="160" />
</p>

# llm-memory

[![CI](https://github.com/Morditux/llm-memory/actions/workflows/ci.yml/badge.svg)](https://github.com/Morditux/llm-memory/actions/workflows/ci.yml)

A local MCP server that stores a **memory shared by several LLMs** in a SQLite file
(Claude Desktop, Claude Code, VS Code, an internal LLM…). When you switch from one client to
another, the new one retrieves the context in a single call: decisions, preferences, facts,
task state and the handoff left by the previous assistant.

- A single binary with no system dependency (pure Go, embedded SQLite, FTS5 included).
- Local full-text search; optional semantic search through an embeddings API.
- stdio transport: each client launches its own instance, all pointing to the same file.
- Sequential use (one LLM at a time); two clients open at the same time do not block each other.
- LLMs never delete anything: `forget` archives, and the history of every memory is kept.

## Installation

Prebuilt binaries are published on [GitHub Releases](https://github.com/Morditux/llm-memory/releases)
for linux/amd64, linux/arm64, darwin/arm64 and windows/amd64 (or built locally with `make release`).

With Go ≥ 1.26:

```bash
go install github.com/Morditux/llm-memory/cmd/llm-memory@latest
```

Or from a local clone:

```bash
make build                  # → bin/llm-memory
./bin/llm-memory version    # llm-memory <version> (schema 2, …)
```

Copy the binary wherever you like (for example `~/.local/bin/llm-memory`) and note its **absolute path**:
MCP clients do not go through a shell and do not necessarily have the same `PATH`.

## Client configuration

All clients launch the same binary with the same database file (by default
`~/.llm-memory/memory.db`, created on first launch, directory with mode 0700, file with mode 0600).

| Client | Example |
| --- | --- |
| Antigravity | [examples/antigravity_mcp_config.json](examples/antigravity_mcp_config.json) (`~/.gemini/config/mcp_config.json`) |
| Claude Desktop | [examples/claude_desktop_config.json](examples/claude_desktop_config.json) |
| Claude Code | [examples/claude_code.md](examples/claude_code.md) |
| VS Code | [examples/vscode_mcp.json](examples/vscode_mcp.json) (project default namespace) |
| Internal LLM | same principle if its client supports MCP over stdio; otherwise `llm-memory context` and `llm-memory search` to paste the context by hand |

For **Antigravity**, add the `llm-memory` entry to `~/.gemini/config/mcp_config.json` (or inspect active servers in the UI via **Additional Options (...) > MCP Servers**), and add the instruction block to `~/.gemini/GEMINI.md` (global) or `GEMINI.md` / `AGENTS.md` in your project.

**Required**: add the instruction block from
[examples/instructions_client.md](examples/instructions_client.md) to each client. Without it, LLMs
do not call the memory reliably.

On Windows, escape backslashes in JSON (`"C:\\tools\\llm-memory.exe"`).

### Parameters

Priority: flag > environment variable > default.

| Flag | Variable | Default | Role |
| --- | --- | --- | --- |
| `--db` | `LLM_MEMORY_DB` | `~/.llm-memory/memory.db` | Database file (`~` expanded by the program) |
| `--namespace` | `LLM_MEMORY_NAMESPACE` | `global` | Namespace used when a tool call does not specify one |
| `--source` | `LLM_MEMORY_SOURCE` | `clientInfo.name` of the MCP client | Author recorded for writes |
| `--log-level` | `LLM_MEMORY_LOG_LEVEL` | `info` | Logs on stderr (`debug`, `info`, `warn`, `error`) |

## MCP tools

| Tool | Role |
| --- | --- |
| `get_context` | Start-of-session Markdown briefing: latest handoff, essential memories, tasks, preferences, recent decisions and facts |
| `remember` | Creates a memory (`preference`, `fact`, `decision`, `task_state`, `note`); reports probable duplicates; `supersedes` archives the replaced memories |
| `recall` | Full-text search (accents and case ignored, words matched as prefixes), namespace / kinds filters |
| `get_memory` | Full content, history optional |
| `update_memory` | Modifies the given fields; the old version stays in history |
| `forget` | Archives (reversible with `llm-memory restore`) |
| `handoff` | Handoff for the next LLM (summary, next steps, open questions); one active per namespace |
| `list_namespaces` | Namespaces, number of active memories, last activity |

A namespace usually maps to a project; `global` holds what applies everywhere
(the user's preferences). A namespace is created by the first `remember` or `handoff` in it.

## Administration (CLI)

Flags may be placed before or after arguments. Writes made from the CLI are
recorded with the author `cli`.

```bash
llm-memory list [--namespace X] [--kind decision,fact] [--archived] [--limit N]
llm-memory show 42                       # full content and history
llm-memory search postgres jsonb         # same engine as recall
llm-memory context --namespace demo      # exactly what get_context would return
llm-memory restore 42                    # un-archives
llm-memory purge --archived-before 2026-01-01 [--yes]   # permanent deletion
llm-memory export [--namespace X] [--history] [-o dump.json]
llm-memory import [--dry-run] dump.json  # or - for stdin; exact duplicates are skipped
llm-memory stats                         # per namespace, kind, source; file size
llm-memory reindex [--all]               # embeddings (semantic search only)
```

## Semantic search (optional)

By default, `recall` is a full-text search (FTS5) and **no data leaves the machine**.
With an embeddings API configured, `recall` and `llm-memory search` also find memories
by meaning ("SGBD" finds "PostgreSQL rather than MySQL"): the full-text and vector rankings
are fused (Reciprocal Rank Fusion).

The API must follow the OpenAI `POST /v1/embeddings` format (OpenAI, Ollama, LM Studio, vLLM, most
internal gateways). Local example with Ollama:

```bash
ollama pull nomic-embed-text
llm-memory reindex --embed-url http://localhost:11434/v1/embeddings --embed-model nomic-embed-text
```

then add the same two flags to the `serve` `args` in each client's configuration.

| Flag | Variable | Default |
| --- | --- | --- |
| `--embed-url` | `LLM_MEMORY_EMBED_URL` | empty = disabled |
| `--embed-model` | `LLM_MEMORY_EMBED_MODEL` | — (required with the URL) |
| — | `LLM_MEMORY_EMBED_API_KEY` | none (sent as `Authorization: Bearer`); never a flag |
| `--embed-min-score` | `LLM_MEMORY_EMBED_MIN_SCORE` | `0.3` (minimum cosine similarity; model-dependent, `-1` to disable) |
| `--embed-timeout` | `LLM_MEMORY_EMBED_TIMEOUT` | `30s` |

How it works:

- Memories without an embedding (new, imported, written by a non-configured client) are
  indexed in batches of 64 at the start of each `recall`; `llm-memory reindex` indexes everything
  at once, `reindex --all` recomputes everything (after a model change, for example).
- Changing the title, content or tags of a memory invalidates its embedding, which is recomputed later.
- If the API is unreachable, `recall` still answers with full-text search (warning on stderr).
- `remember` and duplicate detection remain full-text: writing never depends on the API.
- **Privacy**: the text of memories and queries is sent to the configured URL.
  Prefer a local model if the memory contains sensitive information.

## Backup

- **JSON export** (recommended, readable and portable): `llm-memory export --history -o backup.json`.
  The file is created with mode 0600. Restore: `llm-memory import backup.json` into an empty or
  existing database (memories already present identically are skipped).
- **File copy**: close the clients, then copy `memory.db` **and** `memory.db-wal` if it exists
  (WAL mode). With clients open, prefer `sqlite3 memory.db ".backup copy.db"`.
- Never store secrets in the memory: it is stored in clear text on disk.

## Checking the installation

1. `npx @modelcontextprotocol/inspector /absolute/path/llm-memory`: the 8 tools must appear.
2. Acceptance scenario:
   1. In Claude Desktop: "Note that for the demo project we chose PostgreSQL over MySQL, for JSONB support."
   2. Then: "Write a handoff, I'm continuing in VS Code."
   3. In VS Code or Claude Code, new session: "Let's get back to the demo project." The LLM must call
      `get_context` and mention the decision and the handoff.
   4. `llm-memory list --namespace demo` shows both memories with different `source` values.

   If step 3 fails because the LLM does not call the tool, check the client's instruction block.

## Development

| Target | Role |
| --- | --- |
| `make build` | Static binary (no CGO) in `bin/` |
| `make test` | `go test -race ./...` (unit, golden, MCP end-to-end, persistence) |
| `make lint` | `go vet` + `golangci-lint` v2 |
| `make cover` | Overall coverage |
| `make fmt` | `gofmt -s -w .` |
| `make release` | Cross-platform binaries in `dist/` |

Updating golden files: `go test ./internal/briefing ./cmd/llm-memory -update`.

Architecture: `internal/store` (SQL, no MCP), `internal/briefing` (text of `get_context`),
`internal/semantic` (embeddings client and hybrid search, optional),
`internal/server` (thin MCP handlers, no SQL), `cmd/llm-memory` (binary and CLI).
Contribution rules: [CLAUDE.md](CLAUDE.md).

## License

[MIT](LICENSE)
