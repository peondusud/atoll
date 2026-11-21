----------------------------- MODULE Replication -----------------------------
(* Atoll's most dangerous mechanism, as a model TLC checks: copies made, deletions and modifications propagated,
   copies sent to the trash, with Atoll killed between any two steps, the power cut, a USB disk lost and mounted
   again, and a user acting on any disk at any moment (specs.md §14, §15.12, §17, §20).

   What it keeps: files, their copies on disks (by object, which stands for an inode with its creation time), the
   content each holds (a version number), its change count (what size, modification and change time say) and the count
   of what moved its change time alone (a rename, or a write that kept the size and the dates), the
   names of each disk as the kernel shows them and as they would survive a power cut, the catalog's rows (files,
   copies, jobs, the ops journal), the parts in <workdir>/tmp/ and the entries of <workdir>/trash/, and the mount
   check. What it leaves out: names and paths (one logical path a file), renames and folders, NTFS, the bytes of the
   hashes, the guards (a decision may always wait), the view, the SSD cache, copies at once, repair and scrub, the
   CLI: conformance/models/README.md says more.

   Atoll is one process that runs passes (specs.md §15.12), one step at a time; the user, the disks, a kill -9 and a
   power cut act between any two of its steps. Each action names the section and step of specs.md it formalizes, and
   the invariants at the end carry the numbers of CLAUDE.md's non-negotiable invariants. *)
EXTENDS Naturals, FiniteSets, Sequences, TLC

CONSTANTS
    NDisks,     \* the pool's disks, by position: 1..NDisks
    NFiles,     \* the managed files: 1..NFiles, each at one logical path
    Level,      \* the copies each file wants: replicas + 1 (specs.md §4)
    Phys,       \* [Disks -> Nat]: the physical disk below each disk (specs.md §10, R-06)
    Start,      \* [Files -> SUBSET Disks]: where each file has a copy, in sync and verified, at the start
    LinkDisks,  \* the disks whose driver lacks RENAME_NOREPLACE: link, then unlink (specs.md §17.1)
    MaxUser,    \* how many times a user writes, replaces, creates or deletes
    MaxCrash,   \* how many times Atoll is killed (kill -9)
    MaxCut,     \* how many power cuts
    MaxDrop,    \* how many times a disk is lost: a USB drop (EIO), or a mount that failed (an empty mount point)
    MaxOps,     \* how many operations Atoll may journal: ids never come back (specs.md §9)
    Waits,      \* TRUE: a decision may wait, as for a debounce or settle (specs.md §15.12, Due); FALSE for liveness
    Grace,      \* TRUE: a plan may find a missing disk past health.missing_grace (specs.md §13.4, §18)
    Fixes,      \* the remedies proposed for the flaws it found and still open, by name (README.md); {} is specs.md as it is
    UserActs    \* what a user may do: some of "modify", "replace", "delete", "create", "rewrite"

Disks == 1..NDisks
Files == 1..NFiles
OpIds == 1..MaxOps
NPairs == NFiles * NDisks
PF(i) == (i - 1) \div NDisks + 1     \* the file of the i-th pair (file, disk), file by file
PD(i) == ((i - 1) % NDisks) + 1      \* its disk, by position

ASSUME /\ NDisks \in Nat \ {0} /\ NFiles \in Nat \ {0} /\ Level \in 1..3
       /\ Phys \in [Disks -> Nat] /\ Start \in [Files -> SUBSET Disks]
       /\ LinkDisks \subseteq Disks
       /\ \A f \in Files : \A a, b \in Start[f] : a # b => Phys[a] # Phys[b]
       /\ Fixes = {}    \* no flaw open: each remedy is specs.md's, and the model's whatever Fixes says
       /\ UserActs \subseteq {"modify", "replace", "delete", "create", "rewrite"}

(* Names on a disk: a file's logical path below <root>/, a part <op-id>.part in <workdir>/tmp/,
   and an entry <date>/<op-id>/<logical path> in <workdir>/trash/ (specs.md §6, §14.2, §17). *)
M(f) == f
T(p) == NFiles + p
X(p) == NFiles + MaxOps + p
Names == 1..(NFiles + 2 * MaxOps)
WorkNames == {T(p) : p \in OpIds} \cup {X(p) : p \in OpIds}

Size(v) == v % 2    \* versions 1 and 3 share a size, 2 another: a size alone cannot tell them apart
Min(S) == CHOOSE x \in S : \A y \in S : x <= y

