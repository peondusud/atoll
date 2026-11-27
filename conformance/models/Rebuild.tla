------------------------------- MODULE Rebuild -------------------------------
(* atoll catalog rebuild (specs.md §9, §10, §14.2, §15.4, §15.6, §15.7, §17, §17.2, §19, §21.2, §25; invariants 1, 4,
   10, 11 and 14): a model of its own. One file, at its path on two or three disks; disk 1 is maybe the SSD cache (§19),
   where a new file written through Atoll's own view lands, and from where the transfer moves it to the data disks.

   What it keeps, for each disk: whether its check confirms it, a disk missing and one whose marker or identity is not
   its own (unknown) being alike to every check (§10), and whether it is mounted read-only; the version the copy at the
   file's path holds (0 none) and its change time, as a count; the entries of its trash, each an operation's folder that
   holds one file, and whether its retention has passed; and the parts of its tmp/, each named by an operation and made
   by one catalog. The catalogs: the one at the catalog's path, and each one a rebuild set aside, with the file's version
   and state (none, ok, conflict) and whether its deletion is being sent on; each copy's row, the version and the change
   time it records; the operation planned, and the ids of those it journaled that are over; the entries of the trash it
   knows, and why each went there; what the view holds open (view_ops `open`, §21.2); the copy whose modification the
   guard holds and whether the user rejected it (§15.6); and where its ids start (§9).

   Atoll's daemon is one process, under the catalog's lock: a pass, or one step of an operation, at a time, the reprise
   of what is planned first (§17.2). A pass takes in what each disk its check confirms holds against the rows: a copy it
   does not know, adopted, a conflict when it holds another version than the file's (§15.7); a copy changed, a
   modification, the other copies stale, two changed apart a conflict, the guard maybe holding it; a copy gone, a
   deletion, sent to the trash from every other copy unchanged since in sync (invariant 10). The operations: a copy made
   through a part of tmp/ on a data disk (§14.2), a stale copy or a deleted file's copy moved to the trash (§17.1), the
   transfer of the cache's copy once every data disk holds the file, a move to the trash then a purge at once, which
   waits while the view holds the copy open (§19, §21.2), the purge of an entry its retention passed, the cleaning of the
   parts of tmp/ that operations over name (§14.2).

   atoll catalog rebuild --yes (§9, §25) holds the catalog's lock from its start to its end, so that the daemon, running
   or not, waits for it: its new catalog started past the highest id the trash and tmp/ of each disk its check confirms
   name; each such disk's trash listed again, each entry of the reason unknown; each such disk read whole, a row for each
   copy, then the copies by hash: one version, the file ok; two, a conflict, every version kept; then the swap, the old
   catalog kept, never removed. What the old catalog planned, held, rejected or knew of a disk missing is lost with it.
   Atoll's own view (atoll mount, §21.2) is a process of its own: it opens the catalog once, as it starts, presents
   from it the copies it knows, and journals in it what it holds open, never taking the catalog's lock. A rebuild and
   the view exclude each other (§9, §21.2): the rebuild starts only while no view runs, and a view only while no
   rebuild does, the lock on the catalog's folder that each holds for as long as it runs.

   A user edits a copy in place or deletes it directly on a data disk, or a program writes the file through the view: it
   opens it, writes it, closes it; the user may reject a modification the guard holds; the world loses a disk and brings
   it back, mounts one read-only, and lets the retention of an entry of the trash pass; Atoll's process may be killed
   between any two steps, a rebuild's included; the view may be stopped and started again.

   Flaw 35, fixed (README.md): the new catalog knew nothing of what the view held open, and the view went on with the
   catalog it opened, set aside by the swap, so that the transfer moved and purged a file under the program writing it
   through the view. rebuild-view, the maintainer's remedy of 2026-11-27, is what every instance does: a rebuild and
   the view exclude each other, a lock on the catalog's folder, the view's shared, the rebuild's exclusive.

   What it leaves out: other files, folders, renames and names (Nested.tla's, ViewNames.tla's, Collisions.tla's); the
   link guard's links/, whose ids a new catalog starts past as it starts its operations (§9); a trash entry that is no
   operation's folder of one file; the purge for room, which never takes an entry a rebuild listed again (Trash.tla's);
   the guards but as a modification held or rejected (Guards.tla's); the steps of the pipeline (Replication.tla's);
   first_seen and the original's choice; the rebuild's own renames, a crash between them leaving the old catalog or the
   new one at its path and the other kept beside it (the tests of catalog.SetAside and catalog.KeepLeftover); the view's
   other operations, renames, deletions, parts and moves, whose rows a rebuild loses too; a program writing through the
   view while a user writes another copy directly: see README.md. *)
EXTENDS Integers, FiniteSets, TLC

