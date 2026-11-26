-------------------------------- MODULE Disks --------------------------------
(* A disk's life (specs.md §4, §10, §15.13, §18, §22; invariants 3, 4, 8, 13 and 16): a model of its own. A pool of
   NDisks disks, one file at the same path on the disks of Holders, in sync at the start; with Spare, one more disk the
   machine has mounted, empty, which the configuration does not list yet.

   What it keeps, for each disk: what its device does (answering, failing every read past the cache, unplugged, or
   dead for good), what its mount path shows (its own filesystem mounted from its device, a mount of it that outlived
   that device, the dead superblock a remount gives while the old mount is held, an empty mount point, another
   filesystem, or a clone with its identifier and no marker), whether it is mounted read-only, whether something holds
   a file open on its mount, and the versions of the file its own filesystem holds, at the path and in its trash. The
   catalog: each disk's state in the configuration (in service, draining, failed, or out of it) and its state from the
   last check (online, readonly, missing, and missing past health.missing_grace), the file's current version, whether
   it is in conflict, each copy's row (ok, surplus, stale, pending, or none) and version, and the operation left
   planned on each disk. And Atoll: one process, as each command takes the pool's lock and each pass holds it
   (§15.13), between passes the daemon's check:
   - a pass: the mount check of every disk the configuration lists but a failed one, its verdict recorded (§10); the
     reprise of what is left planned on each disk it can write to, undone here (§17.2); the walk of each disk it can
     read, once its confirmation passes (a read of the head past the page cache, §10), deciding the file from what the
     walk found there (§15.10, §15.12: a copy changed, a modification; a copy gone, a deletion; a copy where the
     catalog has none, recorded; two versions, a conflict); its plan (§13.4: what counts, what is a source, where a
     copy may go, as solver.counts, isSource and planDisk.writable say); then its jobs, a copy, a refresh or a move to
     the trash, each journaled first, then, its device re-checked (the mount the session confirmed still at the path,
     §10), run in one step with its commit, the source looked at again and the target's copy compared with its row;
   - between passes, the daemon's check of a disk missing for a reason a USB reset gives, or not mounted once its
     device left, which unmounts it lazily and mounts it again from /etc/fstab (§15.13), and time passing a disk's
     missing grace (§18);
   - the commands (§18, §25): atoll disk remove --evacuate (draining), disk fail, disk remove, disk replace and disk
     add, each one step under the pool's lock, disk remove refusing a disk with an operation planned on it, or one not
     failed that the file depends on, as catalog.ReadDiskHolds counts it; disk add and replace refusing a disk whose
     identifier is a pool disk's, or one on the physical disk of a pool disk.
   The world acts at any moment: a device unplugged, then plugged again, failing every read, or dead for good; an
   idle mount whose device left unmounted by systemd; a mount path left empty, given another filesystem or a clone;
   a disk mounted read-only, or given another pool's marker; a file held open on a mount, by Atoll's own view or a
   program; the administrator mounting a disk again by hand. A user edits the file directly on a disk, or through
   mergerfs, whose branch of a draining disk takes a write to a file it holds, or while the disk is away from its
   path. And Atoll's process may be killed between any two steps.

   Flaws 29 and 30, open (README.md): disk remove counts what depends on a disk from the catalog alone, so that a
   user's edit made there and not yet taken in by a pass, a disk gone since the last pass, or a surplus copy whose kept
   one is on such a disk, goes out with it; and a disk declared failed with an operation planned on it can never be
   taken out, as nothing reads or writes a failed disk again and disk remove refuses it for that operation. The
   remedies proposed, in Fixes, are the maintainer's to choose.

   What it leaves out: the steps of a copy and of a move, a power cut, the view and the SSD cache (Replication.tla's,
   whose mount check, confirmation and handles held to a mount this model takes as they are), renames and folders,
   the guards (Guards.tla's), the trash's purge and prune of a surplus copy (Trash.tla's), a file's size and the room
   on each disk, so
   that a copy always fits, the physical disks two filesystems share (Replication.tla's, at a fixed topology), standby,
   the readonly reasons but a read-only mount, which are the mount check's pure questions (conformance/vectors/
   disk_check.json), a dirty NTFS volume, disk mark, and the marker of a clone made after disk add, which no check
   can tell from its disk: see README.md. *)
EXTENDS Integers, FiniteSets, TLC

CONSTANTS
    NDisks,      \* the disks of the pool at the start, 1..NDisks, each in service
    Spare,       \* TRUE: disk NDisks + 1, mounted and empty, out of the pool, for disk add and disk replace
    SpareClone,  \* TRUE: the spare's filesystem bears disk 1's identifier, as a clone made by dd does
    SpareShares, \* TRUE: the spare is a filesystem on disk 1's physical disk, another partition
    Level,       \* the replicas the file wants: Level + 1 copies
    Holders,     \* the disks holding the file at the start, each copy ok
    Fuse,        \* TRUE: ntfs-3g's disks: a remount while the old mount is held mounts its dead superblock (§22)
    Remount,     \* health.remount_after_reset, each disk with its /etc/fstab entry
    UserActs,    \* some of "edit", "away" (written while the disk is not mounted at its path, as on another machine)
    UserDisks,   \* the disks a user writes to
    MaxUser,     \* how many times a user acts
    WorldActs,   \* some of "unplug", "eio", "die", "idle", "umount", "other", "clone", "ro", "marker", "hold"
    WorldDisks,  \* the disks the world acts on
    MaxWorld,    \* how many times the world takes something away; what it gives back is not counted
    AtollActs,   \* some of "pass", "kill", "grace", "remount"
    MaxKill,     \* how many times Atoll is killed
    CmdActs,     \* some of "drain", "fail", "remove", "replace", "add"
    CmdDisks,    \* the disks the commands name
    MaxCmd,      \* how many commands run
    Fixes        \* the remedies proposed for the flaws found here and still open, by name (README.md); {} is specs.md as it is

SpareD == NDisks + 1
All == 1..(IF Spare THEN NDisks + 1 ELSE NDisks)
MaxV == 1 + MaxUser
Vers == 1..MaxV
\* the remedies proposed for flaw 29, remove-looks, and for flaw 30, failed-ops-undone or failed-remove-drops (README.md)
FixNames == {"remove-looks", "failed-ops-undone", "failed-remove-drops"}

ASSUME /\ NDisks \in 1..3 /\ Spare \in BOOLEAN /\ SpareClone \in BOOLEAN /\ SpareShares \in BOOLEAN /\ Level \in 0..2
       /\ Holders \subseteq 1..NDisks /\ Holders # {} /\ Fuse \in BOOLEAN /\ Remount \in BOOLEAN
       /\ UserActs \subseteq {"edit", "away"} /\ UserDisks \subseteq All /\ MaxUser \in Nat
       /\ WorldActs \subseteq {"unplug", "eio", "die", "idle", "umount", "other", "clone", "ro", "marker", "hold"}
       /\ WorldDisks \subseteq All /\ MaxWorld \in Nat
       /\ AtollActs \subseteq {"pass", "kill", "grace", "remount"} /\ MaxKill \in Nat
       /\ CmdActs \subseteq {"drain", "fail", "remove", "replace", "add"} /\ CmdDisks \subseteq All /\ MaxCmd \in Nat
       /\ Fixes \subseteq FixNames

VARIABLES
    w,      \* the world: [dv |-> each device: "up", "eio" (every read past the cache fails), "gone" (unplugged), "dead"
            \*   (for good: what it holds is lost), mt |-> each mount path: "live", "stale" (a mount that outlived its
            \*   device), "zombie" (the dead superblock a remount gives while the old mount is held), "none", "other",
            \*   "clone", ro, mk (another pool's marker in its work folder), held, left (its device left since it was
            \*   mounted), lib |-> the version at the file's path
            \*   on its own filesystem (0: none), tr |-> the versions in its trash, and the counts]
    cat,    \* the catalog: [cfg |-> each disk's state in the configuration, cst |-> its state from the last check,
            \*   pg |-> missing past health.missing_grace, cur |-> the file's current version (0: deleted), cfl,
            \*   cp |-> each copy's state (none, ok, surplus, stale, pending), cv |-> its version, op |-> the operation
            \*   planned on each disk]
    pc,     \* Atoll's process: [ph |-> its step, i |-> the disk the step is at, jobs |-> the pass's jobs]
    ses,    \* the session of the pass: [v |-> each disk's verdict at its start, bound |-> TRUE once its confirmation
            \*   passed, while the mount it confirmed is still at the path]
    ghost   \* for the invariants only: [uv |-> the version a user wrote last, gave |-> the versions a user gave up with
            \*   disk fail, died |-> those a disk dead for good took the last of, race |-> those a user wrote, or a
            \*   disk dead for good took, while a removal stood between its look and its edit, flags]

vars == <<w, cat, pc, ses, ghost>>

NoOp == [k |-> "", v |-> 0, was |-> "none", wv |-> 0]
NoSes == [v |-> [d \in All |-> "none"], bound |-> [d \in All |-> FALSE]]
Min(S) == CHOOSE x \in S : \A y \in S : x <= y

Init ==
    /\ w = [dv |-> [d \in All |-> "up"], mt |-> [d \in All |-> "live"], ro |-> [d \in All |-> FALSE],
            mk |-> [d \in All |-> FALSE], held |-> [d \in All |-> FALSE], left |-> [d \in All |-> FALSE],
            lib |-> [d \in All |-> IF d \in Holders THEN 1 ELSE 0], tr |-> [d \in All |-> {}],
            acts |-> 0, users |-> 0, cmds |-> 0, kills |-> 0]
    /\ cat = [cfg |-> [d \in All |-> IF d <= NDisks THEN "in" ELSE "out"],
              cst |-> [d \in All |-> "online"], pg |-> [d \in All |-> FALSE],
              cur |-> 1, cfl |-> FALSE,
              cp |-> [d \in All |-> IF d \in Holders THEN "ok" ELSE "none"],
              cv |-> [d \in All |-> IF d \in Holders THEN 1 ELSE 0],
              op |-> [d \in All |-> NoOp]]
    /\ pc = [ph |-> "idle", i |-> 0, jobs |-> {}]
    /\ ses = NoSes
    /\ ghost = [uv |-> 1, gave |-> {}, died |-> {}, race |-> {}, flags |-> {}]

-----------------------------------------------------------------------------
(* §10, the mount check, answering from the page cache where it can: the state it gives a disk. A mount that
   outlived its device reads device-gone (question 5, measured), the dead superblock io-error (question 10,
   ENOTCONN), an empty mount point not-mounted (question 2), another filesystem wrong-identifier (question 8), a clone
   no-marker (question 11: every disk of this pool had its marker seen at disk add). A device that fails every read
   still passes: only a read past the cache tells (below). A marker of another pool, or of another disk, makes it
   readonly, other-marker: read, never written. *)
Verdict(d) ==
    CASE w.mt[d] = "live" -> IF w.ro[d] \/ w.mk[d] THEN "readonly" ELSE "online"
      [] OTHER            -> "missing"

(* §10, a confirmation: the mount check, then the head of the device read past the page cache. *)
Confirm(d) == Verdict(d) # "missing" /\ w.dv[d] = "up"

(* What a read through the mount path gets: an empty mount point, another filesystem and a clone answer, with what
   they hold, which is not the file; a mount whose device fails or left, and the dead superblock, fail. *)
Answers(d) == \/ w.mt[d] \in {"none", "other", "clone"}
              \/ w.mt[d] = "live" /\ w.dv[d] = "up"
Seen(d) == IF w.mt[d] = "live" THEN w.lib[d] ELSE 0

InUse(d) == cat.cfg[d] \in {"in", "draining"}
(* a copy in sync: ok, or surplus, beyond the file's level (§4) *)
Kept == {"ok", "surplus"}
(* trash.Session: a disk it may write to, online (or draining while online), and one it may read *)
Writable(d) == InUse(d) /\ ses.v[d] = "online"
Readable(d) == InUse(d) /\ ses.v[d] \in {"online", "readonly"}

(* solver.counts: what counts toward the file's level; a draining disk mounted read-only is readonly (heldState), and
   one missing is missing, whose copies count within the grace. *)
CountState(e) == \/ cat.cfg[e] = "in" /\ cat.cst[e] = "online"
                 \/ InUse(e) /\ cat.cst[e] = "readonly"
                 \/ InUse(e) /\ cat.cst[e] = "missing" /\ ~cat.pg[e]
Counts(e) == \/ cat.cp[e] \in Kept /\ cat.cv[e] = cat.cur /\ CountState(e)
             \/ cat.cp[e] = "pending" /\ cat.cfg[e] = "in" /\ cat.cst[e] = "online"
(* solver.isSource, with the session's verdicts *)
Source(e) == cat.cp[e] \in Kept /\ cat.cv[e] = cat.cur /\ Readable(e)
(* planDisk.writable: a copy goes only to a disk in service and online *)
Target(e) == cat.cfg[e] = "in" /\ ses.v[e] = "online" /\ cat.cp[e] \in {"none", "stale"} /\ cat.op[e].k = ""

(* catalog.ReadDiskHolds, §25: a file depends on a disk when it has a copy there, ok, and fewer copies ok on the disks
   online, in standby or readonly than its level, with the disks' states as the catalog holds them, 'draining' for a
   draining disk online; a surplus copy there, or elsewhere, counts for nothing. *)
HoldsForRemove(e) == cat.cp[e] = "ok" /\ (\/ cat.cfg[e] = "in" /\ cat.cst[e] \in {"online", "readonly"}
                                          \/ cat.cfg[e] = "draining" /\ cat.cst[e] = "readonly")
Depending(d) == cat.cp[d] = "ok" /\ (cat.cfl \/ Cardinality({e \in All \ {d} : HoldsForRemove(e)}) < Level + 1)

(* For the invariants: a version a disk of the pool holds, at the path or in its trash, on a device not dead. *)
HoldsV(d, v) == w.lib[d] = v \/ v \in w.tr[d]
KeptOn(v, D) == \E d \in D : InUse(d) /\ w.dv[d] # "dead" /\ HoldsV(d, v)

-----------------------------------------------------------------------------
(* Atoll: a pass. *)
Lose(d) ==
    /\ ses' = [ses EXCEPT !.v[d] = "missing", !.bound[d] = FALSE]
    /\ cat' = [cat EXCEPT !.cst[d] = "missing"]

(* §15.12: the mount check of every disk the configuration lists but a failed one, recorded; the session starts. *)
PassStart ==
    /\ pc.ph = "idle" /\ "pass" \in AtollActs
    /\ LET v == [d \in All |-> IF InUse(d) THEN Verdict(d) ELSE "none"]
       IN /\ ses' = [v |-> v, bound |-> [d \in All |-> FALSE]]
          /\ cat' = [cat EXCEPT !.cst = [d \in All |-> IF v[d] = "none" THEN cat.cst[d] ELSE v[d]],
                                !.pg = [d \in All |-> cat.pg[d] /\ (v[d] = "missing" \/ ~InUse(d))]]
    /\ pc' = [pc EXCEPT !.ph = "reprise", !.i = 1]
    /\ UNCHANGED <<w, ghost>>

Next1(ph) == IF pc.i < Cardinality(All) THEN [pc EXCEPT !.i = pc.i + 1] ELSE [pc EXCEPT !.ph = ph, !.i = 1]

(* §17.2: what is planned on a disk the session can write to is resolved, once its confirmation passed: undone here,
   as each job runs in one step with its commit. A failed disk is never written again (§18): what is planned there
   stays. *)
RepStep ==
    /\ pc.ph = "reprise"
    /\ LET d == pc.i IN
       IF cat.op[d].k # "" /\ cat.cfg[d] = "failed" /\ "failed-ops-undone" \in Fixes
       THEN \* failed-ops-undone, proposed for flaw 30 (README.md): what is planned on a failed disk is undone in the
            \* catalog alone, as nothing reads or writes that disk again (§18)
            /\ cat' = [cat EXCEPT !.cp[d] = cat.op[d].was, !.cv[d] = cat.op[d].wv, !.op[d] = NoOp]
            /\ UNCHANGED ses
       ELSE IF cat.op[d].k # "" /\ Writable(d)
       THEN IF (ses.bound[d] \/ Confirm(d)) /\ Answers(d) /\ w.mt[d] = "live"
            THEN /\ cat' = [cat EXCEPT !.cp[d] = cat.op[d].was, !.cv[d] = cat.op[d].wv, !.op[d] = NoOp]
                 /\ ses' = [ses EXCEPT !.bound[d] = TRUE]
            ELSE Lose(d)
       ELSE UNCHANGED <<cat, ses>>
    /\ pc' = Next1("walk")
    /\ UNCHANGED <<w, ghost>>

(* §15.10, §15.12: what the walk of a disk found at the path, x, decided. A copy gone while the disk's own filesystem
   still holds it is a disk unknown read as a file absent (invariant 4). *)
Decide(d, x) ==
    LET st == cat.cp[d]  v == cat.cv[d]
        flag == IF x = 0 /\ w.lib[d] # 0 THEN {"absent-unknown"} ELSE {}
    IN CASE cat.cur = 0 \/ cat.cfl \/ cat.op[d].k # "" \/ st = "pending" -> UNCHANGED <<cat, ghost>>
         [] st \in Kept /\ x = v -> UNCHANGED <<cat, ghost>>
         [] st \in Kept /\ x = 0 ->
              \* a deletion: the file goes, its other copies to the trash (§15.12)
              /\ cat' = [cat EXCEPT !.cur = 0, !.cp[d] = "none", !.cv[d] = 0]
              /\ ghost' = [ghost EXCEPT !.flags = @ \cup flag]
         [] st \in Kept ->
              \* a modification: its version is current, the other copies are stale (§15.4)
              /\ cat' = [cat EXCEPT !.cur = x, !.cv[d] = x,
                                    !.cp = [e \in All |-> IF e = d THEN "ok" ELSE IF cat.cp[e] \in Kept THEN "stale"
                                                          ELSE cat.cp[e]]]
              /\ UNCHANGED ghost
         [] st = "stale" /\ x = v -> UNCHANGED <<cat, ghost>>
         [] st = "stale" /\ x = 0 ->
              /\ cat' = [cat EXCEPT !.cp[d] = "none", !.cv[d] = 0]
              /\ ghost' = [ghost EXCEPT !.flags = @ \cup flag]
         [] x = 0 -> UNCHANGED <<cat, ghost>>
         [] x = cat.cur -> /\ cat' = [cat EXCEPT !.cp[d] = "ok", !.cv[d] = x]
                           /\ UNCHANGED ghost
         [] OTHER ->
              \* a stale copy written, or a copy where the catalog had none with another version: both kept (§15.7)
              /\ cat' = [cat EXCEPT !.cfl = TRUE]
              /\ UNCHANGED ghost

WalkStep ==
    /\ pc.ph = "walk"
    /\ LET d == pc.i IN
       IF Readable(d)
       THEN IF (ses.bound[d] \/ Confirm(d)) /\ Answers(d)
            THEN /\ Decide(d, Seen(d))
                 /\ ses' = [ses EXCEPT !.bound[d] = TRUE]
            ELSE /\ Lose(d)
                 /\ UNCHANGED ghost
       ELSE UNCHANGED <<cat, ses, ghost>>
    /\ pc' = Next1("plan")
    /\ UNCHANGED w

(* §13.4: the plan, from the catalog as the walks left it and the session's verdicts. *)
PlanJobs ==
    IF cat.cur = 0
    THEN {[k |-> "trash", d |-> e, s |-> 0, j |-> FALSE] :
            e \in {x \in All : cat.cp[x] \in Kept \cup {"stale"} /\ Writable(x) /\ cat.op[x].k = ""}}
    ELSE IF cat.cfl THEN {}
    ELSE LET need == Level + 1 - Cardinality({e \in All : Counts(e)})
             src == {e \in All : Source(e)}
             Tst == {e \in All : Target(e) /\ cat.cp[e] = "stale"}
             Tno == {e \in All : Target(e) /\ cat.cp[e] = "none"}
             pickSt == {e \in Tst : Cardinality({x \in Tst : x < e}) < need}
             pickNo == {e \in Tno : Cardinality({x \in Tno : x < e}) < need - Cardinality(pickSt)}
             drain == {[k |-> "trash", d |-> e, s |-> 0, j |-> FALSE] :
                         e \in {x \in All : cat.cp[x] = "stale" /\ cat.cfg[x] = "draining" /\ Writable(x)
                                            /\ cat.op[x].k = ""}}
         IN drain \cup (IF src # {} /\ need > 0
                        THEN {[k |-> "create", d |-> e, s |-> Min(src), j |-> FALSE] : e \in pickSt \cup pickNo}
                        ELSE {})

(* solver.keep: of the copies in sync that count, the plan keeps the file's level, those on a disk missing last, then
   by position (compareKeep); the others are surplus, and a surplus copy kept is ok again. *)
InSync == {e \in All : cat.cp[e] \in Kept /\ cat.cv[e] = cat.cur /\ CountState(e)}
Before(x, e) == \/ cat.cst[x] # "missing" /\ cat.cst[e] = "missing"
                \/ (cat.cst[x] = "missing") = (cat.cst[e] = "missing") /\ x < e
KeptCopies == {e \in InSync : Cardinality({x \in InSync : Before(x, e)}) < Level + 1}
Marked == [e \in All |-> IF cat.cur = 0 \/ cat.cfl \/ e \notin InSync THEN cat.cp[e]
                         ELSE IF e \in KeptCopies THEN "ok" ELSE "surplus"]

PlanStep ==
    /\ pc.ph = "plan"
    /\ pc' = [pc EXCEPT !.ph = "jobs", !.jobs = PlanJobs]
    /\ cat' = [cat EXCEPT !.cp = Marked]
    /\ UNCHANGED <<w, ses, ghost>>

(* The jobs, the first disk first: journaled, then run with their commit. *)
NextJob == CHOOSE j \in pc.jobs : \A i \in pc.jobs : j.d <= i.d
Drop(j) == pc' = [pc EXCEPT !.jobs = @ \ {j}]

JobJournal ==
    /\ pc.ph = "jobs" /\ pc.jobs # {}
    /\ LET j == NextJob  t == j.d IN
       /\ ~j.j
       /\ IF Writable(t) /\ (j.k = "trash" \/ cat.cfg[t] = "in") /\ cat.op[t].k = ""
          THEN IF ses.bound[t] \/ Confirm(t)
               THEN /\ cat' = [cat EXCEPT !.op[t] = [k |-> j.k, v |-> IF j.k = "create" THEN cat.cur ELSE cat.cv[t],
                                                     was |-> cat.cp[t], wv |-> cat.cv[t]],
                                          !.cp[t] = IF j.k = "create" THEN "pending" ELSE @]
                    /\ ses' = [ses EXCEPT !.bound[t] = TRUE]
                    /\ pc' = [pc EXCEPT !.jobs = (@ \ {j}) \cup {[j EXCEPT !.j = TRUE]}]
               ELSE /\ Lose(t)
                    /\ Drop(j)
          ELSE /\ Drop(j)
               /\ UNCHANGED <<cat, ses>>
    /\ UNCHANGED <<w, ghost>>

