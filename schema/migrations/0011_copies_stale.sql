-- The stale copies, by disk (specs.md §9): few, and none most of the time. Each pass asks which of them has no trash
-- job queued, to give it one again (§15.4), and without this index that question read every copy of the pool.
CREATE INDEX copies_stale ON copies(disk_id, file_id) WHERE state = 'stale';
