# Design: Multi-Machine Sync Server

Status: draft, revised after design review (2026-11-20). Nothing implemented yet.

## Overview

This document describes the architecture for a central synchronization server for `llm-memory`, allowing multiple machines to share memories while keeping each machine's local SQLite database as the primary store for MCP and CLI operations.

It builds on [multi-machine.md](multi-machine.md) (uuid identity, dump format v2, `Store.Merge` / `import --merge`).

## Goals & Principles

- **Local-first & offline-capable:** Every machine keeps its own local SQLite database. MCP (`llm-memory serve`) and CLI commands continue to work locally with zero network dependency.
- **Reuse `multi-machine` semantics:** Leverage the existing `uuid`-based sync identity, the dump format, `Store.Merge` (last-writer-wins on `updated_at`, history union, link union, twin rule), and `internal/store`. Because `Merge` is idempotent, a lost response is harmless: the next sync redoes the same work.
- **Deterministic merge side effects:** Anything a merge writes beyond copying the winning version (archiving a duplicate handoff, a tombstone, a history line) must be a pure function of the rows being reconciled: no `now()`, no actor or local id of the node running the merge. Applying the same rule on the server and on each client must produce byte-identical rows, so the next merge sees them as unchanged instead of ping-ponging between versions.
- **Single repository, separate server binary:** Live in `llm-memory` as a dedicated binary (`cmd/llm-memory-server`) so it shares `internal/store` directly and keeps schemas in lockstep at build time without bloating the local CLI/MCP binary. Deployed versions can still differ: see [Versioning](#versioning--compatibility).
- **On-demand sync + session/handoff hooks:** Synchronization is triggered explicitly (via a new `sync` MCP tool and `llm-memory sync` CLI), with best-effort sync at `SessionStart` (pulling remote updates before briefing) and on `handoff` (pushing end-of-session state) so switching machines is seamless without breaking offline usage.

## Non-goals

- **Confidentiality against the server operator.** The server merges plaintext memories; whoever has access to the data directory can read `users/*.db`. End-to-end encryption is incompatible with server-side merging and is out of scope. "Admins have no access to memory contents" is a web UI restriction, not a confidentiality guarantee; host-level disk encryption is the operator's responsibility.
- Real-time or background sync (no daemon, no push from the server).
- Field-level merge: last-writer-wins stays per whole row.

## Architecture

```
Machine A (local SQLite) ──HTTP/HTTPS + JSON──▶ Sync Server
                                                  ├── server.db (users, sessions, api_keys, audit_log)
Machine B (local SQLite) ──HTTP/HTTPS + JSON──▶   └── users/<user-id>.db (isolated Store per user)
```

### Transport & API

- **Protocol:** JSON over HTTP/HTTPS (`Content-Type: application/json`), gzip-compressed in both directions (`Content-Encoding: gzip`; dumps compress 5–10×). Request bodies are bounded both compressed and decompressed (configurable max payload size, generous by default since v1 exchanges full snapshots); an oversized request gets `413` with an explicit message. TLS is configurable (`--tls-cert` / `--tls-key`) so the server can either terminate TLS directly or run plain HTTP behind a reverse proxy (Caddy/Nginx/Traefik).
- **Per-user isolation & sync scope:** Each approved user has an isolated memory SQLite database (`users/<user-id>.db`) on the server, managed directly via `internal/store` (chosen over logical `user_id` partitioning in a single SQLite file to keep `internal/store` single-tenant/untouched, prevent cross-user query leaks, and avoid SQLite write-lock contention across users). `<user-id>` is a server-generated identifier (UUID), never the login name, so it is always safe in a file path. `POST /v1/sync` resolves the user from the Bearer API key and synchronizes **all namespaces** (including `global`) of that user's store.
- **Open stores:** The server keeps an LRU cache of open `*store.Store` (each is a single-connection `*sql.DB`). Two machines of the same user syncing at once are serialized by the store's `BEGIN IMMEDIATE` transactions.
- **Control database (`server.db`):** Separate SQLite database holding `users` (`role`: `admin`/`user`, `status`: `pending`/`active`/`rejected`/`disabled`), `sessions`, `api_keys`, and `audit_log`.
- **Persistent data directory:** Both `server.db` and `users/<user-id>.db` live under a configurable `--data-dir` (or `LLM_MEMORY_SERVER_DATA_DIR`) so all persistent state resides outside the container when deployed with Docker.
- **Payload format:** The dump format (v3, see [Versioning](#versioning--compatibility)), exported with `WithHistory: true` so conflict losers and memory history propagate across machines, wrapped in an envelope.

  Request:

  ```json
  {
    "protocol": 1,
    "client_version": "1.5.0",
    "schema_version": 7,
    "since": null,
    "dry_run": false,
    "dump": { "format_version": 3, "...": "..." }
  }
  ```

  Response:

  ```json
  {
    "protocol": 1,
    "server_version": "1.5.0",
    "server_time": "2026-11-20T10:00:00.000Z",
    "cursor": null,
    "report": { "inserted": 0, "updated": 2, "kept_local": 1, "...": "..." },
    "dump": { "format_version": 3, "...": "..." }
  }
  ```

  `report` is the server-side `MergeReport` (what this push changed on the server); the client's own merge of `dump` gives what was pulled. `since` / `cursor` are reserved for [incremental sync](#incremental-sync-future) and ignored in v1 (full exchange).
- **Endpoints:**
  - `POST /v1/sync`: Accepts the client's dump, merges it into the user's store, and returns the merged snapshot (with history and tombstones) for the client to merge locally. With `dry_run`, the server merges inside a transaction, exports the would-be snapshot inside that same transaction, then rolls back (needs a `Store` variant of `Merge` that exports before rollback).
  - `GET /v1/whoami`: Authenticated. Returns the user, the key's machine label, the server version, the accepted protocol and schema range, and `server_time`. Used by `llm-memory sync login` (validate the key) and `llm-memory doctor`.
  - `GET /v1/health`: Unauthenticated health check.
- **Embeddings:** Embeddings are never synced over the network; incoming or updated memories have their stale vectors invalidated locally and are re-indexed by the local embeddings engine (lazy catch-up / `llm-memory reindex`). The server never computes embeddings.

### Versioning & compatibility

- **Dumps are decoded strictly.** `llm-memory import` rejects unknown fields (`DisallowUnknownFields` in `cmd/llm-memory/admin.go`), so adding a field to v2 (e.g. link `deleted_at`) would make older binaries fail with an unclear "unknown field" error. Therefore every addition in this document bumps **`FormatVersion` to 3** (link tombstones, memory tombstones). `export --format-version 2` stays available for older binaries (it drops tombstones), and `Import` / `Merge` keep reading v2 (a v2 dump has no tombstones and all its links are active). `Merge` must accept versions 2 and 3 instead of only the latest.
- **Protocol negotiation.** Each request carries `protocol`, `schema_version` and `client_version`. The server accepts the range it supports and otherwise answers `426 Upgrade Required` with a message naming the minimum `llm-memory` version. A client that receives a server protocol older than it supports (via `whoami` or the response) stops with an explicit error. Rollout rule: **upgrade the server first**, then the clients.
- On `426`, the `SessionStart` hook writes a one-line warning to stderr and still renders the local briefing; `doctor` reports the mismatch.

### Clocks

Last-writer-wins compares wall-clock timestamps written by different machines. The server makes skew visible instead of letting it silently decide conflicts:

- **Future timestamps are rejected.** A dump containing a `created_at`, `updated_at`, `archived_at`, link timestamp, tombstone or history `changed_at` more than 5 minutes ahead of `server_time` is rejected (`400`) with the offending memory ids/uuids. A future-dated row would otherwise win every conflict until that date, and later edits on every machine would lose silently (kept only in history). Rejection rather than clamping: a clamped row would differ from the client's copy and the two would replace each other on every sync. The fix is a local `llm-memory update` of the memory, which re-dates it to `now`.
- **Client clock check.** The client compares `server_time` (corrected by half the round trip) with its own clock and refuses to push when the skew exceeds a threshold (default 2 minutes), with an explicit error; `doctor` reports the measured skew.

### Incremental sync (future)

v1 exchanges full snapshots: simple, idempotent, and it reuses `Export` / `Merge` unchanged. When payloads grow, the reserved `since` / `cursor` fields carry an incremental exchange. Constraints for that design:

- **The cursor cannot be `updated_at`.** `Merge` preserves each row's original `updated_at`. A memory edited offline on machine B at T1 and pushed to the server at T3 has `updated_at = T1`; machine A, which last synced at T2 (T1 < T2 < T3), would never see it with a `since T2` filter. The cursor must be a **monotonic sequence assigned by the server** to every change it commits for a user (memory inserted/replaced, link state change, tombstone).
- **Push side.** The client sends the memories changed since its last successful push, tracked by a dedicated local counter (in `meta`). `memory_history.id` is not a reliable change log: it is `INTEGER PRIMARY KEY` without `AUTOINCREMENT`, so ids can be reused after a purge, and link changes made by a merge write no history.
- **Partial exports must keep links.** `exportLinks` (`internal/store/transfer.go`) currently drops a link whose other end is not in the dump. An incremental export must include it; `Merge` already resolves link ends through the local database (`resolveUUID`).
- `Supersede` adds a `supersedes` link without touching the source memory, so the change selection must include link changes, not only memory rows.

### Authentication, Roles & Key Management

Account management ships in two phases. The sync protocol and key format are the same in both.

**Phase A — CLI-managed accounts (server v1).** No web UI. The operator manages everything with the server CLI, which runs with filesystem access to `--data-dir`:

- `llm-memory-server user add|list|disable|enable|delete`
- `llm-memory-server key create --user <user> --name <machine>` (prints the raw key once), `key list`, `key revoke`

**Phase B — Web UI self-service.**

- **Two distinct roles (`admin` and `user`):**
  - **User registration & approval:** New users register via the web UI with an initial `pending` status and cannot log in or create keys until an administrator approves their account (`pending` → `active`, or `rejected`). A `rejected` account cannot log in; an admin can later delete it.
  - **Administrators:** Multiple `admin` accounts are supported (first admin bootstrapped via CLI, e.g., `llm-memory-server admin create`). Admins approve/reject/disable/delete user accounts, manage admin roles, and can inspect or revoke any machine API key. Admins manage accounts and keys only and have **no access to user memory contents** in the web UI (see [Non-goals](#non-goals) for what this does not guarantee).
  - **User self-service API keys:** Once approved (`active`), a user logs into the web UI to generate, list, and revoke their own per-machine API keys (labeled by machine name, e.g., `laptop`, `workstation`). The raw secret key is displayed **once** upon creation.
- **Web UI security:** Password hashing via `argon2id`, `HttpOnly; SameSite=Lax` session cookies (`Secure` when TLS is enabled or behind an HTTPS proxy), CSRF token validation on mutating routes, and login rate limiting.

**Common to both phases:**

- **Account disabling vs deletion:** Authentication checks the user's status on **every** request (web session or API key). Disabling a user therefore takes effect immediately without revoking anything, and re-enabling restores access for the existing machine keys. Deleting a user closes the cached store, then removes the user's keys, sessions, account record, and `users/<user-id>.db` together with its `-wal` and `-shm` files (recorded in `audit_log`).
- **Per-machine API keys:** Clients authenticate sync requests using `Authorization: Bearer <api-key>`. Key format: `lmk_<key-id>_<secret>`, with a 256-bit random secret. The server looks the key up by `<key-id>` and compares the SHA-256 of the secret in constant time (a fast hash is sufficient for a high-entropy secret). Stored metadata: `id`, `user_id`, `name` (machine label), `created_at`, `last_used_at`, `revoked_at`. Failed API authentications are rate-limited per client address.
- **Client credential storage:** Stored locally under `~/.llm-memory/sync.json` (with `0600` file permissions on Unix; `$HOME/.llm-memory` on Linux/macOS and `%USERPROFILE%\.llm-memory` on Windows, matching `memory.db` and `models`), configured via `llm-memory sync login` (which validates the key with `GET /v1/whoami`) and overridable with `LLM_MEMORY_SYNC_URL` / `LLM_MEMORY_SYNC_KEY` environment variables. `llm-memory doctor` reports the sync configuration, server reachability, protocol compatibility and clock skew.
- **Audit log:** Records who performed what action and when (user registration, admin approval/rejection/disablement/deletion, API key creation/revocation). Individual syncs are not audited; `last_used_at` covers them.
- **Server CLI:** Headless commands (e.g., `llm-memory-server admin create`, `user approve`, `key list`, `key revoke`) for bootstrapping and recovery in phase B, and for everything in phase A.

### Sync Triggers

Sync state lives in the local database's `meta` table (`sync_last_ok_at`, `sync_backoff_until`, later `sync_cursor`), so it follows the database rather than the machine.

1. **Explicit `sync` MCP tool & `llm-memory sync` CLI:**
   - Exports local state, sends it to `POST /v1/sync`, merges the server response into local SQLite, and reports counts: pushed (server `report`), pulled (local merge report), conflicts (`kept_local` on either side).
   - `--dry-run` asks the server for a dry run and dry-runs the local merge of the returned snapshot: useful before the first sync of a machine that already has memories.
   - The `sync` tool is not registered in `--read-only` mode.
2. **Best-effort sync at `SessionStart` (`llm-memory hook`):**
   - When a remote sync server is configured, the `SessionStart` hook attempts a sync before rendering the session briefing so recent handoffs and memories from another machine appear automatically.
   - Budget: about 1 s to connect and 3 s overall, well inside the hook's 10 s timeout (`cmd/llm-memory/hooks.go`).
   - Skipped when the last successful sync is less than 60 s old (several sessions or subagents starting in a row), and during a backoff window after a failure (starting at 5 minutes, growing), so that offline session starts are not delayed every time.
   - Must **never** fail or noticeably stall `SessionStart` when offline or unreachable.
3. **Best-effort sync on `handoff`:**
   - When a remote sync server is configured (and `sync_on_handoff` is enabled, default `true`), `handoff` attempts a sync after writing the local handoff.
   - In `llm-memory serve`, the sync runs in a background goroutine after the tool has returned, so the assistant is never blocked. In the CLI (`llm-memory handoff`), it runs synchronously with the same budget as `SessionStart`, since the process exits right after.
   - Must **never** fail the local `handoff` if the server is unreachable or times out: log/return a warning instead so offline work is never blocked.

## Store changes required for sync

All of these live in `internal/store` and also apply to file-based `import --merge`.

### History noise from merges

When an incoming version replaces a local one, `Merge` writes a `merged: newer version from <origin>` history line. That line is new on the merging node, so the history union then copies it to every other node; in a star topology one edit produces about one extra line per machine, replicated everywhere. It converges, but history grows with the number of machines.

**Rule:** write that line only when the incoming memory carries no history. Sync always exports with history, and the author's own history line already records the new version.

### Overlapping Active Handoffs

- When two machines create active handoffs in the same namespace before syncing, the most recent one wins so each namespace keeps at most one active handoff.
- **Winner:** for any namespace with several active (`archived_at IS NULL`) `handoff` memories after the merge, the winner is the one with the latest `updated_at`, then latest `created_at`, then largest `uuid`. `updated_at` rather than `created_at`, so that an older handoff deliberately restored (which bumps `updated_at`) is not re-archived by a newer one from another machine.
- **Deterministic archival:** the existing `archive()` cannot be used, because it stamps `now()` and the merging node's actor, which breaks the [deterministic side effects](#goals--principles) principle. Each loser is archived with values derived from the two rows only:
  - `archived_at = updated_at = max(winner.updated_at, loser.updated_at + 1 ms)` (strictly after the loser's last change, so last-writer-wins propagates it to machines where it is still active),
  - `updated_by = winner.updated_by`,
  - one `archive` history line with `changed_at` and `changed_by` set to the same values and reason `replaced by handoff <winner uuid>` (a uuid, never a local `#id`, which differs between machines and would make the history lines unequal),
  - a `supersedes` link from the winner with `created_at` set to the same timestamp.
- Two nodes resolving the same conflict independently therefore produce identical rows; the next merge sees them as unchanged.
- It runs at the end of the merge transaction, for the namespaces the merge touched (on the server, on each client, and in `import --merge`).

### Link Removals (Tombstones)

- **Schema migration (`0006_link_tombstones.sql`):** Add a nullable `deleted_at TEXT` column to `memory_links` instead of hard-deleting rows on `RemoveLink`. (`supersedes` links remain immutable and are never removed.)
- **State of a link:** active when `deleted_at IS NULL`, deleted otherwise. Its version timestamp is `deleted_at` when deleted, `created_at` when active.
- **Re-adding a removed link:** Re-adding a previously removed `(src_id, dst_id, kind)` link sets `created_at` to the current timestamp and clears `deleted_at` (`created_at` then reads as "linked since").
- **Dump v3:** `DumpLink` gains an optional `deleted_at` field. Removed links are exported as tombstones (both ends must still exist).
- **Merge rule** (last-writer-wins register per `(src_uuid, dst_uuid, kind)`, comparing the local and incoming versions):

  | Local | Incoming | Result |
  |---|---|---|
  | absent | active or deleted | insert as received (a tombstone is stored too, so a stale copy of the link cannot resurrect it later) |
  | version older | version newer | adopt the incoming `created_at` and `deleted_at` as a pair |
  | version newer | version older | keep local |
  | same version, one side deleted | | deleted wins |
  | identical | identical | no-op |

  Two machines that added the same link independently converge on the later `created_at` instead of keeping different values forever.
- **Code impact (contained to `internal/store`: `links.go`, `merge.go`, `transfer.go`):** `insertLink` and the link insert in `Merge` (both `INSERT OR IGNORE` today) become upserts that reactivate a deleted link; `RemoveLink` sets `deleted_at`; `Links()` and the v2 export ignore deleted links; `Import` (non-merge) ignores tombstones.

### Hard Deletes (Memory Tombstones)

- `forget` archives memories (`archived_at`), which propagates via last-writer-wins.
- `Store.Purge` (`llm-memory purge --archived-before`) performs a physical `DELETE`. With full snapshot exchange, a purged memory is not "possibly" resurrected: the server sends it back on the very next sync, every time.
- More importantly, there is otherwise no way to erase data everywhere. `forget` keeps the content, and history is merged as a union and never removed, so a secret stored by mistake would live forever on the server and on every machine.
- **Design:**
  - Schema migration (`0007_memory_tombstones.sql`): `memory_tombstones(uuid TEXT PRIMARY KEY, deleted_at TEXT NOT NULL, deleted_by TEXT NOT NULL)`.
  - `purge` writes a tombstone for each deleted memory in the same transaction. A targeted `llm-memory purge --id N` hard-deletes one memory (archived or not) with its history, links and embeddings. Both stay CLI-only, like `purge` today (not reachable from MCP tools).
  - Dump v3 carries `tombstones: [{uuid, deleted_at, deleted_by}]`.
  - **Merge:** tombstones are applied first and **deletion always wins**, whatever the row's `updated_at` (an erasure must not be undone by a later edit elsewhere): the local row with that uuid is deleted (cascading history, links, embeddings, FTS) and the tombstone is stored. Incoming memories whose uuid is tombstoned locally are skipped before the twin rule runs, and links to them are skipped. The report gains a `deleted` count.
  - Tombstones are kept forever (a uuid and two short strings each). Expiring them would let a machine offline for longer than the retention resurrect deleted memories.
- **Limit:** server backups taken before the deletion still contain the data.

### Needless re-embedding

`Merge` replaces a winning row with an `UPDATE` that always sets `title`, `content` and `tags`. The `AFTER UPDATE OF title, content, tags` triggers (`memories_embeddings_stale` in `0002_embeddings.sql`, `memories_au` in `0001_initial.sql`) fire on any update that names those columns, even with unchanged values. A memory archived on another machine therefore loses its vector and is re-indexed for nothing. Add `WHEN old.title IS NOT new.title OR old.content IS NOT new.content OR old.tags IS NOT new.tags` to both triggers (in a migration), or only set the columns that changed.

### Namespaces (accepted limitations)

Namespaces are matched by name. A description is only filled in when empty locally (no last-writer-wins on descriptions), and a rename on one machine arrives as a move of its memories, leaving the old, empty name on the others. Accepted for v1.

## Deployment (Docker & Docker Compose)

- Include a multi-stage `Dockerfile` for `cmd/llm-memory-server`. The binary builds with `CGO_ENABLED=0` (pure-Go `modernc.org/sqlite`), so the runtime image can be a minimal static image (e.g. distroless `static:nonroot`) running as a non-root user and exposing the configured HTTP/HTTPS port.
- Such an image has no `curl`: the health check runs `llm-memory-server healthcheck`, a subcommand that calls `GET /v1/health` on the local port.
- Include a `docker-compose.yml` mounting an external host directory or named volume onto `--data-dir` (e.g., `/data`) so `server.db` and `users/*.db` are always stored outside the container and survive image upgrades/recreations.
- The data directory must be on a local filesystem: SQLite in WAL mode needs shared memory and reliable locks, which NFS/SMB mounts do not provide.
- **Backups:** `llm-memory-server backup` takes hot copies of `server.db` and of each user store (`VACUUM INTO`, as `Store.Backup` already does). Every client also holds a full copy of its user's memories, so a lost user store can be re-seeded by syncing any machine. What is lost is only what no machine has: tombstones and changes not yet synced.
- **Release:** `make release` builds `llm-memory-server` for the supported platforms, and `release.yml` builds and pushes the image (e.g. `ghcr.io`) on `v*` tags.

## Phases

1. **Store:** dump format v3, link tombstones, memory tombstones and `purge --id`, deterministic handoff resolution in `Merge`, no `merged` history line when the dump carries history, trigger fix. Convergence tests, including a property test: random operations on several replicas, merged in random orders, must end with identical exports. File-based `import --merge` benefits immediately.
2. **Protocol + minimal server + client:** envelope, versioning, clock checks, `POST /v1/sync`, `whoami`, `health`, CLI-managed accounts and keys (phase A), `llm-memory sync` / `sync login` / `doctor`.
3. **Hooks & MCP:** best-effort sync at `SessionStart` and on `handoff`, `sync` MCP tool.
4. **Deployment:** Dockerfile, compose file, server backup command, release pipeline.
5. **Web UI accounts (phase B):** registration, approval, roles, self-service keys.
6. **Incremental sync:** server-assigned cursor, local push counter, partial exports.

## Open questions

1. Skew thresholds: 5 minutes for rejecting future timestamps on the server, 2 minutes for the client refusing to push. Are these the right defaults?
2. Should some namespaces stay local (excluded from sync)? This needs filtering in both directions, since the server sends back everything it has.
3. Is `sync_on_handoff` defaulting to `true` right?
