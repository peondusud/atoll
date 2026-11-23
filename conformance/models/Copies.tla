------------------------------- MODULE Copies -------------------------------
(* Copies at once (specs.md §14, "Copies at once, by the kind of each disk", §13.4, §14.1, §14.2), a model of its own
   beside Replication.tla, which runs one copy at a time as the conformance build does (§29.4): several copies of
   different files to one disk at once, each step of each interleaved with the others', with Atoll killed, the power
   cut and a disk lost between any two of them, a user writing a file of their own to a disk, and the parts Atoll's own
   view writes in parallel to a disk at the same time (§21.2), so that the room of a disk is shared by writers that do
   not wait for each other.

   What it keeps: files of the library, each of a size, with their copies by object and the bytes each holds, as the
   kernel shows them and as a power cut leaves them; each disk's capacity and min_free, and the bytes a user's own files
   and the view's parts take there; the catalog's copies (ok, pending), the replicate jobs (queued, running, the part
   each owns), the journal ops (create), each disk's state from the last mount check. Atoll runs passes: the mount check
   and the reprise of each disk's planned operations (§14.2, Reprise), the tidy of tmp/, the plan (§13.4: pending copies
   kept in the order of the files while they fit past min_free, each holding its room, one whose job is running holding
   none, the others canceled; then the copies a file below its level gets, on the disk with the most room past min_free
   and the pending copies kept), then the run (§14, internal/jobs/parallel.go): a queued job starts as soon as the disk
   it reads and the disk it writes each run fewer copies than their limit, fewer than Total run in all, and no copy of
   its file runs (the per-path lock); then each copy under way takes its steps, journaled, its part made, copied (ENOSPC,
   or a write that would take its disk past min_free, is no-room: the part dropped, undone, queued again), synced,
   verified, renamed without replacing anything, synced, committed. A disk one copy cannot rely on is lost at once for every copy that has not started, and each copy under
   way on it stops at its next step: planned for the reprise on its target, undone with its part kept on its source.

   Since 2026-11-23, with MaxEdit, one file copied to several targets at once, with refreshes (§14, §14.1, §15.4): a
   user writes into a copy in place, a new content of the same size, at any moment; a pass then decides the
   modification from what its look finds (the changed copy current, its hash read, every other copy stale with a trash
   job, every queued job of the file gone with its pending copy; several contents a conflict, never queued), sends each
   stale copy to the trash as §17.1 does, step by step, and its row becomes a pending copy whose replicate job reads the
   copy that changed: several copies of one file to several disks, which the per-path lock runs one at a time. A copy
   starts only from a source that is what its row says, looks at it again once it has read it, and is dropped as
   changed when its part is not the file's hash. With MaxEdit = 0 none of it moves, and every earlier instance explores
   the very states it did.

   What it leaves out: what a walk finds beyond a copy changed in place (no deletion, replacement or rename), the debounce
   and settle (a changed source waits for the next pass), folders and the lock of a disk's names (each step that makes or
   removes a name is one step here, which the lock makes it in the code), more than one part a file, the view's parts
   beyond the room they take (their adoption and their journal are Replication.tla's), a chunk at a time (a copy writes
   its bytes in one step, which the real one does a chunk at a time between the same checks), the purge of the trash,
   and time. *)
EXTENDS Naturals, FiniteSets, Sequences, TLC

CONSTANTS
    NDisks,     \* the pool's disks, by position
    NFiles,     \* the files of the library
    Size,       \* [Files -> Nat]: each file's size
    Cap,        \* [Disks -> Nat]: each disk's capacity
    MinFree,    \* [Disks -> Nat]: min_free of each disk
    Limit,      \* [Disks -> Nat]: copies at once on each disk, read or written (§14): 1 on a spindle, more on flash
    Total,      \* copies at once in all
    Level,      \* the copies each file wants
    Start,      \* [Files -> SUBSET Disks]: where each file has a copy, a user's, verified, at the start
    MaxUser,    \* how many times a user writes a file of their own, of FillBy bytes, to a disk
    FillBy,     \* the bytes a user's file takes
    MaxView,    \* how many times the view writes a part in parallel, of FillBy bytes, to a disk
    MaxCrash,   \* how many times Atoll is killed
    MaxCut,     \* how many power cuts
    MaxDrop,    \* how many times a disk is lost
    MaxOps,     \* how many operations Atoll may journal
    MaxEdit,    \* how many times a user writes into a copy in place, a new content of the same size; 0 for none
    Fixes       \* the remedies proposed for the flaws it found and still open, by name (README.md); {} is specs.md as it is

Disks == 1..NDisks
Files == 1..NFiles
OpIds == 1..MaxOps
M(f) == f                 \* a file's logical path
T(p) == NFiles + p        \* the part <op-id>.part in <workdir>/tmp/
X(p) == NFiles + MaxOps + p   \* the entry <date>/<op-id>/... in <workdir>/trash/
Names == 1..(NFiles + 2 * MaxOps)
(* The contents of file f: f * 10 at the start, then f * 10 plus the count of edits so far. *)
FileOf(v) == v \div 10

ASSUME /\ NDisks \in Nat \ {0} /\ NFiles \in Nat \ {0} /\ Level \in 1..3 /\ Total \in Nat \ {0}
       /\ Size \in [Files -> Nat \ {0}] /\ Cap \in [Disks -> Nat] /\ MinFree \in [Disks -> Nat]
       /\ Limit \in [Disks -> Nat \ {0}] /\ Start \in [Files -> SUBSET Disks]
       /\ Fixes = {}    \* no flaw open: flaw 12's remedies are specs.md's and the code's, and the model's

Min(S) == CHOOSE x \in S : \A y \in S : x <= y
RECURSIVE SumFiles(_)
SumFiles(S) == IF S = {} THEN 0 ELSE LET x == CHOOSE x \in S : TRUE IN Size[x] + SumFiles(S \ {x})

