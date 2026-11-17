# Multi-machine support (global memory identity + merge)

Status: draft, branch `multi-machine`. Phase 1 (identity) implemented, uncommitted.

## Problem

Each database is a local SQLite file, and `memories.id` is a local
auto-increment integer. The same `#42` on two machines is two unrelated
memories. Consequences:

- `export` / `import` (`internal/store/transfer.go`) cannot recognise a memory
  it has already seen. Import assigns **new ids** and de-duplicates on
  `(namespace, kind, content)`. An edited memory is therefore imported as a
  second memory, and the old version is never updated.
- `memory_links` and history snapshots reference local ids. A dump format v1
  carries no links at all, and history snapshot ids "refer to the exporting
  database".
- There is no way to tell where a row was written.

## Goals

1. A stable, globally unique identity for each memory.
2. Idempotent, convergent merge of two databases (A into B, then B into A,
   gives the same content on both sides).
3. Links, history and archive state survive the merge.
4. Works offline, with a plain file as transport. No server required.

## Non-goals (for now)

- Automatic background sync, a hosted sync server, or auth for `serve`.
- Syncing embeddings. They are derived data, recomputed locally.
- Real-time collaboration or CRDT-level merging of memory text.

## Design

### 1. Identity: add `uuid`, keep the integer id

Migration `0005_uuid.sql`:

- `ALTER TABLE memories ADD COLUMN uuid TEXT`, backfilled for existing rows,
  then `CREATE UNIQUE INDEX idx_memories_uuid ON memories(uuid)`.
- New rows get a random UUIDv4 generated in Go (`crypto/rand`) at insert time.
  A safety trigger `AFTER INSERT ... WHEN new.uuid IS NULL` fills it in SQL, so
  no code path (import, tests, future code) can leave a row without one.
  Time-ordering (UUIDv7) buys nothing here: nothing sorts by uuid.
- The integer `id` stays the local primary key. It is what `memory_links`,
  `memory_history`, `memory_embeddings` and the FTS `rowid` already use, and it
  is the `#42` users and assistants see. Switching every foreign key to UUIDs
  would be a large, risky migration for no user-visible gain.
- The UUID is the **sync identity** only. It appears in exports, never needs to
  be typed by users (a `--uuid` lookup can come later).

**Backfill (decided): SQL-only.** The runner (`migrate` in `db.go`) executes
each migration file as one SQL string inside a transaction, which is enough:

```sql
ALTER TABLE memories ADD COLUMN uuid TEXT;
UPDATE memories SET uuid = lower(
  hex(randomblob(4)) || '-' || hex(randomblob(2)) || '-4' ||
  substr(hex(randomblob(2)), 2) || '-' ||
  substr('89ab', (random() & 3) + 1, 1) ||
  substr(hex(randomblob(2)), 2) || '-' || hex(randomblob(6)));
CREATE UNIQUE INDEX idx_memories_uuid ON memories(uuid);
CREATE TRIGGER memories_uuid_default AFTER INSERT ON memories
WHEN new.uuid IS NULL BEGIN
  UPDATE memories SET uuid = <same expression> WHERE id = new.id;
END;
```

SQLite cannot add a `NOT NULL` column without a constant default, so the column
stays nullable at the schema level; the trigger and the Go insert path
guarantee it is never NULL. No Go hook, no new dependency, and the runner is
untouched. Caveat for the backfill: two machines that were already created
from the same copied database file would share nothing (uuids are random per
database), so the first merge between them keeps both copies of each memory.
See open question 5.

### 2. Machine identity

A small `meta(key TEXT PRIMARY KEY, value TEXT)` table holds `instance_id`
(random UUID, generated on first open). It is recorded in exports
(`origin`) and used as a deterministic tie-breaker. It is not stored per row
in phase 1. Caveat: `backup` and a plain file copy duplicate the `instance_id`
along with the data, so two machines restored from the same file share one
until it is regenerated (a future `llm-memory sync --reset-instance`, or
regenerate on restore).

### 3. Dump format v2

`FormatVersion` becomes 2. Additions over v1:

- `memories[].uuid`.
- `links[]`: `{src_uuid, dst_uuid, kind, created_at}`, exported for the exported
  memories (a link whose other end is missing is dropped with a warning).
- `origin` (the exporting `instance_id`) and `exported_at`.
- History entries are keyed by memory uuid, not by local id.

The binary keeps reading v1 dumps: rows without a uuid fall back to today's
`(namespace, kind, content)` duplicate rule and receive a fresh uuid.

### 4. Merge semantics (`import --merge`)

Matching is by `uuid`. For each incoming memory:

| Situation | Action |
|---|---|
| uuid unknown locally | insert (new local id), keep timestamps and authors |
| uuid known, incoming `updated_at` newer | overwrite fields, record an `update` history line |
| uuid known, incoming older | keep local row |
| equal `updated_at`, different content | deterministic tie-break on `origin` (lexicographic) |
| identical | no-op (idempotent) |

Rules that make this converge:

- **Last-writer-wins on `updated_at`** per memory, as a whole row. Wall clocks
  can skew between machines, so the loser's version is preserved as a history
  line (`reason: "merge conflict, replaced by <origin>"`) instead of being
  lost.
- **Archive / restore** propagate as ordinary state (`archived_at`) under the
  same rule. For this to work, every state change must bump `updated_at`.
  Needs an audit (see open questions).
