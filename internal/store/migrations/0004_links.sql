-- 0004_links.sql
CREATE TABLE memory_links (
  src_id     INTEGER NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
  dst_id     INTEGER NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
  kind       TEXT NOT NULL CHECK (kind IN ('supersedes', 'relates_to', 'depends_on')),
  created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
  PRIMARY KEY (src_id, dst_id, kind)
);
CREATE INDEX idx_memory_links_dst ON memory_links(dst_id, kind);
