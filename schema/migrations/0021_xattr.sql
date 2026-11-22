-- Extended attributes of the user. namespace through Atoll's own view (specs.md §9, §21.2): view_ops.kind = xattr is
-- one set or removed on a user's behalf, on the disk the view presents a file from, or on every disk that holds a
-- folder, file_key the inode of the file; and ops.kind = xattr gives another copy of that file the attributes the one
-- changed holds, as a pass does. Kinds added to closed lists: SQLite changes a CHECK only by making each table again,
-- whose rows go back with their ids, the counters keeping the highest id ever given, as 0006_owner and 0020_view_link
-- do.
CREATE TABLE ops_new(
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  disk_id    TEXT NOT NULL,
  kind       TEXT NOT NULL CHECK (kind IN ('create', 'rename', 'trash', 'restore', 'repair', 'mkdir', 'rmdir', 'utimens', 'chmod', 'chown', 'xattr', 'link', 'unlink')),
  path       BLOB NOT NULL,
  path2      BLOB,
  file_key   TEXT,
  started_ns INTEGER NOT NULL,
  state      TEXT NOT NULL CHECK (state IN ('planned', 'done', 'undone')),
  link       INTEGER CHECK (link > 0)
) STRICT;

CREATE TEMP TABLE ops_rows AS SELECT id, disk_id, kind, path, path2, file_key, started_ns, state, link FROM ops;
CREATE TEMP TABLE ops_counter AS SELECT seq FROM sqlite_sequence WHERE name = 'ops';

DROP TABLE ops;
ALTER TABLE ops_new RENAME TO ops;

INSERT INTO ops(id, disk_id, kind, path, path2, file_key, started_ns, state, link)
  SELECT id, disk_id, kind, path, path2, file_key, started_ns, state, link FROM temp.ops_rows ORDER BY id;
UPDATE sqlite_sequence SET seq = max(seq, (SELECT seq FROM temp.ops_counter))
  WHERE name = 'ops' AND EXISTS (SELECT 1 FROM temp.ops_counter);
INSERT INTO sqlite_sequence(name, seq)
  SELECT 'ops', seq FROM temp.ops_counter WHERE NOT EXISTS (SELECT 1 FROM sqlite_sequence WHERE name = 'ops');

DROP TABLE temp.ops_rows;
DROP TABLE temp.ops_counter;

-- What crash recovery reads.
CREATE INDEX ops_planned ON ops(id) WHERE state = 'planned';
-- Whether the link guard ever made a link on a disk (0008_link_guard).
CREATE INDEX ops_by_link ON ops(disk_id) WHERE link IS NOT NULL;
-- The operations of the journal by disk, then by the inode they name (0012_ops_by_key).
CREATE INDEX ops_by_key ON ops(disk_id, file_key) WHERE file_key IS NOT NULL;

CREATE TABLE view_ops_new(
  id       INTEGER PRIMARY KEY AUTOINCREMENT,
  kind     TEXT NOT NULL CHECK (kind IN ('create', 'mkdir', 'symlink', 'write', 'xattr', 'rename', 'replace', 'link', 'unlink', 'rmdir')),
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
  CHECK (kind <> 'link' OR (folder = 0 AND file_key IS NOT NULL)),
  -- The attributes of a file are given from the copy of that inode, which a pass finds by it, wherever it went since.
  CHECK (kind <> 'xattr' OR folder = 1 OR file_key IS NOT NULL)
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
