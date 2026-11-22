------------------------------- MODULE Rename -------------------------------
(* Renames and folders, a model of its own beside Replication.tla (specs.md §15.3, §15.12, §17.2, §17.4, §17.5, and the
   writes 3 and 8 of §6.1, a copy renamed with its missing parents and a folder Atoll made removed, each journaled
   first): a file
   renamed directly on one disk into a folder the other disk may lack, and the pass that takes the rename to its other
   copy, with Atoll killed between any two steps, the power cut, a disk lost and mounted again, and a user acting on any
   disk at any moment.

   What it keeps: two logical paths, A/x and B/x, each in a folder of its own below the root, A and B; on each disk the
   file at each path and each folder, by object (an inode with its creation time), as the kernel shows them and as a
   power cut would leave them, a folder with its mode (private or open) and who made it; the catalog's files, their
   copies (where each sits: its file's path, or old_lpath), the entries of the folders with created_by, and the journal
   ops (rename, mkdir, rmdir). Atoll is one process: the mount check, the reprise of each disk's planned operations
   (§17.2), the walk of each disk and its confirmation with the verdicts there, moved, gone and new (§15.10, §15.12), the
   decision Rename (a file takes its new path, its copies behind get old_lpath), then on each disk the rename of each
   copy behind (§15.12, "On each writable disk"): the look at both names, each missing parent an operation mkdir with
   the mode of its model, the folder on the first disk whose copy is at the target, then any disk, then 0755, and a wait
   for a disk that may hold it and was not looked at (§17.4, jobs.folderLike), then the operation rename, journaled, the
   look right before, RENAME_NOREPLACE or link, sync, the look at the source and unlink, the look after, the syncs and
   the commit; and last, Atoll's own empty folders removed, an operation rmdir each (§17.5).

   What it leaves out: the file's content beyond a version (edits, conflicts and the copy pipeline are Replication.tla's),
   deletions propagated to the trash (a copy gone has its row go, Kept), folder renames and their jobs, cycles and
   exchanges, NTFS and refused names, collisions, the guards and the debounce, more than two disks, a user's file
   replacing another by a rename onto it, and the time of folders. *)
EXTENDS Naturals, FiniteSets, Sequences, TLC

CONSTANTS
    NDisks,     \* the pool's disks, by position, each on a physical disk of its own
    Start,      \* the disks that hold the file at A/x at the start
    StartB,     \* [Disks -> {"none", "private", "open"}]: the folder B on each disk at the start
    LinkDisks,  \* the disks whose driver lacks RENAME_NOREPLACE: link, then unlink (specs.md §17.1)
    MaxUser,    \* how many times a user acts
    MaxCrash,   \* how many times Atoll is killed
    MaxCut,     \* how many power cuts
    MaxDrop,    \* how many times a disk is lost
    MaxOps,     \* how many operations Atoll may journal
    UserActs,   \* some of "rename", "create", "delete", "replace", "edit", "mkdir", "chmod", "rmdir"
    Fixes       \* the remedies proposed for the flaws it found and still open, by name (README.md)

Disks == 1..NDisks
OpIds == 1..MaxOps
Paths == {1, 2}                 \* A/x and B/x; the folder of path p is folder p, A or B
FileIds == {1, 2}               \* the file of the start, and one a user makes
Other(p) == 3 - p
Modes == {"private", "open"}

ASSUME /\ NDisks \in {2} /\ Start \subseteq Disks /\ Start # {}
       /\ StartB \in [Disks -> {"none", "private", "open"}]
       /\ LinkDisks \subseteq Disks
       /\ UserActs \subseteq {"rename", "create", "delete", "replace", "edit", "mkdir", "chmod", "rmdir"}
       /\ Fixes \subseteq {"mkdir-key", "rename-back"}

Min(S) == CHOOSE x \in S : \A y \in S : x <= y

(* A copy's row: st none or ok, obj its file_key, old its old_lpath (0 for none), and the verdict of this pass's walk
   with the name it was found moved to. *)
NoCopy == [st |-> "none", obj |-> 0, old |-> 0, v |-> "none", to |-> 0]
NoOp == [k |-> "none", d |-> 0, p |-> 0, q |-> 0, key |-> 0, mode |-> "none", st |-> "none"]
NoEntry == [obj |-> 0, by |-> "none"]
Idle == [ph |-> "idle"]
NoSess == [d \in Disks |-> 0]

VARIABLES
    objs,    \* files and folders: [kind, d, ver, dver (files: content now, after a power cut), mode, dmode (folders), by]
    fv,      \* [Disks -> [Paths -> object or 0]]: the file at each path, as the kernel shows it
    fd,      \* the same, as a power cut leaves it
    dv,      \* [Disks -> [Paths -> object or 0]]: each folder, as the kernel shows it
    dd,      \* the same, as a power cut leaves it
    dstat,   \* [Disks -> {"up", "gone"}]
    mgen,    \* [Disks -> Nat]: the mount id
    cf,      \* the catalog's files: [FileIds -> [on, path]]
    cc,      \* their copies: [FileIds -> [Disks -> row]]
    ce,      \* the entries of the folders: [Disks -> [Paths -> [obj, by]]]
    cdisk,   \* each disk's state from the last mount check
    ops,     \* the journal
    nextOp,
    pc,      \* where Atoll's process is
    sess,    \* the mount each disk had when this session confirmed it, plus 1; 0 for none
    cnt,     \* how many of each event of the world so far
    nver,    \* the last version a user wrote
    ghost    \* for the invariants: versions written and destroyed by users, and flags

vars == <<objs, fv, fd, dv, dd, dstat, mgen, cf, cc, ce, cdisk, ops, nextOp, pc, sess, cnt, nver, ghost>>
disk == <<objs, fv, fd, dv, dd, dstat, mgen>>
cat == <<cf, cc, ce, cdisk, ops, nextOp>>

-----------------------------------------------------------------------------
(* Helpers *)

Up(d) == dstat[d] = "up"
InSess(d) == d \in Disks /\ sess[d] # 0
Ok(d) == Up(d) /\ sess[d] = mgen[d] + 1
Lose(d) == /\ sess' = [sess EXCEPT ![d] = 0]
           /\ cdisk' = [cdisk EXCEPT ![d] = "missing"]
Flag(g, name, cond) == IF cond THEN [g EXCEPT !.flags = @ \cup {name}] ELSE g
SyncObjs(os, S) == [o \in DOMAIN os |-> IF o \in S THEN [os[o] EXCEPT !.dver = os[o].ver, !.dmode = os[o].mode] ELSE os[o]]
OnDisk(os, d) == {o \in DOMAIN os : os[o].d = d}
Seen(n, key) == IF n = 0 THEN "absent" ELSE IF n = key THEN "same" ELSE "other"
Place(f, d) == IF cc[f][d].old # 0 THEN cc[f][d].old ELSE cf[f].path
PlannedOn(d) == {p \in OpIds : ops[p].st = "planned" /\ ops[p].d = d}
(* What the journal says is under way at a path or a folder on a disk, which a walk takes as unknown (§15.12). *)
PlannedAt(d, p) == \E o \in PlannedOn(d) : p \in {ops[o].p, ops[o].q}
Held(v) == \E d \in Disks, p \in Paths : fd[d][p] # 0 /\ objs[fd[d][p]].dver = v
(* A rename's names are durable as Atoll left them, for as long as nobody changed them since: the target still holding
   the file holds it durably, and the old name that no longer holds it does not durably either (invariant 7). *)
