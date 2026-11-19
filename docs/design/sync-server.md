# Design: Multi-Machine Sync Server

## Overview

This document describes the architecture for a central synchronization server for `llm-memory`, allowing multiple machines to share memories while keeping each machine's local SQLite database as the primary store for MCP and CLI operations.

## Goals & Principles

- **Local-first & offline-capable:** Every machine keeps its own local SQLite database. MCP (`llm-memory serve`) and CLI commands continue to work locally with zero network dependency.
- **Reuse `multi-machine` v2 semantics:** Leverage the existing `uuid`-based sync identity, dump format v2, `Import` merge logic (last-writer-wins on `updated_at`, history preservation, link union), and `internal/store`.
- **Single repository, separate server binary:** Live in `llm-memory` as a dedicated binary (`cmd/llm-memory-server`) so it shares `internal/store` directly and keeps schemas in lockstep without bloating the local CLI/MCP binary.
- **On-demand sync + session/handoff hooks:** Synchronization is triggered explicitly (via a new `sync` MCP tool and `llm-memory sync` CLI), with best-effort sync at `SessionStart` (pulling remote updates before briefing) and on `handoff` (pushing end-of-session state) so switching machines is seamless without breaking offline usage.

## Architecture

```
Machine A (local SQLite) ──HTTP/HTTPS + JSON (v2)──▶ Sync Server
                                                       ├── server.db (users, sessions, api_keys, audit_log)
Machine B (local SQLite) ──HTTP/HTTPS + JSON (v2)──▶   └── users/<user-id>.db (isolated Store per user)
```

### Transport & API

- **Protocol:** JSON over HTTP/HTTPS (`Content-Type: application/json`), with bounded request bodies (configurable max payload size). TLS is configurable (`--tls-cert` / `--tls-key`) so the server can either terminate TLS directly or run plain HTTP behind a reverse proxy (Caddy/Nginx/Traefik).
- **Per-user isolation & sync scope:** Each approved user has an isolated memory SQLite database (`users/<user-id>.db`) on the server, managed directly via `internal/store` (chosen over logical `user_id` partitioning in a single SQLite file to keep `internal/store` single-tenant/untouched, prevent cross-user query leaks, and avoid SQLite write-lock contention across users). `POST /v1/sync` resolves the user from the Bearer API key and synchronizes **all namespaces** (including `global`) of that user's store.
- **Control database (`server.db`):** Separate SQLite database holding `users` (`role`: `admin`/`user`, `status`: `pending`/`active`/`disabled`), `sessions`, `api_keys`, and `audit_log`.
- **Persistent data directory:** Both `server.db` and `users/<user-id>.db` live under a configurable `--data-dir` (or `LLM_MEMORY_SERVER_DATA_DIR`) so all persistent state resides outside the container when deployed with Docker.
- **Payload format:** Dump format v2 (same structure as `Export` / `Import --merge`), exported with `WithHistory: true` so conflict losers and memory history propagate across machines.
- **Endpoints:**
  - `POST /v1/sync`: Accepts a v2 payload from the client, merges it into the server's SQLite store, and returns the merged v2 snapshot (with history) for the client to merge locally.
  - `GET /v1/health`: Unauthenticated health check.
- **Embeddings:** Embeddings are never synced over the network; incoming or updated memories have their stale vectors invalidated locally and are re-indexed by the local embeddings engine (lazy catch-up / `llm-memory reindex`).
- **Incremental optimization (future/optional):** Start with full v2 snapshot exchange (simple, idempotent, reuses existing `Export`/`Import`), and add a `since` / `cursor` parameter if payload sizes grow.

### Authentication, Roles & Key Management

- **Two distinct roles (`admin` and `user`):**
  - **User registration & approval:** New users register via the web UI with an initial `pending` status and cannot log in or create keys until an administrator approves their account (`pending` -> `active`, or `rejected`/`disabled`).
  - **Administrators:** Multiple `admin` accounts are supported (first admin bootstrapped via CLI, e.g., `llm-memory-server admin create`). Admins approve/reject/disable/delete user accounts, manage admin roles, and can inspect or revoke any machine API key. Admins manage accounts and keys only and have **no access to user memory contents** in the web UI.
  - **Account disabling vs deletion:** Disabling a user immediately invalidates their active web sessions and Bearer API keys while keeping `users/<user-id>.db` intact on disk. An administrator can also **permanently delete** a user account, which removes the user's keys, sessions, account record, and `users/<user-id>.db` (recorded in `audit_log`).
  - **User self-service API keys:** Once approved (`active`), a user logs into the web UI to generate, list, and revoke their own per-machine API keys (labeled by machine name, e.g., `laptop`, `workstation`). The raw secret key is displayed **once** upon creation.
