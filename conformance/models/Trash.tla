-------------------------------- MODULE Trash --------------------------------
(* The trash, where Atoll destroys data for good (specs.md §17, §17.1 to §17.4, §19, §21.2; invariants 1, 2, 10, 11
   and 12): a model of its own. Two disks, each holding a copy of each file at the start, at the same path. A file's
   data is an object: a file with its inode number, which its filesystem gives again once no name leads to it, and
   which may have several names, its path in the root, an entry's path in the trash and, with the link guard, its
   link in links/. Time is the commands': a tick between two of them, as the daemon's purge comes after its pass.

   What it keeps: each disk's objects, their inode and version, and their names; whether a disk is mounted, missing
   or read-only; the catalog: each file's row and version, each copy's row (ok or surplus, its inode and version), the
   journal (each operation's kind, state and inode, and for a move to the trash its reason, its expiry and the version
   it was sent with) and the trash's rows; the pass's queue of trash and copy jobs. And Atoll: one process, as each
   command takes the pool's lock, each starting with the reprise of what the journal holds planned on each disk it can
   write to (§17.2), in the order of the operations:
   - a pass, simplified: each file decided from what its disks hold (a copy gone: a deletion, the other copy, unchanged
     since in sync, to the trash, deleted; a copy changed: a modification, the other copy refreshed through the
     trash, overwritten; a copy where the catalog has none: recorded, or its file made again), then its jobs (a move
     to the trash, §17.1: journaled with its row, catalog.BeginTrash; the copy compared with its row right before the
     move, ops.ToTrash; the commit, catalog.FinishTrash; a copy, one step here, Replication.tla's), then with the link
     guard each disk's links decided from one read (solver.PlanLinks) and changed one at a time (trash.Session.Guard:
     kept, removed by ops.DropLink, sent to the trash from the link, made by ops.MakeLink);
   - atoll prune (§17.3): the copies looked at, then the surplus copy to the trash (trash.Session.Prune); with
     "atonce", purged from there at once once the other disk's copy is looked at again, as the SSD cache's transfer
     (§19) and the view's move (§21.2) purge theirs; with "inplace", removed at its path after that look, as the
     view's move does when the trash has no room for it; atoll balance (§13.3), the same as rebalance, by prune's own
     path (trash.Session.Retire);
   - atoll trash restore (§17.4): the newest entry of a path, its path looked at, then the entry moved back without
     replacing anything (catalog.BeginFromTrash, ops.FromTrash, catalog.FinishRestore);
   - a purge (§17, §17.4; trash.Session.Purge, PurgeUntil): what reached its expiry; with "room", what goes for room
     too, surplus, rebalance and overwritten, as the trash may hold more than trash.max_size at any time, a surplus or
     rebalance entry only once a look finds its version on the other disk (trash.Session.purgeFor); with "older",
     anything, as --older-than does, the user's choice; but never an entry the reprise of the same run sent there.
     For each: the links of the guard that lead to it removed first (dropLinksOf, ops.OwnLinksOf), then the operation
     unlink journaled, the name removed if it holds the entry's inode (ops.PurgeEntry), and the commit
     (catalog.FinishPurge).
   A user acts directly on a disk at any moment: deletes, replaces by an atomic save, edits in place or makes a copy;
   edits, removes or takes back by hand an entry of the trash; unmounts a disk or mounts it read-only; and Atoll's
   process may be killed between any two steps.

   What it leaves out: the guards (Guards.tla's: a decision here never waits for one), renames and folders, the
   folders a restore makes among them (Rename.tla's, Folders.tla's, Nested.tla's), the steps of a copy, a move without
   RENAME_NOREPLACE, the power cut and Atoll's own view (Replication.tla's), the SSD cache's own rules and the view's
   move (Replication.tla's, ViewMove.tla's: here their purge at once alone, after its look), the size of each entry
   and the order of a purge for room (conformance/vectors/trash_purge.json), restore --to, the chunks of a repair:
   see README.md.

   Flaw 28, fixed (README.md): with the link guard off, a surplus copy that prune, or the SSD cache's transfer, sent
   to the trash while a user deleted, directly on their disks, the copies that stay, after the look at them and before
   the move, reached the trash as surplus, the deleted file's last data, and a purge for room took it before the
   retention a deletion gives it; balance's, as rebalance, the same. The maintainer's remedy, surplus-looked
   (2026-11-26), with rebalance, is what every instance does. *)
EXTENDS Integers, Sequences, FiniteSets, TLC

CONSTANTS
    NFiles,     \* files, each with a copy on disk 1 and on disk 2 at the start
    Level,      \* 1: disk 1's copy is surplus at the start, disk 2's alone counts; 2: both count
    Guard,      \* trash.link_guard: TRUE, each copy with its link at the start
    UserActs,   \* some of "del", "rep", "edit", "new", "tedit", "trm", "tout", "lose", "ro", "stop"
    MaxUser,    \* how many times a user acts
    AtollActs,  \* some of "pass", "prune", "balance", "atonce", "inplace", "restore", "purge", "room", "older"
    MaxTime,    \* how many ticks
    Retention,  \* [trash].retention, in ticks
    NIno,       \* the inode numbers of a disk
    MaxOps,     \* how many operations the journal holds
    Fixes       \* the remedies proposed for the flaws found here and still open, by name (README.md); {} is specs.md as it is

Files == 1..NFiles
Disks == {1, 2}
Other(d) == 3 - d
Inos == 1..NIno
OpIds == 1..MaxOps
NObj == 2 * NFiles + 2 * MaxUser
Objs == 1..NObj
\* flaw 28 is fixed, with surplus-looked, what every instance does (README.md): no remedy is proposed
FixNames == {}

ASSUME /\ NFiles \in 1..2 /\ Level \in 1..2 /\ Guard \in BOOLEAN /\ MaxUser \in Nat /\ MaxTime \in Nat
       /\ Retention \in Nat /\ NIno >= NFiles /\ MaxOps \in Nat
       /\ UserActs \subseteq {"del", "rep", "edit", "new", "tedit", "trm", "tout", "lose", "ro", "stop"}
       /\ AtollActs \subseteq {"pass", "prune", "balance", "atonce", "inplace", "restore", "purge", "room", "older"}
       /\ Fixes \subseteq FixNames

(* The reasons a purge for room takes before their retention ends (§17), surplus and rebalance ranked first; the
   other one here, deleted, never. *)
RoomRsn == {"surplus", "rebalance", "overwritten"}
(* surplus-looked, the maintainer's remedy of flaw 28 (2026-11-26), with rebalance: these go for room only once a
   look finds their version on another disk (solver.LooksFirst). *)
LooksFirst(rsn) == rsn \in {"surplus", "rebalance"}

VARIABLES
    fs,     \* the disks: [obj |-> [Objs -> [d, ino, v]], root, lk |-> [Disks -> [Files -> object or 0]],
            \*   tr |-> [OpIds -> object or 0]: what the trash path of each move holds, ds |-> [Disks -> state]]
    cat,    \* the catalog: [cf |-> [Files -> [on, v, cfl]], cr |-> [Disks -> [Files -> [st, key, v]]],
            \*   ops |-> [OpIds -> operation], ent |-> [OpIds -> BOOLEAN]: a move's trash row, jobs |-> the queue]
    pc,     \* Atoll's process
    w,      \* [now, users, nv |-> the next version, no |-> the next object, ses |-> [Disks -> state]: each disk as
            \*   the session of the command that runs found it at its start, missing once lost in it]
    ghost   \* for the invariants only: [udest |-> the versions a user's own action took the last of, kept |-> set of
            \*   <<f, v, until>>: a version of f a user deleted while the pool held it elsewhere, uv |-> [Files -> the
            \*   version a user has at the path, 0 once deleted], flags]

vars == <<fs, cat, pc, w, ghost>>

NoOp == [k |-> "", st |-> "", d |-> 0, f |-> 0, key |-> 0, src |-> "", rsn |-> "", exp |-> 0, v |-> 0, e |-> 0, o |-> 0]
NoRow == [st |-> "none", key |-> 0, v |-> 0]
Dead == [d |-> 0, ino |-> 0, v |-> 0]
Idle == [ph |-> "idle"]
Min(S) == CHOOSE x \in S : \A y \in S : x <= y

(* At the start: file f has version f, its object 2f-1 on disk 1 and 2f on disk 2, inode f on each. *)
Init ==
    /\ fs = [obj |-> [o \in Objs |-> IF o <= 2 * NFiles THEN [d |-> 2 - (o % 2), ino |-> (o + 1) \div 2, v |-> (o + 1) \div 2]
                                    ELSE Dead],
             root |-> [d \in Disks |-> [f \in Files |-> 2 * f - 2 + d]],
             lk |-> [d \in Disks |-> [f \in Files |-> IF Guard THEN 2 * f - 2 + d ELSE 0]],
             tr |-> [p \in OpIds |-> 0],
             ds |-> [d \in Disks |-> "on"]]
    /\ cat = [cf |-> [f \in Files |-> [on |-> TRUE, v |-> f, cfl |-> FALSE]],
              cr |-> [d \in Disks |-> [f \in Files |-> [st |-> IF d = 1 /\ Level = 1 THEN "surplus" ELSE "ok", key |-> f, v |-> f]]],
              ops |-> [p \in OpIds |-> NoOp],
              ent |-> [p \in OpIds |-> FALSE],
              jobs |-> <<>>]
    /\ pc = Idle
    /\ w = [now |-> 0, users |-> 0, nv |-> NFiles + 1, no |-> 2 * NFiles + 1, ses |-> [d \in Disks |-> "on"]]
    /\ ghost = [udest |-> {}, kept |-> {}, uv |-> [f \in Files |-> f], flags |-> {}]

-----------------------------------------------------------------------------
(* The objects: those a name leads to, how many names each has, the versions they hold, a free inode. *)
Refs(F) == ({F.root[d][f] : d \in Disks, f \in Files} \cup {F.lk[d][f] : d \in Disks, f \in Files}
            \cup {F.tr[p] : p \in OpIds}) \ {0}
Names(F, o) == Cardinality({x \in Disks \X Files : F.root[x[1]][x[2]] = o})
               + Cardinality({x \in Disks \X Files : F.lk[x[1]][x[2]] = o})
               + Cardinality({p \in OpIds : F.tr[p] = o})
NameSet(F) == {<<"r", x[1], x[2], F.root[x[1]][x[2]]>> : x \in {y \in Disks \X Files : F.root[y[1]][y[2]] # 0}}
              \cup {<<"l", x[1], x[2], F.lk[x[1]][x[2]]>> : x \in {y \in Disks \X Files : F.lk[y[1]][y[2]] # 0}}
              \cup {<<"t", p, 0, F.tr[p]>> : p \in {q \in OpIds : F.tr[q] # 0}}
(* what a step took the last of *)
LostBy(F1, F2) == {F1.obj[o].v : o \in Refs(F1)} \ {F2.obj[o].v : o \in Refs(F2)}
FreeInos(F, d) == {i \in Inos : ~\E o \in Refs(F) : F.obj[o].d = d /\ F.obj[o].ino = i}
(* an object no name leads to is no state of the disk *)
Canon(F) == [F EXCEPT !.obj = [o \in Objs |-> IF o \in Refs(F) THEN F.obj[o] ELSE Dead]]
Up(d) == fs.ds[d] = "on"
(* A session uses a disk only as its mount check found it at its start: one lost since, or unmounted and mounted
   again, is lost for it, as the device id is checked again before each operation (§17.1). *)
Writable(d) == w.ses[d] = "on"
Readable(d) == w.ses[d] # "miss"

(* The catalog's word. *)
InTrash(p) == cat.ent[p] /\ cat.ops[p].k = "trash" /\ cat.ops[p].st = "done"
TrashKeys(d) == {cat.ops[p].key : p \in {q \in OpIds : InTrash(q) /\ cat.ops[q].d = d}}
FreeOps == {p \in OpIds : cat.ops[p].k = ""}
Rows(f) == {d \in Disks : cat.cr[d][f].st # "none"}
(* a copy as its row says it, as §15.10 compares a known copy: its inode, and the version its change time says *)
SameAsRow(d, f) == LET o == fs.root[d][f] IN o # 0 /\ fs.obj[o].ino = cat.cr[d][f].key /\ fs.obj[o].v = cat.cr[d][f].v
(* a removal at its path that a purge at once has planned: the copy it takes is no keeper of a version (§21.2) *)
RemovedAtOnce(d, f) == \E p \in OpIds : cat.ops[p].k = "rmroot" /\ cat.ops[p].st = "planned" /\ cat.ops[p].d = d /\ cat.ops[p].f = f
(* a version of a file the pool holds beside the trash: at the file's path, or with the guard in the link of a copy
   deleted around Atoll, as the guard then sends it to the trash, deleted (§17); not in a link whose data is an entry of
   the trash, Atoll's own move, nor in one whose copy a user replaced, an old version, which goes there overwritten *)
InPool(F, f, v) == \E o \in Refs(F) : F.obj[o].v = v
                       /\ \E d \in Disks : (F.root[d][f] = o /\ ~RemovedAtOnce(d, f))
                                           \/ (F.lk[d][f] = o /\ F.root[d][f] = 0 /\ ~\E p \in OpIds : F.tr[p] = o)

(* What Atoll must keep, in the state before a step of its own: the version a user has at a path; a version a user
   deleted while the pool held it elsewhere, for the retention, as its other copies then go to the trash, deleted
   (§15.5, §17), or with the link guard its link (§17); and an entry deleted, within its retention (§17). *)
KeptNow ==
    (({ghost.uv[f] : f \in Files} \ {0})
     \cup {k[2] : k \in {x \in ghost.kept : w.now < x[3]}}
     \cup {cat.ops[p].v : p \in {q \in OpIds : InTrash(q) /\ cat.ops[q].rsn = "deleted" /\ w.now < cat.ops[q].exp
                                               \* as a user left it there: one edited by hand holds no promise
                                               /\ fs.tr[q] # 0 /\ fs.obj[fs.tr[q]].v = cat.ops[q].v}})
    \ ghost.udest

(* A step of Atoll's, under the operation p (0 for none): the disks as it leaves them; what it took the last of,
   against what it must keep; and no name taken away but by a step of an operation planned (invariant 12). *)
AtollFlags(F, p) ==
    ghost.flags \cup (IF LostBy(fs, F) \cap KeptNow # {} THEN {"lost"} ELSE {})
                \cup (IF NameSet(fs) \ NameSet(F) # {} /\ (p = 0 \/ cat.ops[p].st # "planned") THEN {"unjournaled"} ELSE {})
AtollFs(F, p) ==
    /\ fs' = Canon(F)
    /\ ghost' = [ghost EXCEPT !.flags = AtollFlags(F, p)]
(* the purge of an entry whose retention ended: the trash's promise kept, whatever it held (§17) *)
ExpiredFs(F, p) ==
    /\ fs' = Canon(F)
    /\ ghost' = [ghost EXCEPT !.flags = AtollFlags(F, p) \ ({"lost"} \ ghost.flags)]

NewOp(p, op) == cat' = [cat EXCEPT !.ops[p] = op]
Done(p) == [cat.ops[p] EXCEPT !.st = "done"]
Undone(p) == [cat.ops[p] EXCEPT !.st = "undone"]

-----------------------------------------------------------------------------
(* The world: a user, directly on a disk that is mounted, at any moment. *)
UserStep(a) == a \in UserActs /\ w.users < MaxUser
(* a user who writes a file again moves on from what they deleted of it: an old version now, the trash's to purge *)
UserGhost(F, uv) == [ghost EXCEPT !.udest = @ \cup LostBy(fs, F), !.uv = uv,
                                  !.kept = {k \in @ : uv[k[1]] = ghost.uv[k[1]]}]

(* deletes a copy: a deletion of the file (§15.5), kept for the retention while the pool still holds it; one of an old
   version beside the one the user has is a replacement's, a modification (invariant 10). A mix of versions, the
   user's own, leaves no claim on the file here: what the passes make of it is Replication.tla's. *)
UDel(d, f) ==
    /\ UserStep("del") /\ Up(d) /\ fs.root[d][f] # 0
    /\ LET v == fs.obj[fs.root[d][f]].v
           F == [fs EXCEPT !.root[d][f] = 0]
           left == {F.obj[F.root[e][f]].v : e \in {x \in Disks : F.root[x][f] # 0 /\ ~RemovedAtOnce(x, f)}}
           deletion == ghost.uv[f] \in {v, 0} /\ left \subseteq {v}
       IN /\ fs' = Canon(F)
          /\ ghost' = [ghost EXCEPT !.udest = @ \cup LostBy(fs, F),
                                    !.uv[f] = IF deletion THEN 0 ELSE IF left \subseteq {@} THEN @ ELSE 0,
                                    !.kept = IF deletion /\ InPool(F, f, v) THEN @ \cup {<<f, v, w.now + Retention>>} ELSE @]
          /\ w' = [w EXCEPT !.users = @ + 1]
    /\ UNCHANGED <<cat, pc>>

(* a new file at the path: an atomic save that replaces a copy ("rep"), or a file made where there is none ("new") *)
UWrite(a, d, f) ==
    /\ UserStep(a) /\ Up(d) /\ w.no <= NObj /\ FreeInos(fs, d) # {}
    /\ IF a = "rep" THEN fs.root[d][f] # 0 ELSE fs.root[d][f] = 0
    /\ LET F == [fs EXCEPT !.obj[w.no] = [d |-> d, ino |-> Min(FreeInos(fs, d)), v |-> w.nv], !.root[d][f] = w.no]
       IN /\ fs' = Canon(F)
          /\ ghost' = UserGhost(F, [ghost.uv EXCEPT ![f] = w.nv])
          /\ w' = [w EXCEPT !.users = @ + 1, !.nv = @ + 1, !.no = @ + 1]
    /\ UNCHANGED <<cat, pc>>

(* an edit in place: the same inode, a new version, its change time moved *)
UEdit(d, f) ==
    /\ UserStep("edit") /\ Up(d) /\ fs.root[d][f] # 0
    /\ LET F == [fs EXCEPT !.obj[fs.root[d][f]].v = w.nv]
       IN /\ fs' = Canon(F)
          /\ ghost' = UserGhost(F, [ghost.uv EXCEPT ![f] = w.nv])
          /\ w' = [w EXCEPT !.users = @ + 1, !.nv = @ + 1]
    /\ UNCHANGED <<cat, pc>>

(* an entry of the trash, by hand: edited in place, removed, or taken back to its path *)
UTrash(a, p) ==
    /\ UserStep(a) /\ fs.tr[p] # 0 /\ Up(fs.obj[fs.tr[p]].d)
    /\ LET o == fs.tr[p]
           d == fs.obj[o].d
           f == cat.ops[p].f
           F == CASE a = "tedit" -> [fs EXCEPT !.obj[o].v = w.nv]
                  [] a = "trm" -> [fs EXCEPT !.tr[p] = 0]
                  [] a = "tout" -> [fs EXCEPT !.tr[p] = 0, !.root[d][f] = o]
       IN /\ a = "tout" => fs.root[d][f] = 0
          /\ fs' = Canon(F)
          /\ ghost' = UserGhost(F, IF a = "tout" THEN [ghost.uv EXCEPT ![f] = fs.obj[o].v] ELSE ghost.uv)
          /\ w' = [w EXCEPT !.users = @ + 1, !.nv = IF a = "tedit" THEN @ + 1 ELSE @]
    /\ UNCHANGED <<cat, pc>>

(* a disk unmounted, its mount point empty, or mounted read-only; it comes back by itself (UBack) *)
UDisk(a, d) ==
    /\ UserStep(a) /\ Up(d)
    /\ fs' = [fs EXCEPT !.ds[d] = IF a = "lose" THEN "miss" ELSE "ro"]
    /\ w' = [w EXCEPT !.users = @ + 1, !.ses[d] = "miss"]
    /\ UNCHANGED <<cat, pc, ghost>>
UBack(d) ==
    /\ fs.ds[d] # "on"
    /\ fs' = [fs EXCEPT !.ds[d] = "on"]
    /\ UNCHANGED <<cat, pc, w, ghost>>

(* Atoll killed between two steps: the next command starts from what the journal holds. *)
UStop ==
    /\ UserStep("stop") /\ pc # Idle
    /\ pc' = Idle /\ w' = [w EXCEPT !.users = @ + 1]
    /\ UNCHANGED <<fs, cat, ghost>>

Tick ==
    /\ pc = Idle /\ w.now < MaxTime
    /\ w' = [w EXCEPT !.now = @ + 1]
    /\ UNCHANGED <<fs, cat, pc, ghost>>

World ==
    \/ \E d \in Disks, f \in Files : UDel(d, f) \/ UWrite("rep", d, f) \/ UWrite("new", d, f) \/ UEdit(d, f)
    \/ \E p \in OpIds : UTrash("tedit", p) \/ UTrash("trm", p) \/ UTrash("tout", p)
    \/ \E d \in Disks : UDisk("lose", d) \/ UDisk("ro", d) \/ UBack(d)
    \/ UStop \/ Tick

-----------------------------------------------------------------------------
(* §17.2 The reprise, first in every command: the planned operations of each disk it can write to, in the order of
   their ids, each by what both of its names hold, against the operation's inode. *)
RepTodo == {p \in OpIds : cat.ops[p].st = "planned" /\ Writable(cat.ops[p].d)}
Start(cmd, arg) ==
    /\ pc = Idle
    /\ pc' = [ph |-> "rep", cmd |-> cmd, arg |-> arg, held |-> {}]
    /\ w' = [w EXCEPT !.ses = fs.ds]
    /\ UNCHANGED <<fs, cat, ghost>>

Seen(o, key) == IF o = 0 THEN "absent" ELSE IF fs.obj[o].ino = key THEN "same" ELSE "other"
(* a move to the trash: a target with the copy's inode is the copy only with the version its row says (§17.2); a
   link has no row to be compared with *)
SeenCopy(o, op) == IF o = 0 THEN "absent" ELSE IF fs.obj[o].ino = op.key /\ (op.src = "link" \/ fs.obj[o].v = op.v) THEN "same" ELSE "other"

(* §17.1 step 6: the copy's row gone; a deletion's last copy takes the file's row with it *)
TrashedRows(op) ==
    LET cr2 == IF op.src = "root" THEN [cat.cr EXCEPT ![op.d][op.f] = NoRow] ELSE cat.cr
    IN [cf |-> IF op.rsn = "deleted" /\ \A e \in Disks : cr2[e][op.f].st = "none"
               THEN [cat.cf EXCEPT ![op.f].on = FALSE] ELSE cat.cf,
        cr |-> cr2]
(* §17.4: a restored copy, a user copy, ok, as the move found it; the file made again when the catalog had none, a
   conflict when it holds another version *)
RestoredRows(op, ino, v) ==
    [cf |-> IF ~cat.cf[op.f].on THEN [cat.cf EXCEPT ![op.f] = [on |-> TRUE, v |-> v, cfl |-> FALSE]]
            ELSE IF cat.cf[op.f].v # v THEN [cat.cf EXCEPT ![op.f].cfl = TRUE] ELSE cat.cf,
     cr |-> [cat.cr EXCEPT ![op.d][op.f] = [st |-> "ok", key |-> ino, v |-> v]]]

RepStep ==
    /\ pc.ph = "rep" /\ RepTodo # {}
    /\ LET p == Min(RepTodo)
           op == cat.ops[p]
           d == op.d
       IN CASE op.k = "trash" ->
                 LET T == fs.tr[p]
                     S == IF op.src = "root" THEN fs.root[d][op.f] ELSE fs.lk[d][op.f]
                     t == SeenCopy(T, op)
                     s == Seen(S, op.key)
                     rows == TrashedRows(op)
                 IN IF t = "same"
                    THEN \* it moved: it finishes, its retention starting again, and this run does not purge it
                         /\ cat' = [cat EXCEPT !.ops[p] = [Done(p) EXCEPT !.exp = w.now + Retention, !.o = T],
                                               !.cf = rows.cf, !.cr = rows.cr]
                         /\ pc' = [pc EXCEPT !.held = @ \cup {p}]
                         /\ UNCHANGED <<fs, ghost>>
                    ELSE IF t = "other" /\ s = "absent"
                    THEN \* what took the target's name goes back to the source's first: nothing else writes in the trash
                         /\ AtollFs(IF op.src = "root" THEN [fs EXCEPT !.tr[p] = 0, !.root[d][op.f] = T]
                                    ELSE [fs EXCEPT !.tr[p] = 0, !.lk[d][op.f] = T], p)
                         /\ cat' = [cat EXCEPT !.ops[p] = Undone(p), !.ent[p] = FALSE]
                         /\ UNCHANGED pc
                    ELSE /\ cat' = [cat EXCEPT !.ops[p] = Undone(p), !.ent[p] = FALSE]
                         /\ UNCHANGED <<fs, pc, ghost>>
            [] op.k = "unlink" ->
                 \* a purge: the entry's name by its inode; absent, it finishes
                 /\ IF Seen(fs.tr[op.e], op.key) = "absent"
                    THEN cat' = [cat EXCEPT !.ops[p] = Done(p), !.ent[op.e] = FALSE]
                    ELSE cat' = [cat EXCEPT !.ops[p] = Undone(p)]
                 /\ UNCHANGED <<fs, pc, ghost>>
            [] op.k = "rmroot" ->
                 \* a removal at its path: absent, it finishes
                 /\ IF Seen(fs.root[d][op.f], op.key) = "absent"
                    THEN cat' = [cat EXCEPT !.ops[p] = Done(p), !.cr[d][op.f] = NoRow]
                    ELSE cat' = [cat EXCEPT !.ops[p] = Undone(p)]
                 /\ UNCHANGED <<fs, pc, ghost>>
            [] op.k = "restore" ->
                 LET T == fs.root[d][op.f]
                     rows == RestoredRows(op, op.key, fs.obj[T].v)
                 IN IF Seen(T, op.key) = "same" /\ Seen(fs.tr[op.e], op.key) # "same"
                    THEN /\ cat' = [cat EXCEPT !.ops[p] = Done(p), !.ent[op.e] = FALSE, !.cf = rows.cf, !.cr = rows.cr]
                         /\ ghost' = [ghost EXCEPT
                                      !.flags = IF T # cat.ops[op.e].o /\ cat.ops[op.e].o \in Refs(fs) THEN @ \cup {"restore-other"} ELSE @]
                         /\ UNCHANGED <<fs, pc>>
                    ELSE /\ cat' = [cat EXCEPT !.ops[p] = Undone(p)]
                         /\ UNCHANGED <<fs, pc, ghost>>
            [] op.k = "ldrop" ->
                 /\ cat' = [cat EXCEPT !.ops[p] = IF fs.lk[d][op.f] = 0 THEN Done(p) ELSE Undone(p)]
                 /\ UNCHANGED <<fs, pc, ghost>>
            [] op.k = "link" ->
                 /\ cat' = [cat EXCEPT !.ops[p] = IF Seen(fs.lk[d][op.f], op.key) = "same" THEN Done(p) ELSE Undone(p)]
                 /\ UNCHANGED <<fs, pc, ghost>>
    /\ UNCHANGED w

(* The reprise over: the command's own work. *)
RepEnd ==
    /\ pc.ph = "rep" /\ RepTodo = {}
    /\ pc' = CASE pc.cmd = "pass" -> [ph |-> "dec", todo |-> Files]
               [] pc.cmd = "prune" -> [ph |-> "pr", todo |-> Files, rsn |-> "surplus"]
               [] pc.cmd = "balance" -> [ph |-> "pr", todo |-> Files, rsn |-> "rebalance"]
               [] pc.cmd = "restore" -> [ph |-> "rs", e |-> pc.arg]
               [] OTHER -> [ph |-> "pg", mode |-> pc.cmd, seen |-> {}, held |-> pc.held]
    /\ UNCHANGED <<fs, cat, w, ghost>>

-----------------------------------------------------------------------------
(* §15.12 A pass, simplified: each file decided from what its disks hold, both mounted (a disk away holds its
   decisions), then its jobs, then the link guard. *)
Decide ==
    /\ pc.ph = "dec" /\ pc.todo # {}
    /\ LET f == Min(pc.todo)
           Stale == {d \in Rows(f) : cat.cr[d][f].st = "stale"}
           Changed == {d \in Rows(f) : fs.root[d][f] # 0 /\ ~SameAsRow(d, f)}
           Gone == {d \in Rows(f) : fs.root[d][f] = 0}
           New == {d \in Disks \ Rows(f) : fs.root[d][f] # 0}
           \* a new decision of the file: every queued job of it goes first, as what they were for no longer holds
           rest == SelectSeq(cat.jobs, LAMBDA j : j.f # f)
           copyTo(e, d) == <<[k |-> "copy", d |-> e, f |-> f, rsn |-> "", from |-> d]>>
       IN /\ pc' = [pc EXCEPT !.todo = @ \ {f}]
          /\ IF ~(Writable(1) /\ Writable(2)) \/ cat.cf[f].cfl THEN UNCHANGED cat
             ELSE CASE Changed # {} ->
                       \* two changed, or a stale copy changed: a conflict, every version kept
                       IF Cardinality(Changed) = 2 \/ Changed \cap Stale # {} THEN cat' = [cat EXCEPT !.cf[f].cfl = TRUE, !.jobs = rest]
                       ELSE LET d == CHOOSE x \in Changed : TRUE
                                e == Other(d)
                                o == fs.root[d][f]
                            IN \* §15.4: the other copy stale, refreshed through the trash; a copy gone beside it made again
                               cat' = [cat EXCEPT
                                 !.cf[f] = [on |-> TRUE, v |-> fs.obj[o].v, cfl |-> FALSE],
                                 !.cr[d][f] = [st |-> "ok", key |-> fs.obj[o].ino, v |-> fs.obj[o].v],
                                 !.cr[e][f] = IF e \notin Rows(f) THEN @ ELSE IF fs.root[e][f] = 0 THEN NoRow ELSE [@ EXCEPT !.st = "stale"],
                                 !.jobs = IF e \in Rows(f) /\ fs.root[e][f] # 0
                                          THEN rest \o <<[k |-> "trash", d |-> e, f |-> f, rsn |-> "overwritten", from |-> 0]>> \o copyTo(e, d)
                                          ELSE IF e \in Rows(f) THEN rest \o copyTo(e, d)
                                          ELSE rest]
                  [] Gone \ Stale # {} ->
                       IF Gone = Rows(f)
                       THEN cat' = [cat EXCEPT !.cr = [d \in Disks |-> [cat.cr[d] EXCEPT ![f] = NoRow]], !.cf[f].on = FALSE,
                                               !.jobs = rest]
                       ELSE LET d == CHOOSE x \in Gone \ Stale : TRUE
                            IN \* §15.5: the deletion reaches the other copy, unchanged since in sync, through the trash
                               cat' = [cat EXCEPT !.cr[d][f] = NoRow,
                                         !.jobs = rest \o <<[k |-> "trash", d |-> Other(d), f |-> f, rsn |-> "deleted", from |-> 0]>>]
                  [] Gone # {} ->
                       \* a stale copy gone is no deletion: it is made again from the current one
                       LET d == CHOOSE x \in Gone : TRUE
                       IN cat' = [cat EXCEPT !.cr[d][f] = NoRow, !.jobs = rest \o copyTo(d, Other(d))]
                  [] New # {} ->
                       LET d == CHOOSE x \in New : TRUE
                           o == fs.root[d][f]
                       IN IF cat.cf[f].on /\ cat.cf[f].v # fs.obj[o].v THEN cat' = [cat EXCEPT !.cf[f].cfl = TRUE]
                          ELSE cat' = [cat EXCEPT !.cf[f] = [on |-> TRUE, v |-> fs.obj[o].v, cfl |-> FALSE],
                                                  !.cr[d][f] = [st |-> "ok", key |-> fs.obj[o].ino, v |-> fs.obj[o].v]]
                  [] OTHER -> UNCHANGED cat
    /\ UNCHANGED <<fs, w, ghost>>
DecEnd ==
    /\ pc.ph = "dec" /\ pc.todo = {}
    /\ pc' = [ph |-> "job"]
    /\ UNCHANGED <<fs, cat, w, ghost>>

(* §17.1 A move to the trash, of a copy (src root) or of a link (src link): journaled with its trash row, then the
   move, the copy compared with its row right before it, then the commit. *)
TJournal(d, f, src, rsn, ret) ==
    /\ FreeOps # {}
    /\ LET p == Min(FreeOps)
           key == IF src = "root" THEN cat.cr[d][f].key ELSE fs.obj[fs.lk[d][f]].ino
           v == IF src = "root" THEN cat.cr[d][f].v ELSE fs.obj[fs.lk[d][f]].v
       IN /\ cat' = [cat EXCEPT !.ops[p] = [k |-> "trash", st |-> "planned", d |-> d, f |-> f, key |-> key, src |-> src,
                                            rsn |-> rsn, exp |-> w.now + Retention, v |-> v, e |-> p, o |-> 0],
                                !.ent[p] = TRUE,
                                !.jobs = IF ret.ph = "job" THEN Tail(@) ELSE @]
          /\ pc' = [ph |-> "tm", p |-> p, ret |-> ret]
TMove ==
    /\ pc.ph = "tm"
    /\ LET p == pc.p
           op == cat.ops[p]
           S == IF op.src = "root" THEN fs.root[op.d][op.f] ELSE fs.lk[op.d][op.f]
       IN /\ Writable(op.d)
          /\ IF SeenCopy(S, op) = "same"
             THEN /\ AtollFs(IF op.src = "root" THEN [fs EXCEPT !.root[op.d][op.f] = 0, !.tr[p] = S]
                             ELSE [fs EXCEPT !.lk[op.d][op.f] = 0, !.tr[p] = S], p)
                  /\ cat' = [cat EXCEPT !.ops[p].o = S]
                  /\ pc' = [pc EXCEPT !.ph = "tc"]
             ELSE \* changed: nothing moves
                  /\ cat' = [cat EXCEPT !.ops[p] = Undone(p), !.ent[p] = FALSE]
                  /\ pc' = pc.ret
                  /\ UNCHANGED <<fs, ghost>>
    /\ UNCHANGED w
TCommit ==
    /\ pc.ph = "tc"
    /\ LET p == pc.p
           rows == TrashedRows(cat.ops[p])
       IN /\ cat' = [cat EXCEPT !.ops[p] = Done(p), !.cf = rows.cf, !.cr = rows.cr]
          /\ pc' = IF "atonce" \in AtollActs /\ cat.ops[p].rsn = "surplus" THEN [ph |-> "ao", p |-> p, ret |-> pc.ret] ELSE pc.ret
    /\ UNCHANGED <<fs, w, ghost>>

(* The pass's jobs, in order: a trash job journals its move; a copy is one step here (Replication.tla's pipeline). *)
JobRun ==
    /\ pc.ph = "job" /\ cat.jobs # <<>>
    /\ LET j == Head(cat.jobs)
       IN IF ~(Writable(j.d) /\ (j.k = "trash" \/ Readable(j.from)))
          THEN \* the job waits for its disk: the pass goes on to its end
               /\ pc' = [ph |-> "lgs"] /\ UNCHANGED <<fs, cat, w, ghost>>
          ELSE IF j.k = "trash" /\ cat.cr[j.d][j.f].st # "none"
          THEN TJournal(j.d, j.f, "root", j.rsn, pc) /\ UNCHANGED <<fs, w, ghost>>
          ELSE IF j.k = "copy" /\ fs.root[j.d][j.f] = 0 /\ SameAsRow(j.from, j.f) /\ cat.cr[j.d][j.f].st = "none"
                  /\ w.no <= NObj /\ FreeInos(fs, j.d) # {}
          THEN LET o == w.no
                   i == Min(FreeInos(fs, j.d))
                   v == cat.cr[j.from][j.f].v
               IN /\ AtollFs([fs EXCEPT !.obj[o] = [d |-> j.d, ino |-> i, v |-> v], !.root[j.d][j.f] = o], 0)
                  /\ cat' = [cat EXCEPT !.cr[j.d][j.f] = [st |-> "ok", key |-> i, v |-> v], !.jobs = Tail(@)]
                  /\ w' = [w EXCEPT !.no = @ + 1]
                  /\ UNCHANGED pc
          ELSE \* what the job was for no longer holds: it goes, for the next pass to decide
               /\ cat' = [cat EXCEPT !.jobs = Tail(@)] /\ UNCHANGED <<fs, pc, w, ghost>>
JobEnd ==
    /\ pc.ph = "job" /\ cat.jobs = <<>>
    /\ pc' = [ph |-> "lgs"]
    /\ UNCHANGED <<fs, cat, w, ghost>>

(* §17 The link guard, once everything else of the pass ran, on each disk it can write to: its links decided from
   one read (solver.PlanLinks), then changed one at a time, each operation journaled. *)
LinkPlan(d) ==
    [f \in Files |->
        LET o == fs.lk[d][f]
            i == fs.obj[o].ino
        IN IF o # 0
           THEN IF cat.cr[d][f].st # "none" /\ cat.cr[d][f].key = i THEN "keep"
                ELSE IF (\E g \in Files : cat.cr[d][g].st # "none" /\ cat.cr[d][g].key = i /\ fs.root[d][g] # 0)
                        \/ i \in TrashKeys(d) THEN "unlink"
                ELSE IF cat.cr[d][f].st # "none" THEN "overwritten" ELSE "deleted"
           ELSE IF cat.cr[d][f].st # "none" /\ fs.root[d][f] # 0 /\ fs.obj[fs.root[d][f]].ino = cat.cr[d][f].key
           THEN "make" ELSE "none"]
NoPlan == [f \in Files |-> "none"]
LGStart ==
    /\ pc.ph = "lgs"
    /\ pc' = IF Guard THEN [ph |-> "lg", d |-> 1, plan |-> IF Writable(1) THEN LinkPlan(1) ELSE NoPlan] ELSE Idle
    /\ UNCHANGED <<fs, cat, w, ghost>>
LGStep ==
    /\ pc.ph = "lg"
    /\ LET d == pc.d
           todo == {f \in Files : pc.plan[f] \notin {"keep", "none"}}
       IN IF todo = {} \/ ~Writable(d)
          THEN /\ pc' = IF d = 1 THEN [ph |-> "lg", d |-> 2, plan |-> IF Writable(2) THEN LinkPlan(2) ELSE NoPlan] ELSE Idle
               /\ UNCHANGED <<fs, cat, w, ghost>>
          ELSE LET f == Min(todo)
                   act == pc.plan[f]
                   ret == [pc EXCEPT !.plan[f] = "none"]
               IN IF act \in {"deleted", "overwritten"}
                  THEN \* its data lost every name Atoll knows: to the trash, from the link
                       IF fs.lk[d][f] # 0 THEN TJournal(d, f, "link", act, ret) /\ UNCHANGED <<fs, w, ghost>>
                       ELSE pc' = ret /\ UNCHANGED <<fs, cat, w, ghost>>
                  ELSE /\ FreeOps # {}
                       /\ LET p == Min(FreeOps)
                              key == IF act = "unlink" THEN fs.obj[fs.lk[d][f]].ino ELSE cat.cr[d][f].key
                          IN /\ NewOp(p, [NoOp EXCEPT !.k = IF act = "unlink" THEN "ldrop" ELSE "link", !.st = "planned",
                                                      !.d = d, !.f = f, !.key = key])
                             /\ pc' = [ph |-> "la", p |-> p, ret |-> ret]
                       /\ UNCHANGED <<fs, w, ghost>>

(* A link's own operation: ops.DropLink removes it only when it leads to the operation's inode and that has another
   name; ops.MakeLink links the copy's name when it holds the operation's inode. *)
LAct ==
    /\ pc.ph = "la"
    /\ LET p == pc.p
           op == cat.ops[p]
           d == op.d
           L == fs.lk[d][op.f]
           R == fs.root[d][op.f]
       IN /\ Writable(d)
          /\ IF op.k = "ldrop"
             THEN IF L = 0 THEN pc' = [pc EXCEPT !.ph = "lc"] /\ UNCHANGED <<fs, cat, ghost>>
                  ELSE IF fs.obj[L].ino = op.key /\ Names(fs, L) >= 2
                  THEN /\ AtollFs([fs EXCEPT !.lk[d][op.f] = 0], p) /\ pc' = [pc EXCEPT !.ph = "lc"] /\ UNCHANGED cat
                  ELSE /\ cat' = [cat EXCEPT !.ops[p] = Undone(p)] /\ pc' = pc.ret /\ UNCHANGED <<fs, ghost>>
             ELSE IF R # 0 /\ L = 0 /\ fs.obj[R].ino = op.key
             THEN /\ AtollFs([fs EXCEPT !.lk[d][op.f] = R], p) /\ pc' = [pc EXCEPT !.ph = "lc"] /\ UNCHANGED cat
             ELSE /\ cat' = [cat EXCEPT !.ops[p] = Undone(p)] /\ pc' = pc.ret /\ UNCHANGED <<fs, ghost>>
    /\ UNCHANGED w
LCommit ==
    /\ pc.ph = "lc"
    /\ cat' = [cat EXCEPT !.ops[pc.p] = Done(pc.p)]
    /\ pc' = pc.ret
    /\ UNCHANGED <<fs, w, ghost>>

-----------------------------------------------------------------------------
(* §17.3 atoll prune: the copies of a file with a surplus one looked at, compared with their rows; the surplus copy
   to the trash when the one that stays, ok and unchanged on a disk that can be read, reaches the level; with
   "inplace", removed at its path instead, as the view's move does when the trash has no room. §13.3 atoll balance
   sends its old copy the same way, as rebalance (pc.rsn). *)
PrLook ==
    /\ pc.ph = "pr" /\ pc.todo # {}
    /\ LET f == Min(pc.todo)
           ret == [pc EXCEPT !.todo = @ \ {f}]
           S == {d \in Disks : cat.cr[d][f].st = "surplus" /\ Writable(d)}
       IN IF S # {} /\ ~cat.cf[f].cfl
             /\ \A d \in S : SameAsRow(d, f) /\ cat.cr[Other(d)][f].st = "ok" /\ Readable(Other(d)) /\ SameAsRow(Other(d), f)
          THEN LET d == Min(S) IN
               IF "inplace" \in AtollActs /\ pc.rsn = "surplus"
               THEN /\ FreeOps # {}
                    /\ NewOp(Min(FreeOps), [NoOp EXCEPT !.k = "rmroot", !.st = "planned", !.d = d, !.f = f,
                                                        !.key = cat.cr[d][f].key, !.v = cat.cr[d][f].v])
                    /\ pc' = [ph |-> "ip", p |-> Min(FreeOps), ret |-> ret]
               ELSE TJournal(d, f, "root", pc.rsn, ret)
          ELSE pc' = ret /\ UNCHANGED cat
    /\ UNCHANGED <<fs, w, ghost>>
PrEnd ==
    /\ pc.ph = "pr" /\ pc.todo = {}
    /\ pc' = Idle
    /\ UNCHANGED <<fs, cat, w, ghost>>

(* The purge at once of the SSD cache's transfer and of the view's move: the copy on the other disk looked at again,
   as its row says, holding the version sent, right before (trash.Session.dataCopiesHold); otherwise the entry stays,
   changed. *)
(* ops.OwnLinksOf: the links of the guard that lead to an entry's inode, which must have another name, the entry's *)
OwnLinks(p) == LET op == cat.ops[p]
                   T == fs.tr[p]
               IN IF T # 0 /\ fs.obj[T].ino = op.key /\ Names(fs, T) >= 2
                  THEN {g \in Files : fs.lk[op.d][g] # 0 /\ fs.obj[fs.lk[op.d][g]].ino = op.key} ELSE {}
OtherHolds(op) == LET e == Other(op.d) IN
                  Readable(e) /\ cat.cr[e][op.f].st = "ok" /\ SameAsRow(e, op.f) /\ cat.cr[e][op.f].v = op.v
AtOnceLook ==
    /\ pc.ph = "ao"
    \* then the purge, the links that lead to the entry first (§17, A purge)
    /\ pc' = IF Writable(cat.ops[pc.p].d) /\ OtherHolds(cat.ops[pc.p])
             THEN [ph |-> "pl", e |-> pc.p, links |-> OwnLinks(pc.p), ret |-> pc.ret] ELSE pc.ret
    /\ UNCHANGED <<fs, cat, w, ghost>>

(* §21.2 The view's move when the trash has no room: the copy removed at its path, journaled, after the same look. *)
InPlaceLook ==
    /\ pc.ph = "ip"
    /\ IF Writable(cat.ops[pc.p].d) /\ OtherHolds(cat.ops[pc.p])
       THEN pc' = [pc EXCEPT !.ph = "iu"] /\ UNCHANGED cat
       ELSE cat' = [cat EXCEPT !.ops[pc.p] = Undone(pc.p)] /\ pc' = pc.ret
    /\ UNCHANGED <<fs, w, ghost>>
InPlaceUnlink ==
    /\ pc.ph = "iu"
    /\ LET p == pc.p
           op == cat.ops[p]
           R == fs.root[op.d][op.f]
       IN /\ Writable(op.d)
          /\ IF R # 0 /\ fs.obj[R].ino = op.key /\ fs.obj[R].v = op.v
             THEN /\ AtollFs([fs EXCEPT !.root[op.d][op.f] = 0], p)
                  /\ cat' = [cat EXCEPT !.ops[p] = Done(p), !.cr[op.d][op.f] = NoRow]
             ELSE /\ cat' = [cat EXCEPT !.ops[p] = Undone(p)] /\ UNCHANGED <<fs, ghost>>
          /\ pc' = pc.ret
    /\ UNCHANGED w

-----------------------------------------------------------------------------
(* §17.4 atoll trash restore of a path: its newest entry, back to its own disk, the path looked at first. *)
Newest(p) == \A q \in OpIds : (InTrash(q) /\ cat.ops[q].f = cat.ops[p].f) => q <= p
RsLook ==
    /\ pc.ph = "rs"
    /\ LET p == pc.e
           op == cat.ops[p]
       IN IF InTrash(p) /\ Writable(op.d) /\ fs.root[op.d][op.f] = 0 /\ cat.cr[op.d][op.f].st = "none" /\ FreeOps # {}
          THEN /\ NewOp(Min(FreeOps), [NoOp EXCEPT !.k = "restore", !.st = "planned", !.d = op.d, !.f = op.f, !.key = op.key, !.e = p])
               /\ pc' = [ph |-> "rm", p |-> Min(FreeOps)]
          ELSE \* path-taken, or nothing left to restore
               pc' = Idle /\ UNCHANGED cat
    /\ UNCHANGED <<fs, w, ghost>>
RsMove ==
    /\ pc.ph = "rm"
    /\ LET q == pc.p
           op == cat.ops[q]
           T == fs.tr[op.e]
       IN /\ Writable(op.d)
          /\ IF T # 0 /\ fs.obj[T].ino = op.key /\ fs.root[op.d][op.f] = 0
             THEN \* the user asked it back: the version a user has at the path once it is there
                  /\ fs' = Canon([fs EXCEPT !.tr[op.e] = 0, !.root[op.d][op.f] = T])
                  /\ ghost' = [ghost EXCEPT !.uv[op.f] = fs.obj[T].v,
                                            !.flags = AtollFlags([fs EXCEPT !.tr[op.e] = 0, !.root[op.d][op.f] = T], q)]
                  /\ pc' = [ph |-> "rc", p |-> q, o |-> T, ino |-> fs.obj[T].ino, v |-> fs.obj[T].v] /\ UNCHANGED cat
             ELSE \* changed, or path-taken: undone, the entry stays
                  /\ cat' = [cat EXCEPT !.ops[q] = Undone(q)] /\ pc' = Idle /\ UNCHANGED <<fs, ghost>>
    /\ UNCHANGED w
RsCommit ==
    /\ pc.ph = "rc"
    /\ LET q == pc.p
           op == cat.ops[q]
           rows == RestoredRows(op, pc.ino, pc.v)
       IN /\ cat' = [cat EXCEPT !.ops[q] = Done(q), !.ent[op.e] = FALSE, !.cf = rows.cf, !.cr = rows.cr]
          /\ ghost' = [ghost EXCEPT !.flags = IF pc.o # cat.ops[op.e].o THEN @ \cup {"restore-other"} ELSE @]
    /\ pc' = Idle
    /\ UNCHANGED <<fs, w>>

-----------------------------------------------------------------------------
(* §17, §17.4 A purge: what its mode reaches, each entry once, but none the reprise of the same run sent there. *)
Reaches(mode, p) ==
    CASE mode = "purge" -> cat.ops[p].exp <= w.now
      [] mode = "room" -> cat.ops[p].exp <= w.now \/ cat.ops[p].rsn \in RoomRsn
      [] mode = "older" -> TRUE
PgPick ==
    /\ pc.ph = "pg"
    /\ LET C == {p \in OpIds : InTrash(p) /\ p \notin pc.seen /\ p \notin pc.held /\ Writable(cat.ops[p].d) /\ Reaches(pc.mode, p)}
       IN IF C = {} THEN pc' = Idle
          ELSE LET p == Min(C)
                   op == cat.ops[p]
                   L == OwnLinks(p)
                   \* surplus-looked: a surplus or rebalance entry goes for room before its retention only once a look
                   \* finds its version at the file's path on another disk, as that copy's row says; never by age
                   looks == pc.mode = "room" /\ LooksFirst(op.rsn) /\ op.exp > w.now
               IN pc' = [ph |-> IF looks THEN "pk" ELSE "pl", e |-> p, links |-> L, ret |-> [pc EXCEPT !.seen = @ \cup {p}]]
    /\ UNCHANGED <<fs, cat, w, ghost>>
PgLook ==
    /\ pc.ph = "pk"
    /\ pc' = IF OtherHolds(cat.ops[pc.e]) THEN [pc EXCEPT !.ph = "pl"] ELSE pc.ret
    /\ UNCHANGED <<fs, cat, w, ghost>>
(* the links first, each an operation of its own; the purge goes on whatever their removal ended in *)
PgLinks ==
    /\ pc.ph = "pl"
    /\ IF pc.links = {}
       THEN pc' = [ph |-> "pj", e |-> pc.e, ret |-> pc.ret] /\ UNCHANGED cat
       ELSE /\ FreeOps # {}
            /\ NewOp(Min(FreeOps), [NoOp EXCEPT !.k = "ldrop", !.st = "planned", !.d = cat.ops[pc.e].d, !.f = Min(pc.links),
                                                !.key = cat.ops[pc.e].key])
            /\ pc' = [ph |-> "la", p |-> Min(FreeOps), ret |-> [pc EXCEPT !.links = @ \ {Min(pc.links)}]]
    /\ UNCHANGED <<fs, w, ghost>>
PgJournal ==
    /\ pc.ph = "pj" /\ FreeOps # {}
    /\ LET op == cat.ops[pc.e]
       IN NewOp(Min(FreeOps), [NoOp EXCEPT !.k = "unlink", !.st = "planned", !.d = op.d, !.f = op.f, !.key = op.key, !.e = pc.e])
    /\ pc' = [ph |-> "pu", p |-> Min(FreeOps), ret |-> pc.ret]
    /\ UNCHANGED <<fs, w, ghost>>
PgUnlink ==
    /\ pc.ph = "pu"
    /\ LET q == pc.p
           op == cat.ops[q]
           T == fs.tr[op.e]
           F == [fs EXCEPT !.tr[op.e] = 0]
           older == pc.ret.ph = "pg" /\ pc.ret.mode = "older"
       IN /\ Writable(op.d)
          /\ IF T = 0 THEN pc' = [pc EXCEPT !.ph = "pc"] /\ UNCHANGED <<fs, cat, ghost>>
             ELSE IF fs.obj[T].ino # op.key
             THEN \* another file there stays, with the entry: changed
                  /\ cat' = [cat EXCEPT !.ops[q] = Undone(q)] /\ pc' = pc.ret /\ UNCHANGED <<fs, ghost>>
             ELSE /\ IF older
                     THEN \* --older-than: the user chose what goes
                          /\ fs' = Canon(F)
                          /\ ghost' = [ghost EXCEPT !.udest = @ \cup LostBy(fs, F)]
                     ELSE IF cat.ops[op.e].exp <= w.now THEN ExpiredFs(F, q)
                     ELSE AtollFs(F, q)
                  \* the operation keeps, for PurgedStaysPurged, the data it took from the trash
                  /\ cat' = [cat EXCEPT !.ops[q].o = T]
                  /\ pc' = [pc EXCEPT !.ph = "pc"]
    /\ UNCHANGED w
PgCommit ==
    /\ pc.ph = "pc"
    /\ cat' = [cat EXCEPT !.ops[pc.p] = Done(pc.p), !.ent[cat.ops[pc.p].e] = FALSE]
    /\ pc' = pc.ret
    /\ UNCHANGED <<fs, w, ghost>>

(* A step whose disk is gone ends the command there: what it journaled stays planned, for the reprise. *)
OpDisk == IF pc.ph \in {"tm", "la", "iu", "rm", "pu"} THEN cat.ops[pc.p].d ELSE 0
Abandon ==
    /\ OpDisk # 0 /\ ~Writable(OpDisk)
    /\ pc' = Idle
    /\ UNCHANGED <<fs, cat, w, ghost>>
(* The bound of the model, not of Atoll: a journal of the instance full, a command that would journal ends there, as
   a stop ends it. *)
OutOfOps ==
    /\ FreeOps = {} /\ pc.ph \in {"pj", "pl", "job", "lg", "pr", "rs"}
    /\ pc' = Idle
    /\ UNCHANGED <<fs, cat, w, ghost>>

Atoll ==
    \/ "pass" \in AtollActs /\ Start("pass", 0)
    \/ "prune" \in AtollActs /\ Start("prune", 0)
    \/ "balance" \in AtollActs /\ Start("balance", 0)
    \/ "restore" \in AtollActs /\ \E p \in OpIds : InTrash(p) /\ Newest(p) /\ Start("restore", p)
    \/ \E m \in {"purge", "room", "older"} : m \in AtollActs /\ Start(m, 0)
    \/ RepStep \/ RepEnd \/ Decide \/ DecEnd \/ JobRun \/ JobEnd \/ TMove \/ TCommit
    \/ LGStart \/ LGStep \/ LAct \/ LCommit
    \/ PrLook \/ PrEnd \/ AtOnceLook \/ InPlaceLook \/ InPlaceUnlink
    \/ RsLook \/ RsMove \/ RsCommit
    \/ PgPick \/ PgLook \/ PgLinks \/ PgJournal \/ PgUnlink \/ PgCommit \/ Abandon \/ OutOfOps

Next == Atoll \/ World

Spec == Init /\ [][Next]_vars

(* Atoll goes on, each of its steps fair; a disk comes back. *)
LiveSpec == Spec /\ WF_vars(Atoll) /\ \A d \in Disks : WF_vars(UBack(d))

-----------------------------------------------------------------------------
TypeOK ==
    /\ fs.ds \in [Disks -> {"on", "miss", "ro"}]
    /\ \A p \in OpIds : cat.ops[p].st \in {"", "planned", "done", "undone"}
    /\ ghost.flags \subseteq {"lost", "restore-other", "unjournaled"}

(* §17, invariants 1 and 10: Atoll never takes the last of a version it must keep: the one a user has at a path; one
   a user deleted while the pool held it elsewhere, for the retention; an entry deleted, within its retention.
   Expired entries, and what goes for room, surplus and overwritten, are the trash's to purge (§17), and what
   --older-than purges is the user's choice. *)
NoKeptLost == "lost" \notin ghost.flags

(* §17.4: a restore that finishes puts back the very file its entry holds, as a user left it there, or nothing. *)
RestoreExact == "restore-other" \notin ghost.flags

(* §17.2: never a purge of an entry while a restore of it, or another purge of it, is planned. *)
OneAtATime ==
    \A p, q \in OpIds : (p # q /\ cat.ops[p].st = "planned" /\ cat.ops[q].st = "planned"
                         /\ cat.ops[p].k \in {"unlink", "restore"} /\ cat.ops[q].k \in {"unlink", "restore"})
                        => cat.ops[p].e # cat.ops[q].e

(* Invariant 12: every name Atoll takes away, from the root, a trash or a links folder, is taken by a step of an
   operation planned in the journal. *)
JournaledFirst == "unjournaled" \notin ghost.flags

(* §17, A purge: what a purge took from the trash keeps no link of the guard, which would keep its data once purged
   and send it back to the trash at the next pass; but a user who took it back to a path has it there. *)
PurgedStaysPurged ==
    \A q \in OpIds : (cat.ops[q].k = "unlink" /\ cat.ops[q].st = "done" /\ cat.ops[q].o # 0)
                     => \/ ~\E x \in Disks \X Files : fs.lk[x[1]][x[2]] = cat.ops[q].o
                        \/ \E x \in Disks \X Files : fs.root[x[1]][x[2]] = cat.ops[q].o

(* Liveness: a purge started finishes or is undone, as does a removal at its path and a link's removal. *)
PurgeSettles == <>[](\A p \in OpIds : cat.ops[p].k \in {"unlink", "ldrop", "rmroot"} => cat.ops[p].st # "planned")
=============================================================================
