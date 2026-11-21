-- The journal of what Atoll's own view does on a user's behalf (specs.md §9, §21.2): each operation on one disk,
-- journaled before it runs, and presented over the catalog's rows until a pass has taken it in. A deletion, or the
-- replacement of a name a rename lands on, also sends a copy to the trash, an operation of ops that op_id names; no
-- reprise but the view's own resolves those. Rows taken in stay, as the history of what the view did; no foreign key
-- to disks, as in ops: the journal outlives a disk.
CREATE TABLE view_ops(
  id       INTEGER PRIMARY KEY AUTOINCREMENT,
  kind     TEXT NOT NULL CHECK (kind IN ('create', 'mkdir', 'symlink', 'write', 'rename', 'replace', 'unlink', 'rmdir')),
  disk_id  TEXT NOT NULL,
  lpath    BLOB NOT NULL,
  lpath2   BLOB,
  folder   INTEGER NOT NULL CHECK (folder IN (0, 1)),
  file_key TEXT,
  op_id    INTEGER REFERENCES ops(id),
  state    TEXT NOT NULL CHECK (state IN ('planned', 'done', 'undone')),
  at_ns    INTEGER NOT NULL,
  taken_ns INTEGER,
  CHECK ((kind IN ('rename', 'replace')) = (lpath2 IS NOT NULL)),
  CHECK (kind = 'unlink' OR (kind = 'replace') = (op_id IS NOT NULL))
) STRICT;

-- What the view presents over the catalog, and what a pass takes in: the rows not taken in yet, few at any time.
CREATE INDEX view_ops_open ON view_ops(id) WHERE taken_ns IS NULL;

-- Whose operation of ops is the view's: a reprise of a command or of the daemon asks it of each it would resolve.
CREATE UNIQUE INDEX view_ops_by_op ON view_ops(op_id) WHERE op_id IS NOT NULL;