(* A copy's row: ok, pending or stale, its inode, verified, and the content it describes (rver). *)
NoRow == [st |-> "none", obj |-> 0, verified |-> FALSE, rver |-> 0]
(* A replicate job; wait says that its source was found changed in this run, as settle has it wait (§14.1). *)
NoJob == [on |-> FALSE, src |-> 0, part |-> 0, running |-> FALSE, wait |-> FALSE]
NoTJob == [on |-> FALSE, src |-> 0]
NoOp == [k |-> "none", d |-> 0, f |-> 0, key |-> 0, st |-> "none", part |-> 0]
(* A copy under way: its step, its operation, its part, and the content of its source when it began. *)
NoRun == [s |-> "none", p |-> 0, o |-> 0, sv |-> 0]
Idle == [ph |-> "idle"]
NoSess == [d \in Disks |-> 0]

VARIABLES
    objs,    \* the files on the disks: [d, ver, size (bytes now), dsize (bytes a power cut leaves), atoll]
    vol,     \* [Disks -> [Names -> object or 0]]: the names as the kernel shows them
    dur,     \* the same, as a power cut leaves them
    dstat,   \* [Disks -> {"up", "gone"}]
    mgen,    \* the mount id of each disk
    ufill,   \* [Disks -> Nat]: the bytes of a user's own files on each disk, written durably
    vfill,   \* [Disks -> Nat]: the bytes the view's parts in parallel take on each disk
    crow,    \* the catalog's copies: [Files -> [Disks -> row]]
    rjob,    \* the replicate jobs: [Files -> [Disks -> job]], by target
    tjob,    \* the trash jobs of the stale copies: [Files -> [Disks -> [on, src]]] (reason overwritten)
    fver,    \* the catalog's files: [Files -> [cur (the current content), hash (its hash, 0 for none), conflict]]
    ops,     \* the journal
    nextOp,
    cdisk,   \* each disk's state from the last mount check
    pc,      \* where Atoll's pass is
    sess,    \* the mount each disk had when this session confirmed it, plus 1; 0 for none or lost
    run,     \* [Files -> [Disks -> copy under way]]: lost with the process
    cnt,     \* how many of each event of the world so far
    ghost    \* for the invariants: flags, the disks a user's own file went to, the copies a user wrote into since
             \* their row, and the last content a user wrote to each file

vars == <<objs, vol, dur, dstat, mgen, ufill, vfill, crow, rjob, tjob, fver, ops, nextOp, cdisk, pc, sess, run, cnt, ghost>>
disk == <<objs, vol, dur, dstat, mgen, ufill, vfill>>
cat == <<crow, rjob, tjob, fver, ops, nextOp, cdisk>>

-----------------------------------------------------------------------------
(* Helpers *)

Up(d) == dstat[d] = "up"
InSess(d) == d \in Disks /\ sess[d] # 0
Ok(d) == Up(d) /\ sess[d] = mgen[d] + 1
Lose(d) == /\ sess' = [sess EXCEPT ![d] = 0]
           /\ cdisk' = [cdisk EXCEPT ![d] = "missing"]
Flag(g, name, cond) == IF cond THEN [g EXCEPT !.flags = @ \cup {name}] ELSE g
Seen(d, n, key) == IF vol[d][n] = 0 THEN "absent" ELSE IF key # 0 /\ vol[d][n] = key THEN "same" ELSE "other"
NextIn(d, S) == LET c == {e \in S : e > d} IN IF c = {} THEN NDisks + 1 ELSE Min(c)
(* The bytes a disk's files take, as the kernel shows them: a user's own, the view's parts, and each object a name
   holds, once whatever names it has. *)
On(d) == {vol[d][n] : n \in Names} \ {0}
RECURSIVE SumObjs(_)
SumObjs(S) == IF S = {} THEN 0 ELSE LET x == CHOOSE x \in S : TRUE IN objs[x].size + SumObjs(S \ {x})
Used(d) == ufill[d] + vfill[d] + SumObjs(On(d))
FreeNow(d) == IF Cap[d] >= Used(d) THEN Cap[d] - Used(d) ELSE 0
(* The copies under way, and how many read or write each disk. *)
Running == {<<f, d>> \in Files \X Disks : run[f][d].s # "none"}
Busy(e) == Cardinality({x \in Running : x[2] = e \/ rjob[x[1]][x[2]].src = e})
PlannedOn(d) == {p \in OpIds : ops[p].st = "planned" /\ ops[p].d = d}

-----------------------------------------------------------------------------
Init ==
    /\ objs = [o \in 1..(NFiles * NDisks) |-> [d |-> ((o - 1) % NDisks) + 1, ver |-> ((o - 1) \div NDisks + 1) * 10,
                                                 size |-> Size[(o - 1) \div NDisks + 1], dsize |-> Size[(o - 1) \div NDisks + 1],
                                                 atoll |-> FALSE]]
    /\ vol = [d \in Disks |-> [n \in Names |-> IF n \in Files /\ d \in Start[n] THEN (n - 1) * NDisks + d ELSE 0]]
    /\ dur = vol
    /\ dstat = [d \in Disks |-> "up"]
    /\ mgen = [d \in Disks |-> 0]
    /\ ufill = [d \in Disks |-> 0]
    /\ vfill = [d \in Disks |-> 0]
    /\ crow = [f \in Files |-> [d \in Disks |-> IF d \in Start[f] THEN [st |-> "ok", obj |-> (f - 1) * NDisks + d, verified |-> TRUE,
                                                                       rver |-> f * 10]
                                                ELSE NoRow]]
    /\ rjob = [f \in Files |-> [d \in Disks |-> NoJob]]
    /\ tjob = [f \in Files |-> [d \in Disks |-> NoTJob]]
    /\ fver = [f \in Files |-> [cur |-> f * 10, hash |-> f * 10, conflict |-> FALSE]]
    /\ ops = [p \in OpIds |-> NoOp]
    /\ nextOp = 1
    /\ cdisk = [d \in Disks |-> "online"]
    /\ pc = Idle
    /\ sess = NoSess
    /\ run = [f \in Files |-> [d \in Disks |-> NoRun]]
    /\ cnt = [user |-> 0, view |-> 0, crash |-> 0, cut |-> 0, drop |-> 0, edit |-> 0]
    /\ ghost = [flags |-> {}, filled |-> {}, edited |-> {}, latest |-> [f \in Files |-> f * 10]]

-----------------------------------------------------------------------------
(* The world. *)

(* A user writes a file of their own to a disk, durably, with what came before it there: ENOSPC once the disk is
   full, as for anyone. It takes room past min_free as it likes: min_free is Atoll's reserve, never a user's limit. *)
UserFill(d) ==
    /\ cnt.user < MaxUser /\ Up(d) /\ FreeNow(d) >= FillBy
    /\ ufill' = [ufill EXCEPT ![d] = @ + FillBy]
    /\ dur' = [dur EXCEPT ![d] = vol[d]]
    /\ objs' = [o \in DOMAIN objs |-> IF objs[o].d = d THEN [objs[o] EXCEPT !.dsize = objs[o].size] ELSE objs[o]]
    /\ cnt' = [cnt EXCEPT !.user = @ + 1]
    /\ ghost' = [ghost EXCEPT !.filled = @ \cup {d}]
    /\ UNCHANGED <<vol, dstat, mgen, vfill, cat, pc, sess, run>>

(* §21.2, Copies written in parallel: the view writes a part of a new file to a disk, as the program writes it, beside
   the daemon's copies, with the room it measured past min_free as its filesystem says it, less what the catalog's
   pending copies still have to write there (writer.openPart, writer.pendingLeft, parallel.mirror; README.md's flaw
   12, the remedy view-room). *)
(* What the pending copies of a disk still have to write there: each its size, less what its part holds. *)
PartBytes(f, d) == LET n == vol[d][T(rjob[f][d].part)] IN IF rjob[f][d].part # 0 /\ n # 0 THEN objs[n].size ELSE 0
RECURSIVE SumLeft(_, _)
SumLeft(S, d) == IF S = {} THEN 0 ELSE LET f == CHOOSE x \in S : TRUE IN (Size[f] - PartBytes(f, d)) + SumLeft(S \ {f}, d)
PendingRoom(d) == SumLeft({f \in Files : crow[f][d].st = "pending" /\ rjob[f][d].on}, d)
ViewWrite(d) ==
    /\ cnt.view < MaxView /\ Up(d)
    /\ FreeNow(d) >= FillBy + MinFree[d] + PendingRoom(d)
    /\ vfill' = [vfill EXCEPT ![d] = @ + FillBy]
    /\ cnt' = [cnt EXCEPT !.view = @ + 1]
    /\ UNCHANGED <<objs, vol, dur, dstat, mgen, ufill, cat, pc, sess, run, ghost>>

Drop(d) ==
    /\ cnt.drop < MaxDrop /\ Up(d)
    /\ dstat' = [dstat EXCEPT ![d] = "gone"]
    /\ cnt' = [cnt EXCEPT !.drop = @ + 1]
    /\ UNCHANGED <<objs, vol, dur, mgen, ufill, vfill, cat, pc, sess, run, ghost>>

Return(d) ==
    /\ dstat[d] = "gone"
    /\ dstat' = [dstat EXCEPT ![d] = "up"]
    /\ mgen' = [mgen EXCEPT ![d] = @ + 1]
    /\ vol' = [vol EXCEPT ![d] = dur[d]]
    /\ objs' = [o \in DOMAIN objs |-> IF objs[o].d = d THEN [objs[o] EXCEPT !.size = objs[o].dsize] ELSE objs[o]]
    /\ UNCHANGED <<dur, ufill, vfill, cat, pc, sess, run, cnt, ghost>>

(* kill -9: the copies under way stop where they are; the catalog keeps what it committed. *)
Crash ==
    /\ cnt.crash < MaxCrash /\ pc # Idle
    /\ pc' = Idle /\ sess' = NoSess /\ run' = [f \in Files |-> [d \in Disks |-> NoRun]]
    /\ cnt' = [cnt EXCEPT !.crash = @ + 1]
    /\ UNCHANGED <<disk, cat, ghost>>

(* §20: every disk keeps what was durable. *)
PowerCut ==
    /\ cnt.cut < MaxCut
    /\ vol' = dur
    /\ objs' = [o \in DOMAIN objs |-> [objs[o] EXCEPT !.size = objs[o].dsize]]
    /\ pc' = Idle /\ sess' = NoSess /\ run' = [f \in Files |-> [d \in Disks |-> NoRun]]
    /\ cnt' = [cnt EXCEPT !.cut = @ + 1]
    /\ UNCHANGED <<dur, dstat, mgen, ufill, vfill, cat, ghost>>

(* §15.4: a user writes into a copy in place, durably: a new content, the same inode and size; its change time moves,
   which a copy reading it, or a look comparing it with its row, finds. *)
UserEdit(f, d) ==
    /\ cnt.edit < MaxEdit /\ Up(d) /\ vol[d][M(f)] # 0
    /\ LET o == vol[d][M(f)]
           v == f * 10 + cnt.edit + 1
       IN /\ objs' = [e \in DOMAIN objs |-> IF e = o THEN [objs[e] EXCEPT !.ver = v, !.dsize = objs[e].size]
                                            ELSE IF objs[e].d = d THEN [objs[e] EXCEPT !.dsize = objs[e].size] ELSE objs[e]]
          /\ dur' = [dur EXCEPT ![d] = vol[d]]
          /\ ghost' = [ghost EXCEPT !.edited = @ \cup {o}, !.latest[f] = v]
    /\ cnt' = [cnt EXCEPT !.edit = @ + 1]
    /\ UNCHANGED <<vol, dstat, mgen, ufill, vfill, cat, pc, sess, run>>

World ==
    \/ \E d \in Disks : UserFill(d) \/ ViewWrite(d) \/ Drop(d) \/ Return(d)
    \/ \E f \in Files, d \in Disks : UserEdit(f, d)
    \/ Crash \/ PowerCut

-----------------------------------------------------------------------------
(* §15.12 steps 1 and 2, §14.2 Reprise: the mount check, then each disk's planned operations in the order of their
   ids, the names then the commit; then tmp/ loses the parts no job owns that an operation done or undone names. *)

PassStart ==
    /\ pc = Idle
    /\ cdisk' = [d \in Disks |-> IF Up(d) THEN "online" ELSE "missing"]
    /\ sess' = [d \in Disks |-> IF Up(d) THEN mgen[d] + 1 ELSE 0]
    /\ pc' = [ph |-> "reprise", d |-> NextIn(0, {d \in Disks : Up(d)})]
    /\ UNCHANGED <<disk, crow, rjob, tjob, fver, ops, nextOp, run, cnt, ghost>>

(* The table of §14.2: target same, it finishes, the part's name going if it is still there; otherwise undone, the
   part kept for its job when it is there. *)
(* A move to the trash of a stale copy (§17.2): the entry holding the copy finishes, the source's name going when it
   holds it too; otherwise undone. *)
RepOp ==
    /\ pc.ph = "reprise" /\ pc.d <= NDisks /\ Ok(pc.d) /\ PlannedOn(pc.d) # {}
    /\ LET d == pc.d
           p == Min(PlannedOn(d))
           f == ops[p].f
           key == ops[p].key
           q == ops[p].part
           trash == ops[p].k = "trash"
           tn == IF trash THEN X(p) ELSE M(f)
           sn == IF trash THEN M(f) ELSE T(q)
           tg == Seen(d, tn, key)
           pt == Seen(d, sn, key)
           v1 == IF tg = "same" /\ pt = "same" THEN [vol[d] EXCEPT ![sn] = 0] ELSE vol[d]
       IN /\ vol' = [vol EXCEPT ![d] = v1]
          /\ dur' = [dur EXCEPT ![d][tn] = v1[tn], ![d][sn] = v1[sn]]
          /\ objs' = [o \in DOMAIN objs |-> IF o \in {v1[tn], v1[sn]} THEN [objs[o] EXCEPT !.dsize = objs[o].size] ELSE objs[o]]
          /\ pc' = [ph |-> "repcommit", d |-> d, p |-> p, out |-> IF tg = "same" THEN "done" ELSE "undone", keep |-> pt = "same"]
    /\ UNCHANGED <<dstat, mgen, ufill, vfill, cat, sess, run, cnt, ghost>>

(* A stale copy's move finished: its row becomes a pending copy whose replicate job reads the copy that changed, when its
   trash job stands; without it, the row goes, and the next plan places the copy (§15.12, Trash jobs, Finishing). *)
TrashDone(f, d) ==
    /\ crow' = [crow EXCEPT ![f][d] = IF tjob[f][d].on THEN [NoRow EXCEPT !.st = "pending"] ELSE NoRow]
    /\ rjob' = [rjob EXCEPT ![f][d] = IF tjob[f][d].on THEN [NoJob EXCEPT !.on = TRUE, !.src = tjob[f][d].src] ELSE NoJob]
    /\ tjob' = [tjob EXCEPT ![f][d] = NoTJob]

RepCommit ==
    /\ pc.ph = "repcommit"
    /\ LET p == pc.p
           f == ops[p].f
           d == ops[p].d
           owned == rjob[f][d].on /\ rjob[f][d].part = ops[p].part
       IN /\ ops' = [ops EXCEPT ![p].st = pc.out]
          /\ IF ops[p].k = "trash"
             THEN /\ IF pc.out = "done" THEN TrashDone(f, d) ELSE UNCHANGED <<crow, rjob, tjob>>
                  /\ UNCHANGED ghost
             ELSE IF pc.out = "done"
             THEN /\ crow' = [crow EXCEPT ![f][d] = [st |-> "ok", obj |-> ops[p].key, verified |-> owned, rver |-> objs[ops[p].key].ver]]
                  /\ rjob' = [rjob EXCEPT ![f][d] = NoJob]
                  /\ ghost' = Flag(ghost, "inv7", dur[d][M(f)] # ops[p].key)
                  /\ UNCHANGED tjob
             ELSE /\ rjob' = IF owned THEN [rjob EXCEPT ![f][d] = [@ EXCEPT !.running = FALSE, !.part = IF pc.keep THEN @ ELSE 0]]
                             ELSE rjob
                  /\ UNCHANGED <<crow, tjob, ghost>>
    /\ pc' = [ph |-> "reprise", d |-> pc.d]
    /\ UNCHANGED <<disk, fver, nextOp, cdisk, sess, run, cnt>>

Owned(d, q) == \E f \in Files : rjob[f][d].on /\ rjob[f][d].part = q
Droppable(d, q) ==
    /\ vol[d][T(q)] # 0 /\ ~Owned(d, q)
    /\ \E p \in OpIds : ops[p].d = d /\ ops[p].part = q /\ ops[p].st \in {"done", "undone"}
    /\ ~\E p \in OpIds : ops[p].d = d /\ ops[p].part = q /\ ops[p].st = "planned"

RepNext ==
    /\ pc.ph = "reprise" /\ pc.d <= NDisks /\ (~Ok(pc.d) \/ PlannedOn(pc.d) = {})
    \* the parts removed, then tmp/ synced (ops.CleanTmp)
    /\ IF Ok(pc.d)
       THEN LET gone(n) == \E q \in OpIds : n = T(q) /\ Droppable(pc.d, q) IN
            /\ vol' = [vol EXCEPT ![pc.d] = [n \in Names |-> IF gone(n) THEN 0 ELSE vol[pc.d][n]]]
            /\ dur' = [dur EXCEPT ![pc.d] = [n \in Names |-> IF gone(n) THEN 0 ELSE dur[pc.d][n]]]
            /\ UNCHANGED <<sess, cdisk>>
       ELSE /\ Lose(pc.d) /\ UNCHANGED <<vol, dur>>
    /\ pc' = [ph |-> "reprise", d |-> NextIn(pc.d, {e \in Disks : InSess(e)})]
    /\ UNCHANGED <<objs, dstat, mgen, ufill, vfill, crow, rjob, tjob, fver, ops, nextOp, run, cnt, ghost>>

RepEnd ==
    /\ pc.ph = "reprise" /\ pc.d > NDisks
    /\ pc' = IF MaxEdit = 0 THEN [ph |-> "plan"] ELSE [ph |-> "modify"]
    /\ UNCHANGED <<disk, cat, sess, run, cnt, ghost>>

-----------------------------------------------------------------------------
(* §15.4 In a pass, with MaxEdit: the walks of the session's disks and the modifications, in one step. A copy is
   changed when its name holds its inode with another content than its row describes. A file with a job running, or in
   conflict, waits. One content among its changed copies: modified, read (the same size), the changed copies current
   and verified, every other copy stale, unverified, with a trash job from the first changed copy by position, and
   every queued job of the file gone with its pending copy. Several contents, or a stale copy changed: conflict. *)
Modify ==
    /\ pc.ph = "modify"
    /\ LET changed(f) == {d \in Disks : InSess(d) /\ Ok(d) /\ crow[f][d].st \in {"ok", "stale"} /\ crow[f][d].obj # 0
                                        /\ vol[d][M(f)] = crow[f][d].obj /\ objs[crow[f][d].obj].ver # crow[f][d].rver}
           vs(f) == {objs[crow[f][d].obj].ver : d \in changed(f)}
           waits(f) == fver[f].conflict \/ \E d \in Disks : rjob[f][d].running
           mod(f) == changed(f) # {} /\ ~waits(f) /\ Cardinality(vs(f)) = 1 /\ \A d \in changed(f) : crow[f][d].st = "ok"
           cfl(f) == changed(f) # {} /\ ~waits(f) /\ ~mod(f)
           v(f) == CHOOSE x \in vs(f) : TRUE
           src(f) == Min(changed(f))
           row(f, d) == LET r == crow[f][d] IN
                        IF ~mod(f) THEN IF cfl(f) /\ d \in changed(f) THEN [r EXCEPT !.verified = FALSE] ELSE r
                        ELSE IF d \in changed(f) THEN [r EXCEPT !.rver = v(f), !.verified = TRUE]
                        ELSE IF r.st = "ok" THEN [r EXCEPT !.st = "stale", !.verified = FALSE]
                        ELSE IF r.st = "pending" /\ ~rjob[f][d].running THEN NoRow
                        ELSE r
       IN /\ crow' = [f \in Files |-> [d \in Disks |-> row(f, d)]]
          /\ rjob' = [f \in Files |-> [d \in Disks |->
                         IF mod(f) /\ crow[f][d].st = "pending" /\ ~rjob[f][d].running THEN NoJob ELSE rjob[f][d]]]
          /\ tjob' = [f \in Files |-> [d \in Disks |->
                         IF mod(f) /\ crow[f][d].st = "ok" /\ d \notin changed(f) THEN [on |-> TRUE, src |-> src(f)] ELSE tjob[f][d]]]
          /\ fver' = [f \in Files |-> IF mod(f) THEN [fver[f] EXCEPT !.cur = v(f), !.hash = v(f)]
                                      ELSE IF cfl(f) THEN [fver[f] EXCEPT !.conflict = TRUE] ELSE fver[f]]
          /\ ghost' = [ghost EXCEPT !.edited = @ \ UNION {{crow[f][d].obj : d \in changed(f)} : f \in {g \in Files : mod(g)}}]
    /\ pc' = [ph |-> "trash"]
    /\ UNCHANGED <<disk, ops, nextOp, cdisk, sess, run, cnt>>

