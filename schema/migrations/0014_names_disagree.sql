-- The operations of a disk that its last reprise left planned as a name and its file's record disagree there, as
-- ntfs3 leaves them when a device fails between the syncs of a move's two folders (specs.md §17.2). Atoll concludes
-- nothing of them, and only a repair of the volume can: the alert ntfs-names and atoll status tell of it (§23, §15.11).
-- 0 until a reprise meets one, and again once a reprise resolves them all.
ALTER TABLE disks ADD COLUMN names_disagree INTEGER NOT NULL DEFAULT 0 CHECK (names_disagree >= 0);
