-- A copy's owner as its disk holds it: its user and group ids (specs.md §9). It is compared with the copy's own row,
-- as its mode is, so that a chown is applied to the other copies and a difference that was already there is not
-- (specs.md §15.4). NULL until known: the rows this finds get it from the next look that finds them unchanged, as a
-- chown moves the change time.
ALTER TABLE copies ADD COLUMN uid INTEGER CHECK (uid BETWEEN 0 AND 4294967295);
ALTER TABLE copies ADD COLUMN gid INTEGER CHECK (gid BETWEEN 0 AND 4294967295);

-- The operation chown applies an owner (specs.md §15.4). SQLite changes a CHECK only by making the table again. The
-- foreign keys are checked at the commit (specs.md §9): the rows go back once the new table has the old one's name,
-- so that each trash entry finds its operation again. The counter keeps the highest id ever given, which names a
-- folder of the trash and a part of tmp/, so that none comes back (specs.md §17).
CREATE TABLE ops_new(
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  disk_id    TEXT NOT NULL,
  kind       TEXT NOT NULL CHECK (kind IN ('create', 'rename', 'trash', 'restore', 'repair', 'mkdir', 'rmdir', 'utimens', 'chmod', 'chown', 'link', 'unlink')),
  path       BLOB NOT NULL,
  path2      BLOB,
  file_key   TEXT,
  started_ns INTEGER NOT NULL,
  state      TEXT NOT NULL CHECK (state IN ('planned', 'done', 'undone'))
) STRICT;

CREATE TEMP TABLE ops_rows AS SELECT id, disk_id, kind, path, path2, file_key, started_ns, state FROM ops;
CREATE TEMP TABLE ops_counter AS SELECT seq FROM sqlite_sequence WHERE name = 'ops';

DROP TABLE ops;
ALTER TABLE ops_new RENAME TO ops;

INSERT INTO ops(id, disk_id, kind, path, path2, file_key, started_ns, state)
  SELECT id, disk_id, kind, path, path2, file_key, started_ns, state FROM temp.ops_rows ORDER BY id;
UPDATE sqlite_sequence SET seq = max(seq, (SELECT seq FROM temp.ops_counter))
  WHERE name = 'ops' AND EXISTS (SELECT 1 FROM temp.ops_counter);
INSERT INTO sqlite_sequence(name, seq)
  SELECT 'ops', seq FROM temp.ops_counter WHERE NOT EXISTS (SELECT 1 FROM sqlite_sequence WHERE name = 'ops');

DROP TABLE temp.ops_rows;
DROP TABLE temp.ops_counter;

-- What crash recovery reads.
CREATE INDEX ops_planned ON ops(id) WHERE state = 'planned';
