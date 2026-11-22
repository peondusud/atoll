-- Who asked each operation of Atoll's own view (specs.md §9, §21.2): uid the account and pid the thread, as the kernel
-- named the caller of its FUSE request, so that what the view did on a user's behalf says which. NULL for a row written
-- outside a request and for every row from before this migration; pid NULL where the kernel named no thread. Nothing
-- decides from them: they are the journal's record of who asked, as the daemon's log of an answer to a guard is.
ALTER TABLE view_ops ADD COLUMN uid INTEGER CHECK (uid >= 0);
ALTER TABLE view_ops ADD COLUMN pid INTEGER CHECK (pid > 0);