(* The target, through the session's handle on it: it answers only while its device does and the mount the session
   confirmed is its live one; a write needs it read-write too. *)
TReach(t) == w.mt[t] = "live" /\ w.dv[t] = "up"
WrongFlags(t, k) == (IF w.mt[t] # "live" THEN {"wrong-place"} ELSE {})
                    \cup (IF cat.cfg[t] \notin {"in", "draining"} \/ cat.cst[t] # "online" THEN {"not-writable"} ELSE {})
                    \cup (IF k = "create" /\ cat.cfg[t] # "in" THEN {"onto-leaving"} ELSE {})

JobDo ==
    /\ pc.ph = "jobs" /\ pc.jobs # {}
    /\ LET j == NextJob  t == j.d  s == j.s  op == cat.op[t] IN
       /\ j.j
       /\ Drop(j)
       /\ IF ~ses.bound[t]
          THEN \* the mount at the path is no longer the one confirmed: nothing is written, the operation stays planned
               /\ Lose(t)
               /\ UNCHANGED <<w, ghost>>
          ELSE IF j.k = "create" /\ ~((ses.bound[s] \/ Confirm(s)) /\ Answers(s))
          THEN \* the source cannot be read: it is lost, and the operation stays planned for the reprise
               /\ Lose(s)
               /\ UNCHANGED <<w, ghost>>
          ELSE IF ~TReach(t)
          THEN /\ Lose(t)
               /\ UNCHANGED <<w, ghost>>
          ELSE IF (j.k = "create" /\ Seen(s) # op.v) \/ w.lib[t] # op.wv \/ (j.k = "trash" /\ w.lib[t] # op.v)
          THEN \* the source, or what is at the target's path, is no longer what its row says (a copy compared with
               \* its row before it moves, a rename that replaces nothing): undone, for the next pass
               /\ cat' = [cat EXCEPT !.cp[t] = op.was, !.cv[t] = op.wv, !.op[t] = NoOp]
               /\ UNCHANGED <<w, ses, ghost>>
          ELSE IF w.ro[t]
          THEN /\ Lose(t)
               /\ UNCHANGED <<w, ghost>>
          ELSE /\ w' = [w EXCEPT !.tr[t] = IF w.lib[t] # 0 THEN @ \cup {w.lib[t]} ELSE @,
                                 !.lib[t] = IF j.k = "create" THEN op.v ELSE 0]
               /\ cat' = [cat EXCEPT !.cp[t] = IF j.k = "create" THEN "ok" ELSE "none",
                                     !.cv[t] = IF j.k = "create" THEN op.v ELSE 0, !.op[t] = NoOp]
               /\ ghost' = [ghost EXCEPT !.flags = @ \cup WrongFlags(t, j.k)]
               /\ UNCHANGED ses

PassEnd ==
    /\ pc.ph = "jobs" /\ pc.jobs = {}
    /\ pc' = [pc EXCEPT !.ph = "idle", !.i = 0]
    /\ ses' = NoSes
    /\ UNCHANGED <<w, cat, ghost>>

(* kill -9: the session goes, and what is planned stays planned. *)
Kill ==
    /\ "kill" \in AtollActs /\ pc.ph # "idle" /\ w.kills < MaxKill
    /\ pc' = [ph |-> "idle", i |-> 0, jobs |-> {}]
    /\ ses' = NoSes
    /\ w' = [w EXCEPT !.kills = @ + 1]
    /\ UNCHANGED <<cat, ghost>>

(* §18: a disk missing for longer than health.missing_grace. *)
Grace(d) ==
    /\ "grace" \in AtollActs /\ pc.ph = "idle"
    /\ InUse(d) /\ cat.cst[d] = "missing" /\ ~cat.pg[d]
    /\ cat' = [cat EXCEPT !.pg[d] = TRUE]
    /\ UNCHANGED <<w, pc, ses, ghost>>

(* §15.13, after a USB reset: a disk missing for device-gone, device-unreadable or io-error, or not mounted once its
   device left, not failed, is unmounted lazily and mounted again from /etc/fstab, at each check while it stays
   missing; with its device not there, nothing is mounted. While the old mount is held, ntfs-3g's remount mounts its
   dead superblock (§22). *)
RemountD(d) ==
    /\ "remount" \in AtollActs /\ Remount /\ pc.ph = "idle"
    /\ InUse(d) /\ cat.cst[d] = "missing" /\ Verdict(d) = "missing"
    /\ \/ w.mt[d] \in {"stale", "zombie"}
       \/ w.mt[d] = "none" /\ w.left[d]
    /\ w' = IF w.dv[d] = "up"
            THEN [w EXCEPT !.mt[d] = IF Fuse /\ w.held[d] THEN "zombie" ELSE "live", !.left[d] = FALSE]
            ELSE [w EXCEPT !.mt[d] = "none"]
    /\ w' # w
    /\ UNCHANGED <<cat, pc, ses, ghost>>

-----------------------------------------------------------------------------
(* The commands, each under the pool's lock: between passes. *)
Cmd(c) == c \in CmdActs /\ pc.ph = "idle" /\ w.cmds < MaxCmd
Counted == w' = [w EXCEPT !.cmds = @ + 1]

(* §18, §25: disk remove --evacuate, refused for a failed disk. *)
Drain(d) ==
    /\ Cmd("drain") /\ d \in CmdDisks /\ cat.cfg[d] = "in"
    /\ cat' = [cat EXCEPT !.cfg[d] = "draining"]
    /\ Counted
    /\ UNCHANGED <<pc, ses, ghost>>

(* §18, §25: disk fail, whatever depends on the disk. What only it holds, the user gives up. *)
Fail(d) ==
    /\ Cmd("fail") /\ d \in CmdDisks /\ InUse(d)
    /\ cat' = [cat EXCEPT !.cfg[d] = "failed"]
    /\ ghost' = [ghost EXCEPT !.gave = IF ~KeptOn(ghost.uv, All \ {d}) THEN @ \cup {ghost.uv} ELSE @]
    /\ Counted
    /\ UNCHANGED <<pc, ses>>

(* §18, §25: disk remove, from what the catalog holds: refused for a disk with an operation planned on it, and for
   one not failed that the file depends on. *)
Dropped(d) == "failed-remove-drops" \in Fixes /\ cat.cfg[d] = "failed"
Remove(d) ==
    /\ "remove-looks" \notin Fixes
    /\ Cmd("remove") /\ d \in CmdDisks /\ cat.cfg[d] \in {"in", "draining", "failed"}
    /\ cat.op[d].k = "" \/ Dropped(d)
    /\ cat.cfg[d] = "failed" \/ ~Depending(d)
    /\ cat' = [cat EXCEPT !.cfg[d] = "out", !.op[d] = NoOp,
                          !.cp[d] = IF @ = "pending" THEN cat.op[d].was ELSE @]
    /\ Counted
    /\ UNCHANGED <<pc, ses, ghost>>

(* remove-looks, proposed for flaw 29 (README.md): under the pool's lock, disk remove checks every disk's mount and
   records what it found, as it records the disks' states after its change, before it counts what depends on the
   disk, a surplus copy counted as an ok one, there and elsewhere, as the plan that marked it may have counted a disk
   gone since; and it looks at the disk it takes out, not failed, once its confirmation passes, as a walk would, refusing
   it when the copy at the path is not what its row says, or when it cannot be looked at. Then it edits the
   configuration, a step of its own. *)
NothingNew(d) == IF cat.cp[d] \in Kept \cup {"stale"} THEN Seen(d) = cat.cv[d] ELSE Seen(d) = 0
RmLook(d) ==
    /\ "remove-looks" \in Fixes
    /\ Cmd("remove") /\ d \in CmdDisks /\ cat.cfg[d] \in {"in", "draining", "failed"}
    /\ cat.op[d].k = "" \/ Dropped(d)
    /\ LET v == [e \in All |-> IF InUse(e) THEN Verdict(e) ELSE "none"]
           c2 == [cat EXCEPT !.cst = [e \in All |-> IF v[e] = "none" THEN cat.cst[e] ELSE v[e]]]
           dep == c2.cp[d] \in Kept /\ (c2.cfl \/ Cardinality({e \in All \ {d} :
                     c2.cp[e] \in Kept /\ (\/ c2.cfg[e] = "in" /\ c2.cst[e] \in {"online", "readonly"}
                                           \/ c2.cfg[e] = "draining" /\ c2.cst[e] = "readonly")}) < Level + 1)
       IN /\ \/ cat.cfg[d] = "failed"
             \/ Confirm(d) /\ Answers(d) /\ NothingNew(d) /\ ~dep
          /\ cat' = c2
    /\ pc' = [pc EXCEPT !.ph = "rm", !.i = d]
    /\ UNCHANGED <<w, ses, ghost>>
RmEdit ==
    /\ pc.ph = "rm"
    /\ cat' = [cat EXCEPT !.cfg[pc.i] = "out", !.op[pc.i] = NoOp,
                          !.cp[pc.i] = IF @ = "pending" THEN cat.op[pc.i].was ELSE @]
    /\ pc' = [pc EXCEPT !.ph = "idle", !.i = 0]
    /\ Counted
    /\ UNCHANGED <<ses, ghost>>