CONSTANTS
    NDisks,      \* 2 or 3
    Cache,       \* TRUE: disk 1 is the SSD cache (§19)
    AtStart,     \* the disks that hold the file's version 1 at the start, each with its row
    TrashAt,     \* the disks whose trash holds, at the start, an entry the catalog knows, operation 1, surplus
    UserActs,    \* some of "edit", "delete", "view"
    UserDisks,
    MaxUser,
    WorldActs,   \* some of "lose", "readonly", "tick"
    WorldDisks,
    MaxWorld,
    AtollActs,   \* some of "rebuild", "guard", "kill", "viewstop"
    MaxKill,
    MaxRebuild,
    Fixes        \* the remedies proposed for the flaws found here and still open, by name (README.md); {} is specs.md as it is

Disks == 1..NDisks
Data == IF Cache THEN Disks \ {1} ELSE Disks
Gens == 1..(MaxRebuild + 1)
\* flaw 35 is fixed, with rebuild-view, what every instance does (README.md): no remedy
FixNames == {}

ASSUME /\ NDisks \in 2..3 /\ Cache \in BOOLEAN /\ AtStart \subseteq Disks /\ AtStart # {} /\ TrashAt \subseteq Disks
       /\ (Cache => AtStart = {1})
       /\ UserActs \subseteq {"edit", "delete", "view"} /\ UserDisks \subseteq Disks /\ MaxUser \in Nat
       /\ WorldActs \subseteq {"lose", "readonly", "tick"} /\ WorldDisks \subseteq Disks /\ MaxWorld \in Nat
       /\ AtollActs \subseteq {"rebuild", "guard", "kill", "viewstop"} /\ MaxKill \in Nat /\ MaxRebuild \in Nat
       /\ Fixes \subseteq FixNames

VARIABLES
    w,       \* the disks, and the counts of what the user, the world and Atoll did
    cats,    \* the catalogs, by generation: the one at the catalog's path is cats[gen]
    gen,
    kept,    \* the generations a rebuild set aside
    rb,      \* the rebuild under way
    pc,      \* the step of Atoll's operation under way
    vw,      \* Atoll's own view
    ghost    \* what the invariants judge by

vars == <<w, cats, gen, kept, rb, pc, vw, ghost>>

NoRow == [on |-> FALSE, v |-> 0, ct |-> 0]
NoOp == [k |-> "none", d |-> 0, id |-> 0, v |-> 0, why |-> "none"]
Empty(next) == [row |-> [d \in Disks |-> NoRow], fv |-> 0, st |-> "none", del |-> 0, op |-> NoOp, over |-> {},
                trash |-> {}, holds |-> {}, next |-> next, held |-> 0, rej |-> FALSE, counted |-> {}]
Cat0 == [Empty(2) EXCEPT !.row = [d \in Disks |-> IF d \in AtStart THEN [on |-> TRUE, v |-> 1, ct |-> 1] ELSE NoRow],
                         !.fv = 1, !.st = "ok", !.over = {1}, !.counted = Disks,
                         !.trash = {[d |-> d, id |-> 1, why |-> "surplus"] : d \in TrashAt}]

Init ==
    /\ w = [up |-> [d \in Disks |-> TRUE], ro |-> [d \in Disks |-> FALSE],
            at |-> [d \in Disks |-> IF d \in AtStart THEN 1 ELSE 0], ct |-> [d \in Disks |-> 1],
            tr |-> [d \in Disks |-> IF d \in TrashAt THEN {[id |-> 1, v |-> 1, due |-> FALSE]} ELSE {}],
            tmp |-> [d \in Disks |-> {}], nv |-> 2, user |-> 0, world |-> 0, kills |-> 0, rebuilds |-> 0, views |-> 0]
    /\ cats = [g \in Gens |-> IF g = 1 THEN Cat0 ELSE Empty(0)]
    /\ gen = 1
    /\ kept = {}
    /\ rb = [ph |-> "idle", cat |-> Empty(0), seen |-> {}]
    /\ pc = "idle"
    /\ vw = [on |-> "view" \in UserActs, cat |-> 1, open |-> 0, fd |-> "none", tid |-> 0, v |-> 0]
    /\ ghost = [heads |-> {1}, flags |-> {}]

Cat == cats[gen]
Max(S) == IF S = {} THEN 0 ELSE CHOOSE x \in S : \A y \in S : y <= x
Writable(d) == w.up[d] /\ ~w.ro[d]
\* the versions the trash of a disk holds
InTrash(d) == {e.v : e \in w.tr[d]}
Flag(f) == ghost' = [ghost EXCEPT !.flags = @ \cup f]
\* a user's write over a version: what the user wrote last on a copy is a head, what it wrote over no longer is
HeadsAfter(old, new) == (ghost.heads \ {old}) \cup (IF new > 0 THEN {new} ELSE {})
\* the catalog the view journals in: the one it opened at its start
VCat == vw.cat
\* the copy the view presents (§7, §21.2): of the copies its catalog records in sync, the cache's first, then by position
Presented(c) == LET in == {d \in Disks : c.row[d].on /\ c.row[d].v = c.fv /\ c.st = "ok" /\ w.up[d]}
                IN IF in = {} THEN 0 ELSE CHOOSE d \in in : \A e \in in : d <= e
