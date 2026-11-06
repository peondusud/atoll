-- The catalog of Atoll (specs.md §9). Plain SQL, applied by both versions. No pragma and no
-- transaction here: whoever applies this file does both (schema/README.md).
--
-- STRICT, so that a path written as text is refused rather than stored as TEXT, which Rust
-- could not read back when the bytes are not UTF-8 (specs.md §29.3). CHECK on the closed lists
-- of specs.md §29.3, so that neither version can store a state the other does not know.
--
-- All *_ns columns are integer Unix nanoseconds, UTC. Sizes are integer bytes.

CREATE TABLE meta(
  key   TEXT PRIMARY KEY,                -- pool_id, lpath_key_version (specs.md §9)
  value TEXT NOT NULL
) STRICT, WITHOUT ROWID;

-- A copy of the disks of the configuration, which wins (specs.md §10), rewritten at each start.
-- A retired disk stays, so that its identifier and its history are not lost.
CREATE TABLE disks(
  id                TEXT PRIMARY KEY,    -- human alias: "t1", "u1"
  fs_uuid           TEXT UNIQUE NOT NULL,-- ext4/XFS/btrfs UUID or 64-bit NTFS serial
  fs_type           TEXT NOT NULL CHECK (fs_type IN ('ext4', 'ntfs', 'xfs', 'btrfs')),
  device            TEXT,                -- physical disk seen at the last mount (topology check)
  mount_path        TEXT NOT NULL,
  root              TEXT NOT NULL,
  workdir           TEXT NOT NULL,
  role              TEXT NOT NULL CHECK (role IN ('data', 'cache')),
  position          INTEGER NOT NULL,    -- order in the configuration (tie-break, mergerfs branch order)
  domain            TEXT,
  tags              TEXT NOT NULL CHECK (json_valid(tags)),
  capacity_bytes    INTEGER CHECK (capacity_bytes >= 0),
  free_bytes        INTEGER CHECK (free_bytes >= 0),
  time_precision_ns INTEGER CHECK (time_precision_ns > 0), -- 1 (ext4, XFS, btrfs), 100 (NTFS), 10^9 (ext4, 128-byte inodes)
  state             TEXT NOT NULL CHECK (state IN ('online', 'standby', 'missing', 'readonly', 'draining', 'failed', 'retired')),
  last_seen_ns      INTEGER,
  smart_json        TEXT
) STRICT;

-- One row per managed file. Its chunk hashes are in file_chunks: SQLite rewrites a whole row on
-- each UPDATE, so a state change would rewrite up to 800 KB of hashes (measured, specs.md §9).
-- AUTOINCREMENT: a file id names Atoll/links/<file id> (specs.md §17) and must never come back.
CREATE TABLE files(
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  lpath           BLOB UNIQUE NOT NULL,  -- exact logical path (bytes; WTF-8 on Windows)
  lpath_key       TEXT,                  -- NFD, NTFS upcase, NFC (specs.md §15.8); collisions only; NULL if not UTF-8
  size            INTEGER NOT NULL CHECK (size >= 0), -- current version
  mtime_ns        INTEGER NOT NULL,      -- the original's (specs.md §7)
  blake3          BLOB CHECK (length(blake3) = 32), -- b3sum-compatible; NULL until hashed
  replicas_target INTEGER NOT NULL CHECK (replicas_target BETWEEN 0 AND 2),
  original_disk   TEXT REFERENCES disks(id), -- cached result of specs.md §7
  rule            TEXT,                  -- the label of the rule; NULL when no rule matches
  mode            TEXT NOT NULL CHECK (mode IN ('immediate', 'deferred', 'scheduled', 'manual')),
  state           TEXT NOT NULL CHECK (state IN ('ok', 'pending', 'degraded', 'unprotected', 'unavailable', 'corrupt', 'conflict', 'name-incompatible', 'name-collision')),
  first_seen_ns   INTEGER NOT NULL,
  updated_ns      INTEGER NOT NULL
) STRICT;

CREATE INDEX files_by_key ON files(lpath_key) WHERE lpath_key IS NOT NULL;

-- The BLAKE3 hash of each 4 MiB chunk, concatenated (specs.md §16). An empty file has no chunks,
-- so no row: files.blake3 says whether a file is hashed.
CREATE TABLE file_chunks(
  file_id INTEGER PRIMARY KEY REFERENCES files(id) ON DELETE CASCADE,
  chunks  BLOB NOT NULL CHECK (length(chunks) > 0 AND length(chunks) % 32 = 0)
) STRICT;

-- PRIMARY KEY: never two copies of a file on one disk (R-06). A file with copies cannot be
-- deleted: no foreign key here cascades, so a bug cannot lose this knowledge in passing.
CREATE TABLE copies(
  file_id       INTEGER NOT NULL REFERENCES files(id),
  disk_id       TEXT NOT NULL REFERENCES disks(id),
  file_key      TEXT,                    -- inode (Linux) or NTFS file ID
  nlink         INTEGER,                 -- hard links, Atoll's own link-guard link excluded
  size          INTEGER CHECK (size >= 0), -- as this disk holds it (specs.md §14)
  mtime_ns      INTEGER,                 -- as this disk holds it (specs.md §14)
  ctime_ns      INTEGER,                 -- change time, recorded after each Atoll operation
  btime_ns      INTEGER,                 -- creation time; NULL if absent (specs.md §7)
  created_by    TEXT NOT NULL CHECK (created_by IN ('user', 'atoll')),
  first_seen_ns INTEGER NOT NULL,
  verified_ns   INTEGER,
  state         TEXT NOT NULL CHECK (state IN ('ok', 'pending', 'stale', 'corrupt', 'unavailable', 'surplus')),
  PRIMARY KEY(file_id, disk_id)
) STRICT, WITHOUT ROWID;