(* A copy's row (specs.md §9): st is "none" for no row; obj its file_key (inode and creation time); gen what
   size, mtime and ctime say; cg what the ctime alone says; verified its verified_ns; by its created_by; gone its
   gone_kind; seen its first_seen_ns, as a clock that ticks. gver and doubt are no columns, for the invariants only:
   what the copy held when its row was written, and that a reprise recorded it unverified as its change time may be a
   write's (specs.md §14.2). *)
NoRow == [st |-> "none", obj |-> 0, gen |-> 0, cg |-> 0, gver |-> 0, verified |-> FALSE, by |-> "none", gone |-> "no", seen |-> 0,
          doubt |-> FALSE]
(* An operation of ops (specs.md §9): kind, disk, file (path), file_key, state, the part it names (path2 of a create:
   the op whose id names the part, as a part resumed keeps its name) and the trash entry's reason. *)
NoOp == [k |-> "none", d |-> 0, f |-> 0, key |-> 0, st |-> "none", part |-> 0, reason |-> "none"]
(* A replicate job (specs.md §14.1): src_disk, the part it owns, the chunk hashes recorded (hver: the version they
   describe), the whole-file hash once verified (whash) with the part's change time then (vcg, of part_look), the
   change time its copy had right after the rename of operation pop (pcg, placed; pop 0 for none), and running once its
   operation is journaled. *)
NoRJob == [on |-> FALSE, src |-> 0, part |-> 0, hver |-> 0, whash |-> 0, vcg |-> 0, pop |-> 0, pcg |-> 0, running |-> FALSE]
(* A trash job (specs.md §15.12): src_disk and reason. *)
NoTJob == [on |-> FALSE, src |-> 0, reason |-> "none"]
(* What this pass's look found of a copy edited or replaced: the changed copies of §15.4. *)
NoLook == [obj |-> 0, gen |-> 0, cg |-> 0]
NoLooks == [f \in Files |-> [d \in Disks |-> NoLook]]
Idle == [ph |-> "idle"]
NoSess == [d \in Disks |-> 0]

VARIABLES
    objs,    \* the files on the disks: [d, ver (content now), dver (content a power cut leaves), gen, cg, atoll (a part Atoll
             \* made)]
    vol,     \* [Disks -> [Names -> object or 0]]: the names as the kernel shows them
    dur,     \* [Disks -> [Names -> object or 0]]: the names as they would survive a power cut
    dstat,   \* [Disks -> {"up", "gone"}]: gone answers EIO, or is an empty mount point
    mgen,    \* [Disks -> Nat]: the mount id, new at each mount
    cfile,   \* the catalog's files: [on (a row), ver (blake3: the version it describes, 0 for none), size, conflict]
    crow,    \* the catalog's copies: [Files -> [Disks -> row]]
    cdisk,   \* the catalog's disk states, from the last mount check: "online" or "missing"
    ops,     \* the journal: [OpIds -> operation]
    nextOp,  \* the next operation id
    rjob,    \* the replicate jobs: [Files -> [Disks -> job]], by target
    tjob,    \* the trash jobs: [Files -> [Disks -> job]], by the disk of the copy
    tick,    \* the clock of first_seen_ns
    pc,      \* where Atoll's process is: lost with it
    sess,    \* [Disks -> Nat]: the mount each disk had when this session confirmed it, plus 1; 0 for a disk it did
             \* not confirm or lost (specs.md §10, §17.2): lost with the process
    look,    \* what this pass's walks found changed (specs.md §15.4): lost with the process
    cnt,     \* how many of each event of the world so far
    nver,    \* the last version a user wrote
    ghost    \* for the invariants only: versions written, destroyed or deleted by users, the latest, and flags

phys == <<objs, vol, dur, dstat, mgen>>
cat == <<cfile, crow, cdisk, ops, nextOp, rjob, tjob, tick>>
vars == <<objs, vol, dur, dstat, mgen, cfile, crow, cdisk, ops, nextOp, rjob, tjob, tick, pc, sess, look, cnt, nver, ghost>>

-----------------------------------------------------------------------------
(* Helpers *)

Up(d) == dstat[d] = "up"
InSess(d) == d \in Disks /\ sess[d] # 0
SessDisks == {d \in Disks : InSess(d)}
(* The session's handle on a disk still works: the disk is there, on the mount the session confirmed. A disk lost
   and mounted again is another mount, whose old handles answer EIO (specs.md §10). *)
Ok(d) == Up(d) /\ sess[d] = mgen[d] + 1
ObjOf(f, d) == (f - 1) * NDisks + d
(* What a name holds, compared with an operation's file_key (specs.md §17.2). *)
Seen(d, n, key) == IF vol[d][n] = 0 THEN "absent" ELSE IF key # 0 /\ vol[d][n] = key THEN "same" ELSE "other"
HoldsFile(f, d) == crow[f][d].st \in {"ok", "stale"}       \* a row whose copy sits on its disk (not pending)
Copies(f) == {d \in Disks : HoldsFile(f, d)}
PlannedOn(d) == {p \in OpIds : ops[p].st = "planned" /\ ops[p].d = d}
PlannedAt(d, f) == \E p \in PlannedOn(d) : ops[p].f = f
(* A version some name holds durably, on any disk, missing or not: in the library or in a trash. A part in tmp/ is
   no copy: a file deleted while a replica of it was being made is the user's to lose (specs.md §17, Limits). *)
Kept == {M(f) : f \in Files} \cup {X(p) : p \in OpIds}
Holds(os, ds, v) == \E d \in Disks, n \in Kept : ds[d][n] # 0 /\ os[ds[d][n]].dver = v
SyncObjs(os, S) == [o \in DOMAIN os |-> IF o \in S THEN [os[o] EXCEPT !.dver = os[o].ver] ELSE os[o]]
OnDisk(os, d) == {o \in DOMAIN os : os[o].d = d}
(* A name that still holds what Atoll put there is durable: a commit comes after the syncs (invariant 7). *)
Durable(d, n, o) == vol[d][n] = o => dur[d][n] = o
Flag(g, name, cond) == IF cond THEN [g EXCEPT !.flags = @ \cup {name}] ELSE g
(* A disk this session can no longer rely on (EIO and the like): missing, and the session ends on it (specs.md §17.1). *)
Lose(d) == /\ sess' = [sess EXCEPT ![d] = 0]
           /\ cdisk' = [cdisk EXCEPT ![d] = "missing"]
NextIdx(i, S) == LET c == {j \in S : j >= i} IN IF c = {} THEN NPairs + 1 ELSE Min(c)
TrashIdx == {j \in 1..NPairs : tjob[PF(j)][PD(j)].on}
CopyIdx == {j \in 1..NPairs : rjob[PF(j)][PD(j)].on /\ ~rjob[PF(j)][PD(j)].running}
NextIn(d, S) == LET c == {e \in S : e > d} IN IF c = {} THEN NDisks + 1 ELSE Min(c)

-----------------------------------------------------------------------------
(* The start: each file of Start at its level or below, every copy a user's, verified. *)

Init ==
    /\ objs = [o \in 1..NPairs |-> [d |-> PD(o), ver |-> PF(o), dver |-> PF(o), gen |-> 0, cg |-> 0, atoll |-> FALSE]]
    /\ vol = [d \in Disks |-> [n \in Names |-> IF n \in Files /\ d \in Start[n] THEN ObjOf(n, d) ELSE 0]]
    /\ dur = vol
    /\ dstat = [d \in Disks |-> "up"]
    /\ mgen = [d \in Disks |-> 0]
    /\ cfile = [f \in Files |-> [on |-> TRUE, ver |-> f, size |-> Size(f), conflict |-> FALSE]]
    /\ crow = [f \in Files |-> [d \in Disks |->
            IF d \in Start[f]
            THEN [st |-> "ok", obj |-> ObjOf(f, d), gen |-> 0, cg |-> 0, gver |-> f, verified |-> TRUE, by |-> "user", gone |-> "no",
                  seen |-> 0, doubt |-> FALSE]
            ELSE NoRow]]
    /\ cdisk = [d \in Disks |-> "online"]
    /\ ops = [p \in OpIds |-> NoOp]
    /\ nextOp = 1
    /\ rjob = [f \in Files |-> [d \in Disks |-> NoRJob]]
    /\ tjob = [f \in Files |-> [d \in Disks |-> NoTJob]]
    /\ tick = 0
    /\ pc = Idle
    /\ sess = NoSess
    /\ look = NoLooks
    /\ cnt = [user |-> 0, crash |-> 0, cut |-> 0, drop |-> 0]
    /\ nver = NFiles
    /\ ghost = [written |-> Files, udest |-> {}, udel |-> {}, latest |-> [f \in Files |-> f], flags |-> {}]

-----------------------------------------------------------------------------
(* The world. A user's action reaches its disk durably, and with it what came before it on that disk, as a
   journal commits in order. *)

UserAct(d, vol1, objs1, gone, f, newv, deleted) ==
    LET objs2 == SyncObjs(objs1, OnDisk(objs1, d))
        dur2 == [dur EXCEPT ![d] = vol1[d]]
    IN /\ vol' = vol1
       /\ objs' = objs2
       /\ dur' = dur2
       /\ nver' = IF newv = 0 THEN nver ELSE newv
       /\ cnt' = [cnt EXCEPT !.user = @ + 1]
       /\ ghost' = [ghost EXCEPT
              !.written = IF newv = 0 THEN @ ELSE @ \cup {newv},
              !.latest = IF newv = 0 THEN @ ELSE [@ EXCEPT ![f] = newv],
              !.udel = IF deleted THEN @ \cup {gone} ELSE @,
              !.udest = IF gone # 0 /\ ~Holds(objs2, dur2, gone) THEN @ \cup {gone} ELSE @]
       /\ UNCHANGED <<dstat, mgen, cat, pc, sess, look>>

(* §15.4: an edit in place, by a program that writes into the file: same inode, new content, and its size,
   modification or change time move. *)
UserModify(f, d) ==
    /\ "modify" \in UserActs /\ cnt.user < MaxUser /\ Up(d) /\ vol[d][M(f)] # 0
    /\ LET o == vol[d][M(f)]
           v == nver + 1
       IN UserAct(d, vol, [objs EXCEPT ![o].ver = v, ![o].dver = v, ![o].gen = @ + 1], objs[o].ver, f, v, FALSE)

(* §14.2, §15.4: an edit in place by a program that keeps the size and the dates, or sets them back, as a tagger may:
   same inode, new content, and only the change time moves. *)
UserRewrite(f, d) ==
    /\ "rewrite" \in UserActs /\ cnt.user < MaxUser /\ Up(d) /\ vol[d][M(f)] # 0
    /\ LET o == vol[d][M(f)]
           v == nver + 1
       IN UserAct(d, vol, [objs EXCEPT ![o].ver = v, ![o].dver = v, ![o].cg = @ + 1], objs[o].ver, f, v, FALSE)

(* §15.2, §15.4: an atomic save, rename(tmp, file), as through mergerfs on the first branch: a new file at the name. *)
UserReplace(f, d) ==
    /\ "replace" \in UserActs /\ cnt.user < MaxUser /\ Up(d) /\ vol[d][M(f)] # 0
    /\ LET o == vol[d][M(f)]
           n == Len(objs) + 1
           v == nver + 1
       IN UserAct(d, [vol EXCEPT ![d][M(f)] = n],
                  Append(objs, [d |-> d, ver |-> v, dver |-> v, gen |-> 0, cg |-> 0, atoll |-> FALSE]),
                  objs[o].ver, f, v, FALSE)

(* §15.5: a deletion directly on one disk, or through mergerfs on each branch, one disk at a time. *)
UserDelete(f, d) ==
    /\ "delete" \in UserActs /\ cnt.user < MaxUser /\ Up(d) /\ vol[d][M(f)] # 0
    /\ UserAct(d, [vol EXCEPT ![d][M(f)] = 0], objs, objs[vol[d][M(f)]].ver, f, 0, TRUE)

(* §14, §15.7: a file written where the path was free: a copy a job did not expect (RENAME_NOREPLACE fails), a file
   brought back by hand, or a file new to the library. *)
UserCreate(f, d) ==
    /\ "create" \in UserActs /\ cnt.user < MaxUser /\ Up(d) /\ vol[d][M(f)] = 0
    /\ LET n == Len(objs) + 1
           v == nver + 1
       IN UserAct(d, [vol EXCEPT ![d][M(f)] = n],
                  Append(objs, [d |-> d, ver |-> v, dver |-> v, gen |-> 0, cg |-> 0, atoll |-> FALSE]),
                  0, f, v, FALSE)

(* §10, invariant 4: a disk lost, as a USB drop whose mount answers EIO, or a mount that failed and left its mount
   point empty. *)
Drop(d) ==
    /\ cnt.drop < MaxDrop /\ Up(d)
    /\ dstat' = [dstat EXCEPT ![d] = "gone"]
    /\ cnt' = [cnt EXCEPT !.drop = @ + 1]
    /\ UNCHANGED <<objs, vol, dur, mgen, cat, pc, sess, look, nver, ghost>>

(* §10: the disk mounted again, a new mount: what it had not made durable is gone, as XFS brings a rename back
   after a remount (§17.1, measured). *)
Return(d) ==
    /\ dstat[d] = "gone"
    /\ dstat' = [dstat EXCEPT ![d] = "up"]
    /\ mgen' = [mgen EXCEPT ![d] = @ + 1]
    /\ vol' = [vol EXCEPT ![d] = dur[d]]
    /\ objs' = [o \in DOMAIN objs |-> IF objs[o].d = d THEN [objs[o] EXCEPT !.ver = objs[o].dver] ELSE objs[o]]
    /\ UNCHANGED <<dur, cat, pc, sess, look, cnt, nver, ghost>>

(* kill -9: the catalog keeps what it committed (synchronous = FULL, invariant 12), the process loses the rest. *)
Crash ==
    /\ cnt.crash < MaxCrash /\ pc # Idle
    /\ pc' = Idle /\ sess' = NoSess /\ look' = NoLooks
    /\ cnt' = [cnt EXCEPT !.crash = @ + 1]
    /\ UNCHANGED <<phys, cat, nver, ghost>>

(* §20: a power cut: every disk keeps only what was durable, and Atoll starts again. *)
PowerCut ==
    /\ cnt.cut < MaxCut
    /\ vol' = dur
    /\ objs' = [o \in DOMAIN objs |-> [objs[o] EXCEPT !.ver = objs[o].dver]]
    /\ pc' = Idle /\ sess' = NoSess /\ look' = NoLooks
    /\ cnt' = [cnt EXCEPT !.cut = @ + 1]
    /\ UNCHANGED <<dur, dstat, mgen, cat, nver, ghost>>

World ==
    \/ \E f \in Files, d \in Disks : UserModify(f, d) \/ UserRewrite(f, d) \/ UserReplace(f, d) \/ UserDelete(f, d) \/ UserCreate(f, d)
    \/ \E d \in Disks : Drop(d) \/ Return(d)
    \/ Crash
    \/ PowerCut

-----------------------------------------------------------------------------
(* §15.12 steps 1 and 2: a pass begins with the mount check of every disk, recorded, and confirms each disk it may
   write to: the session's disks. *)

PassStart ==
    /\ pc = Idle
    /\ cdisk' = [d \in Disks |-> IF Up(d) THEN "online" ELSE "missing"]
    /\ sess' = [d \in Disks |-> IF Up(d) THEN mgen[d] + 1 ELSE 0]
    /\ look' = NoLooks
    /\ pc' = [ph |-> "reprise", d |-> NextIn(0, {d \in Disks : Up(d)})]
    /\ UNCHANGED <<phys, cfile, crow, ops, nextOp, rjob, tjob, tick, cnt, nver, ghost>>

-----------------------------------------------------------------------------
(* §17.2, §14.2 Reprise: the planned operations of each disk of the session, in the order of their ids. Each is
   resolved in two steps: what it does on the disk, then the commit, so that a crash may come between. *)

(* §17.2: a move to the trash: the target is the entry in the trash, the source the logical path. First what both
   names lead to is synced (specs.md §17.2, "First, before it looks at either name"). *)
RepTrash(d, p) ==
    LET f == ops[p].f
        key == ops[p].key
        tg == Seen(d, X(p), key)
        sr == Seen(d, M(f), key)
        row == crow[f][d]
        edited == row.obj # key \/ objs[key].gen # row.gen     \* both names lead to it, edited in place since
        v0 == vol[d]
        out == IF tg = "same" /\ ~(sr = "same" /\ edited) THEN "done" ELSE "undone"
        v1 == CASE tg = "same" /\ sr = "same" /\ edited -> [v0 EXCEPT ![X(p)] = 0]
                [] tg = "same" /\ sr = "same" -> [v0 EXCEPT ![M(f)] = 0]
                [] tg = "other" /\ sr = "absent" -> [v0 EXCEPT ![M(f)] = v0[X(p)], ![X(p)] = 0]
                [] OTHER -> v0
    IN /\ vol' = [vol EXCEPT ![d] = v1]
       /\ dur' = [dur EXCEPT ![d][M(f)] = v1[M(f)], ![d][X(p)] = v1[X(p)]]
       /\ objs' = SyncObjs(objs, {v1[M(f)], v1[X(p)]} \ {0})
       /\ pc' = [ph |-> "repcommit", d |-> d, p |-> p, out |-> out, keep |-> FALSE]

(* §14.2 Reprise: a copy: the target is the file's path, the source the part, both compared with file_key. *)
RepCreate(d, p) ==
    LET f == ops[p].f
        key == ops[p].key
        q == ops[p].part
        tg == Seen(d, M(f), key)
        pt == Seen(d, T(q), key)
        owned == rjob[f][d].on /\ rjob[f][d].part = q
        v0 == vol[d]
        v1 == IF tg = "same"
              THEN IF pt = "same" THEN [v0 EXCEPT ![T(q)] = 0] ELSE v0
              ELSE IF ~owned /\ (pt = "same" \/ (key = 0 /\ pt # "absent")) THEN [v0 EXCEPT ![T(q)] = 0] ELSE v0
        \* The name in tmp/ that goes moves the copy's change time, as an unlink does.
        objs1 == IF tg = "same" /\ pt = "same" THEN [objs EXCEPT ![key].cg = @ + 1] ELSE objs
    IN /\ vol' = [vol EXCEPT ![d] = v1]
       /\ dur' = [dur EXCEPT ![d][M(f)] = v1[M(f)], ![d][T(q)] = v1[T(q)]]
       /\ objs' = SyncObjs(objs1, {v1[M(f)], v1[T(q)]} \ {0})
       /\ pc' = [ph |-> "repcommit", d |-> d, p |-> p, out |-> IF tg = "same" THEN "done" ELSE "undone", keep |-> pt = "same"]

RepriseOp ==
    /\ pc.ph = "reprise" /\ InSess(pc.d) /\ Ok(pc.d) /\ PlannedOn(pc.d) # {}
    /\ LET d == pc.d
           p == Min(PlannedOn(d))
       IN IF ops[p].k = "trash" THEN RepTrash(d, p) ELSE RepCreate(d, p)
    /\ UNCHANGED <<dstat, mgen, cat, sess, look, cnt, nver, ghost>>

(* A file whose last copy went: its pending copies and jobs go with it (catalog.dropFileIfEmpty), but for a pending copy
   whose job is running, which stays with that job, and the file with them (§14.1, §9). *)
Emptied(f, crow1, rjob1) ==
    LET keep == \E e \in Disks : rjob1[f][e].running IN
    /\ crow' = [crow1 EXCEPT ![f] = [e \in Disks |-> IF rjob1[f][e].running THEN crow1[f][e] ELSE NoRow]]
    /\ rjob' = [rjob1 EXCEPT ![f] = [e \in Disks |-> IF rjob1[f][e].running THEN rjob1[f][e] ELSE NoRJob]]
    /\ cfile' = IF keep THEN cfile ELSE [cfile EXCEPT ![f].on = FALSE]

(* §17.1 step 6, §15.12 Trash jobs, Finishing: the operation done, the copy's row gone, or for an overwritten copy
   kept as a pending copy with its replicate job (§15.4), the job gone, and the file gone if that was its last copy. *)
TrashFinish(d, p) ==
    LET f == ops[p].f
        key == ops[p].key
        hasRow == HoldsFile(f, d) /\ crow[f][d].obj = key
        toPending == hasRow /\ ops[p].reason = "overwritten" /\ tjob[f][d].on /\ tjob[f][d].reason = "overwritten"
        row2 == IF toPending THEN [NoRow EXCEPT !.st = "pending", !.by = "atoll", !.seen = tick]
                ELSE IF hasRow THEN NoRow ELSE crow[f][d]
        crow1 == [crow EXCEPT ![f][d] = row2]
        rjob1 == IF toPending THEN [rjob EXCEPT ![f][d] = [NoRJob EXCEPT !.on = TRUE, !.src = tjob[f][d].src]] ELSE rjob
        last == ~\E e \in Disks : crow1[f][e].st \in {"ok", "stale"}
    IN /\ ops' = [ops EXCEPT ![p].st = "done"]
       /\ IF last THEN Emptied(f, crow1, rjob1)
          ELSE /\ crow' = crow1 /\ rjob' = rjob1 /\ UNCHANGED cfile
       /\ tjob' = IF last THEN [tjob EXCEPT ![f] = [e \in Disks |-> NoTJob]] ELSE [tjob EXCEPT ![f][d] = NoTJob]

(* §14.2: in one transaction, the operation done, the pending copy ok with what the part was when it was verified,
   the file's hash when it had none, the source verified, the job gone. The part's gen is 0: the row says the copy is
   untouched since it was verified, so that a write into it once its name is in the library, before the commit or
   while Atoll is down, reads as an edit at the next look (flaw 1 of README.md, fixed); its reprise takes it from what
   the job recorded, never from what it finds. The model's gen stands for size, times and change time at once, and cg
   for what moves the change time alone, the rename's and a write's that keeps the size and the dates: cgRow is the
   change time the row takes, which the commit takes from the look right after the rename, and a reprise as RepriseRow
   says. doubt is a copy a reprise records unverified, as its change time may be a write's: the source and the file's
   hash stand. A reprise that finds no job left for the part records the copy as internal/catalog FinishCreate does: ok,
   created_by atoll, never verified, when the file still has a row, with what it finds; and nothing when it has none. *)
CreateFinish(d, p, cgRow, doubt) ==
    LET f == ops[p].f
        key == ops[p].key
        job == rjob[f][d]
        owned == job.on /\ job.part = ops[p].part
        verified == owned /\ job.whash # 0 /\ (cfile[f].ver = 0 \/ cfile[f].ver = job.whash)
        newRow == [st |-> "ok", obj |-> key, gen |-> IF owned THEN 0 ELSE objs[key].gen, cg |-> cgRow, gver |-> objs[key].ver,
                   verified |-> verified /\ ~doubt, by |-> "atoll", gone |-> "no", seen |-> tick + 1, doubt |-> doubt]
    IN /\ ops' = [ops EXCEPT ![p].st = "done"]
       /\ crow' = IF ~cfile[f].on THEN crow
                  ELSE [crow EXCEPT ![f] = [e \in Disks |->
                          IF e = d THEN newRow
                          ELSE IF verified /\ e = job.src THEN [crow[f][e] EXCEPT !.verified = TRUE]
                          ELSE crow[f][e]]]
       /\ cfile' = IF cfile[f].on /\ verified /\ cfile[f].ver = 0
                   THEN [cfile EXCEPT ![f].ver = job.whash, ![f].size = Size(job.whash)] ELSE cfile
       /\ tick' = IF cfile[f].on THEN tick + 1 ELSE tick
       /\ rjob' = IF owned THEN [rjob EXCEPT ![f][d] = NoRJob] ELSE rjob

(* §14.2 Reprise: the change time the row of a copy found at its name takes, and whether it is recorded unverified. The
   change time found is compared with the one the job recorded right after the rename of this operation (placed):
   another is a write since, and the row is the part's, which the next look finds moved; with none recorded, the crash
   having come between the rename and that record, the row takes what the commit would, and the copy is unverified when
   that change time is not the part's (the maintainer, 2026-11-20). With no job, what it finds. *)
RepriseRow(d, p) ==
    LET f == ops[p].f
        key == ops[p].key
        job == rjob[f][d]
        owned == job.on /\ job.part = ops[p].part
        found == objs[key].cg
        nothingElse == objs[key].gen = 0
    IN CASE ~owned -> [cg |-> found, doubt |-> FALSE]
         [] job.pop = p -> [cg |-> IF found = job.pcg /\ nothingElse THEN found ELSE job.vcg, doubt |-> FALSE]
         [] OTHER -> [cg |-> IF nothingElse THEN found ELSE job.vcg, doubt |-> found # job.vcg]

(* §14.2: undone; its job back in the queue, keeping its part when that is still there (catalog.Requeue). What it
   recorded of its rename is of this operation alone. *)
CreateUndo(d, p, keep) ==
    LET f == ops[p].f
        owned == rjob[f][d].on /\ rjob[f][d].part = ops[p].part
    IN /\ ops' = [ops EXCEPT ![p].st = "undone"]
       /\ rjob' = IF ~owned THEN rjob
                  ELSE [rjob EXCEPT ![f][d] = [@ EXCEPT !.running = FALSE, !.pop = 0, !.pcg = 0,
                           !.part = IF keep THEN @ ELSE 0, !.hver = IF keep THEN @ ELSE 0, !.whash = IF keep THEN @ ELSE 0,
                           !.vcg = IF keep THEN @ ELSE 0]]

RepriseCommit ==
    /\ pc.ph = "repcommit"
    /\ LET d == pc.d
           p == pc.p
       IN IF ops[p].k = "trash"
          THEN IF pc.out = "done"
               THEN /\ TrashFinish(d, p)
                    /\ ghost' = Flag(ghost, "inv7", ~Durable(d, X(pc.p), ops[pc.p].key))
                    /\ UNCHANGED tick
               ELSE /\ ops' = [ops EXCEPT ![p].st = "undone"]
                    /\ UNCHANGED <<cfile, crow, rjob, tjob, tick, ghost>>
          ELSE IF pc.out = "done"
               THEN /\ CreateFinish(d, p, RepriseRow(d, p).cg, RepriseRow(d, p).doubt)
                    /\ ghost' = Flag(ghost, "inv7", ~Durable(d, M(ops[pc.p].f), ops[pc.p].key))
                    /\ UNCHANGED tjob
               ELSE /\ CreateUndo(d, p, pc.keep)
                    /\ UNCHANGED <<cfile, crow, tjob, tick, ghost>>
    /\ pc' = [ph |-> "reprise", d |-> pc.d]
    /\ UNCHANGED <<phys, cdisk, nextOp, sess, look, cnt, nver>>

(* §14.2: once a disk's operations are resolved, its tmp/ loses the parts that no job owns and that an operation
   done or undone names; nothing else in it is touched. *)
Droppable(d, q) ==
    /\ vol[d][T(q)] # 0
    /\ \E p \in OpIds : ops[p].k = "create" /\ ops[p].d = d /\ ops[p].part = q /\ ops[p].st \in {"done", "undone"}
    /\ ~\E p \in OpIds : ops[p].k = "create" /\ ops[p].d = d /\ ops[p].part = q /\ ops[p].st = "planned"
    /\ ~\E f \in Files : rjob[f][d].on /\ rjob[f][d].part = q

RepriseTidy ==
    /\ pc.ph = "reprise" /\ InSess(pc.d) /\ Ok(pc.d) /\ PlannedOn(pc.d) = {}
    /\ LET d == pc.d IN
       vol' = [vol EXCEPT ![d] = [n \in Names |-> IF \E q \in OpIds : n = T(q) /\ Droppable(d, q) THEN 0 ELSE vol[d][n]]]
    /\ pc' = [ph |-> "reprise", d |-> NextIn(pc.d, SessDisks)]
    /\ UNCHANGED <<objs, dur, dstat, mgen, cat, sess, look, cnt, nver, ghost>>

RepriseLost ==
    /\ pc.ph = "reprise" /\ InSess(pc.d) /\ ~Ok(pc.d)
    /\ Lose(pc.d)
    /\ pc' = [ph |-> "reprise", d |-> NextIn(pc.d, SessDisks)]
    /\ UNCHANGED <<phys, cfile, crow, ops, nextOp, rjob, tjob, tick, look, cnt, nver, ghost>>

RepriseEnd ==
    /\ pc.ph = "reprise" /\ pc.d > NDisks
    /\ pc' = [ph |-> "walk", d |-> NextIn(0, {e \in Disks : cdisk[e] = "online"})]
    /\ UNCHANGED <<phys, cat, sess, look, cnt, nver, ghost>>

-----------------------------------------------------------------------------
(* §15.12 step 3, §15.10: the walk of each disk not missing, then the confirmation of §10 before anything it found is
   written: the mount check holds again, and the mount is the one the walk held (mgen). A walk of a disk gone reads
   an empty mount point: nothing there. *)

WalkStart ==
    /\ pc.ph = "walk" /\ pc.d <= NDisks
    /\ LET d == pc.d IN
       pc' = [ph |-> "walkend", d |-> d, real |-> Up(d), mg |-> mgen[d],
              snap |-> [f \in Files |-> IF Up(d) /\ vol[d][M(f)] # 0
                                        THEN [obj |-> vol[d][M(f)], gen |-> objs[vol[d][M(f)]].gen, cg |-> objs[vol[d][M(f)]].cg]
                                        ELSE NoLook],
              own |-> IF Up(d) THEN {vol[d][n] : n \in WorkNames} \ {0} ELSE {}]
    /\ UNCHANGED <<phys, cat, sess, look, cnt, nver, ghost>>

(* §15.12 Verdict, for a copy whose row sits on its disk. *)
Verdict(f, d, s, own) ==
    LET r == crow[f][d] IN
    IF PlannedAt(d, f) THEN "unknown"
    ELSE IF s.obj = r.obj /\ s.gen = r.gen /\ s.cg = r.cg THEN "there"
    ELSE IF s.obj = r.obj THEN "edited"
    ELSE IF s.obj # 0 THEN "replaced"
    ELSE IF r.obj \in own THEN "own"
    ELSE "gone"

(* §15.10: what the walk found, written in one transaction: verdicts into gone_kind, new copies as user copies,
   never verified, a file new to the catalog with no hash, another size a conflict. *)
WalkCommit(d) ==
    LET s == pc.snap
        own == pc.own
        isNew(f) == crow[f][d].st = "none" /\ s[f].obj # 0 /\ ~PlannedAt(d, f)
        anyNew == \E f \in Files : isNew(f)
        newRow(f) == [st |-> "ok", obj |-> s[f].obj, gen |-> s[f].gen, cg |-> s[f].cg, gver |-> objs[s[f].obj].ver,
                      verified |-> FALSE, by |-> "user", gone |-> "no", seen |-> tick + 1, doubt |-> FALSE]
        v(f) == Verdict(f, d, s[f], own)
    IN /\ crow' = [f \in Files |-> [e \in Disks |->
              IF e # d THEN crow[f][e]
              ELSE IF HoldsFile(f, d)
                   THEN CASE v(f) \in {"there", "edited", "replaced"} -> [crow[f][d] EXCEPT !.gone = "no"]
                          [] v(f) = "gone" -> [crow[f][d] EXCEPT !.gone = "gone"]
                          [] v(f) = "own" -> [crow[f][d] EXCEPT !.gone = "own"]
                          [] OTHER -> crow[f][d]
              ELSE IF isNew(f) THEN newRow(f)
              ELSE crow[f][d]]]
       /\ cfile' = [f \in Files |->
              IF ~isNew(f) THEN cfile[f]
              ELSE IF ~cfile[f].on THEN [on |-> TRUE, ver |-> 0, size |-> Size(objs[s[f].obj].ver), conflict |-> FALSE]
              ELSE [cfile[f] EXCEPT !.conflict = @ \/ Size(objs[s[f].obj].ver) # cfile[f].size]]
       /\ look' = [f \in Files |-> [e \in Disks |->
              IF e = d /\ HoldsFile(f, d) /\ v(f) \in {"edited", "replaced"} THEN s[f] ELSE look[f][e]]]
       /\ tick' = IF anyNew THEN tick + 1 ELSE tick

WalkEnd ==
    /\ pc.ph = "walkend"
    /\ LET d == pc.d IN
       IF Up(d) /\ mgen[d] = pc.mg /\ InSess(d)
       THEN /\ WalkCommit(d)
            /\ ghost' = Flag(ghost, "inv4", ~pc.real)
            /\ UNCHANGED <<sess, cdisk>>
       ELSE /\ Lose(d)
            /\ UNCHANGED <<crow, cfile, look, tick, ghost>>
    /\ pc' = [ph |-> "walk", d |-> NextIn(pc.d, {e \in Disks : cdisk[e] = "online"})]
    /\ UNCHANGED <<phys, ops, nextOp, rjob, tjob, cnt, nver>>

WalkDone ==
    /\ pc.ph = "walk" /\ pc.d > NDisks
    /\ pc' = [ph |-> "decide", f |-> 1]
    /\ UNCHANGED <<phys, cat, sess, look, cnt, nver>>
    /\ UNCHANGED ghost

-----------------------------------------------------------------------------
(* §15.12 step 4, The decision about a file (no rename: one path a file). A copy may be peer of those gone. *)

Peer(f, e, G) ==
    \/ crow[f][e].by = "atoll"
    \/ /\ crow[f][e].verified
       /\ \A g \in G : crow[f][g].verified /\ crow[f][e].seen <= crow[f][g].seen

DecideFile ==
    /\ pc.ph = "decide" /\ pc.f <= NFiles
    /\ LET f == pc.f
           R == Copies(f)
           G == {d \in R : crow[f][d].gone = "gone"}
           O == {d \in R : crow[f][d].gone = "own"}
           L == R \ (G \cup O)
           \* invariant 11: a copy decided gone that its disk's trash/ or tmp/ holds, visibly or durably, was Atoll's own move
           ownTaken == \E g \in G, n \in WorkNames : crow[f][g].obj \in {vol[g][n], dur[g][n]}
           deletion == /\ O = {} /\ ~cfile[f].conflict /\ L # {}
                       /\ \A e \in L : look[f][e] = NoLook /\ Peer(f, e, G)
           minSeen == Min({crow[f][g].seen : g \in G \cup O})
           dropHash == (\A e \in L : crow[f][e].seen > minSeen) \/ (\A e \in L : ~crow[f][e].verified)
       IN /\ cfile[f].on /\ G \cup O # {}
          \* §14.1, §15.12: a file waits while a job of it is running, as §15.4 has its modifications wait (flaw 2 of
          \* README.md, fixed).
          /\ ~\E d \in Disks : rjob[f][d].running
          /\ IF L = {}
             THEN \* Gone: the rows go, and the file goes with its jobs (cascade), none running.
                  /\ crow' = [crow EXCEPT ![f] = [d \in Disks |-> NoRow]]
                  /\ cfile' = [cfile EXCEPT ![f].on = FALSE]
                  /\ rjob' = [rjob EXCEPT ![f] = [d \in Disks |-> NoRJob]]
                  /\ tjob' = [tjob EXCEPT ![f] = [d \in Disks |-> NoTJob]]
             ELSE IF deletion
             THEN \* Deletion: the rows of those gone go, and each copy left gets a trash job, deleted.
                  /\ crow' = [crow EXCEPT ![f] = [d \in Disks |-> IF d \in G THEN NoRow ELSE crow[f][d]]]
                  /\ tjob' = [tjob EXCEPT ![f] = [d \in Disks |->
                         IF d \in L /\ ~(tjob[f][d].on /\ tjob[f][d].reason = "deleted")
                         THEN [on |-> TRUE, src |-> Min(G), reason |-> "deleted"] ELSE tjob[f][d]]]
                  /\ UNCHANGED <<cfile, rjob>>
             ELSE \* Kept (invariant 10): the rows go, nothing goes to the trash.
                  /\ crow' = [crow EXCEPT ![f] = [d \in Disks |->
                         IF d \in G \cup O THEN NoRow
                         ELSE IF dropHash THEN [crow[f][d] EXCEPT !.verified = FALSE] ELSE crow[f][d]]]
                  /\ cfile' = IF dropHash THEN [cfile EXCEPT ![f].ver = 0] ELSE cfile
                  /\ UNCHANGED <<rjob, tjob>>
          /\ ghost' = Flag(ghost, "inv11", ownTaken)
    /\ pc' = [ph |-> "decide", f |-> pc.f + 1]
    /\ UNCHANGED <<phys, cdisk, ops, nextOp, tick, sess, look, cnt, nver>>

(* Nothing to decide, or not due yet (the debounce, or a guard: specs.md §15.6). *)
DecideSkip ==
    /\ pc.ph = "decide" /\ pc.f <= NFiles
    /\ LET f == pc.f IN
       \/ ~cfile[f].on \/ \A d \in Copies(f) : crow[f][d].gone = "no"
       \/ \E d \in Disks : rjob[f][d].running
       \/ Waits
    /\ pc' = [ph |-> "decide", f |-> pc.f + 1]
    /\ UNCHANGED <<phys, cat, sess, look, cnt, nver, ghost>>

DecideDone ==
    /\ pc.ph = "decide" /\ pc.f > NFiles
    /\ pc' = [ph |-> "modify", f |-> 1]
    /\ UNCHANGED <<phys, cat, sess, look, cnt, nver, ghost>>

-----------------------------------------------------------------------------
(* §15.4 In a pass: the modifications, once the other decisions are made. The changed copies are those this pass's
   walks found edited or replaced by a regular file. A read checks first that each is still what its walk found. *)

Changed(f) == {d \in Copies(f) : look[f][d] # NoLook}

ModifyWaits(f) ==
    \/ cfile[f].conflict
    \/ \E d \in Copies(f) : crow[f][d].gone # "no"
    \/ \E d \in Disks : rjob[f][d].running
    \/ \E d \in Disks : PlannedAt(d, f) /\ crow[f][d].st # "none"
    \/ \E d \in Changed(f) : ~(Ok(d) /\ vol[d][M(f)] = look[f][d].obj /\ objs[look[f][d].obj].gen = look[f][d].gen
                               /\ objs[look[f][d].obj].cg = look[f][d].cg)

ModifyFile ==
    /\ pc.ph = "modify" /\ pc.f <= NFiles
    /\ LET f == pc.f
           C == Changed(f)
           vs == {objs[look[f][d].obj].ver : d \in C}
           v == CHOOSE x \in vs : TRUE
           conflict == (\E d \in C : crow[f][d].st = "stale") \/ Cardinality(vs) > 1
           taken(d) == [crow[f][d] EXCEPT !.obj = look[f][d].obj, !.gen = look[f][d].gen, !.cg = look[f][d].cg,
                          !.gver = objs[look[f][d].obj].ver, !.verified = TRUE, !.doubt = FALSE,
                          !.seen = IF look[f][d].obj # crow[f][d].obj THEN tick + 1 ELSE @]
       IN /\ cfile[f].on /\ C # {} /\ ~ModifyWaits(f)
          /\ IF conflict
             THEN \* conflict: every version stays; the changed copies lose verified_ns, and a stale one its trash job.
                  /\ cfile' = [cfile EXCEPT ![f].conflict = TRUE]
                  /\ crow' = [crow EXCEPT ![f] = [d \in Disks |-> IF d \in C THEN [crow[f][d] EXCEPT !.verified = FALSE] ELSE crow[f][d]]]
                  /\ tjob' = [tjob EXCEPT ![f] = [d \in Disks |-> IF d \in C /\ crow[f][d].st = "stale" THEN NoTJob ELSE tjob[f][d]]]
                  /\ UNCHANGED <<rjob, tick>>
             ELSE IF vs = {cfile[f].ver}
             THEN \* metadata: the same content; the rows take what the look found.
                  /\ crow' = [crow EXCEPT ![f] = [d \in Disks |-> IF d \in C THEN [taken(d) EXCEPT !.by = crow[f][d].by] ELSE crow[f][d]]]
                  /\ tick' = tick + 1
                  /\ UNCHANGED <<cfile, rjob, tjob>>
             ELSE \* modified: the changed copies are the current version, a user's; every other is stale with a
                  \* trash job, overwritten; every queued job of the file goes, with its pending copies.
                  /\ cfile' = [cfile EXCEPT ![f].ver = v, ![f].size = Size(v)]
                  /\ crow' = [crow EXCEPT ![f] = [d \in Disks |->
                         IF d \in C THEN [taken(d) EXCEPT !.by = "user"]
                         ELSE IF HoldsFile(f, d) THEN [crow[f][d] EXCEPT !.st = "stale", !.verified = FALSE]
                         ELSE IF crow[f][d].st = "pending" THEN NoRow
                         ELSE crow[f][d]]]
                  /\ rjob' = [rjob EXCEPT ![f] = [d \in Disks |-> NoRJob]]
                  /\ tjob' = [tjob EXCEPT ![f] = [d \in Disks |->
                         IF HoldsFile(f, d) /\ d \notin C THEN [on |-> TRUE, src |-> Min(C), reason |-> "overwritten"]
                         ELSE NoTJob]]
                  /\ tick' = tick + 1
    /\ pc' = [ph |-> "modify", f |-> pc.f + 1]
    /\ UNCHANGED <<phys, cdisk, ops, nextOp, sess, look, cnt, nver, ghost>>

ModifySkip ==
    /\ pc.ph = "modify" /\ pc.f <= NFiles
    /\ LET f == pc.f IN ~cfile[f].on \/ Changed(f) = {} \/ ModifyWaits(f) \/ Waits
    /\ pc' = [ph |-> "modify", f |-> pc.f + 1]
    /\ UNCHANGED <<phys, cat, sess, look, cnt, nver, ghost>>

(* §15.4 A stale copy: one that lost its trash job gets it again, once the decisions are made. *)
ModifyDone ==
    /\ pc.ph = "modify" /\ pc.f > NFiles
    /\ tjob' = [f \in Files |-> [d \in Disks |->
           IF /\ crow[f][d].st = "stale" /\ ~tjob[f][d].on /\ cfile[f].on /\ ~cfile[f].conflict
              /\ crow[f][d].gone = "no" /\ look[f][d] = NoLook
              /\ \E e \in Disks : crow[f][e].st = "ok"
           THEN [on |-> TRUE, src |-> Min({e \in Disks : crow[f][e].st = "ok"}), reason |-> "overwritten"]
           ELSE tjob[f][d]]]
    /\ pc' = [ph |-> "trash", i |-> 1]
    /\ UNCHANGED <<phys, cfile, crow, cdisk, ops, nextOp, rjob, tick, sess, look, cnt, nver, ghost>>