(* §10, §18, §25: disk add and disk replace check the new disk's identity and topology: refused when its identifier
   is a pool disk's, or when it is on the physical disk of a pool disk (R-06). The new disk comes in empty; disk replace marks the old one draining, unless it is failed. *)
NewDisk == /\ Spare /\ cat.cfg[SpareD] = "out" /\ ~SpareClone /\ ~SpareShares /\ Confirm(SpareD)
           /\ cat.cp[SpareD] = "none" /\ w.lib[SpareD] = 0
Add ==
    /\ Cmd("add") /\ NewDisk
    /\ cat' = [cat EXCEPT !.cfg[SpareD] = "in", !.cst[SpareD] = Verdict(SpareD)]
    /\ Counted
    /\ UNCHANGED <<pc, ses, ghost>>
Replace(d) ==
    /\ Cmd("replace") /\ d \in CmdDisks /\ d # SpareD /\ cat.cfg[d] \in {"in", "draining", "failed"} /\ NewDisk
    /\ cat' = [cat EXCEPT !.cfg[SpareD] = "in", !.cst[SpareD] = Verdict(SpareD),
                          !.cfg[d] = IF @ = "failed" THEN "failed" ELSE "draining"]
    /\ Counted
    /\ UNCHANGED <<pc, ses, ghost>>