NamesDurable(d, p, q, key) == (fv[d][q] = key => fd[d][q] = key) /\ (fv[d][p] # key => fd[d][p] # key)
NextIn(d, S) == LET c == {e \in S : e > d} IN IF c = {} THEN NDisks + 1 ELSE Min(c)
Free(f) == CHOOSE g \in FileIds : ~cf[g].on

-----------------------------------------------------------------------------
(* The start: the file at A/x on the disks of Start, recorded and in sync; the folder A on every disk, B as StartB
   says, each a user's, recorded. Objects 1..NDisks are the file's copies, then each disk's A, then each disk's B. *)

FileObj(d) == d
AObj(d) == NDisks + d
BObj(d) == 2 * NDisks + d
FolderObj(d, q) == IF q = 1 THEN AObj(d) ELSE BObj(d)
HasFolder0(d, q) == q = 1 \/ StartB[d] # "none"

Init ==
    /\ objs = [o \in 1..(3 * NDisks) |->
                 IF o <= NDisks THEN [kind |-> "file", d |-> o, ver |-> 1, dver |-> 1, mode |-> "none", dmode |-> "none", by |-> "user"]
                 ELSE IF o <= 2 * NDisks
                 THEN [kind |-> "dir", d |-> o - NDisks, ver |-> 0, dver |-> 0, mode |-> "open", dmode |-> "open", by |-> "user"]
                 ELSE [kind |-> "dir", d |-> o - 2 * NDisks, ver |-> 0, dver |-> 0,
                       mode |-> IF StartB[o - 2 * NDisks] = "none" THEN "open" ELSE StartB[o - 2 * NDisks],
                       dmode |-> IF StartB[o - 2 * NDisks] = "none" THEN "open" ELSE StartB[o - 2 * NDisks], by |-> "user"]]
    /\ fv = [d \in Disks |-> [p \in Paths |-> IF p = 1 /\ d \in Start THEN FileObj(d) ELSE 0]]
    /\ fd = fv
    /\ dv = [d \in Disks |-> [q \in Paths |-> IF HasFolder0(d, q) THEN FolderObj(d, q) ELSE 0]]
    /\ dd = dv
    /\ dstat = [d \in Disks |-> "up"]
    /\ mgen = [d \in Disks |-> 0]
    /\ cf = [f \in FileIds |-> [on |-> f = 1, path |-> 1]]
    /\ cc = [f \in FileIds |-> [d \in Disks |->
              IF f = 1 /\ d \in Start THEN [NoCopy EXCEPT !.st = "ok", !.obj = FileObj(d)] ELSE NoCopy]]
    /\ ce = [d \in Disks |-> [q \in Paths |-> IF HasFolder0(d, q) THEN [obj |-> FolderObj(d, q), by |-> "user"] ELSE NoEntry]]
    /\ cdisk = [d \in Disks |-> "online"]
    /\ ops = [o \in OpIds |-> NoOp]
    /\ nextOp = 1
    /\ pc = Idle
    /\ sess = NoSess
    /\ cnt = [user |-> 0, crash |-> 0, cut |-> 0, drop |-> 0]
    /\ nver = 1
    /\ ghost = [written |-> {1}, udest |-> {}, flags |-> {}, umoved |-> {}]

-----------------------------------------------------------------------------
(* The world. A user acts on one disk, durably, with what came before on that disk, as in Replication.tla. A folder
   a file needs is made first, with the mode the user gives it. *)

(* moves: the objects a user renamed, each with the path it went to, for RenamesAreUsers. *)
UserMoves(d, objs1, fv1, dv1, gone, newv, moves) ==
    LET objs2 == SyncObjs(objs1, OnDisk(objs1, d))
        fd2 == [fd EXCEPT ![d] = fv1[d]]
    IN /\ objs' = objs2 /\ fv' = fv1 /\ dv' = dv1
       /\ fd' = fd2 /\ dd' = [dd EXCEPT ![d] = dv1[d]]
       /\ nver' = IF newv = 0 THEN nver ELSE newv
       /\ cnt' = [cnt EXCEPT !.user = @ + 1]
       /\ ghost' = [ghost EXCEPT !.written = IF newv = 0 THEN @ ELSE @ \cup {newv},
              !.udest = IF gone # 0 /\ ~(\E e \in Disks, p \in Paths : fd2[e][p] # 0 /\ objs2[fd2[e][p]].dver = gone)
                        THEN @ \cup {gone} ELSE @,
              !.umoved = @ \cup moves]
       /\ UNCHANGED <<dstat, mgen, cat, pc, sess>>

UserAct(d, objs1, fv1, dv1, gone, newv) == UserMoves(d, objs1, fv1, dv1, gone, newv, {})

Allowed(a) == a \in UserActs /\ cnt.user < MaxUser

NewDir(d, m) == [kind |-> "dir", d |-> d, ver |-> 0, dver |-> 0, mode |-> m, dmode |-> m, by |-> "user"]
NewFile(d, v) == [kind |-> "file", d |-> d, ver |-> v, dver |-> v, mode |-> "none", dmode |-> "none", by |-> "user"]

(* §15.3: mv A/x B/x on one disk, mkdir -p B first if it lacks it. *)
URename(d, p, m) ==
    /\ Allowed("rename") /\ Up(d) /\ fv[d][p] # 0 /\ fv[d][Other(p)] = 0
    /\ LET q == Other(p)
           n == Len(objs) + 1
           made == dv[d][q] = 0
       IN UserMoves(d, IF made THEN Append(objs, NewDir(d, m)) ELSE objs,
                    [fv EXCEPT ![d][q] = fv[d][p], ![d][p] = 0],
                    IF made THEN [dv EXCEPT ![d][q] = n] ELSE dv, 0, 0, {<<fv[d][p], q>>})

(* A new file where the path is free on that disk: a new release imported at the old name, say. *)
UCreate(d, p, m) ==
    /\ Allowed("create") /\ Up(d) /\ fv[d][p] = 0
    /\ LET made == dv[d][p] = 0
           objs1 == IF made THEN Append(objs, NewDir(d, m)) ELSE objs
           n == Len(objs1) + 1
       IN UserAct(d, Append(objs1, NewFile(d, nver + 1)), [fv EXCEPT ![d][p] = n],
                  IF made THEN [dv EXCEPT ![d][p] = Len(objs) + 1] ELSE dv, 0, nver + 1)