\* the view's handle follows the file it holds wherever Atoll moves it
FdMoved(d, id) == IF vw.open = d /\ vw.fd = "path" THEN [vw EXCEPT !.fd = "trash", !.tid = id] ELSE vw
FdPurged(d, id) == IF vw.open = d /\ vw.fd = "trash" /\ vw.tid = id THEN [vw EXCEPT !.fd = "gone"] ELSE vw

-----------------------------------------------------------------------------
(* The user. *)
UserStep == w.user < MaxUser /\ rb.ph \in {"idle", "list", "scan"}

\* A write in place, directly on a data disk: a new version, the change time moved, the same file.
Edit(d) ==
    /\ "edit" \in UserActs /\ UserStep /\ d \in UserDisks \cap Data /\ Writable(d) /\ w.at[d] > 0
    /\ w' = [w EXCEPT !.at[d] = w.nv, !.ct[d] = @ + 1, !.nv = @ + 1, !.user = @ + 1]
    /\ vw' = IF vw.open = d /\ vw.fd = "path" THEN [vw EXCEPT !.v = w.nv] ELSE vw
    /\ ghost' = [ghost EXCEPT !.heads = HeadsAfter(w.at[d], w.nv)]
    /\ UNCHANGED <<cats, gen, kept, rb, pc>>

\* A copy removed directly on a data disk.
Delete(d) ==
    /\ "delete" \in UserActs /\ UserStep /\ d \in UserDisks \cap Data /\ Writable(d) /\ w.at[d] > 0
    /\ vw.open # d
    /\ w' = [w EXCEPT !.at[d] = 0, !.ct[d] = @ + 1, !.user = @ + 1]
    /\ ghost' = [ghost EXCEPT !.heads = HeadsAfter(w.at[d], 0)]
    /\ UNCHANGED <<cats, gen, kept, rb, pc, vw>>

\* A program opens the file through the view to write (§21.2, Held open): the view holds the copy it presents open, in
\* its catalog, and waits while a move of Atoll's own of that copy is planned there.
VOpen ==
    /\ "view" \in UserActs /\ UserStep /\ vw.on /\ vw.open = 0
    /\ LET c == cats[VCat]  d == Presented(c) IN
       /\ d # 0 /\ Writable(d) /\ w.at[d] > 0
       /\ ~(c.op.k \in {"trash", "xfer"} /\ c.op.d = d)
       /\ cats' = [cats EXCEPT ![VCat].holds = @ \cup {d}]
       /\ vw' = [vw EXCEPT !.open = d, !.fd = "path", !.v = w.at[d]]
    /\ w' = [w EXCEPT !.user = @ + 1]
    /\ UNCHANGED <<gen, kept, rb, pc, ghost>>

\* It writes through its handle: into the copy at its path, into the file in the trash when Atoll moved it there under
\* the program, or into a file no name holds any more once purged.
VWrite ==
    /\ "view" \in UserActs /\ UserStep /\ vw.open # 0
    /\ LET d == vw.open IN
       /\ w' = [w EXCEPT !.at[d] = IF vw.fd = "path" THEN w.nv ELSE @, !.ct[d] = IF vw.fd = "path" THEN @ + 1 ELSE @,
                         !.tr[d] = IF vw.fd = "trash" THEN {IF e.id = vw.tid THEN [e EXCEPT !.v = w.nv] ELSE e : e \in @}
                                   ELSE @,
                         !.nv = @ + 1, !.user = @ + 1]
       /\ vw' = [vw EXCEPT !.v = w.nv]
       /\ ghost' = [ghost EXCEPT !.heads = HeadsAfter(vw.v, w.nv)]
    /\ UNCHANGED <<cats, gen, kept, rb, pc>>

\* It closes the file: the view holds it no more, in its catalog.
VClose ==
    /\ vw.open # 0
    /\ cats' = [cats EXCEPT ![VCat].holds = @ \ {vw.open}]
    /\ vw' = [vw EXCEPT !.open = 0, !.fd = "none", !.tid = 0, !.v = 0]
    /\ UNCHANGED <<w, gen, kept, rb, pc, ghost>>