(* §15.12 step 5, Trash jobs, §17.1: each stale copy's trash job, one operation: journaled; the copy compared with its
   row right before the move, which is one step with it, changed leaving it and the job going; moved; the entry and
   the source's name synced; then the commit, its row a pending copy with its refresh job. *)
TrashPick ==
    /\ pc.ph = "trash"
    /\ LET due == {x \in Files \X Disks : tjob[x[1]][x[2]].on /\ crow[x[1]][x[2]].st = "stale" /\ InSess(x[2])}
           x == CHOOSE x \in due : \A y \in due : x[1] < y[1] \/ (x[1] = y[1] /\ x[2] <= y[2])
       IN IF due = {} \/ nextOp > MaxOps
          THEN /\ pc' = [ph |-> "plan"] /\ UNCHANGED <<ops, nextOp, sess, cdisk>>
          ELSE IF ~Ok(x[2])
          THEN /\ Lose(x[2]) /\ UNCHANGED <<pc, ops, nextOp>>
          ELSE /\ ops' = [ops EXCEPT ![nextOp] = [k |-> "trash", d |-> x[2], f |-> x[1], key |-> crow[x[1]][x[2]].obj,
                                                  st |-> "planned", part |-> 0]]
               /\ nextOp' = nextOp + 1
               /\ pc' = [ph |-> "tstep", f |-> x[1], d |-> x[2], p |-> nextOp, s |-> "journaled"]
               /\ UNCHANGED <<sess, cdisk>>
    /\ UNCHANGED <<disk, crow, rjob, tjob, fver, run, cnt, ghost>>