UDelete(d, p) ==
    /\ Allowed("delete") /\ Up(d) /\ fv[d][p] # 0
    /\ UserAct(d, objs, [fv EXCEPT ![d][p] = 0], dv, objs[fv[d][p]].ver, 0)

(* An atomic save, rename(tmp, file): a new file at the name, the one it replaces gone. *)
UReplace(d, p) ==
    /\ Allowed("replace") /\ Up(d) /\ fv[d][p] # 0
    /\ UserAct(d, Append(objs, NewFile(d, nver + 1)), [fv EXCEPT ![d][p] = Len(objs) + 1], dv, objs[fv[d][p]].ver, nver + 1)

UEdit(d, p) ==
    /\ Allowed("edit") /\ Up(d) /\ fv[d][p] # 0
    /\ LET o == fv[d][p] IN
       UserAct(d, [objs EXCEPT ![o].ver = nver + 1, ![o].dver = nver + 1], fv, dv, objs[o].ver, nver + 1)

UMkdir(d, q, m) ==
    /\ Allowed("mkdir") /\ Up(d) /\ dv[d][q] = 0
    /\ UserAct(d, Append(objs, NewDir(d, m)), fv, [dv EXCEPT ![d][q] = Len(objs) + 1], 0, 0)

UChmod(d, q) ==
    /\ Allowed("chmod") /\ Up(d) /\ dv[d][q] # 0
    /\ LET o == dv[d][q] IN
       UserAct(d, [objs EXCEPT ![o].mode = IF @ = "open" THEN "private" ELSE "open"], fv, dv, 0, 0)

URmdir(d, q) ==
    /\ Allowed("rmdir") /\ Up(d) /\ dv[d][q] # 0 /\ fv[d][q] = 0
    /\ UserAct(d, objs, fv, [dv EXCEPT ![d][q] = 0], 0, 0)

Drop(d) ==
    /\ cnt.drop < MaxDrop /\ Up(d)
    /\ dstat' = [dstat EXCEPT ![d] = "gone"]
    /\ cnt' = [cnt EXCEPT !.drop = @ + 1]
    /\ UNCHANGED <<objs, fv, fd, dv, dd, mgen, cat, pc, sess, nver, ghost>>

Return(d) ==
    /\ dstat[d] = "gone"
    /\ dstat' = [dstat EXCEPT ![d] = "up"]
    /\ mgen' = [mgen EXCEPT ![d] = @ + 1]
    /\ fv' = [fv EXCEPT ![d] = fd[d]]
    /\ dv' = [dv EXCEPT ![d] = dd[d]]
    /\ objs' = [o \in DOMAIN objs |-> IF objs[o].d = d THEN [objs[o] EXCEPT !.ver = objs[o].dver, !.mode = objs[o].dmode] ELSE objs[o]]
    /\ UNCHANGED <<fd, dd, cat, pc, sess, cnt, nver, ghost>>

Crash ==
    /\ cnt.crash < MaxCrash /\ pc # Idle
    /\ pc' = Idle /\ sess' = NoSess
    /\ cnt' = [cnt EXCEPT !.crash = @ + 1]
    /\ UNCHANGED <<disk, cat, nver, ghost>>

PowerCut ==
    /\ cnt.cut < MaxCut
    /\ fv' = fd /\ dv' = dd
    /\ objs' = [o \in DOMAIN objs |-> [objs[o] EXCEPT !.ver = objs[o].dver, !.mode = objs[o].dmode]]
    /\ pc' = Idle /\ sess' = NoSess
    /\ cnt' = [cnt EXCEPT !.cut = @ + 1]
    /\ UNCHANGED <<fd, dd, dstat, mgen, cat, nver, ghost>>

World ==
    \/ \E d \in Disks, p \in Paths, m \in Modes : URename(d, p, m) \/ UCreate(d, p, m) \/ UMkdir(d, p, m)
    \/ \E d \in Disks, p \in Paths : UDelete(d, p) \/ UReplace(d, p) \/ UEdit(d, p) \/ UChmod(d, p) \/ URmdir(d, p)
    \/ \E d \in Disks : Drop(d) \/ Return(d)
    \/ Crash \/ PowerCut

