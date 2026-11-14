-- 0001_initial.sql
CREATE TABLE namespaces (
  name        TEXT PRIMARY KEY CHECK (name GLOB '[a-z0-9]*' AND length(name) <= 64),
  description TEXT NOT NULL DEFAULT '',
  created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
INSERT INTO namespaces(name, description) VALUES ('global', 'Memory shared by all projects');

CREATE TABLE memories (
  id          INTEGER PRIMARY KEY,
  namespace   TEXT NOT NULL DEFAULT 'global' REFERENCES namespaces(name) ON UPDATE CASCADE,
  kind        TEXT NOT NULL CHECK (kind IN ('preference','fact','decision','task_state','handoff','note')),
  title       TEXT NOT NULL DEFAULT '',
  content     TEXT NOT NULL CHECK (length(content) BETWEEN 1 AND 8000),
  tags        TEXT NOT NULL DEFAULT '',
  importance  INTEGER NOT NULL DEFAULT 2 CHECK (importance BETWEEN 1 AND 3),
  source      TEXT NOT NULL DEFAULT 'unknown',
  created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  updated_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  updated_by  TEXT NOT NULL DEFAULT 'unknown',
  archived_at TEXT
);
CREATE INDEX idx_memories_ns_kind ON memories(namespace, kind) WHERE archived_at IS NULL;
CREATE INDEX idx_memories_updated ON memories(updated_at DESC);

CREATE TABLE memory_history (
  id         INTEGER PRIMARY KEY,
  memory_id  INTEGER NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
  action     TEXT NOT NULL CHECK (action IN ('create','update','archive','restore')),
  snapshot   TEXT NOT NULL,
  changed_by TEXT NOT NULL,
  changed_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);
CREATE INDEX idx_history_memory ON memory_history(memory_id, id);

CREATE VIRTUAL TABLE memories_fts USING fts5(
  title, content, tags,
  content='memories', content_rowid='id',
  tokenize='unicode61 remove_diacritics 2'
);

CREATE TRIGGER memories_ai AFTER INSERT ON memories BEGIN
  INSERT INTO memories_fts(rowid, title, content, tags) VALUES (new.id, new.title, new.content, new.tags);
END;
CREATE TRIGGER memories_ad AFTER DELETE ON memories BEGIN
  INSERT INTO memories_fts(memories_fts, rowid, title, content, tags) VALUES ('delete', old.id, old.title, old.content, old.tags);
END;
CREATE TRIGGER memories_au AFTER UPDATE OF title, content, tags ON memories BEGIN
  INSERT INTO memories_fts(memories_fts, rowid, title, content, tags) VALUES ('delete', old.id, old.title, old.content, old.tags);
  INSERT INTO memories_fts(rowid, title, content, tags) VALUES (new.id, new.title, new.content, new.tags);
END;