TrashMove ==
    /\ pc.ph = "tstep" /\ pc.s = "journaled" /\ Ok(pc.d)
    /\ LET key == ops[pc.p].key
           r == crow[pc.f][pc.d]
       IN IF vol[pc.d][M(pc.f)] = key /\ key # 0 /\ objs[key].ver = r.rver
          THEN /\ vol' = [vol EXCEPT ![pc.d][M(pc.f)] = 0, ![pc.d][X(pc.p)] = key]
               /\ pc' = [pc EXCEPT !.s = "moved"]
               /\ UNCHANGED <<ops, tjob>>
          ELSE /\ ops' = [ops EXCEPT ![pc.p].st = "undone"]
               /\ tjob' = [tjob EXCEPT ![pc.f][pc.d] = NoTJob]
               /\ pc' = [ph |-> "trash"]
               /\ UNCHANGED vol
    /\ UNCHANGED <<objs, dur, dstat, mgen, ufill, vfill, crow, rjob, fver, nextOp, cdisk, sess, run, cnt, ghost>>

TrashSync ==
    /\ pc.ph = "tstep" /\ pc.s = "moved" /\ Ok(pc.d)
    /\ dur' = [dur EXCEPT ![pc.d][X(pc.p)] = vol[pc.d][X(pc.p)], ![pc.d][M(pc.f)] = vol[pc.d][M(pc.f)]]
    /\ pc' = [pc EXCEPT !.s = "synced"]
    /\ UNCHANGED <<objs, vol, dstat, mgen, ufill, vfill, cat, sess, run, cnt, ghost>>

