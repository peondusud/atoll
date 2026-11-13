-- The operations of the journal by disk, then by the inode they name (specs.md §9): where Atoll moved a copy that a walk
-- no longer finds at its name, which a pass asks of the journal before it takes the copy for deleted (invariant 11).
-- Without it that question was put to the disk, by a walk of its whole trash and tmp/ at each pass that found a copy
-- gone. Partial, as an operation on a folder names no inode.
CREATE INDEX ops_by_key ON ops(disk_id, file_key) WHERE file_key IS NOT NULL;