-----------------------------------------------------------------------------
(* §15.12 steps 1 and 2: the mount check, recorded, the session's disks, and the reprise of each. *)

PassStart ==
    /\ pc = Idle
    /\ cdisk' = [d \in Disks |-> IF Up(d) THEN "online" ELSE "missing"]
    /\ sess' = [d \in Disks |-> IF Up(d) THEN mgen[d] + 1 ELSE 0]
    /\ pc' = [ph |-> "reprise", d |-> NextIn(0, {d \in Disks : Up(d)})]
    /\ UNCHANGED <<disk, cf, cc, ce, ops, nextOp, cnt, nver, ghost>>

(* §17.2: a rename, by the identity of §15.1 (the object); a folder made, by what is there: a folder there finishes,
   and takes the mode its operation journaled while it holds nothing (ops.recoverMkdir), recorded Atoll's own; with
   "mkdir-key" in Fixes, a folder that is not the one the operation made is left as it is, undone (README.md, flaw 6);
   a folder removed, by its inode: nothing there finishes. Each in two steps, the names then the commit. *)
RepOp ==
    /\ pc.ph = "reprise" /\ pc.d <= NDisks /\ Ok(pc.d) /\ PlannedOn(pc.d) # {}
    /\ LET d == pc.d
           o == Min(PlannedOn(d))
           op == ops[o]
       IN CASE op.k = "rename" ->
                 LET sr == Seen(fv[d][op.p], op.key)
                     tg == Seen(fv[d][op.q], op.key)
                     fin == tg = "same"
                     fv1 == IF tg = "same" /\ sr = "same" THEN [fv EXCEPT ![d][op.p] = 0] ELSE fv
                 IN /\ fv' = fv1
                    /\ fd' = [fd EXCEPT ![d][op.p] = fv1[d][op.p], ![d][op.q] = fv1[d][op.q]]
                    /\ objs' = SyncObjs(objs, {fv1[d][op.q]} \ {0})
                    /\ pc' = [ph |-> "repcommit", d |-> d, o |-> o, out |-> IF fin THEN "done" ELSE "undone"]
                    /\ UNCHANGED <<dv, dd, ghost>>
            [] op.k = "mkdir" ->
                 LET there == dv[d][op.p]
                     fin == there # 0 /\ ("mkdir-key" \notin Fixes \/ (op.key # 0 /\ there = op.key))
                     adopt == fin /\ fv[d][op.p] = 0 /\ objs[there].mode # op.mode
                 IN /\ objs' = IF adopt THEN SyncObjs([objs EXCEPT ![there].mode = op.mode], {there})
                               ELSE IF fin THEN SyncObjs(objs, {there}) ELSE objs
                    /\ dd' = IF fin THEN [dd EXCEPT ![d][op.p] = there] ELSE dd
                    /\ pc' = [ph |-> "repcommit", d |-> d, o |-> o, out |-> IF fin THEN "done" ELSE "undone", obj |-> there]
                    /\ ghost' = Flag(ghost, "folder-taken", adopt /\ objs[there].by = "user")
                    /\ UNCHANGED <<fv, fd, dv>>
            [] OTHER ->
                 LET there == dv[d][op.p]
                     fin == there = 0
                 IN /\ dd' = IF fin THEN [dd EXCEPT ![d][op.p] = 0] ELSE dd
                    /\ pc' = [ph |-> "repcommit", d |-> d, o |-> o, out |-> IF fin THEN "done" ELSE "undone"]
                    /\ UNCHANGED <<objs, fv, fd, dv, ghost>>
    /\ UNCHANGED <<dstat, mgen, cat, sess, cnt, nver>>

(* The commit: a rename finished gives the copy of that inode on that disk its place, old_lpath NULL when the target
   is its file's path now (§17.2); a folder made is recorded Atoll's own; a folder removed loses its row. *)
RepCommit ==
    /\ pc.ph = "repcommit"
    /\ LET d == pc.d
           o == pc.o
           op == ops[o]
       IN /\ ops' = [ops EXCEPT ![o].st = pc.out]
          /\ cc' = IF op.k = "rename" /\ pc.out = "done"
                   THEN [f \in FileIds |-> [e \in Disks |->
                          IF e = d /\ cc[f][e].st = "ok" /\ cc[f][e].obj = op.key
                          THEN [cc[f][e] EXCEPT !.old = IF cf[f].path = op.q THEN 0 ELSE op.q] ELSE cc[f][e]]]
                   ELSE cc
          /\ ce' = CASE op.k = "mkdir" /\ pc.out = "done" -> [ce EXCEPT ![d][op.p] = [obj |-> pc.obj, by |-> "atoll"]]
                     [] op.k = "rmdir" /\ pc.out = "done" -> [ce EXCEPT ![d][op.p] = NoEntry]
                     [] OTHER -> ce
          /\ ghost' = Flag(ghost, "inv7", op.k = "rename" /\ pc.out = "done" /\ ~NamesDurable(d, op.p, op.q, op.key))
    /\ pc' = [ph |-> "reprise", d |-> pc.d]
    /\ UNCHANGED <<disk, cf, cdisk, nextOp, sess, cnt, nver>>

RepNext ==
    /\ pc.ph = "reprise" /\ pc.d <= NDisks /\ (~Ok(pc.d) \/ PlannedOn(pc.d) = {})
    /\ IF Ok(pc.d) THEN UNCHANGED <<sess, cdisk>> ELSE Lose(pc.d)
    /\ pc' = [ph |-> "reprise", d |-> NextIn(pc.d, {e \in Disks : InSess(e)})]
    /\ UNCHANGED <<disk, cf, cc, ce, ops, nextOp, cnt, nver, ghost>>

RepEnd ==
    /\ pc.ph = "reprise" /\ pc.d > NDisks
    /\ pc' = [ph |-> "walk", d |-> NextIn(0, {e \in Disks : InSess(e)})]
    /\ UNCHANGED <<disk, cat, sess, cnt, nver, ghost>>

-----------------------------------------------------------------------------
(* §15.12 step 3, §15.10: the walk of each disk of the session, then its confirmation, then the verdicts and what is
   new, in one transaction. A copy's verdict: unknown at a name of an operation planned there; there; moved when its
   place does not hold it and the other path does, with no other row of that disk that object; gone when neither does;
   replaced when its place holds another file. A file at a path with no row there of that object is a copy new to the
   catalog of the file at that path, or a file new to it. A folder's entry follows what the walk found: another object
   there is a user's (§15.10). *)

WalkStart ==
    /\ pc.ph = "walk" /\ pc.d <= NDisks
    /\ pc' = [ph |-> "walkend", d |-> pc.d, mg |-> mgen[pc.d], real |-> Up(pc.d),
              fs |-> IF Up(pc.d) THEN fv[pc.d] ELSE [p \in Paths |-> 0],
              ds |-> IF Up(pc.d) THEN dv[pc.d] ELSE [p \in Paths |-> 0]]
    /\ UNCHANGED <<disk, cat, sess, cnt, nver, ghost>>

WalkCommit(d, fs, ds) ==
    LET verdict(f) ==
            LET r == cc[f][d]
                pl == Place(f, d)
            IN IF PlannedAt(d, pl) \/ PlannedAt(d, Other(pl)) THEN "unknown"
               ELSE IF fs[pl] = r.obj THEN "there"
               ELSE IF fs[Other(pl)] = r.obj THEN "moved"
               ELSE IF fs[pl] # 0 THEN "replaced"
               ELSE "gone"
        known(o) == \E f \in FileIds : cc[f][d].st = "ok" /\ cc[f][d].obj = o
        atPath(p) == {f \in FileIds : cf[f].on /\ cf[f].path = p}
        newAt(p) == fs[p] # 0 /\ ~known(fs[p]) /\ ~PlannedAt(d, p)
        cc1 == [f \in FileIds |-> [e \in Disks |->
                  IF e # d \/ cc[f][e].st # "ok" THEN cc[f][e]
                  ELSE [cc[f][e] EXCEPT !.v = verdict(f), !.to = IF verdict(f) = "moved" THEN Other(Place(f, d)) ELSE 0]]]
        \* a copy new to the catalog of the file at that path, when that file has none on this disk
        joins(f, p) == newAt(p) /\ f \in atPath(p) /\ cc1[f][d].st = "none"
        cc2 == [f \in FileIds |-> [e \in Disks |->
                  IF e = d /\ \E p \in Paths : joins(f, p)
                  THEN [NoCopy EXCEPT !.st = "ok", !.obj = fs[CHOOSE p \in Paths : joins(f, p)], !.v = "there"]
                  ELSE cc1[f][e]]]
        \* a file new to the catalog, where no file row holds the path
        fresh == {p \in Paths : newAt(p) /\ atPath(p) = {} /\ \E g \in FileIds : ~cf[g].on}
        pnew == IF fresh = {} THEN 0 ELSE Min(fresh)
        g == IF pnew = 0 THEN 0 ELSE Free(1)
    IN /\ cc' = IF pnew = 0 THEN cc2
                ELSE [cc2 EXCEPT ![g] = [e \in Disks |-> IF e = d THEN [NoCopy EXCEPT !.st = "ok", !.obj = fs[pnew], !.v = "there"]
                                                         ELSE NoCopy]]
       /\ cf' = IF pnew = 0 THEN cf ELSE [cf EXCEPT ![g] = [on |-> TRUE, path |-> pnew]]
       /\ ce' = [ce EXCEPT ![d] = [q \in Paths |->
                   IF PlannedAt(d, q) THEN ce[d][q]
                   ELSE IF ds[q] = 0 THEN NoEntry
                   ELSE IF ds[q] = ce[d][q].obj THEN ce[d][q]
                   ELSE [obj |-> ds[q], by |-> "user"]]]