TrashCommit ==
    /\ pc.ph = "tstep" /\ pc.s = "synced"
    /\ ops' = [ops EXCEPT ![pc.p].st = "done"]
    /\ TrashDone(pc.f, pc.d)
    /\ ghost' = Flag(ghost, "inv7", dur[pc.d][X(pc.p)] # ops[pc.p].key)
    /\ pc' = [ph |-> "trash"]
    /\ UNCHANGED <<disk, fver, nextOp, cdisk, sess, run, cnt>>

TrashLost ==
    /\ pc.ph = "tstep" /\ pc.s # "synced" /\ ~Ok(pc.d)
    /\ Lose(pc.d)
    /\ pc' = [ph |-> "trash"]
    /\ UNCHANGED <<disk, crow, rjob, tjob, fver, ops, nextOp, run, cnt, ghost>>

-----------------------------------------------------------------------------
(* §13.4 The placement plan, from each disk's free space as its filesystem says it now: the pending copies kept, file
   by file, while each fits past min_free and those kept before it, one whose job is running holding no room; those
   that do not fit canceled; then each file below its level gets its copies, one at a time, on the disk online with the
   most room past min_free and what the plan holds there, that holds no copy of it, then by position, its source the
   first copy ok on a disk online. *)

(* The copies of one file placed one at a time until its level, each on the candidate with the most room left. *)
RECURSIVE PlaceFile(_, _, _, _)
PlaceFile(f, room, rows, jobs) ==
    \* a stale copy counts, as it is refreshed in its place; a file in conflict is never queued (§14.1)
    LET count == Cardinality({d \in Disks : rows[f][d].st \in {"ok", "pending", "stale"}})
        sources == {d \in Disks : rows[f][d].st = "ok" /\ cdisk[d] = "online"}
        cands == {d \in Disks : rows[f][d].st = "none" /\ cdisk[d] = "online" /\ room[d] >= Size[f]}
        best == CHOOSE d \in cands : \A e \in cands : room[d] > room[e] \/ (room[d] = room[e] /\ d <= e)
    IN IF count < Level /\ sources # {} /\ cands # {} /\ ~fver[f].conflict
       THEN PlaceFile(f, [room EXCEPT ![best] = @ - Size[f]],
                      [rows EXCEPT ![f][best] = [NoRow EXCEPT !.st = "pending"]],
                      [jobs EXCEPT ![f][best] = [NoJob EXCEPT !.on = TRUE, !.src = Min(sources)]])
       ELSE [room |-> room, rows |-> rows, jobs |-> jobs]

