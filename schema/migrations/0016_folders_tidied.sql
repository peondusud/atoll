-- The last operation whose folders a run looked at for its own empty folders (specs.md §17.5): a run that writes looks
-- at the folders its operations left, then at those every operation done after this one left, which a run stopped
-- before its end could not look at. A catalog made before starts here, as nothing told it which runs ended.
INSERT INTO meta(key, value) SELECT 'folders_tidied_op', CAST(coalesce(max(id), 0) AS TEXT) FROM ops;