\* The view stopped, with no file open (an unmount refuses a busy view), and started again: it opens the catalog at
\* the catalog's path, and holds nothing in it (§21.2).
VStop ==
    /\ "viewstop" \in AtollActs /\ vw.on /\ vw.open = 0 /\ w.views < 1
    /\ vw' = [vw EXCEPT !.on = FALSE]
    /\ w' = [w EXCEPT !.views = @ + 1]
    /\ UNCHANGED <<cats, gen, kept, rb, pc, ghost>>
VStart ==
    /\ "view" \in UserActs /\ ~vw.on
    \* the view's side of the lock, shared: never while a rebuild holds it (rebuild-view, flaw 35)
    /\ rb.ph = "idle"
    /\ vw' = [vw EXCEPT !.on = TRUE, !.cat = gen]
    /\ cats' = [cats EXCEPT ![gen].holds = {}]
    /\ UNCHANGED <<w, gen, kept, rb, pc, ghost>>

\* The guard holds a modification, and the user rejects it: what it covered is kept for good (§15.6, reject-kept).
Reject ==
    /\ "guard" \in AtollActs /\ Cat.held # 0 /\ ~Cat.rej /\ rb.ph = "idle"
    /\ cats' = [cats EXCEPT ![gen].rej = TRUE]
    /\ ghost' = [ghost EXCEPT !.heads = @ \cup {w.at[e] : e \in {x \in Disks \ {Cat.held} : Cat.row[x].on /\ w.at[x] > 0}}]
    /\ UNCHANGED <<w, gen, kept, rb, pc, vw>>

-----------------------------------------------------------------------------
(* The world. *)
WorldStep == w.world < MaxWorld

Lose(d) ==
    /\ "lose" \in WorldActs /\ WorldStep /\ d \in WorldDisks /\ w.up[d]
    /\ w' = [w EXCEPT !.up[d] = FALSE, !.world = @ + 1]
    /\ UNCHANGED <<cats, gen, kept, rb, pc, vw, ghost>>
Back(d) ==
    /\ ~w.up[d]
    /\ w' = [w EXCEPT !.up[d] = TRUE]
    /\ UNCHANGED <<cats, gen, kept, rb, pc, vw, ghost>>
ReadOnly(d) ==
    /\ "readonly" \in WorldActs /\ WorldStep /\ d \in WorldDisks /\ ~w.ro[d]
    /\ w' = [w EXCEPT !.ro[d] = TRUE, !.world = @ + 1]
    /\ UNCHANGED <<cats, gen, kept, rb, pc, vw, ghost>>
\* The retention of an entry of the trash passes, counted from when it truly went there.
Tick(d) ==
    /\ "tick" \in WorldActs /\ WorldStep /\ \E e \in w.tr[d] : ~e.due
    /\ \E e \in w.tr[d] : ~e.due /\ w' = [w EXCEPT !.tr[d] = (@ \ {e}) \cup {[e EXCEPT !.due = TRUE]}, !.world = @ + 1]
    /\ UNCHANGED <<cats, gen, kept, rb, pc, vw, ghost>>

-----------------------------------------------------------------------------
(* Atoll's daemon: one step at a time, under the catalog's lock, which a rebuild holds through its steps. *)
Daemon == rb.ph = "idle"

