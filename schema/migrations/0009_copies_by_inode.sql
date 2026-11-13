-- The copies of a disk by their inode (specs.md §9). What the names of a file record when one of them changes, and what
-- those names are, are looked up by inode: once for each operation on a file with more than one name, as every copy has
-- with the link guard on (§17). Without this index each lookup read every copy of the disk, and the first pass with the
-- guard on, which makes a link for each copy, had not ended after 8 hours on 200,000 files a disk (§26). Partial, as a
-- pending copy has no inode, and so that a query naming no inode never takes it for an index of the disk.
CREATE INDEX copies_by_inode ON copies(disk_id, file_key) WHERE file_key IS NOT NULL;