-----------------------------------------------------------------------------
(* §15.12 step 5, Trash jobs, and §17.1: one operation a copy. *)

(* §15.12: a deletion no longer stands when the file holds a copy that no job covers, as one restored since. *)
Gained(f) == \E e \in Disks : crow[f][e].st # "none" /\ ~tjob[f][e].on /\ ~rjob[f][e].on

TrashPick ==
    /\ pc.ph = "trash"
    /\ LET i == NextIdx(pc.i, TrashIdx)
           f == PF(i)
           d == PD(i)
           job == tjob[f][d]
           nexti == [ph |-> "trash", i |-> i + 1]
       IN IF i > NPairs THEN /\ pc' = [ph |-> "plan"] /\ UNCHANGED <<cat, sess>>
          ELSE IF ~InSess(d) THEN /\ pc' = nexti /\ UNCHANGED <<cat, sess>>          \* disk-unavailable: it waits
          ELSE IF ~Ok(d) THEN /\ Lose(d) /\ pc' = nexti
                              /\ UNCHANGED <<cfile, crow, ops, nextOp, rjob, tjob, tick>>
          ELSE IF job.reason = "deleted" /\ (cfile[f].conflict \/ Gained(f))
          THEN \* canceled: the deletion no longer stands, every queued trash job of the file goes
               /\ tjob' = [tjob EXCEPT ![f] = [e \in Disks |-> NoTJob]]
               /\ pc' = nexti
               /\ UNCHANGED <<cfile, crow, cdisk, ops, nextOp, rjob, tick, sess>>
          ELSE IF job.reason = "overwritten" /\ (crow[f][d].st # "stale" \/ cfile[f].conflict \/ ~\E e \in Disks : crow[f][e].st = "ok")
          THEN /\ tjob' = [tjob EXCEPT ![f][d] = NoTJob]
               /\ pc' = nexti
               /\ UNCHANGED <<cfile, crow, cdisk, ops, nextOp, rjob, tick, sess>>
          ELSE IF nextOp > MaxOps THEN /\ pc' = nexti /\ UNCHANGED <<cat, sess>>      \* the model's bound
          ELSE \* §17.1 step 1: the ops row planned, the trash row and the event, in one transaction.
               /\ ops' = [ops EXCEPT ![nextOp] = [k |-> "trash", d |-> d, f |-> f, key |-> crow[f][d].obj, st |-> "planned",
                                                  part |-> 0, reason |-> job.reason]]
               /\ nextOp' = nextOp + 1
               /\ pc' = [ph |-> "tstep", i |-> i, p |-> nextOp, s |-> "journaled"]
               /\ UNCHANGED <<cfile, crow, cdisk, rjob, tjob, tick, sess>>
    /\ UNCHANGED <<phys, look, cnt, nver, ghost>>