CREATE INDEX copies_by_disk ON copies(disk_id);

-- Everything else the view must list.
CREATE TABLE entries(
  lpath      BLOB NOT NULL,
  lpath_key  TEXT,                       -- as in files
  disk_id    TEXT NOT NULL REFERENCES disks(id),
  kind       TEXT NOT NULL CHECK (kind IN ('dir', 'symlink', 'ignored')),
  mode       INTEGER,
  mtime_ns   INTEGER,
  target     BLOB,                       -- symlink target
  created_by TEXT NOT NULL CHECK (created_by IN ('user', 'atoll')),
  PRIMARY KEY(lpath, disk_id)
) STRICT, WITHOUT ROWID;

CREATE INDEX entries_by_disk ON entries(disk_id);
CREATE INDEX entries_by_key ON entries(lpath_key) WHERE lpath_key IS NOT NULL;

-- Atoll's own filesystem operations, journaled before each runs. No foreign key to disks: the
-- journal outlives a disk. AUTOINCREMENT: an operation's id names its folder in the trash
-- (specs.md §17), so it must never come back after the row is pruned.
CREATE TABLE ops(
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  disk_id    TEXT NOT NULL,
  kind       TEXT NOT NULL CHECK (kind IN ('create', 'rename', 'trash', 'restore', 'repair', 'mkdir', 'rmdir', 'utimens', 'chmod', 'link', 'unlink')),
  path       BLOB NOT NULL,
  path2      BLOB,
  file_key   TEXT,
  started_ns INTEGER NOT NULL,
  state      TEXT NOT NULL CHECK (state IN ('planned', 'done', 'undone'))
) STRICT;

-- What crash recovery reads.
CREATE INDEX ops_planned ON ops(id) WHERE state = 'planned';

-- No foreign key to disks, as in ops.
CREATE TABLE trash(
  id         INTEGER PRIMARY KEY,
  lpath      BLOB NOT NULL,
  disk_id    TEXT NOT NULL,
  trash_path BLOB NOT NULL,              -- location inside Atoll/trash/
  op_id      INTEGER NOT NULL REFERENCES ops(id),
  size       INTEGER CHECK (size >= 0),
  blake3     BLOB CHECK (length(blake3) = 32),
  reason     TEXT NOT NULL CHECK (reason IN ('deleted', 'overwritten', 'repaired', 'quarantined', 'surplus', 'rebalance', 'renamed')),
  deleted_ns INTEGER NOT NULL,
  expires_ns INTEGER NOT NULL
) STRICT;

CREATE INDEX trash_by_expiry ON trash(expires_ns);

CREATE TABLE jobs(
  id            INTEGER PRIMARY KEY,
  kind          TEXT NOT NULL CHECK (kind IN ('replicate', 'rename', 'trash', 'purge', 'scrub', 'repair', 'refresh', 'transfer')),
  file_id       INTEGER REFERENCES files(id) ON DELETE CASCADE,
  src_disk      TEXT REFERENCES disks(id),
  dst_disk      TEXT REFERENCES disks(id),
  payload       TEXT,                    -- JSON, what the job needs beyond these columns
  priority      INTEGER NOT NULL DEFAULT 0,
  not_before_ns INTEGER,
  attempts      INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
  state         TEXT NOT NULL CHECK (state IN ('queued', 'running', 'done', 'failed', 'canceled')),
  reason        TEXT
) STRICT;

CREATE INDEX jobs_to_do ON jobs(priority DESC, id) WHERE state IN ('queued', 'running');
-- So that deleting a file finds its jobs without scanning them all.
CREATE INDEX jobs_by_file ON jobs(file_id) WHERE file_id IS NOT NULL;

-- The hash of each chunk a copy has read so far, for resuming it (specs.md §14). A row each, as
-- rewriting them all in jobs.payload would write up to 1.6 MB of JSON per chunk.
CREATE TABLE job_chunks(
  job_id INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
  idx    INTEGER NOT NULL CHECK (idx >= 0),
  hash   BLOB NOT NULL CHECK (length(hash) = 32),
  PRIMARY KEY(job_id, idx)
) STRICT, WITHOUT ROWID;

-- kind has no CHECK: each module adds its own, and this is the largest log to rebuild. The list
-- lives in the specification (specs.md §29.3).
CREATE TABLE events(
  id    INTEGER PRIMARY KEY,
  ts_ns INTEGER NOT NULL,
  level TEXT NOT NULL CHECK (level IN ('info', 'warning', 'error')),
  kind  TEXT NOT NULL,
  data  TEXT                             -- JSON
) STRICT;

CREATE INDEX events_by_time ON events(ts_ns);
