-- A write, or a new file, that Atoll's own view holds open (specs.md §9, §21.2; conformance/models/README.md, flaw 10):
-- view_ops.state = open, from the open until the view lets go of its last handle on the file, then done. A pass never
-- takes in a row open, and a transfer of the SSD cache never moves a copy one touches (§19). A value added to a closed
-- list: the table is made again, as 0023_view_move does, the rows going back with their ids, the counter keeping the
-- highest id ever given, and the indexes made again.
CREATE TABLE view_ops_new(
  id       INTEGER PRIMARY KEY AUTOINCREMENT,
  kind     TEXT NOT NULL CHECK (kind IN ('create', 'mkdir', 'symlink', 'write', 'xattr', 'rename', 'replace', 'link', 'unlink', 'rmdir', 'part', 'move')),
  disk_id  TEXT NOT NULL,
  lpath    BLOB NOT NULL,
  lpath2   BLOB,
  folder   INTEGER NOT NULL CHECK (folder IN (0, 1)),
  file_key TEXT,
  op_id    INTEGER REFERENCES ops(id),
  state    TEXT NOT NULL CHECK (state IN ('planned', 'done', 'undone', 'open')),
  at_ns    INTEGER NOT NULL,
  taken_ns INTEGER,
  disk2_id TEXT,
  uid      INTEGER CHECK (uid >= 0),
  pid      INTEGER CHECK (pid > 0),
  CHECK ((kind IN ('rename', 'replace', 'link')) = (lpath2 IS NOT NULL)),
  CHECK (kind IN ('unlink', 'part', 'move') OR (kind = 'replace') = (op_id IS NOT NULL)),
  CHECK (kind <> 'link' OR (folder = 0 AND file_key IS NOT NULL)),
  CHECK (kind <> 'xattr' OR folder = 1 OR file_key IS NOT NULL),
  CHECK (kind <> 'part' OR (folder = 0 AND op_id IS NOT NULL)),
  -- A move names the disk it leaves, the inode of the file there and the operation it runs, and is a file's.
  CHECK ((kind = 'move') = (disk2_id IS NOT NULL)),
  CHECK (kind <> 'move' OR (folder = 0 AND file_key IS NOT NULL AND op_id IS NOT NULL AND disk2_id <> disk_id)),
  -- Only a file written or made is held open.
  CHECK (state <> 'open' OR (kind IN ('create', 'write') AND folder = 0))
) STRICT;

CREATE TEMP TABLE view_ops_rows AS
  SELECT id, kind, disk_id, lpath, lpath2, folder, file_key, op_id, state, at_ns, taken_ns, disk2_id, uid, pid FROM view_ops;
CREATE TEMP TABLE view_ops_counter AS SELECT seq FROM sqlite_sequence WHERE name = 'view_ops';

DROP TABLE view_ops;
ALTER TABLE view_ops_new RENAME TO view_ops;

INSERT INTO view_ops(id, kind, disk_id, lpath, lpath2, folder, file_key, op_id, state, at_ns, taken_ns, disk2_id, uid, pid)
  SELECT id, kind, disk_id, lpath, lpath2, folder, file_key, op_id, state, at_ns, taken_ns, disk2_id, uid, pid
  FROM temp.view_ops_rows ORDER BY id;
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