(* §17.1 steps 2 to 4: the name still holds the copy, compared with its row right before the move; then the move
   without replacing anything. Without RENAME_NOREPLACE: link, fsync of the file then of the target folder, a look
   at the source's name (inode, size, mtime), unlink. Write 4 of §6.1, and the first half of write 2. *)
TrashMove ==
    /\ pc.ph = "tstep" /\ pc.s = "journaled"
    /\ LET p == pc.p
           f == ops[p].f
           d == ops[p].d
           key == ops[p].key
           untouched == vol[d][M(f)] = key /\ key # 0 /\ objs[key].gen = crow[f][d].gen /\ objs[key].cg = crow[f][d].cg
           replacing == ops[p].reason = "deleted" /\ objs[key].ver = ghost.latest[f] /\ ghost.latest[f] \notin ghost.udel
       IN /\ Ok(d)
          /\ IF ~untouched
             THEN \* changed: nothing moved; undone at once, and the job goes.
                  /\ ops' = [ops EXCEPT ![p].st = "undone"]
                  /\ tjob' = [tjob EXCEPT ![f][d] = NoTJob]
                  /\ pc' = [ph |-> "trash", i |-> pc.i + 1]
                  /\ UNCHANGED <<objs, vol, ghost>>
             ELSE IF d \in LinkDisks
             THEN /\ vol' = [vol EXCEPT ![d][X(p)] = key]
                  /\ pc' = [pc EXCEPT !.s = "linked"]
                  /\ UNCHANGED <<objs, ops, tjob, ghost>>
             ELSE /\ vol' = [vol EXCEPT ![d][M(f)] = 0, ![d][X(p)] = key]
                  /\ ghost' = Flag(Flag(ghost, "inv10-replacement", replacing),
                                   "inv10-unchanged", objs[key].ver # crow[f][d].gver)
                  /\ pc' = [pc EXCEPT !.s = "moved"]
                  /\ UNCHANGED <<objs, ops, tjob>>
    /\ UNCHANGED <<dur, dstat, mgen, cfile, crow, cdisk, nextOp, rjob, tick, sess, look, cnt, nver>>

TrashLinkSync ==
    /\ pc.ph = "tstep" /\ pc.s = "linked"
    /\ LET p == pc.p
           d == ops[p].d
       IN /\ Ok(d)
          /\ dur' = [dur EXCEPT ![d][X(p)] = vol[d][X(p)]]
          /\ objs' = SyncObjs(objs, {vol[d][X(p)]} \ {0})
    /\ pc' = [pc EXCEPT !.s = "lsynced"]
    /\ UNCHANGED <<vol, dstat, mgen, cat, sess, look, cnt, nver, ghost>>

TrashUnlink ==
    /\ pc.ph = "tstep" /\ pc.s = "lsynced"
    /\ LET p == pc.p
           f == ops[p].f
           d == ops[p].d
           key == ops[p].key
           linked == vol[d][M(f)] = key /\ objs[key].gen = crow[f][d].gen
           replacing == ops[p].reason = "deleted" /\ objs[key].ver = ghost.latest[f] /\ ghost.latest[f] \notin ghost.udel
       IN /\ Ok(d)
          /\ IF vol[d][M(f)] = 0
             THEN \* the source's name went by itself: the link is the file's name now, the move is done
                  /\ pc' = [pc EXCEPT !.s = "moved"]
                  /\ UNCHANGED <<objs, vol, ops, tjob, ghost>>
             ELSE IF ~linked
             THEN \* an edit or a replacement since the look: the link goes, the outcome is changed, undone
                  /\ vol' = [vol EXCEPT ![d][X(p)] = 0]
                  /\ ops' = [ops EXCEPT ![p].st = "undone"]
                  /\ tjob' = [tjob EXCEPT ![f][d] = NoTJob]
                  /\ pc' = [ph |-> "trash", i |-> pc.i + 1]
                  /\ UNCHANGED <<objs, ghost>>
             ELSE /\ vol' = [vol EXCEPT ![d][M(f)] = 0]
                  /\ ghost' = Flag(Flag(ghost, "inv10-replacement", replacing),
                                   "inv10-unchanged", objs[key].ver # crow[f][d].gver)
                  /\ pc' = [pc EXCEPT !.s = "moved"]
                  /\ UNCHANGED <<objs, ops, tjob>>
    /\ UNCHANGED <<dur, dstat, mgen, cfile, crow, cdisk, nextOp, rjob, tick, sess, look, cnt, nver>>

(* §17.1 step 5: fsync of the file at its new name, then of the target folder (target-synced); the time of the
   source's folder restored, then its fsync (source-synced). *)
TrashSync ==
    /\ pc.ph = "tstep" /\ pc.s \in {"moved", "tsynced"}
    /\ LET p == pc.p
           d == ops[p].d
           n == IF pc.s = "moved" THEN X(p) ELSE M(ops[p].f)
       IN /\ Ok(d)
          /\ dur' = [dur EXCEPT ![d][n] = vol[d][n]]
          /\ objs' = SyncObjs(objs, {vol[d][n]} \ {0})
    /\ pc' = [pc EXCEPT !.s = IF pc.s = "moved" THEN "tsynced" ELSE "ssynced"]
    /\ UNCHANGED <<vol, dstat, mgen, cat, sess, look, cnt, nver, ghost>>

(* §17.1 step 6: in one transaction, the operation done and the row removed. *)
TrashCommit ==
    /\ pc.ph = "tstep" /\ pc.s = "ssynced"
    /\ LET p == pc.p
           d == ops[p].d
       IN /\ TrashFinish(d, p)
          /\ ghost' = Flag(ghost, "inv7", ~Durable(d, X(p), ops[p].key) \/ dur[d][M(ops[p].f)] = ops[p].key)
    /\ pc' = [ph |-> "trash", i |-> pc.i + 1]
    /\ UNCHANGED <<phys, cdisk, nextOp, tick, sess, look, cnt, nver>>

(* §17.1: an I/O error at any step leaves the operation planned, the disk missing, and the session ends on it. *)
TrashLost ==
    /\ pc.ph = "tstep" /\ ~Ok(ops[pc.p].d)
    /\ Lose(ops[pc.p].d)
    /\ pc' = [ph |-> "trash", i |-> pc.i + 1]
    /\ UNCHANGED <<phys, cfile, crow, ops, nextOp, rjob, tjob, tick, look, cnt, nver, ghost>>

-----------------------------------------------------------------------------
(* §13.4 The placement plan, §14.1: pending copies kept or canceled, then a replicate job and its pending copy for
   each file below its level, with a source, not leaving and not in conflict. A pending copy counts only on a disk the
   plan can write to: one on a missing disk is canceled, its row with its job, but for one whose job is running, which
   counts as an ok copy there would and is never canceled. The plan canceled those too until the remedy of flaw 2 of
   README.md. *)

Leaving(f) == \E d \in Disks : tjob[f][d].on /\ tjob[f][d].reason = "deleted"

PlanStep ==
    /\ pc.ph = "plan"
    /\ \E past \in SUBSET {d \in Disks : cdisk[d] = "missing"} :
         /\ (Grace \/ past = {})
         /\ LET counts(f, d) ==
                    \/ crow[f][d].st = "ok" /\ (cdisk[d] = "online" \/ d \notin past)
                    \/ crow[f][d].st = "pending" /\ cdisk[d] = "online" /\ ~Leaving(f)
                    \* A pending copy whose operation only a reprise can conclude counts as an ok copy there would.
                    \/ crow[f][d].st = "pending" /\ rjob[f][d].running /\ (cdisk[d] = "online" \/ d \notin past)
                \* ... and is never canceled (flaw 2 of README.md, fixed).
                cancel(f, d) == crow[f][d].st = "pending" /\ ~counts(f, d) /\ ~rjob[f][d].running
                count(f) == Cardinality({d \in Disks : counts(f, d)})
                sources(f) == {d \in Disks : crow[f][d].st = "ok" /\ crow[f][d].gone = "no" /\ cdisk[d] = "online"}
                held(f) == {Phys[e] : e \in {x \in Disks : crow[f][x].st # "none"}}
                cands(f) == {d \in Disks : cdisk[d] = "online" /\ crow[f][d].st = "none" /\ Phys[d] \notin held(f)}
                queue(f) == /\ cfile[f].on /\ ~cfile[f].conflict /\ ~Leaving(f)
                            /\ count(f) < Level /\ sources(f) # {} /\ cands(f) # {}
                target(f) == Min(cands(f))
            IN /\ crow' = [f \in Files |-> [d \in Disks |->
                      IF cancel(f, d) THEN NoRow
                      ELSE IF queue(f) /\ d = target(f) THEN [NoRow EXCEPT !.st = "pending", !.by = "atoll", !.seen = tick]
                      ELSE crow[f][d]]]
               /\ rjob' = [f \in Files |-> [d \in Disks |->
                      IF cancel(f, d) THEN NoRJob
                      ELSE IF queue(f) /\ d = target(f) THEN [NoRJob EXCEPT !.on = TRUE, !.src = Min(sources(f))]
                      ELSE rjob[f][d]]]
               /\ ghost' = Flag(ghost, "inv13", \E f \in Files : queue(f) /\
                                  \E e \in Disks : e # target(f) /\ Phys[e] = Phys[target(f)] /\ crow[f][e].st # "none")
    /\ pc' = [ph |-> "copy", i |-> 1]
    /\ UNCHANGED <<phys, cfile, cdisk, ops, nextOp, tjob, tick, sess, look, cnt, nver>>

