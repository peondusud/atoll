-- An entry of the trash that a catalog rebuild lists again from its disk (specs.md §9, §17): nothing on the disk says
-- why it went there, so its reason is unknown, and it never goes for room before its time, as a deleted file's would
-- not. SQLite changes a CHECK only by making the table again. Nothing refers to the trash's rows, and each keeps its
-- id and its operation.
CREATE TABLE trash_new(
  id         INTEGER PRIMARY KEY,
  lpath      BLOB NOT NULL,
  disk_id    TEXT NOT NULL,
  trash_path BLOB NOT NULL,              -- location inside Atoll/trash/
  op_id      INTEGER NOT NULL REFERENCES ops(id),
  size       INTEGER CHECK (size >= 0),
  blake3     BLOB CHECK (length(blake3) = 32),
  reason     TEXT NOT NULL CHECK (reason IN ('deleted', 'overwritten', 'repaired', 'quarantined', 'surplus', 'rebalance', 'renamed', 'unknown')),
  deleted_ns INTEGER NOT NULL,
  expires_ns INTEGER NOT NULL
) STRICT;

INSERT INTO trash_new(id, lpath, disk_id, trash_path, op_id, size, blake3, reason, deleted_ns, expires_ns)
  SELECT id, lpath, disk_id, trash_path, op_id, size, blake3, reason, deleted_ns, expires_ns FROM trash ORDER BY id;

DROP TABLE trash;
ALTER TABLE trash_new RENAME TO trash;

CREATE INDEX trash_by_expiry ON trash(expires_ns);
CREATE INDEX trash_by_path ON trash(lpath, op_id);