RECURSIVE PlanFiles(_, _, _, _)
PlanFiles(i, room, rows, jobs) ==
    IF i > NFiles THEN [rows |-> rows, jobs |-> jobs]
    ELSE LET f == i
             keep(d) == rows[f][d].st = "pending" /\ jobs[f][d].running
             fits(d) == rows[f][d].st = "pending" /\ ~jobs[f][d].running /\ cdisk[d] = "online" /\ room[d] >= Size[f]
             kept == {d \in Disks : keep(d) \/ fits(d)}
             room1 == [d \in Disks |-> IF fits(d) THEN room[d] - Size[f] ELSE room[d]]
             rows1 == [rows EXCEPT ![f] = [d \in Disks |-> IF rows[f][d].st = "pending" /\ d \notin kept THEN NoRow ELSE rows[f][d]]]
             jobs1 == [jobs EXCEPT ![f] = [d \in Disks |-> IF rows[f][d].st = "pending" /\ d \notin kept THEN NoJob ELSE jobs[f][d]]]
             placed == PlaceFile(f, room1, rows1, jobs1)
         IN PlanFiles(i + 1, placed.room, placed.rows, placed.jobs)

PlanStep ==
    /\ pc.ph = "plan"
    /\ LET room0 == [d \in Disks |-> IF FreeNow(d) >= MinFree[d] THEN FreeNow(d) - MinFree[d] ELSE 0]
           \* the pending copies a plan made now keeps, made from rows with only those already pending
           out == PlanFiles(1, room0, crow, rjob)
       IN /\ crow' = out.rows
          \* a job that waited for its source's settle in the last run may start again in this one
          /\ rjob' = [f \in Files |-> [d \in Disks |-> [out.jobs[f][d] EXCEPT !.wait = FALSE]]]
    /\ pc' = [ph |-> "run"]
    /\ UNCHANGED <<disk, tjob, fver, ops, nextOp, cdisk, sess, run, cnt, ghost>>

-----------------------------------------------------------------------------
(* §14 The run: copies at once. *)

CanStart(f, d) ==
    LET j == rjob[f][d] IN
    /\ j.on /\ ~j.running /\ ~j.wait /\ run[f][d].s = "none" /\ ~fver[f].conflict
    /\ Cardinality(Running) < Total
    /\ Busy(d) < Limit[d] /\ Busy(j.src) < Limit[j.src]
    \* the per-path lock: no other copy of its file runs
    /\ ~\E e \in Disks : run[f][e].s # "none"
    /\ InSess(d) /\ InSess(j.src)

(* A job that ends with no copy made: its pending copy goes with it. *)
EndJob(f, d) ==
    /\ crow' = [crow EXCEPT ![f][d] = IF crow[f][d].st = "pending" THEN NoRow ELSE @]
    /\ rjob' = [rjob EXCEPT ![f][d] = NoJob]

(* The look at the target, then, for a copy that starts from nothing, the look at the source: no longer a source, its
   job goes with its pending copy, changed (§14.1); not what its row says, the job waits for its settle, nothing
   journaled (§14.2); then the operation journaled and the job running, owning its part. *)
CopyStart(f, d) ==
    /\ pc.ph = "run" /\ CanStart(f, d)
    /\ LET j == rjob[f][d]
           s == crow[f][j.src]
       IN IF ~Ok(d) \/ ~Ok(j.src) THEN /\ UNCHANGED <<crow, rjob, ops, nextOp, run>>
                                        /\ IF ~Ok(d) THEN Lose(d) ELSE Lose(j.src)
          ELSE IF vol[d][M(f)] # 0 THEN /\ EndJob(f, d) /\ UNCHANGED <<ops, nextOp, run, sess, cdisk>>  \* conflict
          ELSE IF s.st # "ok" THEN /\ EndJob(f, d) /\ UNCHANGED <<ops, nextOp, run, sess, cdisk>>      \* changed
          ELSE IF vol[j.src][M(f)] # s.obj \/ s.obj = 0 \/ objs[s.obj].ver # s.rver
          THEN /\ rjob' = [rjob EXCEPT ![f][d].wait = TRUE] /\ UNCHANGED <<crow, ops, nextOp, run, sess, cdisk>>
          ELSE IF nextOp > MaxOps THEN /\ EndJob(f, d) /\ UNCHANGED <<ops, nextOp, run, sess, cdisk>>   \* the model's bound
          ELSE LET q == IF j.part # 0 THEN j.part ELSE nextOp IN
               /\ ops' = [ops EXCEPT ![nextOp] = [k |-> "create", d |-> d, f |-> f, key |-> 0, st |-> "planned", part |-> q]]
               /\ nextOp' = nextOp + 1
               /\ rjob' = [rjob EXCEPT ![f][d].running = TRUE, ![f][d].part = q]
               /\ run' = [run EXCEPT ![f][d] = [s |-> "journaled", p |-> nextOp, o |-> 0, sv |-> objs[s.obj].ver]]
               /\ UNCHANGED <<crow, sess, cdisk>>
    /\ UNCHANGED <<disk, tjob, fver, pc, cnt, ghost>>