-----------------------------------------------------------------------------
(* §14, §14.2 The operation create, step by step, with the crash points of the conformance build. *)

(* A job that ends with no copy made: its pending copy goes with it, and the file if nothing is left of it. *)
EndJob(f, d) ==
    LET crow1 == [crow EXCEPT ![f][d] = IF crow[f][d].st = "pending" THEN NoRow ELSE @]
        rjob1 == [rjob EXCEPT ![f][d] = NoRJob]
        empty == ~\E e \in Disks : crow1[f][e].st \in {"ok", "stale"}
    IN IF empty THEN Emptied(f, crow1, rjob1)
       ELSE /\ crow' = crow1 /\ rjob' = rjob1 /\ UNCHANGED cfile

CopyPick ==
    /\ pc.ph = "copy"
    /\ LET i == NextIdx(pc.i, CopyIdx)
           f == PF(i)
           d == PD(i)
           job == rjob[f][d]
           s == job.src
           r == crow[f][s]
           nexti == [ph |-> "copy", i |-> i + 1]
       IN IF i > NPairs
          THEN /\ pc' = Idle /\ sess' = NoSess /\ look' = NoLooks
               /\ UNCHANGED <<cat>>
          ELSE IF ~InSess(d) \/ ~InSess(s) THEN /\ pc' = nexti /\ UNCHANGED <<cat, sess, look>>  \* disk-unavailable
          ELSE IF ~Ok(d) THEN /\ Lose(d) /\ pc' = nexti /\ UNCHANGED <<cfile, crow, ops, nextOp, rjob, tjob, tick, look>>
          ELSE IF ~Ok(s) THEN /\ Lose(s) /\ pc' = nexti /\ UNCHANGED <<cfile, crow, ops, nextOp, rjob, tjob, tick, look>>
          ELSE IF vol[d][M(f)] # 0
          THEN \* a look at the target: something at the path ends the job as conflict, nothing journaled
               /\ EndJob(f, d) /\ pc' = nexti
               /\ UNCHANGED <<cdisk, ops, nextOp, tjob, tick, sess, look>>
          ELSE IF r.st # "ok" \/ r.gone # "no"
          THEN \* no longer a source (§14.1): the job goes with its pending copy
               /\ EndJob(f, d) /\ pc' = nexti
               /\ UNCHANGED <<cdisk, ops, nextOp, tjob, tick, sess, look>>
          ELSE IF vol[s][M(f)] # r.obj \/ objs[r.obj].gen # r.gen \/ objs[r.obj].cg # r.cg \/ nextOp > MaxOps
          THEN \* the source no longer what its row says: changed, nothing journaled, it waits settle
               /\ pc' = nexti /\ UNCHANGED <<cat, sess, look>>
          ELSE \* journaled: the operation create, planned; the job running and owning its part. What an earlier
               \* operation recorded of its rename is not this one's.
               LET q == IF job.part # 0 THEN job.part ELSE nextOp IN
               /\ ops' = [ops EXCEPT ![nextOp] = [k |-> "create", d |-> d, f |-> f, key |-> 0, st |-> "planned",
                                                  part |-> q, reason |-> "none"]]
               /\ nextOp' = nextOp + 1
               /\ rjob' = [rjob EXCEPT ![f][d].running = TRUE, ![f][d].part = q, ![f][d].pop = 0, ![f][d].pcg = 0]
               /\ pc' = [ph |-> "cstep", i |-> i, p |-> nextOp, s |-> "journaled", o |-> 0, cg |-> 0]
               /\ UNCHANGED <<cfile, crow, cdisk, tjob, tick, sess, look>>
    /\ UNCHANGED <<phys, cnt, nver, ghost>>

