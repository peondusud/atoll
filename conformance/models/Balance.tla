------------------------------- MODULE Balance -------------------------------
(* atoll balance (specs.md §12.4, §13.3, §13.4, §14.2, §15.13, §17.1, §17.3; invariants 1, 6, 9, 13 and 18): a model of
   its own. One file, a copy at its path on some of three or four disks, the file at its level at the start, two of the
   disks maybe filesystems of one physical disk (Shared, R-06). The file's rule says tags_mode = "strict": the disks it
   lists are Listed, and an Atoll copy on another is misplaced (§13.3), as a change of rule or of a disk's tags leaves it.

   What it keeps, for each disk: whether its device answers and its mount is read-only; the version the copy at its
   path holds (0 none), the name it sits at (a user may rename it), whether Atoll placed it, the part of a copy in its
   tmp/, what its trash holds, and its room past min_free, in copies. The catalog: the file's version (0 once deleted),
   name and state (ok, conflict); each copy's row (none, ok, surplus, pending, stale), who made it, the version it
   records and whether it was verified; what the last check of each disk found (seen, read-only, room); whether it is
   draining; and the operation planned on it (a copy's create, or a move to the trash). Atoll is one process: the balance
   holds the catalog's lock through a run's steps, and the passes come in between (§13.3, §15.13):
   - atoll balance, or the daemon's, by hand or inside balance.schedule: its plan, made with each misplaced copy counting
     for nothing, an oracle that picks any target the hard constraints of §12.4 allow (listed, writable, no copy of the
     file there nor on its physical disk, room past min_free) and any source, places one copy there (§13.4, invariant
     18); the copy pipeline (§14.2): the operation journaled and the part written, min_free checked at the write
     (jobs.room), the source looked at again, the part placed without replacing anything at the file's name as the
     catalog has it then, the commit; with --move-originals, the commit makes the new copy the user copy and the old one
     an Atoll copy; the plan made again, which marks the old copy surplus; then the old copy retired as prune would
     (trash.Session.Retire, §17.3): the copies that stay, ok, verified, looked at and found as their rows say, reach the
     file's level, and the file is not in conflict; the move journaled, then the copy looked at right before it moves to
     the trash (§17.1, step 4). A run with no copy to make, as one stopped after a copy was made leaves it, goes straight
     to the plan and the retire;
   - a pass (§15.4, §15.10, §15.12, §13.4), each part of it a step of its own between the balance's runs: the check of
     each disk, recorded; a copy changed against its row taken in as a modification, the other copies stale; two
     changed, a conflict; a copy gone from its name a deletion, sent to the trash from every other copy, all unchanged
     since in sync; a copy found at another name a rename, followed on the other disks; a stale copy refreshed from a
     source, what it held to the trash; a pending copy made, or canceled when it no longer fits, counts or has a disk to
     go to; a copy made for a file below its level (the plan of the pool, its strict setting kept); the marks;
   - the reprise of what is planned on a disk that answers (§17.2).
   A user edits a copy in place, deletes it or renames it, directly or through the view, which writes to a disk as a
   user does; the world loses a disk and brings it back, mounts one read-only, drains one (disk remove --evacuate), or
   fills one past its room with other files; Atoll's process may be killed between any two steps.

   What it leaves out: a second file, so that room is copies, and colocate and the equalizing of the free space;
   domains and spread_mode (the plan.json vectors' and the property tests'); --move-originals' hand-over of a user copy
   no copy stands in for; first_seen, the copies' times and their precision (Scrub.tla's); the guards, as a pass that
   does not run (Guards.tla's); the pipeline's own faults (Replication.tla's); a conflict's resolution (Conflicts.tla's);
   and the trash's purges (Trash.tla's): see README.md. *)
EXTENDS Integers, FiniteSets, TLC

CONSTANTS
    NDisks,     \* 3 or 4
    Shared,     \* two disks that are filesystems of one physical disk (R-06), or {}
    Listed,     \* the disks the file's strict setting lists
    Level,      \* replicas + 1
    UserAt,     \* the disks holding a user copy at the start
    AtollAt,    \* the disks holding an Atoll copy at the start
    Full,       \* the disks with no room past min_free at the start; each other has room for one copy
    UserActs,   \* some of "edit", "delete", "rename", "create"
    UserDisks,
    MaxUser,
    WorldActs,  \* some of "lose", "readonly", "drain", "fill"
    WorldDisks,
    MaxWorld,
    AtollActs,  \* some of "balance", "originals", "pass", "kill"
    MaxKill,
    Fixes       \* the remedies proposed for the flaws found here and still open, by name (README.md); {} is specs.md as it is

Disks == 1..NDisks
FixNames == {}
Holders == UserAt \cup AtollAt
\* the physical disk a filesystem lives on
Phys(d) == IF d \in Shared THEN 0 ELSE d

ASSUME /\ NDisks \in 3..4 /\ Shared \subseteq Disks /\ Cardinality(Shared) \in {0, 2} /\ Listed \subseteq Disks
       /\ Level \in 1..2 /\ UserAt \subseteq Disks /\ AtollAt \subseteq Disks /\ UserAt \cap AtollAt = {}
       /\ Cardinality(Holders) = Level /\ UserAt # {} /\ (Shared # {} => ~(Shared \subseteq Holders)) /\ Full \subseteq Disks
       /\ UserActs \subseteq {"edit", "delete", "rename", "create"} /\ UserDisks \subseteq Disks /\ MaxUser \in Nat
       /\ WorldActs \subseteq {"lose", "readonly", "drain", "fill"} /\ WorldDisks \subseteq Disks /\ MaxWorld \in Nat
       /\ AtollActs \subseteq {"balance", "originals", "pass", "kill"} /\ MaxKill \in Nat
       /\ Fixes \subseteq FixNames

VARIABLES
    w,      \* the world: [up, ro |-> its mount read-only, at |-> the version at its path, 0 none, nm |-> the name it sits
            \*   at, ours |-> placed by Atoll's pipeline, part |-> the version of a part in tmp/, tr |-> the versions its
            \*   trash holds, room |-> copies it has room for past min_free, nv |-> the next version a user writes,
            \*   users, acts, kills]
    cat,    \* the catalog: [want |-> the file's version, 0 once deleted, nm |-> its name, fst |-> ok, conflict; cs |->
            \*   each copy's row, by |-> who made it, ver |-> the version it records, vfd |-> verified; seen, ro, free
            \*   |-> what the last check found of each disk; drain; op |-> [k |-> none, create, trash; v |-> version]]
    pc,     \* the balance: [ph, o |-> the misplaced copy, t |-> the target, s |-> the source, v |-> the version copied,
            \*   orig |-> --move-originals]
    ghost   \* [flags, last |-> the version a user wrote last, 0 none, deleted |-> a user deleted the file, since |-> a
            \*   user or the world acted since the retire's look, users0 |-> the user copies at the start]

vars == <<w, cat, pc, ghost>>

Idle == [ph |-> "idle", o |-> 0, t |-> 0, s |-> 0, v |-> 0, orig |-> FALSE]
NoOp == [k |-> "none", v |-> 0]

Init ==
    /\ w = [up |-> [d \in Disks |-> TRUE], ro |-> [d \in Disks |-> FALSE],
            at |-> [d \in Disks |-> IF d \in Holders THEN 1 ELSE 0], nm |-> [d \in Disks |-> 1],
            ours |-> [d \in Disks |-> d \in AtollAt], part |-> [d \in Disks |-> 0], tr |-> [d \in Disks |-> {}],
            room |-> [d \in Disks |-> IF d \in Full THEN 0 ELSE 1], nv |-> 2, users |-> 0, acts |-> 0, kills |-> 0]
    /\ cat = [want |-> 1, nm |-> 1, fst |-> "ok",
              cs |-> [d \in Disks |-> IF d \in Holders THEN "ok" ELSE "none"],
              by |-> [d \in Disks |-> IF d \in UserAt THEN "user" ELSE "atoll"],
              ver |-> [d \in Disks |-> IF d \in Holders THEN 1 ELSE 0], vfd |-> [d \in Disks |-> d \in Holders],
              seen |-> [d \in Disks |-> TRUE], ro |-> [d \in Disks |-> FALSE],
              free |-> [d \in Disks |-> IF d \in Full THEN 0 ELSE 1], drain |-> [d \in Disks |-> FALSE],
              op |-> [d \in Disks |-> NoOp]]
    /\ pc = Idle
    /\ ghost = [flags |-> {}, last |-> 0, deleted |-> FALSE, since |-> FALSE, users0 |-> Cardinality(UserAt)]

-----------------------------------------------------------------------------
First(S) == CHOOSE x \in S : \A y \in S : x <= y
Flag(f) == ghost' = [ghost EXCEPT !.flags = @ \cup f]

(* The plan (§13.4), of a catalog c: a copy counts when it is ok or surplus, on a disk not draining, one missing
   within its grace included; the copies kept, the level's number of them, a copy not on a missing disk first, then a
   user copy, then one on a listed disk, then by position. *)
CountsIn(c, d) == c.cs[d] \in {"ok", "surplus"} /\ ~c.drain[d]
RankIn(c, d) == (IF c.seen[d] THEN 0 ELSE 8) + (IF c.by[d] = "user" THEN 0 ELSE 4) + (IF d \in Listed THEN 0 ELSE 2)
BeforeIn(c, d, e) == RankIn(c, d) < RankIn(c, e) \/ (RankIn(c, d) = RankIn(c, e) /\ d < e)
KeptIn(c) == {d \in Disks : CountsIn(c, d) /\ Cardinality({e \in Disks : CountsIn(c, e) /\ BeforeIn(c, e, d)}) < Level}
(* the marks: a copy kept ok, one that counts and is not kept surplus; a pending copy beyond the level canceled, but
   one whose job is running, its operation planned: only the reprise of its disk can say what is at its target (§13.4,
   flaw 2) *)
Marked(c) == [d \in Disks |-> CASE CountsIn(c, d) -> IF d \in KeptIn(c) THEN "ok" ELSE "surplus"
                                [] c.cs[d] = "pending" /\ c.op[d].k = "none" /\ Cardinality(KeptIn(c)) >= Level -> "none"
                                [] OTHER -> c.cs[d]]

Writable(d) == cat.seen[d] /\ ~cat.ro[d] /\ ~cat.drain[d]
Pendings == {d \in Disks : cat.cs[d] = "pending" /\ Writable(d)}
(* §12.4's hard constraints, by the catalog: listed (tags_mode strict), writable, no copy of the file there nor on its
   physical disk in any state, room past min_free, nothing planned there *)
Candidate(t) == /\ t \in Listed /\ Writable(t) /\ cat.cs[t] = "none" /\ cat.free[t] >= 1 /\ cat.op[t].k = "none"
                /\ \A e \in Disks \ {t} : cat.cs[e] # "none" => Phys(e) # Phys(t)
(* a source: ok or surplus, on a disk the last check found, recording the file's version *)
Source(s) == cat.cs[s] \in {"ok", "surplus"} /\ cat.seen[s] /\ cat.ver[s] = cat.want
Misplaced(d, orig) == cat.cs[d] \in {"ok", "surplus"} /\ d \notin Listed /\ (cat.by[d] = "atoll" \/ orig)

(* what a look finds, by the world: the copy at the file's name, as its row says *)
AtName(d) == w.at[d] # 0 /\ w.nm[d] = cat.nm
Unchanged(d) == w.up[d] /\ AtName(d) /\ w.at[d] = cat.ver[d]
NoOps == \A d \in Disks : cat.op[d].k = "none" \/ ~w.up[d]
(* the last check of every disk is what the world is: a balance run's session checks every disk as it opens *)
Checked == \A d \in Disks : cat.seen[d] = w.up[d] /\ (w.up[d] => cat.ro[d] = w.ro[d] /\ cat.free[d] = w.room[d])

-----------------------------------------------------------------------------
(* atoll balance, and the daemon's: the plan of the file with its misplaced copies counting for nothing, one copy to
   make where the setting wants it, or none, as a run stopped after a copy was made leaves it. *)
BalStart(orig) ==
    /\ "balance" \in AtollActs /\ (orig => "originals" \in AtollActs)
    /\ pc.ph = "idle" /\ cat.want # 0 /\ cat.fst = "ok" /\ NoOps /\ Checked
    /\ \E o \in Disks : Misplaced(o, orig)
    /\ LET M == {d \in Disks : Misplaced(d, orig)}
           U == {d \in M : cat.by[d] = "user"}
           \* a user copy first, with --move-originals, by position (jobs.usersFirst)
           o == IF U # {} THEN First(U) ELSE First(M)
           good == {d \in Disks : CountsIn(cat, d) /\ ~Misplaced(d, orig)}
       IN IF Cardinality(good) + Cardinality(Pendings) < Level
          THEN \E t \in Disks, s \in Disks :
                 /\ Candidate(t) /\ Source(s)
                 /\ cat' = [cat EXCEPT !.cs[t] = "pending", !.by[t] = "atoll", !.ver[t] = 0, !.vfd[t] = FALSE]
                 /\ pc' = [Idle EXCEPT !.ph = "part", !.o = o, !.t = t, !.s = s, !.orig = orig]
          ELSE \* no copy to make: an Atoll copy goes once the plan marks it; a user copy's hand-over is left out
               /\ cat.by[o] = "atoll"
               /\ pc' = [Idle EXCEPT !.ph = "settle", !.o = o, !.orig = orig]
               /\ UNCHANGED cat
    /\ UNCHANGED <<w, ghost>>

(* The copy pipeline (§14.2): the operation journaled and the part written from the source, min_free checked as it
   writes; a target lost, read-only or full, or a source lost or changed, stops the copy, the row pending for the next
   plan. *)
BalPart ==
    /\ pc.ph = "part"
    /\ LET t == pc.t  s == pc.s IN
       /\ IF ~w.up[t] \/ ~Unchanged(s) \/ w.ro[t] \/ w.room[t] < 1
          THEN /\ pc' = Idle /\ UNCHANGED <<w, cat>>
          ELSE /\ w' = [w EXCEPT !.part[t] = w.at[s], !.room[t] = @ - 1]
               /\ cat' = [cat EXCEPT !.op[t] = [k |-> "create", v |-> w.at[s]]]
               /\ pc' = [pc EXCEPT !.ph = "look", !.v = w.at[s]]
       /\ UNCHANGED ghost

(* The part read back, and the source looked at again: changed, the part goes and the operation is undone. *)
BalLook ==
    /\ pc.ph = "look"
    /\ LET t == pc.t  s == pc.s IN
       /\ IF ~w.up[t]
          THEN /\ pc' = Idle /\ UNCHANGED <<w, cat>>   \* planned, for the reprise
          ELSE IF ~Unchanged(s) \/ w.at[s] # pc.v
          THEN /\ w' = [w EXCEPT !.part[t] = 0, !.room[t] = @ + 1]
               /\ cat' = [cat EXCEPT !.op[t] = NoOp]
               /\ pc' = Idle
          ELSE /\ pc' = [pc EXCEPT !.ph = "place"] /\ UNCHANGED <<w, cat>>
       /\ UNCHANGED ghost

(* The part placed without replacing anything, at the file's name as the catalog has it now (invariant 7). *)
BalPlace ==
    /\ pc.ph = "place"
    /\ LET t == pc.t IN
       /\ IF ~w.up[t]
          THEN /\ pc' = Idle /\ UNCHANGED <<w, cat>>
          ELSE IF w.at[t] # 0
          THEN /\ w' = [w EXCEPT !.part[t] = 0, !.room[t] = @ + 1]   \* the name taken: nothing replaced
               /\ cat' = [cat EXCEPT !.op[t] = NoOp]
               /\ pc' = Idle
          ELSE /\ w' = [w EXCEPT !.at[t] = w.part[t], !.nm[t] = cat.nm, !.ours[t] = TRUE, !.part[t] = 0]
               /\ pc' = [pc EXCEPT !.ph = "commit"]
               /\ UNCHANGED cat
       /\ UNCHANGED ghost

(* The commit: the copy ok and verified; with --move-originals, the new copy the user copy, the old one an Atoll copy,
   in the same transaction (§13.3). *)
BalCommit ==
    /\ pc.ph = "commit"
    /\ LET t == pc.t  o == pc.o  hand == pc.orig /\ cat.by[o] = "user" IN
       /\ cat' = [cat EXCEPT !.cs[t] = "ok", !.ver[t] = pc.v, !.vfd[t] = TRUE, !.op[t] = NoOp,
                             !.by = [d \in Disks |-> CASE hand /\ d = t -> "user"
                                                       [] hand /\ d = o -> "atoll"
                                                       [] OTHER -> cat.by[d]]]
       /\ pc' = [pc EXCEPT !.ph = "settle"]
    /\ UNCHANGED <<w, ghost>>

(* The plan of the pool, made again: the old copy surplus, as the new one stands in for it. *)
BalSettle ==
    /\ pc.ph = "settle"
    /\ cat' = [cat EXCEPT !.cs = Marked(cat)]
    /\ pc' = [pc EXCEPT !.ph = "retire"]
    /\ UNCHANGED <<w, ghost>>

(* The retire, as prune would (trash.Session.Retire, §17.3): the old copy surplus and an Atoll copy; the copies that
   stay, ok, verified, looked at and found as their rows say, reach the level; the file not in conflict. Then the move
   journaled. *)
Stays(o) == {e \in Disks \ {o} : cat.cs[e] = "ok" /\ cat.vfd[e] /\ Unchanged(e)}
BalRetire ==
    /\ pc.ph = "retire"
    /\ LET o == pc.o IN
       IF cat.cs[o] = "surplus" /\ cat.by[o] = "atoll" /\ cat.fst = "ok" /\ Cardinality(Stays(o)) >= Level
       THEN /\ cat' = [cat EXCEPT !.op[o] = [k |-> "trash", v |-> cat.ver[o]]]
            /\ pc' = [pc EXCEPT !.ph = "move"]
            /\ ghost' = [ghost EXCEPT !.since = FALSE]
            /\ UNCHANGED w
       ELSE /\ pc' = Idle /\ UNCHANGED <<w, cat, ghost>>   \* it stays, surplus: below-level, changed

(* §17.1, step 4: the copy looked at right before it moves; changed, nothing moves and the operation is undone. *)
BalMove ==
    /\ pc.ph = "move"
    /\ LET o == pc.o
           left == {e \in Disks \ {o} : w.up[e] /\ w.at[e] = cat.want}
       IN IF ~w.up[o]
          THEN /\ pc' = Idle /\ UNCHANGED <<w, cat, ghost>>   \* planned, for the reprise
          ELSE IF ~Unchanged(o)
          THEN /\ cat' = [cat EXCEPT !.op[o] = NoOp]
               /\ pc' = Idle /\ UNCHANGED <<w, ghost>>
          ELSE /\ w' = [w EXCEPT !.tr[o] = @ \cup {w.at[o]}, !.at[o] = 0, !.ours[o] = FALSE]
               /\ cat' = [cat EXCEPT !.cs[o] = "none", !.op[o] = NoOp, !.ver[o] = 0, !.vfd[o] = FALSE]
               /\ Flag((IF ~ghost.since /\ Cardinality(left) < Level THEN {"below"} ELSE {})
                       \cup (IF cat.by[o] = "user" THEN {"user-moved"} ELSE {})
                       \cup (IF ~ghost.deleted /\ w.at[o] = (IF ghost.last = 0 THEN 1 ELSE ghost.last)
                                /\ \A e \in Disks \ {o} : w.at[e] # w.at[o] THEN {"only-version"} ELSE {}))
               /\ pc' = Idle

-----------------------------------------------------------------------------
(* The passes, each part a step of its own between the balance's runs, once the reprise ran on the disks that answer. *)
PassReady == "pass" \in AtollActs /\ pc.ph = "idle" /\ NoOps

(* The check of each disk, recorded: found, read-only, its room; a pass's, and the one each balance run's session
   makes as it opens (trash.Open, watch.CheckDisks), so that a run plans with what it found. *)
PassCheck ==
    /\ ("pass" \in AtollActs \/ "balance" \in AtollActs) /\ pc.ph = "idle"
    /\ ~Checked
    /\ cat' = [cat EXCEPT !.seen = w.up,
                          !.ro = [d \in Disks |-> IF w.up[d] THEN w.ro[d] ELSE cat.ro[d]],
                          !.free = [d \in Disks |-> IF w.up[d] THEN w.room[d] ELSE cat.free[d]]]
    /\ UNCHANGED <<w, pc, ghost>>

(* A copy in sync changed against its row, at its name: a modification, its version the file's, the other copies
   stale, a pending one canceled. Two changed: a conflict, every version kept (§15.7). *)
Changed(d) == w.up[d] /\ cat.seen[d] /\ cat.cs[d] \in {"ok", "surplus", "stale"} /\ AtName(d) /\ w.at[d] # cat.ver[d]
PassModify(d) ==
    /\ PassReady /\ cat.want # 0 /\ cat.fst = "ok" /\ Changed(d)
    /\ IF \E e \in Disks \ {d} : Changed(e) \/ cat.cs[d] = "stale"
       THEN cat' = [cat EXCEPT !.fst = "conflict"]
       ELSE cat' = [cat EXCEPT !.want = w.at[d], !.ver[d] = w.at[d], !.vfd[d] = TRUE,
                    !.cs = [e \in Disks |-> CASE e = d -> "ok"
                                              [] cat.cs[e] \in {"ok", "surplus"} -> "stale"
                                              [] cat.cs[e] = "pending" -> "none"
                                              [] OTHER -> cat.cs[e]]]
    /\ UNCHANGED <<w, pc, ghost>>

(* A copy gone from its name, found nowhere on its disk: a deletion, sent to the trash from every other copy, each
   unchanged since in sync, once every one can be looked at; one changed since, a modification, waits for its step. *)
PassDelete(d) ==
    /\ PassReady /\ cat.want # 0 /\ cat.fst = "ok"
    /\ w.up[d] /\ cat.seen[d] /\ cat.cs[d] \in {"ok", "surplus", "stale"} /\ w.at[d] = 0
    /\ \A e \in Disks \ {d} : cat.cs[e] \in {"ok", "surplus", "stale"} => Unchanged(e)
    /\ w' = [w EXCEPT !.tr = [e \in Disks |-> IF e # d /\ cat.cs[e] \in {"ok", "surplus", "stale"} THEN w.tr[e] \cup {w.at[e]}
                                              ELSE w.tr[e]],
                      !.at = [e \in Disks |-> IF e # d /\ cat.cs[e] \in {"ok", "surplus", "stale"} THEN 0 ELSE w.at[e]]]
    /\ cat' = [cat EXCEPT !.want = 0, !.cs = [e \in Disks |-> "none"], !.ver = [e \in Disks |-> 0],
                          !.vfd = [e \in Disks |-> FALSE]]
    /\ UNCHANGED <<pc, ghost>>

(* A copy in sync found at another name: a rename, the file's name from then on, followed on every other disk that
   answers; one away follows it once back (§15.12). *)
PassRename(d) ==
    /\ PassReady /\ cat.want # 0 /\ cat.fst = "ok"
    /\ w.up[d] /\ cat.seen[d] /\ cat.cs[d] \in {"ok", "surplus", "stale"} /\ w.at[d] = cat.ver[d] /\ w.nm[d] # cat.nm
    /\ cat' = [cat EXCEPT !.nm = w.nm[d]]
    /\ w' = [w EXCEPT !.nm = [e \in Disks |-> IF w.up[e] /\ w.at[e] # 0 THEN w.nm[d] ELSE w.nm[e]]]
    /\ UNCHANGED <<pc, ghost>>
PassFollow(e) ==
    /\ PassReady /\ cat.want # 0 /\ w.up[e] /\ w.at[e] # 0 /\ w.at[e] = cat.ver[e] /\ w.nm[e] # cat.nm
    /\ \A d \in Disks : w.up[d] /\ w.at[d] # 0 => w.nm[d] = cat.nm \/ d = e
    /\ w' = [w EXCEPT !.nm[e] = cat.nm]
    /\ UNCHANGED <<cat, pc, ghost>>

(* A file at the file's name where the catalog has no copy, or a pending one: recorded, never verified; holding
   another version, the file is a conflict, every version kept (§15.7, §15.10). *)
PassFound(d) ==
    /\ PassReady /\ cat.want # 0 /\ cat.fst = "ok"
    /\ w.up[d] /\ cat.seen[d] /\ cat.cs[d] \in {"none", "pending"} /\ cat.op[d].k = "none" /\ AtName(d)
    /\ cat' = [cat EXCEPT !.cs[d] = "ok", !.by[d] = "user", !.ver[d] = w.at[d], !.vfd[d] = FALSE,
                          !.fst = IF w.at[d] = cat.want THEN @ ELSE "conflict"]
    /\ UNCHANGED <<w, pc, ghost>>

(* A stale copy, as its row says, on a writable disk, refreshed from a source that holds the file's version: what it
   held to the trash first (§15.4, invariant 1). *)
PassRefresh(e) ==
    /\ PassReady /\ cat.want # 0 /\ cat.fst = "ok"
    /\ cat.cs[e] = "stale" /\ Writable(e) /\ Unchanged(e) /\ ~w.ro[e]
    /\ \E s \in Disks : Source(s) /\ Unchanged(s)
    /\ w' = [w EXCEPT !.tr[e] = @ \cup {w.at[e]}, !.at[e] = cat.want]
    /\ cat' = [cat EXCEPT !.cs[e] = "ok", !.ver[e] = cat.want, !.vfd[e] = TRUE]
    /\ UNCHANGED <<pc, ghost>>

(* A copy made, its job run in one step: a pending one kept, or one the plan of the pool places for a file below its
   level, its strict setting kept; min_free checked as it writes. *)
Below == Cardinality({d \in Disks : CountsIn(cat, d)}) + Cardinality(Pendings) < Level
PassMake(t) ==
    /\ PassReady /\ cat.want # 0 /\ cat.fst = "ok"
    /\ \/ cat.cs[t] = "pending" /\ Writable(t)
       \/ Below /\ Candidate(t)
    /\ w.up[t] /\ ~w.ro[t] /\ w.room[t] >= 1 /\ w.at[t] = 0
    /\ \E s \in Disks : Source(s) /\ Unchanged(s)
    /\ w' = [w EXCEPT !.at[t] = cat.want, !.nm[t] = cat.nm, !.ours[t] = TRUE, !.room[t] = @ - 1]
    /\ cat' = [cat EXCEPT !.cs[t] = "ok", !.by[t] = "atoll", !.ver[t] = cat.want, !.vfd[t] = TRUE]
    /\ UNCHANGED <<pc, ghost>>
(* A pending copy that no longer fits, nor counts, nor has a disk to go to: canceled (§13.4). *)
PassCancel(t) ==
    /\ PassReady /\ cat.cs[t] = "pending" /\ cat.op[t].k = "none"
    /\ \/ ~Writable(t) \/ cat.free[t] < 1 \/ cat.want = 0
       \/ Cardinality({d \in Disks : CountsIn(cat, d)}) >= Level
    /\ cat' = [cat EXCEPT !.cs[t] = "none"]
    /\ UNCHANGED <<w, pc, ghost>>
(* The marks of the plan of the pool. *)
PassMarks ==
    /\ PassReady /\ cat.want # 0 /\ Marked(cat) # cat.cs
    /\ cat' = [cat EXCEPT !.cs = Marked(cat)]
    /\ UNCHANGED <<w, pc, ghost>>

(* §17.2: what is planned on a disk that answers. A create whose part reached the file's name is committed, as the
   journal names its part; one whose part is in tmp/ undone, the part gone; a move to the trash done once the copy is
   there, undone otherwise. *)
Reprise(d) ==
    /\ pc.ph = "idle" /\ w.up[d] /\ cat.op[d].k # "none"
    /\ IF cat.op[d].k = "create"
       THEN IF w.part[d] = 0 /\ w.at[d] # 0 /\ w.ours[d] /\ cat.cs[d] = "pending"
            THEN /\ cat' = [cat EXCEPT !.cs[d] = "ok", !.ver[d] = cat.op[d].v, !.vfd[d] = TRUE, !.op[d] = NoOp]
                 /\ UNCHANGED w
            ELSE /\ w' = [w EXCEPT !.part[d] = 0, !.room[d] = IF w.part[d] # 0 THEN @ + 1 ELSE @]
                 /\ cat' = [cat EXCEPT !.op[d] = NoOp]
       ELSE IF w.at[d] = 0 /\ cat.op[d].v \in w.tr[d]
       THEN /\ cat' = [cat EXCEPT !.cs[d] = "none", !.ver[d] = 0, !.vfd[d] = FALSE, !.op[d] = NoOp]
            /\ UNCHANGED w
       ELSE /\ cat' = [cat EXCEPT !.op[d] = NoOp] /\ UNCHANGED w
    /\ UNCHANGED <<pc, ghost>>

Kill ==
    /\ "kill" \in AtollActs /\ pc.ph # "idle" /\ w.kills < MaxKill
    /\ pc' = Idle
    /\ w' = [w EXCEPT !.kills = @ + 1]
    /\ UNCHANGED <<cat, ghost>>

-----------------------------------------------------------------------------
(* A user, directly on a disk or through the view, which writes to one disk as a user does. *)
UserCan(a, d) == a \in UserActs /\ d \in UserDisks /\ w.users < MaxUser /\ w.up[d] /\ w.at[d] # 0
Edit(d) ==
    /\ UserCan("edit", d)
    /\ w' = [w EXCEPT !.at[d] = w.nv, !.nv = @ + 1, !.users = @ + 1]
    /\ ghost' = [ghost EXCEPT !.last = w.nv, !.since = TRUE]
    /\ UNCHANGED <<cat, pc>>
Delete(d) ==
    /\ UserCan("delete", d)
    /\ w' = [w EXCEPT !.at[d] = 0, !.ours[d] = FALSE, !.users = @ + 1]
    /\ ghost' = [ghost EXCEPT !.deleted = TRUE, !.since = TRUE]
    /\ UNCHANGED <<cat, pc>>
(* a file of the user's at the file's name, on a disk with no copy of it, as one copied there by hand: another
   version, never on a filesystem of a physical disk another holds a copy on *)
Create(d) ==
    /\ "create" \in UserActs /\ d \in UserDisks /\ w.users < MaxUser /\ w.up[d] /\ w.at[d] = 0 /\ d \notin Shared
    /\ w' = [w EXCEPT !.at[d] = w.nv, !.nm[d] = cat.nm, !.ours[d] = FALSE, !.nv = @ + 1, !.users = @ + 1]
    /\ ghost' = [ghost EXCEPT !.last = w.nv, !.since = TRUE]
    /\ UNCHANGED <<cat, pc>>
Rename(d) ==
    /\ UserCan("rename", d)
    /\ w' = [w EXCEPT !.nm[d] = 3 - @, !.users = @ + 1]
    /\ ghost' = [ghost EXCEPT !.since = TRUE]
    /\ UNCHANGED <<cat, pc>>

Away(a, d) == a \in WorldActs /\ d \in WorldDisks /\ w.acts < MaxWorld
Lose(d) ==
    /\ Away("lose", d) /\ w.up[d]
    /\ w' = [w EXCEPT !.up[d] = FALSE, !.acts = @ + 1]
    /\ ghost' = [ghost EXCEPT !.since = TRUE]
    /\ UNCHANGED <<cat, pc>>
Back(d) ==
    /\ ~w.up[d]
    /\ w' = [w EXCEPT !.up[d] = TRUE]
    /\ UNCHANGED <<cat, pc, ghost>>
ReadOnly(d) ==
    /\ Away("readonly", d) /\ ~w.ro[d]
    /\ w' = [w EXCEPT !.ro[d] = TRUE, !.acts = @ + 1]
    /\ UNCHANGED <<cat, pc, ghost>>
Drain(d) ==
    /\ Away("drain", d) /\ ~cat.drain[d] /\ pc.ph = "idle"
    /\ w' = [w EXCEPT !.acts = @ + 1]
    /\ cat' = [cat EXCEPT !.drain[d] = TRUE]
    /\ ghost' = [ghost EXCEPT !.since = TRUE]
    /\ UNCHANGED pc
Fill(d) ==
    /\ Away("fill", d) /\ w.room[d] >= 1
    /\ w' = [w EXCEPT !.room[d] = @ - 1, !.acts = @ + 1]
    /\ UNCHANGED <<cat, pc, ghost>>

-----------------------------------------------------------------------------
BalSteps == BalPart \/ BalLook \/ BalPlace \/ BalCommit \/ BalSettle \/ BalRetire \/ BalMove
PassSteps == PassCheck \/ PassMarks \/ \E d \in Disks : PassModify(d) \/ PassDelete(d) \/ PassRename(d) \/ PassFollow(d)
                                                        \/ PassFound(d) \/ PassRefresh(d) \/ PassMake(d) \/ PassCancel(d)
Work == \/ BalSteps \/ PassSteps \/ \E orig \in BOOLEAN : BalStart(orig) \/ \E d \in Disks : Reprise(d)
Next == \/ Work \/ Kill
        \/ \E d \in Disks : Edit(d) \/ Delete(d) \/ Create(d) \/ Rename(d) \/ Lose(d) \/ Back(d) \/ ReadOnly(d) \/ Drain(d) \/ Fill(d)

Spec == Init /\ [][Next]_vars

(* Liveness: the balance's steps go on, a run starts and the passes run once the lock lets them, a disk comes back. *)
LiveSpec == /\ Spec /\ WF_vars(BalSteps)
            /\ SF_vars(\E orig \in BOOLEAN : BalStart(orig)) /\ SF_vars(PassCheck) /\ SF_vars(PassMarks)
            /\ SF_vars(\E d \in Disks : PassModify(d)) /\ SF_vars(\E d \in Disks : PassDelete(d))
            /\ SF_vars(\E d \in Disks : PassRename(d)) /\ SF_vars(\E d \in Disks : PassFollow(d))
            /\ SF_vars(\E d \in Disks : PassFound(d))
            /\ SF_vars(\E d \in Disks : PassRefresh(d)) /\ SF_vars(\E d \in Disks : PassMake(d))
            /\ SF_vars(\E d \in Disks : PassCancel(d)) /\ SF_vars(\E d \in Disks : Reprise(d))
            /\ WF_vars(\E d \in Disks : Back(d))

-----------------------------------------------------------------------------
TypeOK ==
    /\ \A d \in Disks : /\ cat.cs[d] \in {"none", "ok", "surplus", "pending", "stale"} /\ cat.by[d] \in {"user", "atoll"}
                        /\ cat.op[d].k \in {"none", "create", "trash"} /\ w.nm[d] \in {1, 2}
    /\ cat.fst \in {"ok", "conflict"}
    /\ pc.ph \in {"idle", "part", "look", "place", "commit", "settle", "retire", "move"}
    /\ ghost.flags \subseteq {"below", "user-moved", "only-version"}

(* Invariant 13 (R-06): never two copies of the file on one physical disk, at any step, a copy at another name
   included. *)
Inv13_OnePhysical == \A d1, d2 \in Disks : d1 # d2 /\ w.at[d1] # 0 /\ w.at[d2] # 0 => Phys(d1) # Phys(d2)
(* §13.3: a balance never takes the file below its level, even for a moment: its move to the trash leaves the level's
   number of copies of the file's version on disks that answer, but for what a user or the world did since its look. *)
NoBelowLevel == "below" \notin ghost.flags
(* §12.4, §13.3, §6.1 item 9: a balance never sends a user copy to the trash; --move-originals hands its part to the
   copy made first. And the file never has fewer user copies than at the start, but once a user deleted it. *)
NoUserCopyMoved == "user-moved" \notin ghost.flags
UserCopiesKept == ghost.deleted \/ Cardinality({d \in Disks : cat.by[d] = "user" /\ cat.cs[d] # "none"}) >= ghost.users0
(* Invariant 9, §13.3: a user's edit during a balance is never lost: the version a user wrote last is at a path or in a
   trash, on any disk, but once the user deleted the file; and a balance never sends to the trash a version no other
   copy holds. *)
Latest == IF ghost.last = 0 THEN 1 ELSE ghost.last
EditKept == ghost.deleted \/ \E d \in Disks : w.at[d] = Latest \/ w.part[d] = Latest \/ Latest \in w.tr[d]
NoOnlyVersionMoved == "only-version" \notin ghost.flags
(* §12.4: min_free respected: no write takes a disk's room past it. *)
MinFree == \A d \in Disks : w.room[d] >= 0

(* Liveness: once the world is done, nothing is left for Atoll to do: a balance started finished or was undone, its
   operations resolved, and the pool settled. *)
EventuallySettled == <>[](pc.ph = "idle" /\ ~ENABLED Work)
=============================================================================