-----------------------------------------------------------------------------
(* A user, directly on a disk or through mergerfs: a new version written into the copy a disk holds; or while it is
   not mounted at its path, as on another machine. *)
Edit(d) ==
    /\ w.users < MaxUser /\ d \in UserDisks /\ InUse(d) /\ w.dv[d] = "up" /\ w.lib[d] # 0
    /\ \/ "edit" \in UserActs /\ w.mt[d] = "live" /\ ~w.ro[d]
       \/ "away" \in UserActs /\ w.mt[d] \in {"none", "other", "clone"}
    /\ LET v == 2 + w.users IN
       /\ w' = [w EXCEPT !.lib[d] = v, !.users = @ + 1]
       \* written on the disk a removal is taking out, once its look is over: the user's own race with it
       /\ ghost' = [ghost EXCEPT !.uv = v, !.race = IF pc.ph = "rm" /\ pc.i = d THEN @ \cup {v} ELSE @]
    /\ UNCHANGED <<cat, pc, ses>>

(* The world, taking something away: each counted. *)
Away(a) == a \in WorldActs /\ w.acts < MaxWorld
Unbind(d) == ses' = [ses EXCEPT !.bound[d] = FALSE]

Unplug(d) ==
    /\ Away("unplug") /\ d \in WorldDisks /\ w.dv[d] = "up"
    /\ w' = [w EXCEPT !.dv[d] = "gone", !.left[d] = TRUE, !.acts = @ + 1,
                      !.mt[d] = IF @ \in {"live", "zombie"} THEN "stale" ELSE @]
    /\ UNCHANGED <<cat, pc, ses, ghost>>