(* <op id>.part made with mode 0600, never replacing anything, and synced, then its folder; a part resumed is the
   one there. *)
CopyPart ==
    /\ pc.ph = "cstep" /\ pc.s = "journaled"
    /\ LET p == pc.p
           d == ops[p].d
           q == ops[p].part
           n == Len(objs) + 1
       IN /\ Ok(d)
          /\ IF vol[d][T(q)] # 0
             THEN /\ pc' = [pc EXCEPT !.s = "partmade", !.o = vol[d][T(q)]]
                  /\ UNCHANGED <<objs, vol, dur>>
             ELSE /\ objs' = Append(objs, [d |-> d, ver |-> 0, dver |-> 0, gen |-> 0, cg |-> 0, atoll |-> TRUE])
                  /\ vol' = [vol EXCEPT ![d][T(q)] = n]
                  /\ dur' = [dur EXCEPT ![d][T(q)] = n]
                  /\ pc' = [pc EXCEPT !.s = "partmade", !.o = n]
    /\ UNCHANGED <<dstat, mgen, cat, sess, look, cnt, nver, ghost>>

(* file_key written once the part is made, before anything is copied into it. *)
CopyKey ==
    /\ pc.ph = "cstep" /\ pc.s = "partmade"
    /\ ops' = [ops EXCEPT ![pc.p].key = pc.o]
    /\ pc' = [pc EXCEPT !.s = "keyed"]
    /\ UNCHANGED <<phys, cfile, crow, cdisk, nextOp, rjob, tjob, tick, sess, look, cnt, nver, ghost>>

