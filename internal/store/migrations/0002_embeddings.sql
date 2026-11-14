-- 0002_embeddings.sql — optional semantic search (phase 6).
-- Empty unless an embeddings API is configured.
CREATE TABLE memory_embeddings (
  memory_id  INTEGER NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
  model      TEXT NOT NULL,
  dims       INTEGER NOT NULL CHECK (dims > 0),
  vector     BLOB NOT NULL CHECK (length(vector) = dims * 4),   -- float32 little-endian
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  PRIMARY KEY (memory_id, model)
);

-- An embedding describes the text it was computed from: drop it when the text changes.
CREATE TRIGGER memories_embeddings_stale AFTER UPDATE OF title, content, tags ON memories BEGIN
  DELETE FROM memory_embeddings WHERE memory_id = new.id;
END;
