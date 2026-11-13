-- The entries of the trash by path, then by operation (specs.md §9): the order its list is read in, a page at a time,
-- and the way an entry is found, by the path it left and the operation that sent it to the trash. Without this index
-- the list sorted the whole trash for each page, and each purge, restore and undone operation read every entry to
-- find its own (§26).
CREATE INDEX trash_by_path ON trash(lpath, op_id);