(* The source lost while it is read: undone, the part kept for the job to resume (§14). *)
CopySourceLost ==
    /\ pc.ph = "cstep" /\ pc.s \in {"keyed", "copied"}
    /\ LET p == pc.p
           f == ops[p].f
           d == ops[p].d
           s == rjob[f][d].src
       IN /\ Ok(d) /\ ~Ok(s)
          /\ Lose(s)
          /\ ops' = [ops EXCEPT ![p].st = "undone"]
          /\ rjob' = [rjob EXCEPT ![f][d].running = FALSE]
    /\ pc' = [ph |-> "copy", i |-> pc.i + 1]
    /\ UNCHANGED <<phys, cfile, crow, nextOp, tjob, tick, look, cnt, nver, ghost>>

(* The copy, its chunk hashes recorded in job_chunks (copied). Read through the source's handle: what its row's
   file holds now. *)
CopyData ==
    /\ pc.ph = "cstep" /\ pc.s = "keyed"
    /\ LET p == pc.p
           f == ops[p].f
           d == ops[p].d
           s == rjob[f][d].src
           v == objs[crow[f][s].obj].ver
       IN /\ Ok(d) /\ Ok(s)
          /\ objs' = [objs EXCEPT ![pc.o].ver = v]
          /\ rjob' = [rjob EXCEPT ![f][d].hver = v]
    /\ pc' = [pc EXCEPT !.s = "copied"]
    /\ UNCHANGED <<vol, dur, dstat, mgen, cfile, crow, cdisk, ops, nextOp, tjob, tick, sess, look, cnt, nver, ghost>>

(* The source looked at again: changed drops the part and undoes the operation. Then times restored, fsync. *)
CopyRecheck ==
    /\ pc.ph = "cstep" /\ pc.s = "copied"
    /\ LET p == pc.p
           f == ops[p].f
           d == ops[p].d
           s == rjob[f][d].src
           r == crow[f][s]
       IN /\ Ok(d) /\ Ok(s)
          /\ IF vol[s][M(f)] = r.obj /\ objs[r.obj].gen = r.gen /\ objs[r.obj].cg = r.cg
             THEN /\ objs' = SyncObjs(objs, {pc.o})
                  /\ pc' = [pc EXCEPT !.s = "synced"]
                  /\ UNCHANGED <<vol, ops, rjob>>
             ELSE /\ vol' = [vol EXCEPT ![d][T(ops[p].part)] = 0]
                  /\ ops' = [ops EXCEPT ![p].st = "undone"]
                  /\ rjob' = [rjob EXCEPT ![f][d] = [@ EXCEPT !.running = FALSE, !.part = 0, !.hver = 0, !.whash = 0]]
                  /\ pc' = [ph |-> "copy", i |-> pc.i + 1]
                  /\ UNCHANGED objs
    /\ UNCHANGED <<dur, dstat, mgen, cfile, crow, cdisk, nextOp, tjob, tick, sess, look, cnt, nver, ghost>>

(* The re-read without cache against the chunk hashes, which gives the whole-file hash, recorded before anything is
   renamed with what the part is then, its change time among it (part_look; verified); one that is not the file's hash
   drops the copy as changed. *)
CopyVerify ==
    /\ pc.ph = "cstep" /\ pc.s = "synced"
    /\ LET p == pc.p
           f == ops[p].f
           d == ops[p].d
           h == objs[pc.o].dver
       IN /\ Ok(d)
          /\ IF h = rjob[f][d].hver /\ (cfile[f].ver = 0 \/ cfile[f].ver = h)
             THEN /\ rjob' = [rjob EXCEPT ![f][d].whash = h, ![f][d].vcg = objs[pc.o].cg]
                  /\ pc' = [pc EXCEPT !.s = "verified"]
                  /\ UNCHANGED <<vol, ops>>
             ELSE /\ vol' = [vol EXCEPT ![d][T(ops[p].part)] = 0]
                  /\ ops' = [ops EXCEPT ![p].st = "undone"]
                  /\ rjob' = [rjob EXCEPT ![f][d] = [@ EXCEPT !.running = FALSE, !.part = 0, !.hver = 0, !.whash = 0]]
                  /\ pc' = [ph |-> "copy", i |-> pc.i + 1]
    /\ UNCHANGED <<objs, dur, dstat, mgen, cfile, crow, cdisk, nextOp, tjob, tick, sess, look, cnt, nver, ghost>>

(* The target resolved, the rename without replacing anything, which moves the copy's change time: a path taken since
   the look ends as conflict, with nothing overwritten (§14). Then a look at the copy, in the same step: what it finds of
   the change time is the row's. Without RENAME_NOREPLACE: link, fsync of the file and of the target folder, unlink,
   and the look after the unlink, so that a write that keeps the size and the dates between the link and that look is
   the limit of §14.2 there. Write 1 of §6.1, or write 2 for the copy that takes the place of a stale one. *)
CopyRename ==
    /\ pc.ph = "cstep" /\ pc.s = "verified"
    /\ LET p == pc.p
           f == ops[p].f
           d == ops[p].d
           q == ops[p].part
           objs1 == [objs EXCEPT ![pc.o].cg = @ + 1]
       IN /\ Ok(d)
          /\ IF vol[d][M(f)] # 0
             THEN /\ ops' = [ops EXCEPT ![p].st = "undone"]
                  /\ EndJob(f, d)
                  /\ pc' = [ph |-> "copy", i |-> pc.i + 1]
                  /\ UNCHANGED <<vol, dur, objs>>
             ELSE IF d \in LinkDisks
             THEN /\ vol' = [vol EXCEPT ![d][M(f)] = pc.o]
                  /\ objs' = objs1
                  /\ pc' = [pc EXCEPT !.s = "linked"]
                  /\ UNCHANGED <<dur, ops, cfile, crow, rjob>>
             ELSE /\ vol' = [vol EXCEPT ![d][M(f)] = pc.o, ![d][T(q)] = 0]
                  /\ objs' = objs1
                  /\ pc' = [pc EXCEPT !.s = "moved", !.cg = objs1[pc.o].cg]
                  /\ UNCHANGED <<dur, ops, cfile, crow, rjob>>
    /\ UNCHANGED <<dstat, mgen, cdisk, nextOp, tjob, tick, sess, look, cnt, nver, ghost>>