WalkEnd ==
    /\ pc.ph = "walkend"
    /\ LET d == pc.d IN
       IF Up(d) /\ mgen[d] = pc.mg /\ InSess(d)
       THEN /\ WalkCommit(d, pc.fs, pc.ds)
            /\ ghost' = Flag(ghost, "inv4", ~pc.real)
            /\ UNCHANGED <<sess, cdisk>>
       ELSE /\ Lose(d)
            /\ UNCHANGED <<cf, cc, ce, ghost>>
    /\ pc' = [ph |-> "walk", d |-> NextIn(pc.d, {e \in Disks : InSess(e)})]
    /\ UNCHANGED <<disk, ops, nextOp, cnt, nver>>

WalkDone ==
    /\ pc.ph = "walk" /\ pc.d > NDisks
    /\ pc' = [ph |-> "decide"]
    /\ UNCHANGED <<disk, cat, sess, cnt, nver, ghost>>

-----------------------------------------------------------------------------
(* §15.12 step 4, the decisions, in one step: a file with a copy moved takes its target when no other file row holds
   it; its copy moved loses old_lpath, the others there, unknown or replaced at the path it leaves get it; a copy gone
   has its row go (Kept: nothing goes to the trash in this model), and a file with no copy left goes. *)

Decide ==
    /\ pc.ph = "decide"
    /\ LET moved(f) == {d \in Disks : InSess(d) /\ cc[f][d].st = "ok" /\ cc[f][d].v = "moved"}
           target(f) == cc[f][Min(moved(f))].to
           renames(f) == cf[f].on /\ moved(f) # {} /\ ~\E g \in FileIds : g # f /\ cf[g].on /\ cf[g].path = target(f)
           \* a disk away has its copies unknown: they get old_lpath, as it keeps it until it is back (§15.12)
           row(f, d) ==
               LET r == cc[f][d] IN
               IF r.st # "ok" THEN r
               ELSE IF ~InSess(d) THEN IF renames(f) /\ Place(f, d) # target(f) THEN [r EXCEPT !.old = Place(f, d)] ELSE r
               ELSE IF renames(f)
               THEN CASE r.v = "moved" /\ r.to = target(f) -> [r EXCEPT !.old = 0, !.v = "none"]
                      [] r.v = "gone" -> NoCopy
                      [] r.v = "moved" -> [r EXCEPT !.v = "none"]
                      [] OTHER -> [r EXCEPT !.old = IF Place(f, d) = target(f) THEN 0 ELSE Place(f, d), !.v = "none"]
               ELSE IF r.v = "gone" THEN NoCopy
               ELSE [r EXCEPT !.v = "none"]
           cc1 == [f \in FileIds |-> [d \in Disks |-> row(f, d)]]
       IN /\ cc' = [f \in FileIds |-> IF \E d \in Disks : cc1[f][d].st = "ok" THEN cc1[f] ELSE [d \in Disks |-> NoCopy]]
          /\ cf' = [f \in FileIds |->
                IF ~\E d \in Disks : cc1[f][d].st = "ok" THEN [cf[f] EXCEPT !.on = FALSE]
                ELSE IF renames(f) THEN [cf[f] EXCEPT !.path = target(f)] ELSE cf[f]]
          \* a file takes only a path a user moved one of its copies to; a copy that caught up with its file's own path
          \* is no rename
          /\ ghost' = Flag(ghost, "rename-reverted",
                           \E f \in FileIds : renames(f) /\ target(f) # cf[f].path
                                              /\ <<cc[f][Min(moved(f))].obj, target(f)>> \notin ghost.umoved)
    /\ pc' = [ph |-> "rename", d |-> NextIn(0, {e \in Disks : InSess(e)})]
    /\ UNCHANGED <<disk, ce, cdisk, ops, nextOp, sess, cnt, nver>>

-----------------------------------------------------------------------------
(* §15.12, On each writable disk: each copy behind, one at a time. *)

