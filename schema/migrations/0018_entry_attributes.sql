-- What Atoll's own view presents of a folder, a symlink or an ignored file without waking its disk (specs.md §9,
-- §21.2): its size, as its filesystem gives it, and its owner, beside the mode and modification time it already
-- has. A walk records them; an entry a catalog made before this one holds is NULL in each until a walk finds it
-- again, and the view then presents what §21.2 says it does of an attribute the catalog lacks.
ALTER TABLE entries ADD COLUMN size INTEGER CHECK (size >= 0);
ALTER TABLE entries ADD COLUMN uid INTEGER CHECK (uid BETWEEN 0 AND 4294967295);
ALTER TABLE entries ADD COLUMN gid INTEGER CHECK (gid BETWEEN 0 AND 4294967295);
