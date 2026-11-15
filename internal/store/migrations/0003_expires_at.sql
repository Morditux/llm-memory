-- 0003_expires_at.sql — optional expiration timestamp (TTL) for memories.
ALTER TABLE memories ADD COLUMN expires_at TEXT;

CREATE INDEX idx_memories_expires ON memories(expires_at)
  WHERE expires_at IS NOT NULL AND archived_at IS NULL;