Fails(d) ==
    /\ Away("eio") /\ d \in WorldDisks /\ w.dv[d] = "up"
    /\ w' = [w EXCEPT !.dv[d] = "eio", !.acts = @ + 1]
    /\ UNCHANGED <<cat, pc, ses, ghost>>
Die(d) ==
    /\ Away("die") /\ d \in WorldDisks /\ w.dv[d] # "dead"
    /\ w' = [w EXCEPT !.dv[d] = "dead", !.acts = @ + 1, !.mt[d] = IF @ \in {"live", "zombie"} THEN "stale" ELSE @]
    /\ ghost' = [ghost EXCEPT !.died = IF ~KeptOn(ghost.uv, All \ {d}) THEN @ \cup {ghost.uv} ELSE @,
                              !.race = IF pc.ph = "rm" THEN @ \cup {ghost.uv} ELSE @]
    /\ UNCHANGED <<cat, pc, ses>>
(* systemd unmounts an idle mount whose device left (§15.13) *)
IdleOut(d) ==
    /\ Away("idle") /\ d \in WorldDisks /\ w.mt[d] = "stale" /\ ~w.held[d]
    /\ w' = [w EXCEPT !.mt[d] = "none", !.acts = @ + 1]
    /\ Unbind(d)
    /\ UNCHANGED <<cat, pc, ghost>>