- **History** is merged as a set union, de-duplicated on
  `(memory uuid, action, changed_at, changed_by, hash(snapshot))`. Snapshot
  ids inside snapshots are rewritten or ignored on read.
- **Links** are merged as a set union on `(src_uuid, dst_uuid, kind)`. A
  `supersedes` link is never removed (already enforced), which makes the union
  safe. Removing a `relates_to` / `depends_on` link on one machine will not
  propagate in phase 1 (no tombstones). That is an accepted limitation, to
  document.
- **Namespaces** are matched by name. Memories carry their namespace, so a
  rename on one machine arrives as a move of its memories; the old name may
  linger empty on the other machine.
- **Embeddings** are never exported. Imported or overwritten rows are
  re-embedded on the next catch-up (the stale trigger already deletes vectors
  when title/content/tags change).
- Everything runs in one transaction with `--dry-run` support, like `Import`
  today, and returns a report: `inserted`, `updated`, `unchanged`,
  `conflicts`, `links_added`.

### 5. CLI surface

- `llm-memory export` writes v2 by default (`--format-version 1` kept for a
  release if needed).
- `llm-memory import --merge FILE` applies the rules above. Plain `import`
  keeps its current "add new, skip duplicates" behaviour for v1 files, so
  nothing existing breaks.
- Later: `export --since <timestamp>` for incremental exchange.
- A one-shot `llm-memory sync A.db B.db` is a thin wrapper (export + merge both
  ways) and can come after the primitives are solid.

### 6. Deletions

`forget` archives, it does not delete, so it propagates as state. Hard
deletion (purge / expiry cleanup in `maintenance.go`) is local and would
resurrect on the next merge from a machine that still has the row.
Options: tombstone table `(uuid, deleted_at)` merged like any other row, or
document that purge is per machine. Decision needed before phase 3.

## `updated_at` audit (done)

Last-writer-wins needs every state change of a memory to bump `updated_at`.
Checked against `internal/store`:

| Operation | Bumps `updated_at`? | Where |
|---|---|---|
| `Update` (fields, expiry) | yes | `memories.go:307` |
| `Archive` / `Supersede` target / `ArchiveExpired` | yes | `memories.go:381` |
| `Restore` | yes | `memories.go:422` |
| `AddLink` / `RemoveLink` | yes (source memory) | `links.go:52` |
| `RenameNamespace` / `MergeNamespace` | yes, every affected memory | `namespaces.go:179,225` |
| `Supersede` link insert | **no** for the source memory | `links.go` (`insertLink` called directly) |
| `PurgeArchived` | n/a, **hard delete** | `maintenance.go:44` |

Findings:

- Every in-place state change is covered. Timestamps have millisecond
  precision (`%f`) and are RFC 3339 UTC strings, so lexicographic comparison is
  chronological. Ties are rare but possible, hence the `origin` tie-break.
- `Supersede` adds a `supersedes` link on the source memory without touching
  it. Harmless for merge because links are merged as a set union rather than by
  row timestamp, but it must not be relied on as a change signal for
  incremental export (`export --since`). Either bump the source there, or have
  incremental export also select by link `created_at`.
- `PurgeArchived` hard-deletes archived rows (and cascades links, history,
  embeddings). A machine that still holds the row would re-send it, and the
  merge would resurrect it as an archived memory. Not data loss, but the purge
  is silently undone. Phase 1 position: document it; tombstones are phase 5.
- Two machines running `ArchiveExpired` on the same memory each archive it at
  slightly different times. Both end archived, so the merge converges.

## Phases

1. **Identity**: migration `0005`, uuid generation on insert, `meta.instance_id`,
   uuid in `Memory` JSON. No behaviour change otherwise.
2. **Export v2**: uuid, links, history keyed by uuid, `origin`. Reader accepts v1.
3. **Merge import**: LWW rules, history/link union, dry-run, report.
4. **Hardening**: conflict tests, convergence property test, docs in README.
5. **Optional**: incremental export, tombstones, `sync` wrapper, transport
   over `serve` once Bearer auth exists.

## Tests to write

- Backfill: an existing v4 database migrates, every row gets a distinct uuid.
- Round trip: export A, merge into empty B, B equals A (uuid, timestamps,
  archive state, links, history).
- Idempotence: merging the same dump twice changes nothing.
- Convergence: diverge A and B (edit, archive, add, link), merge both ways,
  both end identical.
- Conflict: same uuid edited on both sides, newer wins, loser is in history.
- v1 dump import still works as before.
- Import of a link whose endpoint is missing.

## Open questions

1. ~~Backfill mechanism~~: decided, SQL-only UUIDv4 (see Design 1).
2. ~~`updated_at` audit~~: done, see section above. One gap (`Supersede`
   link on the source) and one hard-delete caveat (`PurgeArchived`).
3. **Clock skew**: is wall-clock LWW acceptable, or do we want a per-row
   `rev` counter plus `origin`? Wall clock is simpler and matches the
   single-user, few-machines use case.
4. **Tombstones** for hard deletes and link removal: phase 1 or later?
5. **Dedup on first merge**: two machines that independently stored the same
   fact have different uuids. Merging keeps both. Should `import --merge` also
   apply the old `(namespace, kind, content)` rule to collapse them, and which
   uuid wins?
6. **Transport**: file only for now. Revisit after Bearer auth for `serve`.
