-- A conflict found while the file is corrupt (specs.md §4, §16). A file shows one state, corrupt first, and state is
-- all that remembers either: a file corrupt whose copies also turned out to differ kept nothing of the conflict, and
-- was ok once its corrupt copy was repaired, its copies still different. This holds the conflict, 1 only while the
-- file is corrupt; it becomes the file's state once no copy of it is corrupt, and any other state lets it go.
ALTER TABLE files ADD COLUMN conflict_held INTEGER NOT NULL DEFAULT 0 CHECK (conflict_held IN (0, 1));