Umount(d) ==
    /\ Away("umount") /\ d \in WorldDisks /\ w.mt[d] = "live"
    /\ w' = [w EXCEPT !.mt[d] = "none", !.acts = @ + 1]
    /\ Unbind(d)
    /\ UNCHANGED <<cat, pc, ghost>>
MountOther(d) ==
    /\ \E a \in {"other", "clone"} : Away(a) /\ d \in WorldDisks /\ w.mt[d] = "none"
                                     /\ w' = [w EXCEPT !.mt[d] = a, !.acts = @ + 1]
    /\ Unbind(d)
    /\ UNCHANGED <<cat, pc, ghost>>
ReadOnly(d) ==
    /\ Away("ro") /\ d \in WorldDisks /\ w.mt[d] = "live" /\ ~w.ro[d]
    /\ w' = [w EXCEPT !.ro[d] = TRUE, !.acts = @ + 1]
    /\ UNCHANGED <<cat, pc, ses, ghost>>
Marker(d) ==
    /\ Away("marker") /\ d \in WorldDisks /\ w.mt[d] = "live" /\ ~w.mk[d]
    /\ w' = [w EXCEPT !.mk[d] = TRUE, !.acts = @ + 1]
    /\ UNCHANGED <<cat, pc, ses, ghost>>
