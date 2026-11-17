<p align="center">
  <img src="assets/logo.jpg" alt="llm-memory logo" width="160" />
</p>

# llm-memory

[![CI](https://github.com/Morditux/llm-memory/actions/workflows/ci.yml/badge.svg)](https://github.com/Morditux/llm-memory/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/Morditux/llm-memory)](https://github.com/Morditux/llm-memory/releases)
[![Go](https://img.shields.io/github/go-mod/go-version/Morditux/llm-memory?logo=go&logoColor=white)](https://go.dev/)
[![SQLite](https://img.shields.io/badge/SQLite-FTS5-003B57?logo=sqlite&logoColor=white)](https://www.sqlite.org/)
[![License](https://img.shields.io/github/license/Morditux/llm-memory)](LICENSE)

A local MCP server that stores a **memory shared by several LLMs** in a SQLite file
(Claude Desktop, Claude Code, VS Code, an internal LLM…). When you switch from one client to
another, the new one retrieves the context in a single call: decisions, preferences, facts,
task state and the handoff left by the previous assistant.

- A single binary with no system dependency (pure Go, embedded SQLite, FTS5 included).
- Local full-text search; optional semantic search through an embeddings API.
- stdio transport: each client launches its own instance, all pointing to the same file.
- Sequential use (one LLM at a time); two clients open at the same time do not block each other.
- LLMs never delete anything: `forget` archives, and the history of every memory is kept.

## Why

AI assistants have no lasting memory, and no shared memory at all.

1. **Every session starts from scratch.** Decisions, conventions and pitfalls found the day before
   are lost, and you have to explain everything again.
2. **Each tool is a silo.** Start a task in Claude Desktop and continue it in Claude Code, VS Code
   or Codex, and the new assistant knows nothing about what was done or what is left to do.
3. **Existing options are a poor fit.**
   - Built-in memories (ChatGPT, Claude.ai…) belong to one vendor, often live in the cloud, and
     are not shared across tools.
   - `CLAUDE.md`, `AGENTS.md` and `GEMINI.md` files belong to one client. They clutter the
     repository, get duplicated from one tool to the next, cannot be searched, and grow with no
     structure.
   - The conversation context gets compacted or cleared (`/clear`), and whatever it held is gone.
4. **The *why* gets lost.** Code shows what was done, rarely why a choice was made or why an
   approach was dropped. Without that, the next assistant may make the same mistake again.

llm-memory addresses this with:

- **One memory for every client**, through the standard MCP protocol: any compatible assistant
  can read and write it.
- **Local and private storage**: a SQLite file on your machine, with no cloud and no account.
- **Structure**: kinds, importance, per-project namespaces, links between memories and history,
  rather than a loose text file.
- **Explicit handoffs** (`handoff`) so one tool picks up the work where another left it.
- **Automatic loading** through session-start hooks, so the memory does not depend on the model
  remembering to read it.

In short, you no longer have to be the memory between your AI assistants.

## How it compares

Compared with memory servers built on a vector database (Chroma, Qdrant…) or a knowledge graph
(Neo4j, Graphiti…), llm-memory is simpler to deploy, easier to read and more predictable. In
return, it is worse at finding a memory phrased with different words, and keeping memory
structured takes more discipline from the model.

### Strengths

**1. Zero infrastructure**
- A single Go binary with SQLite and FTS5 built in: no Docker, no Qdrant or Neo4j server, no
  Python, no required embeddings model.
- Full-text search works offline, with no GPU, no API key and no per-query cost.
- Install with `go install` or by copying one file, and `setup --write` configures each client.

**2. Deterministic, explainable results**
- Full-text search finds what contains the words searched for, and you can see why a result
  matched.
- Vector search always returns the "k nearest" items, even when nothing is relevant. That brings
  up noise, and the noise ends up in the model's context.
- `llm-memory context` shows exactly what the assistant will receive.

**3. Organized memory rather than a pile of fragments**
- Many solutions split everything into chunks and embed them. Here, each memory has a kind
  (preference, decision…), an importance, tags, a namespace, an expiry date and links.
- The start-of-session briefing is ordered by these fields: essential memories first, then the
  handoff, preferences and recent decisions. It does not depend on similarity to a query.
- The handoff (`handoff`) is a first-class concept, which is rare elsewhere.

**4. No hidden model calls**
- Systems such as Mem0 call a model on every write to extract and merge "facts". That costs
  money, adds latency, and memories can be rewritten or lost without you seeing it.
- Here, the assistant explicitly decides what to remember, and the server only stores it.

**5. Safety and traceability**
- The model cannot delete anything: `forget` archives, every change is kept in history, and
  memories can be restored.
- Detected secrets are rejected, and the author of every write is recorded.
- The file is local, with restricted permissions (0700/0600).
- A vector database usually keeps only the latest version, with no history.

**6. Easy to administer**
- The data lives in a single SQLite file that any tool can read.
- The CLI covers JSON export/import, hot backups, `doctor`, `stats`, `gc`, and merging and
  renaming namespaces.
- With embeddings, changing models forces a full reindex. Full-text search does not depend on
  one.

**7. Built for several clients**
- Per-project namespaces, automatic namespace detection by the hooks, and handoffs between tools:
  multi-client use is the main goal, not an add-on.

### Limitations

- **Synonyms and rephrasing**: a search for "database" will not find a memory that mentions
  "PostgreSQL" without that word. This is where vectors win. The optional semantic search partly
  fills the gap, with its own costs.
- **Large volumes**: to query thousands of documents or a document base (RAG), a real vector
  database is a better fit. llm-memory is a working memory: hundreds or a few thousand chosen
  memories.
- **Reasoning over relationships**: links are simple (supersedes, relates to, depends on). A
  graph such as Graphiti or Zep supports queries over entities and how they change over time.
- **Reliance on the model**: with no automatic extraction, memory quality depends on how
  consistently the assistant calls `remember`. That is why the instruction block and the hooks
  exist.
- **Local, sequential use**: one user and one assistant at a time. It is not a team memory shared
  over the network.

### When to choose what

| Need | Good fit |
| --- | --- |
| Personal working memory shared between several AI assistants, simple and safe | **llm-memory** |
| Querying a large document corpus (RAG) | Vector database |
| Memory extracted automatically from conversations, e.g. for agents or a product | Mem0 |
| Reasoning about entities and their relationships over time | Knowledge graph (Zep/Graphiti) |

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
| Codex | [examples/codex_config.toml](examples/codex_config.toml) (`~/.codex/config.toml`) |
| OpenCode | [examples/opencode.json](examples/opencode.json) (`~/.config/opencode/opencode.json`) |
| VS Code | [examples/vscode_mcp.json](examples/vscode_mcp.json) (project default namespace) |
| Internal LLM | same principle if its client supports MCP over stdio; otherwise `llm-memory context` and `llm-memory search` to paste the context by hand |

For **Antigravity**, add the `llm-memory` entry to `~/.gemini/config/mcp_config.json` (or inspect active servers in the UI via **Additional Options (...) > MCP Servers**), and add the instruction block to `~/.gemini/GEMINI.md` (global) or `GEMINI.md` / `AGENTS.md` in your project.

**Required**: add the instruction block from
[examples/instructions_client.md](examples/instructions_client.md) to each client. Without it, LLMs
do not call the memory reliably.

On Windows, escape backslashes in JSON (`"C:\\tools\\llm-memory.exe"`).

### Automatic briefing (session-start hooks)

Clients that support hooks can inject the briefing themselves at the start of every session,
so the memory no longer depends on the LLM remembering to call `get_context`:

```bash
llm-memory setup --hooks --write claude-code   # ~/.claude/settings.json
llm-memory setup --hooks --write codex         # ~/.codex/hooks.json (then trust it with /hooks)
llm-memory setup --hooks --write antigravity   # ~/.gemini/config/hooks.json
```

`llm-memory hook --write <client>` does the same patch from the hook command itself, and also keeps
`--max-chars` and `--focus` in the installed command line (e.g.
`llm-memory hook --write --max-chars 3000 claude-code`).

Without `--write`, the JSON to add is printed. The hook runs `llm-memory hook <client> --namespace auto`:
the namespace is detected from the session's directory (`cwd` or `workspacePaths` sent by the client),
the hook also fires after `/clear` and compaction, and a database error never blocks the session
(message on stderr, exit status 0). Rerunning the command replaces the previous entry and keeps your
other hooks. Keep the instruction block anyway: it covers writes (`remember`, `handoff`) and the
clients without hooks (Claude Desktop, VS Code, OpenCode).

### Parameters

Priority: flag > environment variable > default.

| Flag | Variable | Default | Role |
| --- | --- | --- | --- |
| `--db` | `LLM_MEMORY_DB` | `~/.llm-memory/memory.db` | Database file (`~` expanded by the program) |
| `--namespace` | `LLM_MEMORY_NAMESPACE` | `global` | Namespace used when a tool call does not specify one |
| `--source` | `LLM_MEMORY_SOURCE` | `clientInfo.name` of the MCP client | Author recorded for writes |
| `--log-level` | `LLM_MEMORY_LOG_LEVEL` | `info` | Logs on stderr (`debug`, `info`, `warn`, `error`) |

## MCP tools, resources & prompts

| Tool | Role |
| --- | --- |
| `get_context` | Start-of-session Markdown briefing: latest handoff, essential memories, tasks, preferences, recent decisions and facts (supports `focus`, including semantic search when enabled) |
| `remember` | Creates a memory (`preference`, `fact`, `decision`, `task_state`, `note`); reports probable duplicates; `supersedes`, `related_to` and `depends_on` create structured links; rejects detected secrets |
| `recall` | Full-text search (accents and case ignored, words matched as prefixes), namespace / kinds / tags filters |
| `get_memory` | Full content, change history (optional) and structured links (`supersedes`, `relates_to`, `depends_on`) |
| `update_memory` | Modifies the given fields or adds/removes links; the old version stays in history |
| `forget` | Archives (reversible with `llm-memory restore`) |
| `handoff` | Handoff for the next LLM (summary, next steps, open questions, optional `supersedes` and `namespace_description`); one active per namespace |
| `list_namespaces` | Namespaces, number of active memories, last activity |

Resources & prompts:

- **Resources**: `memory://context` (default namespace briefing) and `memory://context/{namespace}` (Markdown).
- **Prompts**: `briefing` and `handoff` (both accept an optional `namespace` argument).

A namespace usually maps to a project; `global` holds what applies everywhere
(the user's preferences). A namespace is created by the first `remember` or `handoff` in it.

## Administration (CLI)

Flags may be placed before or after arguments. Writes made from the CLI are
recorded with the author `cli` (overridable with `--source`).

```bash
llm-memory remember --kind decision [--namespace X] [--title T] [--tag t1,t2] [--importance 2] [--supersedes 1,2] [--expires-in 7d] [--json] <content|->
llm-memory update [--kind K] [--title T] [--content C] [--tag t1,t2] [--importance N] [--namespace X] [--expires-in D] [--clear-expires] [--add-link relates_to:5] [--remove-link relates_to:5] [--json] <id> [content|-]
llm-memory handoff [--namespace X] [--next "step 1,step 2"] [--questions "q1"] [--supersedes 3] [--description "..."] [--json] <summary|->
llm-memory forget [--reason "..."] [--json] <id>...
llm-memory list [--namespace X] [--kind decision,fact] [--tag db,api] [--archived] [--limit N] [--json]
llm-memory show [--json] 42              # full content, links and history
llm-memory search [--tag db] [--json] postgres jsonb   # same engine as recall
llm-memory context --namespace demo [--focus Q] [--json] # exactly what get_context would return
llm-memory restore 42                    # un-archives
llm-memory purge --archived-before 2026-01-01 [--yes]   # permanent deletion
llm-memory export [--namespace X] [--history] [-o dump.json]
llm-memory import [--dry-run] dump.json  # or - for stdin; exact duplicates are skipped
llm-memory stats [--json]                # per namespace, kind, source; file size
llm-memory namespace <list|rename|merge|archive> ... # manage namespaces
llm-memory gc [--json]                   # archive expired (TTL) memories
llm-memory backup [--keep N] [--verify] [--json] [<dest>] # hot SQLite backup (VACUUM INTO)
llm-memory doctor [--json]               # check database integrity, permissions, embeddings and client configs
llm-memory setup [--ollama] [--write|--remove] [--config PATH] <client> # print, write or remove MCP config (antigravity, claude-desktop, claude-code, codex, opencode, vscode)
llm-memory setup --hooks [--events SessionStart,PreCompact,Stop] [--write|--remove] [--config PATH] <client> # lifecycle hooks (antigravity, claude-code, codex)
llm-memory hook [--event E] [--focus Q] <client>     # hook entry point: prints the payload in the client's hook format
llm-memory hook --write [--events LIST] [--config PATH] [--max-chars N] [--focus Q] <client>  # install hook(s) in the client config
llm-memory hook --remove [--config PATH] <client>    # remove llm-memory hooks from the client config
llm-memory reindex [--all]               # embeddings (semantic search only)
llm-memory ui [--addr 127.0.0.1:8765]    # local web explorer (create/edit, tags, TTL, links, namespaces, diff history, similar/supersede, GC)
llm-memory completion <bash|zsh|fish>    # print shell completion script
llm-memory serve [--listen 127.0.0.1:8766] [--stateless] [--read-only] # stdio (default) or Streamable HTTP (/mcp, /healthz)
```

> **Note:** Both `serve --listen` (Streamable HTTP) and `ui` have no user authentication; keep them bound to loopback (`127.0.0.1`) or place an authenticating reverse proxy in front.

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

- Active memories without an embedding (new, imported, written by a non-configured client) in the
  searched scope are indexed in batches of 64 at the start of each `recall` (archived memories are
  only indexed when `include_archived` is set); `llm-memory reindex` indexes all memories across all
  namespaces (including archived ones), and `reindex --all` recomputes everything (after a model
  change, for example).
- Changing the title, content or tags of a memory invalidates its embedding, which is recomputed later.
- If the API is unreachable, `recall` still answers with full-text search (warning on stderr).
- `remember` also uses embeddings for duplicate detection when indexed memories exist in the target
  namespace, ignoring API errors and falling back to full-text duplicate detection: writing never
  depends on the API.
- **Privacy**: the text of indexed memories, search queries, and new `remember` entries (when
  indexed memories exist in the target namespace) is sent to the configured URL. `recall` only
  backfills active memories within its namespace filter (unless `include_archived` is true), whereas
  `llm-memory reindex` indexes all namespaces including archived memories. Prefer a local model if
  the memory contains sensitive information.

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

## License

[MIT](LICENSE)
