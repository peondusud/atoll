-- When the disk's own marker was first seen, Atoll/disk.toml naming this pool, this disk and this filesystem: by a
-- confirmation, by disk add once it wrote it, or by catalog rebuild (specs.md §9, §10). NULL until one did, so that a
-- pool from before keeps working as it did. Once set, a confirmation of a session that may write that finds no marker
-- makes the disk readonly, no-marker: a filesystem with this disk's identifier and none of its marker, as a clone made
-- before disk add and mounted in its place, is not taken for it. A disk whose id comes back with another identifier
-- is another row, which starts with none (§9).
ALTER TABLE disks ADD COLUMN marker_seen_ns INTEGER;
