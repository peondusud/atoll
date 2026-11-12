-- The alerts standing (specs.md §9, §23): one row per condition the daemon sent, by its key, so that a restart
-- sends again only what is due. A condition that ends loses its row.
CREATE TABLE alerts(
  key      TEXT PRIMARY KEY,
  first_ns INTEGER NOT NULL,            -- when the condition began
  sent_ns  INTEGER NOT NULL,            -- when it was last sent
  count    INTEGER NOT NULL CHECK (count >= 0)
) STRICT;