Hold(d) ==
    /\ Away("hold") /\ d \in WorldDisks /\ ~w.held[d] /\ w.mt[d] \in {"live", "stale"}
    /\ w' = [w EXCEPT !.held[d] = TRUE, !.acts = @ + 1]
    /\ UNCHANGED <<cat, pc, ses, ghost>>

(* The world, giving back: never counted, so that liveness may ask it. *)
Plug(d) ==
    /\ w.dv[d] \in {"gone", "eio"}
    /\ w' = [w EXCEPT !.dv[d] = "up"]
    /\ UNCHANGED <<cat, pc, ses, ghost>>
Release(d) ==
    /\ w.held[d]
    /\ w' = [w EXCEPT !.held[d] = FALSE]
    /\ UNCHANGED <<cat, pc, ses, ghost>>
(* the administrator takes away what is at the path, or mounts the disk again by hand *)
AdminBack(d) ==
    /\ \/ w.mt[d] \in {"other", "clone"} /\ w' = [w EXCEPT !.mt[d] = "none"]
       \/ \/ w.mt[d] = "none" /\ ~w.left[d]
          \/ w.mt[d] \in {"stale", "zombie"}
          \* umount, then mount by hand
          /\ w.dv[d] = "up"
          /\ w' = [w EXCEPT !.mt[d] = IF Fuse /\ w.held[d] THEN "zombie" ELSE "live", !.left[d] = FALSE]
          /\ w' # w
       \/ w.ro[d] /\ w.mt[d] = "live" /\ w' = [w EXCEPT !.ro[d] = FALSE]
       \/ w.mk[d] /\ w' = [w EXCEPT !.mk[d] = FALSE]
    /\ Unbind(d)
    /\ UNCHANGED <<cat, pc, ghost>>

-----------------------------------------------------------------------------
Atoll == PassStart \/ RepStep \/ WalkStep \/ PlanStep \/ JobJournal \/ JobDo \/ PassEnd
         \/ \E d \in All : Grace(d) \/ RemountD(d)