Behind(d) == {f \in FileIds : cf[f].on /\ cc[f][d].st = "ok" /\ cc[f][d].old # 0}

(* Step 2, the look at both names: the copy where it sits and nothing at the target goes on; nothing where it sits
   and the copy at the target is already-there, its row follows; anything else stays, for the next look. *)
RenPick ==
    /\ pc.ph = "rename" /\ pc.d <= NDisks
    /\ LET d == pc.d
           next == [ph |-> "rename", d |-> NextIn(d, {e \in Disks : InSess(e)})]
       IN IF Behind(d) = {} \/ ~InSess(d) THEN /\ pc' = next /\ UNCHANGED <<cc, sess, cdisk>>
          ELSE IF ~Ok(d) THEN /\ Lose(d) /\ pc' = next /\ UNCHANGED cc
          ELSE LET f == Min(Behind(d))
                   r == cc[f][d]
                   src == fv[d][r.old]
                   tgt == fv[d][cf[f].path]
               IN IF src = r.obj /\ tgt = 0
                  THEN /\ pc' = [ph |-> "parent", d |-> d, f |-> f] /\ UNCHANGED <<cc, sess, cdisk>>
                  ELSE IF src = 0 /\ tgt = r.obj
                  THEN /\ cc' = [cc EXCEPT ![f][d].old = 0] /\ pc' = [ph |-> "rename", d |-> d] /\ UNCHANGED <<sess, cdisk>>
                  ELSE /\ pc' = next /\ UNCHANGED <<cc, sess, cdisk>>
    /\ UNCHANGED <<disk, cf, ce, ops, nextOp, cnt, nver, ghost>>

(* Step 3: a missing parent is an operation mkdir, with the mode of the same folder on the first disk whose copy is at
   the target, or on any disk when none of those holds it, and 0755 (open) when none does; a disk that may hold it and
   was not looked at, or was lost since, makes it wait, io-error (jobs.folderLike). The look at the model and the
   journal are one step here. *)
Model(f, d, q) ==
    LET holders == {e \in Disks \ {d} : cc[f][e].st = "ok" /\ cc[f][e].old = 0}
        \* by position: a disk not looked at is passed over, for the end; the first looked at that cannot be relied on
        \* now, or that holds the folder, answers
        pick(S) == LET met == {e \in S : InSess(e) /\ (~Ok(e) \/ dv[e][q] # 0)} IN
                   IF met # {} THEN IF Ok(Min(met)) THEN objs[dv[Min(met)][q]].mode ELSE "wait"
                   ELSE IF \E e \in S : ~InSess(e) THEN "wait"
                   ELSE "none"
        first == pick(holders)
    IN IF first # "none" THEN first
       ELSE LET any == pick(Disks \ {d}) IN IF any = "none" THEN "open" ELSE any

RenParent ==
    /\ pc.ph = "parent" /\ Ok(pc.d)
    /\ LET d == pc.d
           f == pc.f
           q == cf[f].path
           m == Model(f, d, q)
       IN IF dv[d][q] # 0
          THEN /\ pc' = [ph |-> "journal", d |-> d, f |-> f] /\ UNCHANGED <<ops, nextOp, ghost>>
          ELSE IF m = "wait" \/ nextOp > MaxOps
          THEN /\ pc' = [ph |-> "rename", d |-> NextIn(d, {e \in Disks : InSess(e)})] /\ UNCHANGED <<ops, nextOp, ghost>>
          ELSE /\ ops' = [ops EXCEPT ![nextOp] = [NoOp EXCEPT !.k = "mkdir", !.d = d, !.p = q, !.mode = m, !.st = "planned"]]
               /\ nextOp' = nextOp + 1
               /\ pc' = [ph |-> "mkdir", d |-> d, f |-> f, o |-> nextOp]
               \* a folder made open while another disk holds that folder private (NDisks = 2)
               /\ ghost' = Flag(ghost, "folder-opened", m = "open" /\ \E e \in Disks \ {d} : dv[e][q] # 0 /\ objs[dv[e][q]].mode = "private")
    /\ UNCHANGED <<disk, cf, cc, ce, cdisk, sess, cnt, nver>>

(* The folder made, with that mode, never replacing anything: one there since is EEXIST, undone (makeParents); its
   inode recorded with "mkdir-key"; then synced, then the commit, its entry Atoll's own. *)
MkMake ==
    /\ pc.ph = "mkdir" /\ Ok(pc.d)
    /\ LET d == pc.d
           o == pc.o
           q == ops[o].p
           n == Len(objs) + 1
       IN IF dv[d][q] # 0
          THEN /\ ops' = [ops EXCEPT ![o].st = "undone"]
               /\ pc' = [ph |-> "rename", d |-> NextIn(d, {e \in Disks : InSess(e)})]
               /\ UNCHANGED <<objs, dv>>
          ELSE /\ objs' = Append(objs, [kind |-> "dir", d |-> d, ver |-> 0, dver |-> 0, mode |-> ops[o].mode, dmode |-> "none", by |-> "atoll"])
               /\ dv' = [dv EXCEPT ![d][q] = n]
               /\ ops' = IF "mkdir-key" \in Fixes THEN [ops EXCEPT ![o].key = n] ELSE ops
               /\ pc' = [ph |-> "mksync", d |-> d, f |-> pc.f, o |-> o, obj |-> n]
    /\ UNCHANGED <<fv, fd, dd, dstat, mgen, cf, cc, ce, cdisk, nextOp, sess, cnt, nver, ghost>>

MkSync ==
    /\ pc.ph = "mksync" /\ Ok(pc.d)
    /\ dd' = [dd EXCEPT ![pc.d][ops[pc.o].p] = dv[pc.d][ops[pc.o].p]]
    /\ objs' = SyncObjs(objs, {dv[pc.d][ops[pc.o].p]} \ {0})
    /\ pc' = [pc EXCEPT !.ph = "mkcommit"]
    /\ UNCHANGED <<fv, fd, dv, dstat, mgen, cat, sess, cnt, nver, ghost>>

MkCommit ==
    /\ pc.ph = "mkcommit"
    /\ ops' = [ops EXCEPT ![pc.o].st = "done"]
    /\ ce' = [ce EXCEPT ![pc.d][ops[pc.o].p] = [obj |-> pc.obj, by |-> "atoll"]]
    /\ pc' = [ph |-> "journal", d |-> pc.d, f |-> pc.f]
    /\ UNCHANGED <<disk, cf, cc, cdisk, nextOp, sess, cnt, nver, ghost>>

(* Step 4: the operation rename journaled, then the look right before, the move without replacing anything (or link,
   its sync, the look at the source, unlink), the look after, which must find the copy by its identity or the move is
   resolved by the table of §17.2 at once, the syncs, and the commit: old_lpath NULL. *)
RenJournal ==
    /\ pc.ph = "journal"
    /\ LET d == pc.d
           f == pc.f
       IN IF nextOp > MaxOps
          THEN /\ pc' = [ph |-> "rename", d |-> NextIn(d, {e \in Disks : InSess(e)})] /\ UNCHANGED <<ops, nextOp>>
          ELSE /\ ops' = [ops EXCEPT ![nextOp] = [NoOp EXCEPT !.k = "rename", !.d = d, !.p = cc[f][d].old, !.q = cf[f].path,
                                                   !.key = cc[f][d].obj, !.st = "planned"]]
               /\ nextOp' = nextOp + 1
               /\ pc' = [ph |-> "rstep", d |-> d, f |-> f, o |-> nextOp, s |-> "journaled"]
    /\ UNCHANGED <<disk, cf, cc, ce, cdisk, sess, cnt, nver, ghost>>

RenLook ==
    /\ pc.ph = "rstep" /\ pc.s = "journaled" /\ Ok(pc.d)
    /\ LET op == ops[pc.o] IN
       IF fv[pc.d][op.p] = op.key /\ fv[pc.d][op.q] = 0
       THEN /\ pc' = [pc EXCEPT !.s = "looked"] /\ UNCHANGED ops
       ELSE /\ ops' = [ops EXCEPT ![pc.o].st = "undone"]
            /\ pc' = [ph |-> "rename", d |-> NextIn(pc.d, {e \in Disks : InSess(e)})]
    /\ UNCHANGED <<disk, cf, cc, ce, cdisk, nextOp, sess, cnt, nver, ghost>>

RenMove ==
    /\ pc.ph = "rstep" /\ pc.s = "looked" /\ Ok(pc.d)
    /\ LET d == pc.d
           op == ops[pc.o]
           src == fv[d][op.p]
       IN IF src = 0 \/ fv[d][op.q] # 0
          THEN \* ENOENT or EEXIST: nothing moved, undone
               /\ ops' = [ops EXCEPT ![pc.o].st = "undone"]
               /\ pc' = [ph |-> "rename", d |-> NextIn(d, {e \in Disks : InSess(e)})]
               /\ UNCHANGED fv
          ELSE IF d \in LinkDisks
          THEN /\ fv' = [fv EXCEPT ![d][op.q] = src]
               /\ pc' = [ph |-> "rstep", d |-> d, f |-> pc.f, o |-> pc.o, s |-> "linked", moved |-> src]
               /\ UNCHANGED ops
          ELSE \* rename(2) moves what the name holds now, whatever it is
               /\ fv' = [fv EXCEPT ![d][op.q] = src, ![d][op.p] = 0]
               /\ pc' = [ph |-> "rstep", d |-> d, f |-> pc.f, o |-> pc.o, s |-> "after", moved |-> src]
               /\ UNCHANGED ops
    /\ UNCHANGED <<objs, fd, dv, dd, dstat, mgen, cf, cc, ce, cdisk, nextOp, sess, cnt, nver, ghost>>

RenLinkSync ==
    /\ pc.ph = "rstep" /\ pc.s = "linked" /\ Ok(pc.d)
    /\ fd' = [fd EXCEPT ![pc.d][ops[pc.o].q] = fv[pc.d][ops[pc.o].q]]
    /\ objs' = SyncObjs(objs, {fv[pc.d][ops[pc.o].q]} \ {0})
    /\ pc' = [pc EXCEPT !.s = "lsynced"]
    /\ UNCHANGED <<fv, dv, dd, dstat, mgen, cat, sess, cnt, nver, ghost>>

RenUnlink ==
    /\ pc.ph = "rstep" /\ pc.s = "lsynced" /\ Ok(pc.d)
    /\ LET d == pc.d
           op == ops[pc.o]
           src == fv[d][op.p]
       IN IF src = 0
          THEN /\ pc' = [ph |-> "rstep", d |-> d, f |-> pc.f, o |-> pc.o, s |-> "after", moved |-> fv[d][op.q]]
               /\ UNCHANGED <<fv, ops>>
          ELSE IF src # op.key \/ fv[d][op.q] # op.key
          THEN \* another file since the look: the link it made goes, undone (moveNoReplace)
               /\ fv' = [fv EXCEPT ![d][op.q] = IF fv[d][op.q] = pc.moved THEN 0 ELSE @]
               /\ ops' = [ops EXCEPT ![pc.o].st = "undone"]
               /\ pc' = [ph |-> "rename", d |-> NextIn(d, {e \in Disks : InSess(e)})]
          ELSE /\ fv' = [fv EXCEPT ![d][op.p] = 0]
               /\ pc' = [ph |-> "rstep", d |-> d, f |-> pc.f, o |-> pc.o, s |-> "after", moved |-> op.key]
               /\ UNCHANGED ops
    /\ UNCHANGED <<objs, fd, dv, dd, dstat, mgen, cf, cc, ce, cdisk, nextOp, sess, cnt, nver, ghost>>

(* The look after: the target holds the copy, by its identity; otherwise the table of §17.2, at once: for a rename, a
   logical path is anyone's, so what is at the target stays, and the operation is undone, even when it is what the move
   itself took from the copy's old name, a file that took that name between the look right before and the move (README,
   flaw 7). With "rename-back" in Fixes, such a file goes back to the name it took, as §17.2 has a move to the trash
   put back what took its source's name, synced there. *)
RenAfter ==
    /\ pc.ph = "rstep" /\ pc.s = "after" /\ Ok(pc.d)
    /\ LET d == pc.d
           op == ops[pc.o]
           taken == fv[d][op.q] = pc.moved /\ pc.moved # op.key /\ pc.moved # 0
           back == taken /\ "rename-back" \in Fixes /\ fv[d][op.p] = 0
       IN IF fv[d][op.q] = op.key
          THEN /\ pc' = [pc EXCEPT !.s = "moved"] /\ UNCHANGED <<objs, fv, fd, ops, ghost>>
          ELSE /\ ops' = [ops EXCEPT ![pc.o].st = "undone"]
               /\ fv' = IF back THEN [fv EXCEPT ![d][op.p] = fv[d][op.q], ![d][op.q] = 0] ELSE fv
               /\ fd' = IF back THEN [fd EXCEPT ![d][op.p] = fv[d][op.q], ![d][op.q] = 0] ELSE fd
               /\ objs' = IF back THEN SyncObjs(objs, {fv[d][op.q]}) ELSE objs
               /\ ghost' = Flag(ghost, "name-altered", taken /\ ~back)
               /\ pc' = [ph |-> "rename", d |-> NextIn(d, {e \in Disks : InSess(e)})]
    /\ UNCHANGED <<dv, dd, dstat, mgen, cf, cc, ce, cdisk, nextOp, sess, cnt, nver>>

RenSync ==
    /\ pc.ph = "rstep" /\ pc.s \in {"moved", "tsynced"} /\ Ok(pc.d)
    /\ LET op == ops[pc.o]
           n == IF pc.s = "moved" THEN op.q ELSE op.p
       IN /\ fd' = [fd EXCEPT ![pc.d][n] = fv[pc.d][n]]
          /\ objs' = SyncObjs(objs, {fv[pc.d][n]} \ {0})
    /\ pc' = [pc EXCEPT !.s = IF pc.s = "moved" THEN "tsynced" ELSE "ssynced"]
    /\ UNCHANGED <<fv, dv, dd, dstat, mgen, cat, sess, cnt, nver, ghost>>

RenCommit ==
    /\ pc.ph = "rstep" /\ pc.s = "ssynced"
    /\ LET op == ops[pc.o] IN
       /\ ops' = [ops EXCEPT ![pc.o].st = "done"]
       /\ cc' = [cc EXCEPT ![pc.f][pc.d].old = IF cf[pc.f].path = op.q THEN 0 ELSE op.q]
       /\ ghost' = Flag(ghost, "inv7", ~NamesDurable(pc.d, op.p, op.q, op.key))
    /\ pc' = [ph |-> "rename", d |-> pc.d]
    /\ UNCHANGED <<disk, cf, ce, cdisk, nextOp, sess, cnt, nver>>

(* Any step on a disk that cannot be relied on: the operation stays planned, the disk missing. *)
RenLost ==
    /\ pc.ph \in {"parent", "mkdir", "mksync", "journal", "rstep"} /\ ~Ok(pc.d)
    /\ Lose(pc.d)
    /\ pc' = [ph |-> "rename", d |-> NextIn(pc.d, {e \in Disks : InSess(e)})]
    /\ UNCHANGED <<disk, cf, cc, ce, ops, nextOp, cnt, nver, ghost>>

RenEnd ==
    /\ pc.ph = "rename" /\ pc.d > NDisks
    /\ pc' = [ph |-> "tidy", done |-> {}]
    /\ UNCHANGED <<disk, cat, sess, cnt, nver, ghost>>

-----------------------------------------------------------------------------
(* §17.5: a folder Atoll made, its row saying so, that the catalog holds nothing of below on its disk, goes: an
   operation rmdir, its inode the row's, the name looked at (that folder, empty: unlinkat with AT_REMOVEDIR), removed,
   its parent synced, and the commit. One a step; one that is not that folder or not empty stays, undone. *)
Tidyable(d, q) ==
    /\ InSess(d) /\ ce[d][q].by = "atoll" /\ ce[d][q].obj # 0
    /\ ~\E f \in FileIds : cf[f].on /\ cc[f][d].st = "ok" /\ Place(f, d) = q
    /\ ~PlannedAt(d, q)

Tidy ==
    /\ pc.ph = "tidy"
    /\ LET T == {<<d, q>> \in Disks \X Paths : Tidyable(d, q) /\ <<d, q>> \notin pc.done} IN
       IF T = {} \/ nextOp > MaxOps
       THEN /\ pc' = Idle /\ sess' = NoSess /\ UNCHANGED <<ops, nextOp>>
       ELSE LET t == CHOOSE t \in T : \A u \in T : t[1] < u[1] \/ (t[1] = u[1] /\ t[2] <= u[2]) IN
            /\ ops' = [ops EXCEPT ![nextOp] = [NoOp EXCEPT !.k = "rmdir", !.d = t[1], !.p = t[2], !.key = ce[t[1]][t[2]].obj,
                                               !.st = "planned"]]
            /\ nextOp' = nextOp + 1
            /\ pc' = [ph |-> "rmdir", d |-> t[1], o |-> nextOp, done |-> pc.done \cup {t}]
            /\ UNCHANGED sess
    /\ UNCHANGED <<disk, cf, cc, ce, cdisk, cnt, nver, ghost>>

