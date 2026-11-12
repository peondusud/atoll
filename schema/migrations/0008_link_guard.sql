-- The link guard (specs.md §17): an operation on the link Atoll keeps of a copy in <workdir>/links/, named by the id of
-- its file, which this column holds. With kind = link it makes that link from the copy at path; with kind = unlink it
-- removes it, as its data has another name; with kind = trash it moves it to the trash at path2, as a copy at path
-- would go. NULL for every other operation, whose names are below the root or in the trash. A column added rather
-- than the table made again: link and unlink were in the list of kinds from the start.
ALTER TABLE ops ADD COLUMN link INTEGER CHECK (link > 0);
