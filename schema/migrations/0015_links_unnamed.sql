-- The links of the guard on a disk whose data nothing names, with neither a row nor a copy-gone event, as its last run
-- found them (specs.md §17): they stay, as the data may be someone's, and the alert link-unnamed and atoll status tell
-- of them (§23, §15.11). 0 until a run finds one, and again once a run finds none.
ALTER TABLE disks ADD COLUMN links_unnamed INTEGER NOT NULL DEFAULT 0 CHECK (links_unnamed >= 0);