- **Per-machine API keys:** Clients authenticate sync requests using `Authorization: Bearer <api-key>`.
- **Client credential storage:** Stored locally under `~/.llm-memory/sync.json` (with `0600` file permissions on Unix; `$HOME/.llm-memory` on Linux/macOS and `%USERPROFILE%\.llm-memory` on Windows, matching `memory.db` and `models`), configurable via `llm-memory sync login` and overridable with `LLM_MEMORY_SYNC_URL` / `LLM_MEMORY_SYNC_KEY` environment variables.
- **Server-side storage:** API keys are stored hashed (e.g., SHA-256) with metadata: `id`, `user_id`, `name` (machine label), `created_at`, `last_used_at`, `revoked_at`.
- **Auditability & Web UI security:**
  - **Audit log:** Records who performed what action and when (user registration, admin approval/rejection/disablement, API key creation/revocation).
  - **Web UI security:** Password hashing via `argon2id` (or `bcrypt`), `HttpOnly; SameSite=Lax` session cookies (`Secure` when TLS is enabled or behind an HTTPS proxy), CSRF token validation on mutating routes, and login rate limiting.
  - **Server CLI:** Headless commands (e.g., `llm-memory-server admin create`, `user approve`, `key list`, `key revoke`) for bootstrapping and recovery.

### Sync Triggers

1. **Explicit `sync` MCP tool & `llm-memory sync` CLI:**
   - Exports local v2 state, sends to `POST /v1/sync`, merges server response into local SQLite, and reports counts (pushed, pulled, conflicts/merged).
2. **Best-effort sync at `SessionStart` (`llm-memory hook`):**
   - When a remote sync server is configured, the `SessionStart` hook attempts a sync with a short timeout before rendering the session briefing so recent handoffs and memories from another machine appear automatically.
   - Must **never** fail or noticeably stall `SessionStart` when offline or unreachable.
3. **Best-effort sync on `handoff`:**
   - When a remote sync server is configured (and `sync_on_handoff` is enabled, default `true`), `handoff` attempts a sync after writing the local handoff.
   - Must **never** fail the local `handoff` if the server is unreachable or times out: log/return a warning instead so offline work is never blocked.

### Link Removals (Tombstones)

- **Schema migration (`0006_link_tombstones.sql`):** Add a nullable `deleted_at TEXT` column to `memory_links` instead of hard-deleting rows on `RemoveLink`. (`supersedes` links remain immutable and are never removed.)
- **Re-adding a removed link:** Re-adding a previously removed `(src_id, dst_id, kind)` link updates `created_at` to the current timestamp and clears `deleted_at` (`NULL`).
- **Dump v2 extension:** Add an optional `deleted_at` field (`omitempty`) to `DumpLink` while keeping `FormatVersion = 2` backward-compatible (existing dumps without `deleted_at` represent active links).
- **Merge rule:** For each `(src_uuid, dst_uuid, kind)`, compare the latest timestamp between `created_at` and `deleted_at` (with deterministic tie-break favoring deletion if equal, or canonical state). If `deleted_at` is newer than `created_at`, the link stays deleted (`deleted_at` preserved), preventing deleted links from being resurrected.

### Hard Deletes (`PurgeArchived`)

- `forget` archives memories (`archived_at`), which propagates via last-writer-wins.
- `PurgeArchived` performs a physical `DELETE` in SQLite; without memory tombstones, a purged memory still present on another machine would be resurrected on the next sync.
- **v1 position:** Keep `PurgeArchived` as local-only maintenance and warn/discourage running it when remote sync is configured (memory tombstones are deferred unless needed later).

### Overlapping Active Handoffs

- When two machines create active handoffs in the same namespace before syncing, the **newer handoff supersedes the older one** so each namespace keeps at most one active handoff.
- **Merge behavior:** Inside the merge transaction (both on the server and on the client), for any namespace with multiple active (`archived_at IS NULL`) `handoff` memories, select the winner by latest `created_at` (with deterministic tie-break on `uuid`), archive the older active handoffs, and record a `supersedes` link from the winner to each superseded handoff.

### Deployment (Docker & Docker Compose)

- Include a multi-stage `Dockerfile` for `cmd/llm-memory-server` (running as a non-root user, exposing the configured HTTP/HTTPS port, with a health check targeting `GET /v1/health`).
- Include a `docker-compose.yml` mounting an external host directory or named volume onto `--data-dir` (e.g., `/data`) so `server.db` and `users/*.db` are always stored outside the container and survive image upgrades/recreations.
