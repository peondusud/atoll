-- A copy's mode as its disk holds it: the permission bits, setuid, setgid and sticky, without the file type
-- (specs.md §9). It is compared with the copy's own row, as its times are, so that a chmod is applied to the other
-- copies and a difference that was already there is not (specs.md §15.4). NULL until known: the rows this finds
-- get it from the next look that finds them unchanged, as a chmod moves the change time.
ALTER TABLE copies ADD COLUMN mode INTEGER CHECK (mode BETWEEN 0 AND 4095);
