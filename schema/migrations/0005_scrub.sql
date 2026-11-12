-- Where the scrub of each disk stopped when its budget was spent (specs.md §9, §16): the copy it was reading and the
-- next chunk to read, with the size and times its row had then, so that the next scrub resumes there, and starts
-- the copy again if it changed since. A disk whose scrub ended between two copies has no row.
CREATE TABLE scrub_cursor(
  disk_id  TEXT PRIMARY KEY,
  file_id  INTEGER NOT NULL,
  chunk    INTEGER NOT NULL CHECK (chunk >= 0),
  size     INTEGER NOT NULL CHECK (size >= 0),
  mtime_ns INTEGER NOT NULL,
  ctime_ns INTEGER NOT NULL
) STRICT;