(* A copy whose target cannot be relied on stops: planned for the reprise. One whose source cannot be relied on while
   it reads it is undone, its part kept for its job to resume. *)
CopyHalt(f, d) ==
    /\ pc.ph = "run" /\ run[f][d].s # "none"
    /\ LET p == run[f][d].p
           src == rjob[f][d].src
           \* with MaxEdit, the source is looked at again once read, as it is in the code
           reading == run[f][d].s \in IF MaxEdit = 0 THEN {"journaled", "made"} ELSE {"journaled", "made", "copied"}
       IN IF ~Ok(d)
          THEN /\ Lose(d)
               /\ run' = [run EXCEPT ![f][d] = NoRun]
               /\ UNCHANGED <<crow, rjob, ops>>
          ELSE /\ reading /\ ~Ok(src)
               /\ Lose(src)
               /\ ops' = [ops EXCEPT ![p].st = "undone"]
               /\ rjob' = [rjob EXCEPT ![f][d].running = FALSE]
               /\ run' = [run EXCEPT ![f][d] = NoRun]
               /\ UNCHANGED crow
    /\ UNCHANGED <<disk, tjob, fver, nextOp, pc, cnt, ghost>>

(* One step of a copy under way, its disks there on the mounts the session confirmed. *)
CopyStep(f, d) ==
    /\ pc.ph = "run" /\ run[f][d].s # "none" /\ Ok(d)
    /\ LET r == run[f][d]
           p == r.p
           q == ops[p].part
           src == rjob[f][d].src
           s == crow[f][src]
           n == Len(objs) + 1
       IN CASE r.s = "journaled" ->
                 \* <op id>.part made with mode 0600, synced, its inode recorded; a part resumed is the one there
                 /\ Ok(src)
                 /\ IF vol[d][T(q)] # 0
                    THEN /\ run' = [run EXCEPT ![f][d] = [r EXCEPT !.s = "made", !.o = vol[d][T(q)]]]
                         /\ ops' = [ops EXCEPT ![p].key = vol[d][T(q)]]
                         /\ UNCHANGED <<objs, vol, dur>>
                    ELSE /\ objs' = Append(objs, [d |-> d, ver |-> 0, size |-> 0, dsize |-> 0, atoll |-> TRUE])
                         /\ vol' = [vol EXCEPT ![d][T(q)] = n]
                         /\ dur' = [dur EXCEPT ![d][T(q)] = n]
                         /\ run' = [run EXCEPT ![f][d] = [r EXCEPT !.s = "made", !.o = n]]
                         /\ ops' = [ops EXCEPT ![p].key = n]
                 /\ UNCHANGED <<crow, rjob, ghost>>
            [] r.s = "made" ->
                 \* the copy, read from the source: ENOSPC, or a write that would take the disk past min_free
                 \* (room.keeps; README.md's flaw 12, the remedy copy-room), is no-room: the part goes,
                 \* undone, the job queued again
                 /\ Ok(src)
                 /\ LET grow == Size[f] - objs[r.o].size
                    IN IF FreeNow(d) >= grow + MinFree[d]
                       THEN /\ objs' = [objs EXCEPT ![r.o].size = Size[f], ![r.o].ver = objs[s.obj].ver]
                            /\ run' = [run EXCEPT ![f][d].s = "copied"]
                            /\ UNCHANGED <<vol, dur, ops, rjob>>
                       ELSE /\ vol' = [vol EXCEPT ![d][T(q)] = 0]
                            /\ dur' = [dur EXCEPT ![d][T(q)] = 0]
                            /\ ops' = [ops EXCEPT ![p].st = "undone"]
                            /\ rjob' = [rjob EXCEPT ![f][d] = [@ EXCEPT !.running = FALSE, !.part = 0]]
                            /\ run' = [run EXCEPT ![f][d] = NoRun]
                            /\ UNCHANGED objs
                 /\ UNCHANGED <<crow, ghost>>
            [] r.s = "copied" ->
                 \* the source looked at again, the times restored, fsync; then the re-read without cache, which gives
                 \* the whole-file hash. With MaxEdit: a source whose content or change time moved since the copy
                 \* began, or a part that is not the file's hash, drops the copy as changed, its part gone, undone, the
                 \* job queued again to wait for its source's settle (§14.2)
                 IF MaxEdit > 0 /\ ~(Ok(src) /\ vol[src][M(f)] = s.obj /\ objs[s.obj].ver = r.sv /\ objs[r.o].ver = fver[f].hash)
                 THEN /\ Ok(src)
                      /\ vol' = [vol EXCEPT ![d][T(q)] = 0]
                      /\ dur' = [dur EXCEPT ![d][T(q)] = 0]
                      /\ ops' = [ops EXCEPT ![p].st = "undone"]
                      /\ rjob' = [rjob EXCEPT ![f][d] = [@ EXCEPT !.running = FALSE, !.part = 0, !.wait = TRUE]]
                      /\ run' = [run EXCEPT ![f][d] = NoRun]
                      /\ UNCHANGED <<objs, crow, ghost>>
                 ELSE /\ objs' = [objs EXCEPT ![r.o].dsize = objs[r.o].size]
                      /\ run' = [run EXCEPT ![f][d].s = "verified"]
                      /\ UNCHANGED <<vol, dur, crow, rjob, ops, ghost>>
            [] r.s = "verified" ->
                 \* the rename without replacing anything: a path taken since is conflict, nothing overwritten
                 IF vol[d][M(f)] # 0
                 THEN /\ ops' = [ops EXCEPT ![p].st = "undone"]
                      /\ EndJob(f, d)
                      /\ run' = [run EXCEPT ![f][d] = NoRun]
                      /\ UNCHANGED <<objs, vol, dur, ghost>>
                 ELSE /\ vol' = [vol EXCEPT ![d][M(f)] = r.o, ![d][T(q)] = 0]
                      /\ run' = [run EXCEPT ![f][d].s = "moved"]
                      /\ UNCHANGED <<objs, dur, crow, rjob, ops, ghost>>
            [] r.s = "moved" ->
                 \* fsync of the file at its new name and of its folder, then of tmp/
                 /\ dur' = [dur EXCEPT ![d][M(f)] = vol[d][M(f)], ![d][T(q)] = vol[d][T(q)]]
                 /\ run' = [run EXCEPT ![f][d].s = "synced"]
                 /\ UNCHANGED <<objs, vol, crow, rjob, ops, ghost>>
            [] r.s = "synced" ->
                 \* the commit: a replica exists only now
                 /\ ops' = [ops EXCEPT ![p].st = "done"]
                 /\ crow' = [crow EXCEPT ![f][d] = [st |-> "ok", obj |-> r.o, verified |-> TRUE, rver |-> objs[r.o].ver]]
                 /\ rjob' = [rjob EXCEPT ![f][d] = NoJob]
                 /\ run' = [run EXCEPT ![f][d] = NoRun]
                 /\ ghost' = Flag(ghost, "inv7", dur[d][M(f)] # r.o \/ objs[r.o].dsize # Size[f])
                 /\ UNCHANGED <<objs, vol, dur>>
            [] OTHER -> FALSE
    /\ UNCHANGED <<dstat, mgen, ufill, vfill, tjob, fver, nextOp, cdisk, pc, sess, cnt>>

(* The run ends once no copy is under way and none can start. *)
RunEnd ==
    /\ pc.ph = "run" /\ Running = {}
    /\ ~\E f \in Files, d \in Disks : CanStart(f, d)
    /\ pc' = Idle /\ sess' = NoSess
    /\ UNCHANGED <<disk, cat, run, cnt, ghost>>

Atoll ==
    \/ PassStart \/ RepOp \/ RepCommit \/ RepNext \/ RepEnd \/ PlanStep \/ RunEnd
    \/ Modify \/ TrashPick \/ TrashMove \/ TrashSync \/ TrashCommit \/ TrashLost
    \/ \E f \in Files, d \in Disks : CopyStart(f, d) \/ CopyHalt(f, d) \/ CopyStep(f, d)

Next == Atoll \/ World

Spec == Init /\ [][Next]_vars

FairSpec == Spec /\ WF_vars(Atoll) /\ \A d \in Disks : WF_vars(Return(d))

-----------------------------------------------------------------------------
(* Invariants. *)

(* §12.4, §13.4, §14: min_free is never violated by Atoll: the bytes Atoll's copies and the view's parts take on a disk
   never leave it less free than min_free, but where a user's own file took that room, as nothing keeps a user from
   filling a disk. *)
MinFreeKept == \A d \in Disks : d \notin ghost.filled => Used(d) + MinFree[d] <= Cap[d]

(* Nothing is written past a disk's capacity: ENOSPC, for Atoll as for anyone. *)
CapacityKept == \A d \in Disks : Used(d) <= Cap[d]

(* The per-path lock: never two copies of one file under way at once (§14). *)
OneCopyOfAFile == \A f \in Files : Cardinality({d \in Disks : run[f][d].s # "none"}) <= 1

(* Each disk's limit of copies at once, read and written (§14), and the limit in all. *)
LimitsKept == Cardinality(Running) <= Total /\ \A e \in Disks : Busy(e) <= Limit[e]

(* 6, 7: no copy of Atoll's at a name of the library without all its bytes, durable, and no commit before the names are
   durable. *)
Inv7_PipelineOrder ==
    /\ \A d \in Disks, f \in Files : LET o == vol[d][M(f)] IN
         (o # 0 /\ objs[o].atoll) => (objs[o].dsize = Size[f] /\ FileOf(objs[o].ver) = f)
    /\ "inv7" \notin ghost.flags

(* 11: no copy Atoll made sits in the library unknown to the catalog, unless its operation is still planned. *)
Inv11_OwnOpsNotUsers ==
    \A d \in Disks, f \in Files : LET o == vol[d][M(f)] IN
        (o # 0 /\ objs[o].atoll) =>
            (crow[f][d].obj = o \/ \E p \in OpIds : ops[p].st = "planned" /\ ops[p].d = d /\ ops[p].f = f)

(* 12: every part in a tmp/, and every entry in a trash, visible or durable, is named by an operation of that disk the
   catalog committed. *)
Inv12_JournalFirst ==
    \A d \in Disks, p \in OpIds :
        /\ (vol[d][T(p)] # 0 \/ dur[d][T(p)] # 0) => (ops[p].k = "create" /\ ops[p].d = d)
        /\ (vol[d][X(p)] # 0 \/ dur[d][X(p)] # 0) => (ops[p].k = "trash" /\ ops[p].d = d)

(* 1: every copy the catalog calls ok and verified holds its file's current content, but for one a user wrote into
   since its row, which the next look finds. *)
Inv1_CopiesHold ==
    \A f \in Files, d \in Disks : (crow[f][d].st = "ok" /\ crow[f][d].verified /\ Up(d) /\ crow[f][d].obj \notin ghost.edited) =>
        (vol[d][M(f)] = crow[f][d].obj /\ objs[crow[f][d].obj].ver = fver[f].cur)

(* The last content a user wrote into a file is durably at its path or in a trash, on some disk, missing or not: a
   refresh sends to the trash only what it overwrites, and copies only from what holds the current content. *)
LatestKept ==
    \A f \in Files : \E d \in Disks, n \in {M(f)} \cup {X(p) : p \in OpIds} :
        dur[d][n] # 0 /\ objs[dur[d][n]].ver = ghost.latest[f]

(* A copy the catalog calls ok, verified, and that nobody wrote into, holds the content its row describes: a refresh
   or a copy made at once with another of its file never records another version (§14.2, §15.4). *)
ReplicaInSync ==
    \A f \in Files, d \in Disks : (crow[f][d].st = "ok" /\ crow[f][d].verified /\ Up(d) /\ crow[f][d].obj \notin ghost.edited
                                   /\ vol[d][M(f)] = crow[f][d].obj) => objs[crow[f][d].obj].ver = crow[f][d].rver

TypeOK ==
    /\ \A p \in OpIds : ops[p].st \in {"none", "planned", "done", "undone"}
    /\ nextOp \in 1..(MaxOps + 1)

(* Liveness: every file reaches its level, where room allows, and no operation stays planned. *)
Settled ==
    /\ \A f \in Files : Cardinality({d \in Disks : crow[f][d].st = "ok"}) >= Level
    /\ \A f \in Files, d \in Disks : crow[f][d].st = "ok" => crow[f][d].rver = fver[f].cur
    /\ \A p \in OpIds : ops[p].st # "planned"

EventuallySettled == <>[]Settled

=============================================================================
