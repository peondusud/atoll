-- Copies written in parallel by Atoll's own view (specs.md §9, §12.3, §21.2): view_ops.kind = part is a part the view
-- writes into <workdir>/tmp/ of a replica's target, disk_id, as a new file in immediate mode is written, op_id the
-- operation create of ops that names it, which no reprise but the view's own resolves. Planned while the program
-- writes, done once it closed the file and the part holds what the original does, and taken in once a pass's job adopts
-- the part, or the pass drops it. A kind added to a closed list: the table is made again, as 0020_view_link does.
CREATE TABLE view_ops_new(
  id       INTEGER PRIMARY KEY AUTOINCREMENT,
  kind     TEXT NOT NULL CHECK (kind IN ('create', 'mkdir', 'symlink', 'write', 'xattr', 'rename', 'replace', 'link', 'unlink', 'rmdir', 'part')),
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
  CHECK (kind IN ('unlink', 'part') OR (kind = 'replace') = (op_id IS NOT NULL)),
  CHECK (kind <> 'link' OR (folder = 0 AND file_key IS NOT NULL)),
  CHECK (kind <> 'xattr' OR folder = 1 OR file_key IS NOT NULL),
  -- A part names the operation create of its target, and is a file's.
  CHECK (kind <> 'part' OR (folder = 0 AND op_id IS NOT NULL))
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