RmRemove ==
    /\ pc.ph = "rmdir" /\ Ok(pc.d)
    /\ LET d == pc.d
           op == ops[pc.o]
       IN IF dv[d][op.p] = op.key /\ fv[d][op.p] = 0
          THEN /\ dv' = [dv EXCEPT ![d][op.p] = 0]
               /\ pc' = [pc EXCEPT !.ph = "rmsync"]
               /\ ghost' = Flag(ghost, "folder-taken", objs[op.key].by = "user")
               /\ UNCHANGED ops
          ELSE /\ ops' = [ops EXCEPT ![pc.o].st = "undone"]
               /\ pc' = [ph |-> "tidy", done |-> pc.done]
               /\ UNCHANGED <<dv, ghost>>
    /\ UNCHANGED <<objs, fv, fd, dd, dstat, mgen, cf, cc, ce, cdisk, nextOp, sess, cnt, nver>>

RmSync ==
    /\ pc.ph = "rmsync" /\ Ok(pc.d)
    /\ dd' = [dd EXCEPT ![pc.d][ops[pc.o].p] = dv[pc.d][ops[pc.o].p]]
    /\ pc' = [ph |-> "rmcommit", d |-> pc.d, o |-> pc.o, done |-> pc.done]
    /\ UNCHANGED <<objs, fv, fd, dv, dstat, mgen, cat, sess, cnt, nver, ghost>>

