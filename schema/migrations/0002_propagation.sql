-- What propagation needs to remember between two looks (specs.md §15.12). Each is NULL for nearly
-- every row, so none of it costs anything until a change is seen.

-- Since when a look has found that the name no longer holds this copy, and what the last look found.
-- An edit in place starts no clock: found at all, gone_ns is NULL.
ALTER TABLE copies ADD COLUMN gone_ns INTEGER;
ALTER TABLE copies ADD COLUMN gone_kind TEXT CHECK (gone_kind IN ('moved', 'gone', 'uncertain', 'own'));

-- This copy has not followed its file's rename yet, and still sits at this path on its disk. So a
-- rename waiting for a disk is one column, and its target is always files.lpath, resolved when it
-- runs. Every reader of a copy's place reads COALESCE(old_lpath, files.lpath).
ALTER TABLE copies ADD COLUMN old_lpath BLOB;
ALTER TABLE copies ADD COLUMN old_lpath_key TEXT;

CREATE INDEX copies_old ON copies(disk_id, old_lpath) WHERE old_lpath IS NOT NULL;
CREATE INDEX copies_old_key ON copies(old_lpath_key) WHERE old_lpath_key IS NOT NULL;
-- What the decisions and the guard read.
CREATE INDEX copies_gone ON copies(disk_id) WHERE gone_ns IS NOT NULL;

-- A folder's identity: a rename keeps both, so they tell a folder renamed from files moved out one
-- by one (specs.md §15.12).
ALTER TABLE entries ADD COLUMN file_key TEXT;
ALTER TABLE entries ADD COLUMN btime_ns INTEGER;
