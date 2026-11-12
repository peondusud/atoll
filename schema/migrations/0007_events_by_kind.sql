-- The guards read the events log at every pass, for each disk (specs.md §15.6): the last answer to each, the guard that
-- stands since, and what each counts within its window. With only events_by_time, each but the counts went over the
-- whole log, which only grows (§9): a pass with nothing to do took 0.8 s over a million events, against 64 ms. By kind
-- then time, each goes over the events of its kinds, and a count over its window of them.
CREATE INDEX events_by_kind ON events(kind, ts_ns);
