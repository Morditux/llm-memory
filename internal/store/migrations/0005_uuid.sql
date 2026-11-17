-- 0005_uuid.sql — global identity of a memory (multi-machine merge).
-- The integer id stays the local key (links, history, embeddings, FTS rowid);
-- the uuid is the identity shared between databases.
-- The column stays nullable (SQLite cannot add NOT NULL without a constant
-- default); the Go insert path and the trigger below make sure it is never NULL.
ALTER TABLE memories ADD COLUMN uuid TEXT;

UPDATE memories SET uuid = lower(
  hex(randomblob(4)) || '-' || hex(randomblob(2)) || '-4' ||
  substr(hex(randomblob(2)), 2) || '-' ||
  substr('89ab', (random() & 3) + 1, 1) || substr(hex(randomblob(2)), 2) || '-' ||
  hex(randomblob(6)));

CREATE UNIQUE INDEX idx_memories_uuid ON memories(uuid);

CREATE TRIGGER memories_uuid_default AFTER INSERT ON memories
WHEN new.uuid IS NULL BEGIN
  UPDATE memories SET uuid = lower(
    hex(randomblob(4)) || '-' || hex(randomblob(2)) || '-4' ||
    substr(hex(randomblob(2)), 2) || '-' ||
    substr('89ab', (random() & 3) + 1, 1) || substr(hex(randomblob(2)), 2) || '-' ||
    hex(randomblob(6)))
  WHERE id = new.id;
END;

-- Per-database settings. instance_id identifies this database in exports.
CREATE TABLE meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
INSERT INTO meta(key, value) VALUES ('instance_id', lower(
  hex(randomblob(4)) || '-' || hex(randomblob(2)) || '-4' ||
  substr(hex(randomblob(2)), 2) || '-' ||
  substr('89ab', (random() & 3) + 1, 1) || substr(hex(randomblob(2)), 2) || '-' ||
  hex(randomblob(6))));
