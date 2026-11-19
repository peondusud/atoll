-- Hard links through Atoll's own view (specs.md §9, §21.2): kind = link gives the file at lpath a second name, lpath2,
-- on the disk the view presents it from, and file_key is its inode; each name is then a file of its own, as a pass
-- records it (specs.md §15.1). A kind added to a closed list: SQLite changes a CHECK only by making the table again.
-- The rows go back with their ids, and the counter keeps the highest id ever given, as 0006_owner does for ops.
CREATE TABLE view_ops_new(
  id       INTEGER PRIMARY KEY AUTOINCREMENT,
  kind     TEXT NOT NULL CHECK (kind IN ('create', 'mkdir', 'symlink', 'write', 'rename', 'replace', 'link', 'unlink', 'rmdir')),
  disk_id  TEXT NOT NULL,
  lpath    BLOB NOT NULL,
  lpath2   BLOB,
  folder   INTEGER NOT NULL CHECK (folder IN (0, 1)),
  file_key TEXT,
  op_id    INTEGER REFERENCES ops(id),
  state    TEXT NOT NULL CHECK (state IN ('planned', 'done', 'undone')),
  at_ns    INTEGER NOT NULL,
  taken_ns INTEGER,
  CHECK ((kind IN ('rename', 'replace', 'link')) = (lpath2 IS NOT NULL)),
  CHECK (kind = 'unlink' OR (kind = 'replace') = (op_id IS NOT NULL)),
  -- A hard link names a file, never a folder, and the inode it links, which its reprise looks for at the new name.
  CHECK (kind <> 'link' OR (folder = 0 AND file_key IS NOT NULL))
) STRICT;

CREATE TEMP TABLE view_ops_rows AS SELECT id, kind, disk_id, lpath, lpath2, folder, file_key, op_id, state, at_ns, taken_ns FROM view_ops;
CREATE TEMP TABLE view_ops_counter AS SELECT seq FROM sqlite_sequence WHERE name = 'view_ops';

DROP TABLE view_ops;
ALTER TABLE view_ops_new RENAME TO view_ops;

INSERT INTO view_ops(id, kind, disk_id, lpath, lpath2, folder, file_key, op_id, state, at_ns, taken_ns)
  SELECT id, kind, disk_id, lpath, lpath2, folder, file_key, op_id, state, at_ns, taken_ns FROM temp.view_ops_rows ORDER BY id;
UPDATE sqlite_sequence SET seq = max(seq, (SELECT seq FROM temp.view_ops_counter))
  WHERE name = 'view_ops' AND EXISTS (SELECT 1 FROM temp.view_ops_counter);
INSERT INTO sqlite_sequence(name, seq)
  SELECT 'view_ops', seq FROM temp.view_ops_counter WHERE NOT EXISTS (SELECT 1 FROM sqlite_sequence WHERE name = 'view_ops');

DROP TABLE temp.view_ops_rows;
DROP TABLE temp.view_ops_counter;

-- What the view presents over the catalog, and what a pass takes in: the rows not taken in yet, few at any time.
CREATE INDEX view_ops_open ON view_ops(id) WHERE taken_ns IS NULL;

-- Whose operation of ops is the view's: a reprise of a command or of the daemon asks it of each it would resolve.
CREATE UNIQUE INDEX view_ops_by_op ON view_ops(op_id) WHERE op_id IS NOT NULL;
