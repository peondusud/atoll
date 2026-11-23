-- The jobs of a disk, by their kind (specs.md §9). A pass reads the folder jobs and the trash jobs of each disk several
-- times, and a reprise and the cleaning of tmp/ read the copies to make on a disk for the one that owns a part: each was
-- a walk of every job, 200,000 rows while a new pool makes its first replicas (§26). Each index is partial, on the one
-- kind its reads name, so that it holds those jobs alone and no query of another kind takes it. Without statistics, an
-- index on jobs(kind, dst_disk) would be SQLite's choice for the copies still to run, which it would then sort whole
-- for each page of them, and for the trash jobs of a file, which it would read among every trash job.
--
-- The folder jobs of a disk, in the order they were queued.
CREATE INDEX jobs_folders ON jobs(dst_disk) WHERE kind = 'rename';
-- The trash jobs of a disk, and that of a copy: its disk, then its file.
CREATE INDEX jobs_trash ON jobs(dst_disk, file_id) WHERE kind = 'trash';
-- The copies to make on a disk that hold a payload, among them every one that owns a part, which only its payload names
-- (§14.2): a copy queued holds none until its first operation, unless it stands in for a user copy or a stale one.
CREATE INDEX jobs_parts ON jobs(dst_disk) WHERE kind = 'replicate' AND payload IS NOT NULL;