(* A pass (§15.4, §15.7, §15.10): what each disk its check confirms holds, against the rows. *)
Changed == {d \in Disks : w.up[d] /\ Cat.row[d].on /\ w.at[d] > 0 /\ w.ct[d] # Cat.row[d].ct /\ w.at[d] # Cat.row[d].v}
Gone == {d \in Disks : w.up[d] /\ Cat.row[d].on /\ w.at[d] = 0}
Found == {d \in Disks : w.up[d] /\ ~Cat.row[d].on /\ w.at[d] > 0}
Touched == {d \in Disks : w.up[d] /\ Cat.row[d].on /\ w.at[d] > 0 /\ w.ct[d] # Cat.row[d].ct /\ w.at[d] = Cat.row[d].v}
Seen(d) == [on |-> TRUE, v |-> w.at[d], ct |-> w.ct[d]]

Pass ==
    /\ Daemon /\ pc = "idle" /\ Cat.op.k = "none"
    /\ Changed \cup Gone \cup Found \cup Touched # {}
    /\ LET c == Cat
           news == {w.at[d] : d \in Changed}
           \* a modification: one new version, from a copy that was in sync, no guard holding it
           conflict == \/ Cardinality(news) > 1
                       \/ \E d \in Changed : c.row[d].v # c.fv
                       \/ Changed # {} /\ c.rej
                       \/ \E d \in Found : c.st # "none" /\ w.at[d] # (IF Changed # {} THEN CHOOSE x \in news : TRUE ELSE c.fv)
           guarded == "guard" \in AtollActs /\ Changed # {} /\ ~c.rej /\ ~conflict
           st1 == IF c.st = "conflict" \/ (c.st = "ok" /\ conflict) THEN "conflict"
                  ELSE IF c.st = "none" /\ Found # {} THEN (IF Cardinality({w.at[d] : d \in Found}) > 1 THEN "conflict" ELSE "ok")
                  ELSE c.st
           fv1 == IF c.st = "none" /\ Found # {} THEN Max({w.at[d] : d \in Found})
                  ELSE IF st1 = "ok" /\ Changed # {} /\ ~guarded THEN CHOOSE x \in news : TRUE
                  ELSE c.fv
           take == IF guarded THEN Disks \ Changed ELSE Disks
           row1 == [d \in Disks |-> IF d \in (Changed \cup Found \cup Touched) \cap take THEN Seen(d)
                                    ELSE IF d \in Gone THEN NoRow ELSE c.row[d]]
           \* a deletion of a copy that held the file's version, sent on while the file is ok and no copy changed, to
           \* the copies known to have held what went (§15.5, invariant 10); a stale copy gone only loses its row
           gone == {d \in Gone : c.row[d].v = c.fv}
           del1 == IF st1 = "ok" /\ Changed = {} /\ gone # {} THEN c.fv
                   ELSE IF Changed # {} \/ st1 # "ok" THEN 0 ELSE c.del
       IN cats' = [cats EXCEPT ![gen] = [c EXCEPT !.row = row1, !.st = st1, !.fv = fv1, !.del = del1,
                                                  !.held = IF guarded THEN CHOOSE d \in Changed : TRUE ELSE 0]]
    /\ Flag(IF \E d \in Changed \cup Gone \cup Found \cup Touched : ~w.up[d] THEN {"missing-read"} ELSE {})
    /\ UNCHANGED <<w, gen, kept, rb, pc, vw>>

\* The file gone once its deletion left no copy (§15.4).
FileGone ==
    /\ Daemon /\ pc = "idle" /\ Cat.op.k = "none" /\ Cat.del # 0 /\ \A d \in Disks : ~Cat.row[d].on
    /\ cats' = [cats EXCEPT ![gen] = [@ EXCEPT !.st = "none", !.fv = 0, !.del = 0]]
    /\ UNCHANGED <<w, gen, kept, rb, pc, vw, ghost>>

\* What the catalog says of a copy is what its disk holds.
InSync(c, d) == c.row[d].on /\ c.row[d].v = c.fv /\ w.at[d] = c.fv /\ w.ct[d] = c.row[d].ct
Unchanged(c, d) == c.row[d].on /\ w.at[d] = c.row[d].v /\ w.ct[d] = c.row[d].ct

(* An operation, journaled (§17.1, §14.2): its id the catalog's next. *)
Journal(k, d, v, why) ==
    /\ cats' = [cats EXCEPT ![gen] = [@ EXCEPT !.op = [k |-> k, d |-> d, id |-> Cat.next, v |-> v, why |-> why],
                                               !.next = @ + 1]]
\* A copy to make on a data disk: the operation and its part, made with no name it would replace: a part of that id
\* there already, one an earlier catalog's operation left, makes it undone at once, io-error.
JCreate(d) ==
    /\ Daemon /\ pc = "idle" /\ Cat.op.k = "none" /\ Cat.st = "ok" /\ Cat.del = 0 /\ Cat.held = 0
    /\ d \in Data /\ Writable(d) /\ ~Cat.row[d].on /\ w.at[d] = 0
    /\ \E s \in Disks : w.up[s] /\ InSync(Cat, s)
    /\ IF \E p \in w.tmp[d] : p.id = Cat.next
       THEN /\ cats' = [cats EXCEPT ![gen] = [@ EXCEPT !.next = @ + 1, !.over = @ \cup {Cat.next}]]
            /\ UNCHANGED <<w, pc>>
       ELSE /\ Journal("create", d, Cat.fv, "none")
            /\ w' = [w EXCEPT !.tmp[d] = @ \cup {[id |-> Cat.next, v |-> Cat.fv, g |-> gen]}]
            /\ pc' = "act"
    /\ UNCHANGED <<gen, kept, rb, vw, ghost>>