CopyUnlink ==
    /\ pc.ph = "cstep" /\ pc.s = "linked"
    /\ LET p == pc.p
           d == ops[p].d
           f == ops[p].f
       IN /\ Ok(d)
          /\ dur' = [dur EXCEPT ![d][M(f)] = vol[d][M(f)]]
          /\ vol' = [vol EXCEPT ![d][T(ops[p].part)] = 0]
    /\ pc' = [pc EXCEPT !.s = "moved", !.cg = objs[pc.o].cg]
    /\ UNCHANGED <<objs, dstat, mgen, cat, sess, look, cnt, nver, ghost>>

(* The change time the look right after the rename found, recorded in the job in a commit of its own, with the
   operation's id (placed): a catalog write, which needs no disk (the maintainer, 2026-11-20). A crash before it, at
   moved, leaves none for the reprise. *)
CopyRecord ==
    /\ pc.ph = "cstep" /\ pc.s = "moved"
    /\ LET p == pc.p
           f == ops[p].f
           d == ops[p].d
       IN rjob' = [rjob EXCEPT ![f][d].pop = p, ![f][d].pcg = pc.cg]
    /\ pc' = [pc EXCEPT !.s = "placed"]
    /\ UNCHANGED <<phys, cfile, crow, cdisk, ops, nextOp, tjob, tick, sess, look, cnt, nver, ghost>>

(* fsync of the file at its new name, then of the target folder (target-synced); fsync of the tmp folder
   (source-synced). *)
CopySync ==
    /\ pc.ph = "cstep" /\ pc.s \in {"placed", "tsynced"}
    /\ LET p == pc.p
           d == ops[p].d
           n == IF pc.s = "placed" THEN M(ops[p].f) ELSE T(ops[p].part)
       IN /\ Ok(d)
          /\ dur' = [dur EXCEPT ![d][n] = vol[d][n]]
          /\ objs' = SyncObjs(objs, {vol[d][n]} \ {0})
    /\ pc' = [pc EXCEPT !.s = IF pc.s = "placed" THEN "tsynced" ELSE "ssynced"]
    /\ UNCHANGED <<vol, dstat, mgen, cat, sess, look, cnt, nver, ghost>>

(* The catalog commit: a replica exists only now (invariant 7). The row takes the change time of the look right after
   the rename. *)
CopyCommit ==
    /\ pc.ph = "cstep" /\ pc.s = "ssynced"
    /\ LET p == pc.p
           d == ops[p].d
       IN /\ CreateFinish(d, p, pc.cg, FALSE)
          /\ ghost' = Flag(ghost, "inv7", ~Durable(d, M(ops[p].f), pc.o) \/ objs[pc.o].dver # objs[pc.o].ver)
    /\ pc' = [ph |-> "copy", i |-> pc.i + 1]
    /\ UNCHANGED <<phys, cdisk, nextOp, tjob, sess, look, cnt, nver>>

(* The target lost at any step: the operation stays planned, for the reprise; the job stays running. *)
CopyLost ==
    /\ pc.ph = "cstep" /\ ~Ok(ops[pc.p].d)
    /\ Lose(ops[pc.p].d)
    /\ pc' = [ph |-> "copy", i |-> pc.i + 1]
    /\ UNCHANGED <<phys, cfile, crow, ops, nextOp, rjob, tjob, tick, look, cnt, nver, ghost>>

-----------------------------------------------------------------------------

Atoll ==
    \/ PassStart
    \/ RepriseOp \/ RepriseCommit \/ RepriseTidy \/ RepriseLost \/ RepriseEnd
    \/ WalkStart \/ WalkEnd \/ WalkDone
    \/ DecideFile \/ DecideSkip \/ DecideDone
    \/ ModifyFile \/ ModifySkip \/ ModifyDone
    \/ TrashPick \/ TrashMove \/ TrashLinkSync \/ TrashUnlink \/ TrashSync \/ TrashCommit \/ TrashLost
    \/ PlanStep
    \/ CopyPick \/ CopyPart \/ CopyKey \/ CopySourceLost \/ CopyData \/ CopyRecheck \/ CopyVerify
    \/ CopyRename \/ CopyUnlink \/ CopyRecord \/ CopySync \/ CopyCommit \/ CopyLost

Next == Atoll \/ World

Spec == Init /\ [][Next]_vars

(* For liveness: Atoll keeps running its passes, and a disk lost is mounted again in time. *)
FairSpec == Spec /\ WF_vars(Atoll) /\ \A d \in Disks : WF_vars(Return(d))

-----------------------------------------------------------------------------
(* Invariants, named after CLAUDE.md's non-negotiable invariants. *)

(* No user data lost: every version a user wrote, or the library held at the start, is still durably on some disk,
   in the library or a trash, missing disks included, unless a user's own action took its last copy there. *)
NoUserDataLost == \A v \in ghost.written \ ghost.udest : Holds(objs, dur, v)

(* 1. Never delete or overwrite the last verified copy of a file: the version the catalog vouches for stays on some
   disk; and Atoll never makes a name hold something else than it held (flag set by no action of the spec). *)
Inv1_LastVerifiedCopyKept ==
    /\ \A f \in Files : (cfile[f].on /\ cfile[f].ver # 0 /\ \E d \in Disks : crow[f][d].verified)
                         => (cfile[f].ver \in ghost.udest \/ Holds(objs, dur, cfile[f].ver))
    /\ "inv1" \notin ghost.flags

(* 4. EIO and an empty mount point mean "disk unknown", never "file absent": no walk is recorded from a disk that
   was not there (the confirmation after the walk, specs.md §10, §15.10). *)
Inv4_UnknownNeverAbsent == "inv4" \notin ghost.flags

(* 6, 7. A replica is the same name (by construction here) and is made by the copy pipeline in its order: no copy of
   Atoll's at a name of the library without its content durable and verified, and no commit before the names are
   durable (a trash entry durable and the copy's name gone, for a move to the trash). *)
Inv7_PipelineOrder ==
    /\ \A d \in Disks, f \in Files : LET o == vol[d][M(f)] IN
         (o # 0 /\ objs[o].atoll) => (objs[o].dver = objs[o].ver /\ objs[o].ver # 0)
    /\ "inv7" \notin ghost.flags

(* 10. A deletion propagates only to copies unchanged since they were last in sync, and only to the trash; a
   replacement is a modification, never a deletion: the latest version a user wrote, and did not delete, is never
   sent to the trash as deleted. *)
Inv10_DeletionOnlyUnchangedToTrash ==
    /\ "inv10-unchanged" \notin ghost.flags
    /\ "inv10-replacement" \notin ghost.flags

(* 11. Atoll's own operations are never taken for a user's: no copy Atoll moved is decided gone, and no copy Atoll
   made sits in the library unknown to the catalog (the next walk would take it for a user's new file) unless its
   operation is still planned. *)
Inv11_OwnOpsNotUsers ==
    /\ "inv11" \notin ghost.flags
    /\ \A d \in Disks, f \in Files : LET o == vol[d][M(f)] IN
         (o # 0 /\ objs[o].atoll) =>
            (crow[f][d].obj = o \/ \E p \in OpIds : ops[p].k = "create" /\ ops[p].st = "planned" /\ ops[p].d = d /\ ops[p].f = f)

(* 12. The journal comes first: every part in a tmp/ and every entry in a trash, visible or durable, is named by an
   operation of that disk that the catalog committed. *)
Inv12_JournalFirst ==
    \A d \in Disks, p \in OpIds :
        /\ (vol[d][T(p)] # 0 \/ dur[d][T(p)] # 0) => (ops[p].k = "create" /\ ops[p].d = d)
        /\ (vol[d][X(p)] # 0 \/ dur[d][X(p)] # 0) => (ops[p].k = "trash" /\ ops[p].d = d)

(* 13. Never two copies of a file on one physical disk: no plan queues one where a disk on the same physical disk
   holds a copy of it. *)
Inv13_OnePhysicalDisk == "inv13" \notin ghost.flags

(* A replica is a replica: a copy Atoll made, that the catalog calls ok and that nobody touched since its row was
   written, holds the version the catalog says is current (G2: a file with N replicas survives N disks). But for one a
   reprise recorded unverified, as nothing told a write that moved only its change time from the rename's (doubt): the
   limit of §14.2 that the maintainer accepted on 2026-11-20, whose write a later read finds a conflict. *)
ReplicaInSync ==
    \A f \in Files, d \in Disks : LET r == crow[f][d] IN
        (/\ cfile[f].on /\ ~cfile[f].conflict /\ cfile[f].ver # 0
         /\ r.st = "ok" /\ r.by = "atoll" /\ Up(d) /\ ~r.doubt
         /\ vol[d][M(f)] = r.obj /\ objs[r.obj].gen = r.gen /\ objs[r.obj].cg = r.cg)
        => objs[r.obj].ver = cfile[f].ver

(* A copy a reprise recorded in doubt is never vouched for: it is unverified until a read takes it again (§14.2). *)
DoubtUnverified ==
    \A f \in Files, d \in Disks : crow[f][d].doubt => ~crow[f][d].verified

TypeOK ==
    /\ dstat \in [Disks -> {"up", "gone"}]
    /\ cdisk \in [Disks -> {"online", "missing"}]
    /\ \A f \in Files, d \in Disks : crow[f][d].st \in {"none", "ok", "stale", "pending"}
    /\ \A p \in OpIds : ops[p].st \in {"none", "planned", "done", "undone"}
    /\ nextOp \in 1..(MaxOps + 1)

-----------------------------------------------------------------------------
(* Liveness, under FairSpec, with Waits = FALSE and no disk lost for good: each file reaches its level, and no
   operation stays planned. *)

Settled ==
    /\ \A f \in Files : (cfile[f].on /\ ~cfile[f].conflict) => Cardinality({d \in Disks : crow[f][d].st = "ok"}) >= Level
    /\ \A p \in OpIds : ops[p].st # "planned"

EventuallySettled == <>[]Settled

=============================================================================