RmCommit ==
    /\ pc.ph = "rmcommit"
    /\ ops' = [ops EXCEPT ![pc.o].st = "done"]
    /\ ce' = [ce EXCEPT ![pc.d][ops[pc.o].p] = NoEntry]
    /\ pc' = [ph |-> "tidy", done |-> pc.done]
    /\ UNCHANGED <<disk, cf, cc, cdisk, nextOp, sess, cnt, nver, ghost>>

RmLost ==
    /\ pc.ph \in {"rmdir", "rmsync"} /\ ~Ok(pc.d)
    /\ Lose(pc.d)
    /\ pc' = [ph |-> "tidy", done |-> pc.done]
    /\ UNCHANGED <<disk, cf, cc, ce, ops, nextOp, cnt, nver, ghost>>

-----------------------------------------------------------------------------

Atoll ==
    \/ PassStart \/ RepOp \/ RepCommit \/ RepNext \/ RepEnd
    \/ WalkStart \/ WalkEnd \/ WalkDone \/ Decide
    \/ RenPick \/ RenParent \/ MkMake \/ MkSync \/ MkCommit \/ RenJournal \/ RenLook \/ RenMove \/ RenLinkSync \/ RenUnlink
    \/ RenAfter \/ RenSync \/ RenCommit \/ RenLost \/ RenEnd
    \/ Tidy \/ RmRemove \/ RmSync \/ RmCommit \/ RmLost

Next == Atoll \/ World

Spec == Init /\ [][Next]_vars

FairSpec == Spec /\ WF_vars(Atoll) /\ \A d \in Disks : WF_vars(Return(d))

-----------------------------------------------------------------------------
(* Invariants. *)

(* No version a user wrote is lost: each is durably at a path of some disk, missing disks included, unless a user's
   own action took its last copy (a deletion, an edit in place). *)
NoUserDataLost == \A v \in ghost.written \ ghost.udest : Held(v)

(* 7. A rename is done only once its target durably holds the copy and its old name no longer does: no row is ever
   said to be at a name it is not at (§17.2). *)
Inv7_RenameDurable == "inv7" \notin ghost.flags

(* 4. Nothing a walk of a disk gone found is recorded. *)
Inv4_UnknownNeverAbsent == "inv4" \notin ghost.flags

(* §15.12 step 3, §17.4: a folder Atoll makes for a rename is never open where the other disk holds that folder
   private, as one made with no model would stay open for good. *)
FolderNeverOpened == "folder-opened" \notin ghost.flags

(* §17.5, invariant 2 and the guiding principles: Atoll gives its mode to, records as its own, or removes, only a
   folder it made; a folder a user made is theirs. *)
UserFolderKept == "folder-taken" \notin ghost.flags

(* 12. The journal first: every folder Atoll made, visible or durable, is named by an operation mkdir of that disk. A
   rename moves only from its journaled operation (RenMove), so it needs no check of its own. *)
Inv12_JournalFirst ==
    \A d \in Disks, q \in Paths :
        ((dv[d][q] # 0 /\ objs[dv[d][q]].by = "atoll") \/ (dd[d][q] # 0 /\ objs[dd[d][q]].by = "atoll"))
        => \E o \in OpIds : ops[o].k = "mkdir" /\ ops[o].d = d /\ ops[o].p = q

(* 15. Never alter a file name: Atoll's rename never leaves a file it did not mean to move at the name it gave it. *)
NamesKept == "name-altered" \notin ghost.flags

(* §15.12, A disk away: a file takes only a path a user moved one of its copies to, so that a copy left behind, back,
   never reads as a user renaming the file back. *)
RenamesAreUsers == "rename-reverted" \notin ghost.flags

(* 13 on one disk: a file has one copy row a disk, and two files never one object. *)
OneRowAnObject ==
    \A d \in Disks, f, g \in FileIds :
        (f # g /\ cc[f][d].st = "ok" /\ cc[g][d].st = "ok") => cc[f][d].obj # cc[g][d].obj

TypeOK ==
    /\ \A o \in OpIds : ops[o].st \in {"none", "planned", "done", "undone"}
    /\ nextOp \in 1..(MaxOps + 1)

(* Liveness: every copy follows its file's rename, and no operation stays planned. *)
Followed ==
    /\ \A f \in FileIds, d \in Disks : (cf[f].on /\ cc[f][d].st = "ok") => cc[f][d].old = 0
    /\ \A o \in OpIds : ops[o].st # "planned"

EventuallyFollowed == <>[]Followed

=============================================================================