\* A copy to send to the trash: a stale one (overwritten), or one of a file whose deletion is sent on (deleted).
JTrash(d) ==
    /\ Daemon /\ pc = "idle" /\ Cat.op.k = "none" /\ Writable(d) /\ Unchanged(Cat, d) /\ d \in Data
    \* a deleted file's copies known to have held what went, and its stale ones (§15.5)
    /\ \/ Cat.st = "ok" /\ Cat.del # 0 /\ (Cat.row[d].v = Cat.del \/ Cat.row[d].v # Cat.fv)
       \/ Cat.st = "ok" /\ Cat.del = 0 /\ Cat.held = 0 /\ Cat.row[d].v # Cat.fv /\ \E s \in Disks : w.up[s] /\ InSync(Cat, s)
    /\ Journal("trash", d, w.at[d], IF Cat.del # 0 THEN "deleted" ELSE "overwritten")
    /\ pc' = "act"
    /\ UNCHANGED <<w, gen, kept, rb, vw, ghost>>
\* The transfer of the cache's copy (§19): once every data disk holds the file, while the view holds it not open, read
\* in the transaction that journals it (TrashStart.ViewFree).
JXfer ==
    /\ Cache /\ Daemon /\ pc = "idle" /\ Cat.op.k = "none" /\ Cat.st = "ok" /\ Cat.del = 0 /\ Cat.held = 0
    /\ Writable(1) /\ InSync(Cat, 1) /\ \A d \in Data : w.up[d] /\ InSync(Cat, d)
    /\ 1 \notin Cat.holds
    /\ Journal("xfer", 1, w.at[1], "surplus")
    /\ pc' = "act"
    /\ UNCHANGED <<w, gen, kept, rb, vw, ghost>>

(* The operation's steps on its disk, then its commit. *)
Act ==
    /\ Daemon /\ pc = "act" /\ w.up[Cat.op.d]
    /\ LET o == Cat.op  d == o.d IN
       IF o.k = "create"
       THEN \* the part placed at the file's path, never replacing anything there
            IF w.at[d] = 0 /\ \E p \in w.tmp[d] : p.id = o.id
            THEN /\ w' = [w EXCEPT !.at[d] = o.v, !.ct[d] = @ + 1, !.tmp[d] = {p \in @ : p.id # o.id}]
                 /\ pc' = "commit" /\ UNCHANGED <<cats, vw>>
            ELSE /\ cats' = [cats EXCEPT ![gen] = [@ EXCEPT !.op = NoOp, !.over = @ \cup {o.id}]]
                 /\ pc' = "idle" /\ UNCHANGED <<w, vw>>
       ELSE \* the copy looked at right before it moves (§17.1): as its row says, or the move is undone
            IF Unchanged(Cat, d) /\ ~\E e \in w.tr[d] : e.id = o.id
            THEN /\ w' = [w EXCEPT !.at[d] = 0, !.ct[d] = @ + 1, !.tr[d] = @ \cup {[id |-> o.id, v |-> w.at[d], due |-> FALSE]}]
                 /\ vw' = FdMoved(d, o.id)
                 /\ pc' = IF o.k = "xfer" THEN "purge" ELSE "commit"
                 /\ UNCHANGED cats
            ELSE /\ cats' = [cats EXCEPT ![gen] = [@ EXCEPT !.op = NoOp, !.over = @ \cup {o.id}]]
                 /\ pc' = "idle" /\ UNCHANGED <<w, vw>>
    /\ UNCHANGED <<gen, kept, rb, ghost>>
\* The transfer's purge at once of the copy it moved (§19).
XPurge ==
    /\ Daemon /\ pc = "purge" /\ w.up[1]
    /\ w' = [w EXCEPT !.tr[1] = {e \in @ : e.id # Cat.op.id}]
    /\ vw' = FdPurged(1, Cat.op.id)
    /\ pc' = "commit"
    /\ UNCHANGED <<cats, gen, kept, rb, ghost>>
Done(c, o) ==
    [c EXCEPT !.op = NoOp, !.over = @ \cup {o.id},
              !.row[o.d] = IF o.k = "create" THEN [on |-> TRUE, v |-> o.v, ct |-> w.ct[o.d]] ELSE NoRow,
              !.trash = IF o.k = "trash" THEN @ \cup {[d |-> o.d, id |-> o.id, why |-> o.why]} ELSE @]
Commit ==
    /\ Daemon /\ pc = "commit"
    /\ cats' = [cats EXCEPT ![gen] = Done(Cat, Cat.op)]
    /\ pc' = "idle"
    /\ UNCHANGED <<w, gen, kept, rb, vw, ghost>>

\* Atoll killed: what is planned stays planned, for the reprise; a rebuild under way stops, the old catalog in place.
Kill ==
    /\ "kill" \in AtollActs /\ w.kills < MaxKill /\ (pc # "idle" \/ rb.ph # "idle")
    /\ w' = [w EXCEPT !.kills = @ + 1]
    /\ pc' = "idle"
    /\ rb' = [rb EXCEPT !.ph = "idle"]
    /\ UNCHANGED <<cats, gen, kept, vw, ghost>>

\* The reprise (§14.2, §17.2) of what is planned on a disk its check confirms, from what the disk holds.
Reprise ==
    /\ Daemon /\ pc = "idle" /\ Cat.op.k # "none" /\ w.up[Cat.op.d]
    /\ LET o == Cat.op  d == o.d  moved == \E e \in w.tr[d] : e.id = o.id IN
       CASE o.k = "create" ->
              IF (~\E p \in w.tmp[d] : p.id = o.id) /\ w.at[d] = o.v
              THEN /\ cats' = [cats EXCEPT ![gen] = Done(Cat, o)] /\ UNCHANGED <<w, vw>>
              ELSE /\ cats' = [cats EXCEPT ![gen] = [@ EXCEPT !.op = NoOp, !.over = @ \cup {o.id}]] /\ UNCHANGED <<w, vw>>
         [] o.k = "trash" ->
              /\ cats' = [cats EXCEPT ![gen] = IF moved THEN Done(Cat, o) ELSE [@ EXCEPT !.op = NoOp, !.over = @ \cup {o.id}]]
              /\ UNCHANGED <<w, vw>>
         [] o.k = "xfer" ->
              /\ w' = [w EXCEPT !.tr[d] = {e \in @ : e.id # o.id}]
              /\ vw' = FdPurged(d, o.id)
              /\ cats' = [cats EXCEPT ![gen] = IF moved \/ w.at[d] = 0 THEN Done(Cat, o)
                                               ELSE [@ EXCEPT !.op = NoOp, !.over = @ \cup {o.id}]]
    /\ UNCHANGED <<gen, kept, rb, pc, ghost>>

\* The purge of an entry the catalog knows whose retention passed (§17.3): from the disk, then from the catalog.
Purge(d) ==
    /\ Daemon /\ pc = "idle" /\ Cat.op.k = "none" /\ Writable(d)
    /\ \E t \in Cat.trash : t.d = d /\ \E e \in w.tr[d] : e.id = t.id /\ e.due
    /\ \E t \in Cat.trash : t.d = d /\ \E e \in w.tr[d] : e.id = t.id /\ e.due
         /\ w' = [w EXCEPT !.tr[d] = {x \in @ : x.id # t.id}]
         /\ cats' = [cats EXCEPT ![gen].trash = @ \ {t}]
         /\ vw' = FdPurged(d, t.id)
    /\ UNCHANGED <<gen, kept, rb, pc, ghost>>

\* tmp/ cleaned (§14.2): a part that an operation over names, on its disk, goes; any other stays.
Clean(d) ==
    /\ Daemon /\ pc = "idle" /\ Cat.op.k = "none" /\ Writable(d)
    /\ \E p \in w.tmp[d] : p.id \in Cat.over
    /\ w' = [w EXCEPT !.tmp[d] = {p \in @ : p.id \notin Cat.over}]
    \* of a disk whose names the catalog's ids started past: a disk missing then is the limit its warning says
    /\ Flag(IF d \in Cat.counted /\ \E p \in w.tmp[d] : p.id \in Cat.over /\ p.g # gen THEN {"part-of-another"} ELSE {})
    /\ UNCHANGED <<cats, gen, kept, rb, pc, vw>>

-----------------------------------------------------------------------------
(* atoll catalog rebuild --yes (§9, §25): under the catalog's lock, which the daemon's step under way, if any, lets go
   of first, so that none runs while it does. *)
RbStart ==
    /\ "rebuild" \in AtollActs /\ w.rebuilds < MaxRebuild /\ rb.ph = "idle" /\ pc = "idle"
    \* the rebuild's side of the lock, exclusive: never while a view holds it (rebuild-view, flaw 35)
    /\ ~vw.on
    /\ LET up == {x \in Disks : w.up[x]}
           named == UNION {{e.id : e \in w.tr[d]} \cup {p.id : p \in w.tmp[d]} : d \in up}
       IN rb' = [ph |-> "list", cat |-> [Empty(Max(named) + 1) EXCEPT !.counted = up], seen |-> {}]
    /\ UNCHANGED <<w, cats, gen, kept, pc, vw, ghost>>
\* Each disk's trash listed again, of a disk its check confirms: each entry of the reason unknown; a missing one's not.
RbList(d) ==
    /\ rb.ph = "list" /\ d \notin rb.seen
    /\ rb' = [rb EXCEPT !.cat.trash = @ \cup (IF w.up[d] THEN {[d |-> d, id |-> e.id, why |-> "unknown"] : e \in w.tr[d]} ELSE {}),
                        !.seen = IF @ \cup {d} = Disks THEN {} ELSE @ \cup {d},
                        !.ph = IF rb.seen \cup {d} = Disks THEN "scan" ELSE "list"]
    /\ UNCHANGED <<w, cats, gen, kept, pc, vw, ghost>>
\* Each disk its check confirms read whole: a row for the copy it finds, as it finds it.
RbScan(d) ==
    /\ rb.ph = "scan" /\ d \notin rb.seen
    /\ rb' = [rb EXCEPT !.cat.row[d] = IF w.up[d] /\ w.at[d] > 0 THEN Seen(d) ELSE NoRow, !.seen = @ \cup {d}]
    /\ UNCHANGED <<w, cats, gen, kept, pc, vw, ghost>>
\* The copies by hash, then the swap: the new catalog at the catalog's path, the old one kept beside it.
RbSwap ==
    /\ rb.ph = "scan" /\ rb.seen = Disks
    /\ LET c == rb.cat
           vs == {c.row[d].v : d \in {x \in Disks : c.row[x].on}}
           made == [c EXCEPT !.st = IF vs = {} THEN "none" ELSE IF Cardinality(vs) = 1 THEN "ok" ELSE "conflict",
                             !.fv = Max(vs)]
       IN /\ cats' = [cats EXCEPT ![gen + 1] = made]
          /\ Flag(IF Cardinality(vs) > 1 /\ made.st = "ok" THEN {"rebuilt-one"} ELSE {})
    /\ gen' = gen + 1
    /\ kept' = kept \cup {gen}
    /\ rb' = [rb EXCEPT !.ph = "idle", !.seen = {}]
    /\ w' = [w EXCEPT !.rebuilds = @ + 1]
    /\ UNCHANGED <<pc, vw>>

-----------------------------------------------------------------------------
UserNext == \/ \E d \in Disks : Edit(d) \/ Delete(d)
            \/ VOpen \/ VWrite \/ VClose \/ VStop \/ VStart \/ Reject
WorldNext == \E d \in Disks : Lose(d) \/ Back(d) \/ ReadOnly(d) \/ Tick(d)
AtollNext == \/ Pass \/ FileGone \/ Act \/ XPurge \/ Commit \/ Reprise \/ JXfer
             \/ \E d \in Disks : JCreate(d) \/ JTrash(d) \/ Purge(d) \/ Clean(d)
RebuildNext == RbStart \/ RbSwap \/ \E d \in Disks : RbList(d) \/ RbScan(d)
Next == UserNext \/ WorldNext \/ AtollNext \/ RebuildNext \/ Kill

Spec == Init /\ [][Next]_vars
\* The daemon and a rebuild once started go on, the view closes what it opened, a disk lost comes back.
LiveSpec == Spec /\ WF_vars(AtollNext) /\ WF_vars(RbSwap \/ \E d \in Disks : RbList(d) \/ RbScan(d))
                 /\ WF_vars(VClose) /\ \A d \in Disks : WF_vars(Back(d))

-----------------------------------------------------------------------------
(* What it checks. *)
TypeOK ==
    /\ gen \in Gens /\ kept \subseteq Gens /\ pc \in {"idle", "act", "purge", "commit"}
    /\ rb.ph \in {"idle", "list", "scan"} /\ vw.fd \in {"none", "path", "trash", "gone"}
    /\ Cat.st \in {"none", "ok", "conflict"}

\* Invariants 1 and 9, §15.7, §21.2: what a user or a program last wrote on a copy, directly or through the view, every
\* version a reject kept among them, is at the file's path on some disk: never overwritten, sent to the trash or lost
\* by Atoll, whatever a rebuild lost of what the catalog knew. A disk missing keeps what it holds.
NoUserDataLost == \A v \in ghost.heads : \E d \in Disks : w.at[d] = v
\* §9, §15.7: a rebuild makes a file whose copies hold two versions a conflict, every version kept.
RebuiltConflict == "rebuilt-one" \notin ghost.flags
\* Invariant 4: a pass decides nothing from a disk its check does not confirm.
MissingNotEmpty == "missing-read" \notin ghost.flags
\* §9, §14.2: what a catalog's operation left in tmp/ is never cleaned by another catalog, which knows nothing of it: a
\* new catalog's operations start past every id the disks name.
PartsKept == "part-of-another" \notin ghost.flags
\* §9: every catalog a rebuild set aside is kept.
OldKept == kept = 1..(gen - 1)

\* Liveness: once the user and the world are done, every disk back, the pool settles: nothing planned or under way,
\* the file in conflict, gone, or in sync at its path on every data disk, the cache's copy transferred.
Settled ==
    /\ pc = "idle" /\ rb.ph = "idle" /\ Cat.op.k = "none" /\ vw.open = 0
    /\ \/ Cat.st = "conflict" \/ Cat.held # 0
       \/ Cat.st = "none" /\ \A d \in Disks : w.at[d] = 0
       \/ /\ Cat.st = "ok"
          /\ \A d \in Data : ~Writable(d) \/ InSync(Cat, d)
          /\ Cache /\ Writable(1) => ~Cat.row[1].on
EventuallySettled == <>[]Settled
=============================================================================