Commands == \/ \E d \in All : Drain(d) \/ Fail(d) \/ Remove(d) \/ RmLook(d) \/ Replace(d)
            \/ Add \/ RmEdit
World == \/ Kill
         \/ \E d \in All : \/ Edit(d)
                           \/ Unplug(d) \/ Fails(d) \/ Die(d) \/ IdleOut(d) \/ Umount(d) \/ MountOther(d)
                           \/ ReadOnly(d) \/ Marker(d) \/ Hold(d)
                           \/ Plug(d) \/ Release(d) \/ AdminBack(d)

Next == Atoll \/ Commands \/ World

Spec == Init /\ [][Next]_vars

(* Atoll goes on, each step of a pass fair, and the daemon's check, which waits for the pool's lock between two
   passes, strongly fair; a device comes back, what holds a mount lets go, and the administrator puts back what he
   took. *)
Pass == PassStart \/ RepStep \/ WalkStep \/ PlanStep \/ JobJournal \/ JobDo \/ PassEnd
LiveSpec == /\ Spec /\ WF_vars(Pass) /\ WF_vars(RmEdit)
            /\ \A d \in All : /\ SF_vars(Grace(d)) /\ SF_vars(RemountD(d))
                              /\ WF_vars(Plug(d)) /\ WF_vars(Release(d)) /\ WF_vars(AdminBack(d))

-----------------------------------------------------------------------------
TypeOK ==
    /\ \A d \in All : /\ w.dv[d] \in {"up", "eio", "gone", "dead"}
                      /\ w.mt[d] \in {"live", "stale", "zombie", "none", "other", "clone"}
                      /\ w.lib[d] \in 0..MaxV /\ w.tr[d] \subseteq Vers
                      /\ cat.cfg[d] \in {"in", "draining", "failed", "out"}
                      /\ cat.cst[d] \in {"online", "readonly", "missing"}
                      /\ cat.cp[d] \in {"none", "ok", "surplus", "stale", "pending"}
                      /\ cat.op[d].k \in {"", "create", "trash"}
                      /\ (w.mt[d] = "live" => w.dv[d] \in {"up", "eio"})
    /\ pc.ph \in {"idle", "reprise", "walk", "plan", "jobs", "rm"}
    /\ ghost.flags \subseteq {"absent-unknown", "wrong-place", "not-writable", "onto-leaving"}

(* Invariant 3, §10: Atoll writes only to a disk whose own filesystem the session confirmed, its mount still the one
   confirmed, through a handle held to it: never to an empty mount point, another filesystem or a clone; only to a
   disk in the pool, not failed, online; and a copy only to a disk in service (§18: a draining disk is a target for
   nothing). *)
Inv3_WritesConfirmed == "wrong-place" \notin ghost.flags /\ "not-writable" \notin ghost.flags
NoCopyOntoLeaving == "onto-leaving" \notin ghost.flags

(* Invariant 4, §10: no copy is decided gone from what a disk missing, unknown or empty showed. *)
Inv4_UnknownNeverAbsent == "absent-unknown" \notin ghost.flags

(* §18, invariant 1: the version a user wrote last is kept by a disk of the pool, in service or draining, at the path
   or in its trash, on a device not dead, but for what the user gave up by declaring its last disk failed, and what a
   disk dead for good took the last of while the pool held it nowhere else: disk remove, disk replace and disk fail
   never take it out of the pool. *)
NoLastCopyLost ==
    \/ KeptOn(ghost.uv, All)
    \/ ghost.uv \in ghost.gave \cup ghost.died \cup ghost.race

(* Invariant 8, §10: one identifier, one disk: never two disks of the configuration with one identifier; and
   invariant 13 at disk add: never two pool filesystems on one physical disk (R-06). *)
OneIdOneDisk == ~(Spare /\ SpareClone /\ cat.cfg[SpareD] # "out")
OnePhysicalDisk == ~(Spare /\ SpareShares /\ cat.cfg[SpareD] # "out")

(* Liveness, with fairness: once the world is done, every disk of the pool whose own filesystem is mounted again,
   read-write, from a device that answers is online in the catalog with nothing planned on it; the file is at its
   level on such disks, as far as they go, unless in conflict or with no copy left to read; a draining disk ends its
   drain, nothing depending on it, once enough disks are in service; and a failed disk can be taken out, nothing
   planned on it. *)
Up(d) == w.dv[d] = "up" /\ w.mt[d] = "live" /\ ~w.ro[d] /\ ~w.mk[d]
InService == {e \in All : cat.cfg[e] = "in" /\ Up(e)}
AtLevel == Cardinality({e \in InService : cat.cp[e] \in Kept /\ cat.cv[e] = cat.cur})
               >= IF Level + 1 < Cardinality(InService) THEN Level + 1 ELSE Cardinality(InService)
(* §18, §25: a failed disk can be taken out: nothing is planned on it that disk remove refuses it for. *)
FailedCanGo == \A d \in All : cat.cfg[d] = "failed" => (cat.op[d].k = "" \/ "failed-remove-drops" \in Fixes)
Settled ==
    /\ \A d \in All : InUse(d) /\ Up(d) => cat.cst[d] = "online" /\ cat.op[d].k = ""
    /\ (cat.cur # 0 /\ ~cat.cfl /\ \E e \in All : InUse(e) /\ Up(e) /\ cat.cp[e] \in Kept /\ cat.cv[e] = cat.cur) => AtLevel
    /\ \A d \in All : (cat.cfg[d] = "draining" /\ Up(d) /\ ~cat.cfl /\ Cardinality(InService) >= Level + 1) => ~Depending(d)
    /\ FailedCanGo
EventuallySettled == <>[]Settled
FailedLeaves == <>[]FailedCanGo
=============================================================================
