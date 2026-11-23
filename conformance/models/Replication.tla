----------------------------- MODULE Replication -----------------------------
(* Atoll's most dangerous mechanism, as a model TLC checks: copies made, deletions and modifications propagated,
   copies sent to the trash, with Atoll killed between any two steps, the power cut, a USB disk lost and mounted
   again, and a user acting on any disk at any moment (specs.md §14, §15.12, §17, §20).

   What it keeps: files, their copies on disks (by object, which stands for an inode with its creation time), the
   content each holds (a version number), its change count (what size, modification and change time say) and the count
   of what moved its change time alone (a rename, or a write that kept the size and the dates), the
   names of each disk as the kernel shows them and as they would survive a power cut, the catalog's rows (files,
   copies, jobs, the ops journal), the parts in <workdir>/tmp/, the entries of <workdir>/trash/ and the links of
   <workdir>/links/, and the mount check; and since 2026-11-22 Atoll's own view (§21.2), the SSD cache's transfer (§19)
   and the link guard (§17), each with constants that leave every action as it was without them. What it leaves out:
   names and paths (one logical path a file), renames and folders (Rename.tla's), NTFS, the bytes of the hashes, the
   guards (a decision may always wait), copies at once (Copies.tla's), repair and scrub, the CLI:
   conformance/models/README.md says more.

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
    UserActs,   \* what a user may do directly on a disk: some of "modify", "replace", "delete", "create", "rewrite"
    ViewActs,   \* what a user may do through Atoll's own view (specs.md §21.2): some of "create", "write", "delete"; {} for
                \* no view, which leaves every variable of the view as it starts
    Parallel,   \* TRUE: a new file the view makes is written in parallel to its replica's target (specs.md §21.2)
    MaxStop,    \* how many times atoll mount is stopped short (kill -9)
    MaxVOps,    \* how many operations the view may journal: ids never come back
    CacheDisks, \* the disks of role cache, the SSD cache (specs.md §19); {} for none, which leaves every action as it was
    Guard       \* the link guard, trash.link_guard (specs.md §17): "none" as before it; "on"; or "off", on at the start and
                \* turned off by the user once, at any moment

Disks == 1..NDisks
Files == 1..NFiles
OpIds == 1..MaxOps
VIds == 1..MaxVOps
NPairs == NFiles * NDisks
PF(i) == (i - 1) \div NDisks + 1     \* the file of the i-th pair (file, disk), file by file
PD(i) == ((i - 1) % NDisks) + 1      \* its disk, by position

ASSUME /\ NDisks \in Nat \ {0} /\ NFiles \in Nat \ {0} /\ Level \in 1..3
       /\ Phys \in [Disks -> Nat] /\ Start \in [Files -> SUBSET Disks]
       /\ LinkDisks \subseteq Disks
       /\ \A f \in Files : \A a, b \in Start[f] : a # b => Phys[a] # Phys[b]
       /\ Fixes = {}    \* no flaw open: each remedy is specs.md's and the code's, and the model's whatever Fixes says
       /\ UserActs \subseteq {"modify", "replace", "delete", "create", "rewrite"}
       /\ CacheDisks \subseteq Disks /\ CacheDisks \cap LinkDisks = {}
       /\ Guard \in {"none", "on", "off"} /\ (Guard # "none" => LinkDisks = {})
       /\ ViewActs \subseteq {"create", "write", "delete"}
       /\ Parallel \in BOOLEAN /\ MaxStop \in Nat /\ MaxVOps \in Nat

(* Names on a disk: a file's logical path below <root>/, a part <op-id>.part in <workdir>/tmp/,
   and an entry <date>/<op-id>/<logical path> in <workdir>/trash/ (specs.md §6, §14.2, §17). *)
M(f) == f
T(p) == NFiles + p
X(p) == NFiles + MaxOps + p
(* With the link guard, the link of each file at <workdir>/links/<file id> (specs.md §17). *)
Ln(f) == NFiles + 2 * MaxOps + f
LinkNames == IF Guard = "none" THEN {} ELSE {Ln(f) : f \in Files}
Names == 1..(NFiles + 2 * MaxOps + (IF Guard = "none" THEN 0 ELSE NFiles))
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
NoOp == [k |-> "none", d |-> 0, f |-> 0, key |-> 0, st |-> "none", part |-> 0, reason |-> "none", link |-> FALSE]
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
(* An operation of view_ops (specs.md §9, §21.2): kind (create, write, unlink or part), disk_id, lpath (the file),
   op_id (the operation of ops it runs: the trash of a deletion, the create of a part), file_key, the look its deletion
   compares the copy with (gen and cg, as no row says it), state, and taken_ns (taken). *)
NoVOp == [k |-> "none", d |-> 0, f |-> 0, p |-> 0, key |-> 0, gen |-> 0, cg |-> 0, st |-> "none", taken |-> FALSE]
(* What the view holds in memory of a part it writes in parallel (internal/view/writer, parallel.go): st none, begun
   (journaled), live (made, taking the program's writes), failed (fell behind, or a write failed), closing (the file
   closed), sealed (synced, its size the original's) or drop; p its operation create of ops, v its row of view_ops, o the
   part's object, size the original's when the program closed it. *)
NoVPart == [st |-> "none", p |-> 0, v |-> 0, o |-> 0, size |-> 0]
NoVParts == [d \in Disks |-> NoVPart]
(* The file a program holds open through the view: the object its handle writes, wherever its names go. *)
NoVFile == [open |-> FALSE, f |-> 0, d |-> 0, o |-> 0]
VDown == [ph |-> "down"]
VIdle == [ph |-> "idle"]

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
    ghost,   \* for the invariants only: versions written, destroyed or deleted by users, the latest, and flags
    vops,    \* the view's journal, view_ops: [VIds -> operation of the view] (specs.md §9, §21.2)
    nextV,   \* the next id of view_ops
    vpc,     \* where atoll mount is: down, at its reprise, idle (mounted), or in an operation; lost with it
    vsess,   \* [Disks -> Nat]: the mount each disk had when the view's own mount check opened it, plus 1; 0 for none
             \* (specs.md §21.2, "It checks each disk's mount as the daemon does"): lost with the process
    vpart,   \* [Disks -> part]: the parts of the file being written in parallel, as the view holds them: lost with it
    vfile    \* the file a program holds open through the view: lost with the view, as its handle is

phys == <<objs, vol, dur, dstat, mgen>>
cat == <<cfile, crow, cdisk, ops, nextOp, rjob, tjob, tick>>
vw == <<vops, nextV, vpc, vsess, vpart, vfile>>
vars == <<objs, vol, dur, dstat, mgen, cfile, crow, cdisk, ops, nextOp, rjob, tjob, tick, pc, sess, look, cnt, nver, ghost,
          vops, nextV, vpc, vsess, vpart, vfile>>

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
(* §21.2, The view's journal: an operation of ops that a row of view_ops names is the view's, which a command's or the
   daemon's reprise leaves alone (catalog.PlannedOps); the walk of a disk waits at the paths of the view's operations
   still planned there, but for a part, which is in tmp/ (jobs.viewPlannedOn); and what the view sent to the trash for
   a deletion is a user's, through the view, no place of Atoll's own operations (catalog.PlacesOf, ops.OwnFiles). *)
ViewNamed(p) == \E v \in VIds : vops[v].k \in {"part", "unlink"} /\ vops[v].p = p
ViewPlannedAt(d, f) == \E v \in VIds : vops[v].st = "planned" /\ vops[v].d = d /\ vops[v].f = f /\ vops[v].k # "part"
ViewTrashName(d, n) == \E v \in VIds : vops[v].k = "unlink" /\ vops[v].d = d /\ n = X(vops[v].p)
PlannedOn(d) == {p \in OpIds : ops[p].st = "planned" /\ ops[p].d = d /\ ~ViewNamed(p)}
(* What a walk waits at: an operation planned at the file's path, but for one of the link guard, which leaves the root
   as it is (jobs.plannedOn, §15.12), or one of the view's. *)
PlannedAt(d, f) == (\E p \in PlannedOn(d) : ops[p].f = f /\ ~ops[p].link) \/ ViewPlannedAt(d, f)
(* A version some name holds durably, on any disk, missing or not: in the library or in a trash. A part in tmp/ is
   no copy: a file deleted while a replica of it was being made is the user's to lose (specs.md §17, Limits). *)
Kept == {M(f) : f \in Files} \cup {X(p) : p \in OpIds} \cup LinkNames
Holds(os, ds, v) == \E d \in Disks, n \in Kept : ds[d][n] # 0 /\ os[ds[d][n]].dver = v
(* The same without the entries of the trash a transfer of the SSD cache made, which it purges at once (§19): a version
   a user's action leaves only there is the user's to lose, as one whose last copy they edit or delete directly; and
   with the cache, a version the kernel no longer shows at any other name, as once the transfer moved its copy, a name
   the source's sync is about to take away holding it durably still. *)
Surplus(n) == \E p \in OpIds : n = X(p) /\ ops[p].k = "trash" /\ ops[p].reason = "surplus"
HoldsKept(os, ds, v) == \E d \in Disks, n \in Kept : ds[d][n] # 0 /\ os[ds[d][n]].dver = v /\ ~Surplus(n)
SeesKept(os, vs, v) == \E d \in Disks, n \in Kept : vs[d][n] # 0 /\ os[vs[d][n]].ver = v /\ ~Surplus(n)
UserLost(os, ds, vs, v) == v # 0 /\ (~HoldsKept(os, ds, v) \/ (CacheDisks # {} /\ ~SeesKept(os, vs, v)))
SyncObjs(os, S) == [o \in DOMAIN os |-> IF o \in S THEN [os[o] EXCEPT !.dver = os[o].ver] ELSE os[o]]
OnDisk(os, d) == {o \in DOMAIN os : os[o].d = d}
(* A name that still holds what Atoll put there is durable: a commit comes after the syncs (invariant 7). *)
Durable(d, n, o) == vol[d][n] = o => dur[d][n] = o
Flag(g, name, cond) == IF cond THEN [g EXCEPT !.flags = @ \cup {name}] ELSE g
(* A disk this session can no longer rely on (EIO and the like): missing, and the session ends on it (specs.md §17.1). *)
Lose(d) == /\ sess' = [sess EXCEPT ![d] = 0]
           /\ cdisk' = [cdisk EXCEPT ![d] = "missing"]
(* The end of a pass, its session and looks lost with it; with the link guard, its links kept in step first (GuardPick). *)
AfterTakeIn ==
    IF Guard = "none" THEN pc' = Idle /\ sess' = NoSess /\ look' = NoLooks
    ELSE pc' = [ph |-> "guard", d |-> 1, f |-> 1, s |-> "link"] /\ UNCHANGED <<sess, look>>
NextIdx(i, S) == LET c == {j \in S : j >= i} IN IF c = {} THEN NPairs + 1 ELSE Min(c)
TrashIdx == {j \in 1..NPairs : tjob[PF(j)][PD(j)].on}
CopyIdx == {j \in 1..NPairs : rjob[PF(j)][PD(j)].on /\ ~rjob[PF(j)][PD(j)].running}
NextIn(d, S) == LET c == {e \in S : e > d} IN IF c = {} THEN NDisks + 1 ELSE Min(c)
Max(S) == CHOOSE x \in S : \A y \in S : x >= y

(* §21.2 What a path presents. The catalog alone: the original (§7), the copy ok on a disk that presents, its clock not
   running, with the oldest creation time, which the order the model makes its objects in stands for. With the view's
   operations no pass has taken in, planned or done: the latest at the path says, a file made or written presenting its
   disk, a deletion nothing (view.Resolve). 0 is nothing. *)
VOk(d) == Up(d) /\ vsess[d] = mgen[d] + 1
ViewCheck == [d \in Disks |-> IF Up(d) THEN mgen[d] + 1 ELSE 0]
OpenAt(f) == {v \in VIds : vops[v].k \in {"create", "write", "unlink"} /\ vops[v].f = f /\ vops[v].st \in {"planned", "done"}
                           /\ ~vops[v].taken}
Presenting(f) == {d \in Disks : crow[f][d].st = "ok" /\ crow[f][d].gone = "no" /\ cdisk[d] = "online"}
CatDisk(f) == IF cfile[f].on /\ Presenting(f) # {}
              THEN CHOOSE d \in Presenting(f) : \A e \in Presenting(f) : crow[f][d].obj <= crow[f][e].obj
              ELSE 0
ViewDisk(f) == IF OpenAt(f) = {} THEN CatDisk(f)
               ELSE LET v == Max(OpenAt(f)) IN IF vops[v].k = "unlink" THEN 0 ELSE vops[v].d
AllOnline == \A e \in Disks : Up(e) /\ cdisk[e] = "online"
(* What the view reads a file from (view.Tree.Open, read.policy = "oldest"): the disk it presents, or the next copy that
   holds the file at its path, in the order of §7, when the one it presents from the catalog is no longer at its name,
   as a move of Atoll's own leaves it until its commit (README.md, flaw 9, fixed: the view once read nothing then). *)
HereAt(f, e) == crow[f][e].obj # 0 /\ vol[e][M(f)] = crow[f][e].obj
ReadDisk(f) ==
    LET d == ViewDisk(f)
        next == {e \in Presenting(f) : HereAt(f, e)}
    IN IF d # 0 /\ OpenAt(f) = {} /\ cfile[f].on /\ ~HereAt(f, d) /\ next # {}
       THEN CHOOSE e \in next : \A x \in next : crow[f][e].obj <= crow[f][x].obj
       ELSE d

-----------------------------------------------------------------------------
(* The start: each file of Start at its level or below, every copy a user's, verified. *)

Init ==
    /\ objs = [o \in 1..NPairs |-> [d |-> PD(o), ver |-> PF(o), dver |-> PF(o), gen |-> 0, cg |-> 0, atoll |-> FALSE]]
    /\ vol = [d \in Disks |-> [n \in Names |-> IF n \in Files /\ d \in Start[n] THEN ObjOf(n, d) ELSE 0]]
    /\ dur = vol
    /\ dstat = [d \in Disks |-> "up"]
    /\ mgen = [d \in Disks |-> 0]
    \* A file with no copy at the start is none yet: a path the view makes a file at.
    /\ cfile = [f \in Files |-> [on |-> Start[f] # {}, ver |-> IF Start[f] # {} THEN f ELSE 0, size |-> Size(f), conflict |-> FALSE]]
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
    /\ cnt = [user |-> 0, crash |-> 0, cut |-> 0, drop |-> 0, stop |-> 0, off |-> 0]
    /\ nver = NFiles
    \* vlast: the version a user last wrote through the view; 0 for no claim, as before any or once deleted through it
    /\ ghost = [written |-> {f \in Files : Start[f] # {}}, udest |-> {}, udel |-> {}, latest |-> [f \in Files |-> f], flags |-> {},
                vlast |-> [f \in Files |-> 0]]
    /\ vops = [v \in VIds |-> NoVOp]
    /\ nextV = 1
    /\ vpc = VDown
    /\ vsess = NoSess
    /\ vpart = NoVParts
    /\ vfile = NoVFile

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
              !.udest = IF UserLost(objs2, dur2, vol1, gone) THEN @ \cup {gone} ELSE @,
              \* a change made directly on a disk is the passes' to take to the view: no claim on what it presents
              !.vlast = [@ EXCEPT ![f] = 0]]
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

(* §20: a power cut: every disk keeps only what was durable, and Atoll starts again, the daemon and atoll mount, which
   loses what it held in memory. *)
PowerCut ==
    /\ cnt.cut < MaxCut
    /\ vol' = dur
    /\ objs' = [o \in DOMAIN objs |-> [objs[o] EXCEPT !.ver = objs[o].dver]]
    /\ pc' = Idle /\ sess' = NoSess /\ look' = NoLooks
    /\ cnt' = [cnt EXCEPT !.cut = @ + 1]
    /\ vpc' = VDown /\ vsess' = NoSess /\ vpart' = NoVParts /\ vfile' = NoVFile
    /\ UNCHANGED <<dur, dstat, mgen, cat, nver, ghost, vops, nextV>>

(* The world but the power cut, none of which touches the view's journal or its process. *)
(* §17: the user turns the link guard off in the configuration (trash.link_guard = false), once, at any moment. *)
TurnOff ==
    /\ Guard = "off" /\ cnt.off = 0
    /\ cnt' = [cnt EXCEPT !.off = 1]
    /\ UNCHANGED <<phys, cat, pc, sess, look, nver, ghost>>

World ==
    \/ \E f \in Files, d \in Disks : UserModify(f, d) \/ UserRewrite(f, d) \/ UserReplace(f, d) \/ UserDelete(f, d) \/ UserCreate(f, d)
    \/ \E d \in Disks : Drop(d) \/ Return(d)
    \/ Crash
    \/ TurnOff

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
   names lead to is synced (specs.md §17.2, "First, before it looks at either name"). A look knows an object by its
   inode, its size and its modification time (gen), never by its inode alone (the maintainer, 2026-11-21): the
   trash's name alone holding the copy's object is the copy only with the size and modification time its row says, and
   with others it is "other", as a file that took the copy's name with its inode number would be; a row of no such
   object says nothing, and the inode is all there is. Both names leading to it are one file, edited in place since or
   not. *)
(* The source of a move to the trash: the logical path, or for the link guard's the link (§17.2, "A kind = trash from a
   link follows the table above with the link as its source, and nothing is ever found edited in place"). *)
Src(p) == IF ops[p].link THEN Ln(ops[p].f) ELSE M(ops[p].f)

TrashLook(d, p) ==
    LET f == ops[p].f
        key == ops[p].key
        sr == Seen(d, Src(p), key)
        row == crow[f][d]
        edited == ~ops[p].link /\ (row.obj # key \/ objs[key].gen # row.gen)     \* both names lead to it, edited since
        tg0 == Seen(d, X(p), key)
        tg == IF ~ops[p].link /\ tg0 = "same" /\ sr # "same" /\ row.obj = key /\ objs[key].gen # row.gen THEN "other" ELSE tg0
    IN [tg |-> tg, sr |-> sr, edited |-> edited]

(* What the table of §17.2 makes the two names hold, and whether the operation is done or undone. *)
TrashNames(d, p) ==
    LET s == TrashLook(d, p)
        v0 == vol[d]
    IN CASE s.tg = "same" /\ s.sr = "same" /\ s.edited -> [v0 EXCEPT ![X(p)] = 0]
         [] s.tg = "same" /\ s.sr = "same" -> [v0 EXCEPT ![Src(p)] = 0]
         [] s.tg = "other" /\ s.sr = "absent" -> [v0 EXCEPT ![Src(p)] = v0[X(p)], ![X(p)] = 0]
         [] OTHER -> v0
TrashOut(d, p) ==
    LET s == TrashLook(d, p) IN IF s.tg = "same" /\ ~(s.sr = "same" /\ s.edited) THEN "done" ELSE "undone"

(* What resolving it does on the disk: the names as the table says, each made durable. *)
TrashResolveNames(d, p) ==
    LET v1 == TrashNames(d, p)
    IN /\ vol' = [vol EXCEPT ![d] = v1]
       /\ dur' = [dur EXCEPT ![d][Src(p)] = v1[Src(p)], ![d][X(p)] = v1[X(p)]]
       /\ objs' = SyncObjs(objs, {v1[Src(p)], v1[X(p)]} \ {0})

RepTrash(d, p) ==
    /\ TrashResolveNames(d, p)
    /\ pc' = [ph |-> "repcommit", d |-> d, p |-> p, out |-> TrashOut(d, p), keep |-> FALSE]

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

(* §17.2: a purge (kind unlink), the transfer's of the SSD cache (§19): the file in the trash, by its inode: absent, its
   folder synced, it finishes; same, it is undone, and the entry stays; other, undone (ops.recoverUnlink). *)
RepUnlink(d, p) ==
    LET n == X(ops[p].part)
        s == Seen(d, n, ops[p].key)
    IN /\ dur' = IF s = "absent" THEN [dur EXCEPT ![d][n] = 0] ELSE dur
       /\ pc' = [ph |-> "repcommit", d |-> d, p |-> p, out |-> IF s = "absent" THEN "done" ELSE "undone", keep |-> FALSE]
       /\ UNCHANGED <<objs, vol>>

(* §17.2: the link guard's operations on a link: link finishes when the link is there with its inode, undone otherwise;
   unlink finishes when nothing is there, undone otherwise; the file's names take the change time they find now, as what
   it had before is gone with the crash (ops.recoverMakeLink, recoverDropLink), in the commit. What the link leads to is
   synced first, as the move's names are. *)
RepLink(d, p) ==
    LET n == Ln(ops[p].f)
        s == Seen(d, n, ops[p].key)
        fin == IF ops[p].k = "link" THEN s = "same" ELSE s = "absent"
    IN /\ dur' = [dur EXCEPT ![d][n] = vol[d][n]]
       /\ objs' = SyncObjs(objs, {vol[d][n]} \ {0})
       /\ pc' = [ph |-> "repcommit", d |-> d, p |-> p, out |-> IF fin THEN "done" ELSE "undone", keep |-> FALSE]
       /\ UNCHANGED vol

RepriseOp ==
    /\ pc.ph = "reprise" /\ InSess(pc.d) /\ Ok(pc.d) /\ PlannedOn(pc.d) # {}
    /\ LET d == pc.d
           p == Min(PlannedOn(d))
       IN CASE ops[p].k = "trash" -> RepTrash(d, p)
            [] ops[p].k \in {"link", "unlink"} /\ ops[p].link -> RepLink(d, p)
            [] ops[p].k = "unlink" -> RepUnlink(d, p)
            [] OTHER -> RepCreate(d, p)
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
                          \* the source read against the file's hash: verified, in doubt no more (§14.2)
                          ELSE IF verified /\ e = job.src THEN [crow[f][e] EXCEPT !.verified = TRUE, !.doubt = FALSE]
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
          ELSE IF ops[p].link
          THEN \* a link or its removal: done or undone, the copy's row keeping the change time it recorded, for the next
               \* look to read the change (§17.2; README.md, flaw 11, fixed: the row once took the one found now, a write
               \* that kept the dates with it)
               /\ ops' = [ops EXCEPT ![p].st = pc.out]
               /\ UNCHANGED <<cfile, crow, rjob, tjob, tick, ghost>>
          ELSE IF ops[p].k = "unlink"
          THEN /\ ops' = [ops EXCEPT ![p].st = pc.out]
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
              own |-> IF Up(d) THEN {vol[d][n] : n \in {m \in WorkNames : ~ViewTrashName(d, m)}} \ {0} ELSE {}]
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
           \* invariant 11: a copy decided gone that its disk's trash/ or tmp/ holds, visibly or durably, was Atoll's own
           \* move, but for one the view sent there for a deletion, which a user asked (§21.2)
           ownTaken == \E g \in G, n \in WorkNames : ~ViewTrashName(g, n) /\ crow[f][g].obj \in {vol[g][n], dur[g][n]}
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
                                                  part |-> 0, reason |-> job.reason, link |-> FALSE]]
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
          /\ IF vol[d][M(f)] = 0 /\ crow[f][d].obj = key /\ objs[key].gen # crow[f][d].gen
             THEN \* the source's name went by itself, but the link no longer holds what the copy's row says: edited
                  \* through the source's name before it went. The look after the move (§17.1, step 4) finds it is not
                  \* the copy, and the move is resolved at once, as a reprise would (TrashResolve).
                  /\ pc' = [pc EXCEPT !.s = "resolve"]
                  /\ UNCHANGED <<objs, vol, dur, ops, tjob, ghost>>
             ELSE IF vol[d][M(f)] = 0
             THEN \* the source's name went by itself: the link is the file's name now, the move is done
                  /\ pc' = [pc EXCEPT !.s = "moved"]
                  /\ UNCHANGED <<objs, vol, dur, ops, tjob, ghost>>
             ELSE IF ~linked
             THEN \* an edit or a replacement since the look: the link goes, the outcome is changed, undone
                  /\ vol' = [vol EXCEPT ![d][X(p)] = 0]
                  /\ ops' = [ops EXCEPT ![p].st = "undone"]
                  /\ tjob' = [tjob EXCEPT ![f][d] = NoTJob]
                  /\ pc' = [ph |-> "trash", i |-> pc.i + 1]
                  /\ UNCHANGED <<objs, dur, ghost>>
             ELSE /\ vol' = [vol EXCEPT ![d][M(f)] = 0]
                  /\ ghost' = Flag(Flag(ghost, "inv10-replacement", replacing),
                                   "inv10-unchanged", objs[key].ver # crow[f][d].gver)
                  /\ pc' = [pc EXCEPT !.s = "moved"]
                  /\ UNCHANGED <<objs, dur, ops, tjob>>
    /\ UNCHANGED <<dstat, mgen, cfile, crow, cdisk, nextOp, rjob, tick, sess, look, cnt, nver>>

(* §17.1, step 4, and §17.2: a move whose look after it found that what moved is not the copy is resolved at once, as a
   reprise would, from what both names hold then: first the names, as the table says, each made durable; then the
   commit, a step of its own, so that a crash may come between. Undone, its job goes, as when nothing moved. *)
TrashResolve ==
    /\ pc.ph = "tstep" /\ pc.s = "resolve"
    /\ LET p == pc.p
           d == ops[p].d
       IN /\ Ok(d)
          /\ TrashResolveNames(d, p)
          /\ pc' = [ph |-> "tstep", i |-> pc.i, p |-> p, s |-> "resolved", out |-> TrashOut(d, p)]
    /\ UNCHANGED <<dstat, mgen, cat, sess, look, cnt, nver, ghost>>

TrashResolved ==
    /\ pc.ph = "tstep" /\ pc.s = "resolved"
    /\ LET p == pc.p
           f == ops[p].f
           d == ops[p].d
       IN IF pc.out = "done"
          THEN /\ TrashFinish(d, p)
               /\ ghost' = Flag(ghost, "inv7", ~Durable(d, X(p), ops[p].key))
          ELSE /\ ops' = [ops EXCEPT ![p].st = "undone"]
               /\ tjob' = [tjob EXCEPT ![f][d] = NoTJob]
               /\ UNCHANGED <<cfile, crow, rjob, ghost>>
    /\ pc' = [ph |-> "trash", i |-> pc.i + 1]
    /\ UNCHANGED <<phys, cdisk, nextOp, tick, sess, look, cnt, nver>>

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

(* §19: a file with a copy on a cache disk is resident there or leaves it, which the caller tells the plan: the time of
   its residency and replication.window, which the model leaves out, so that a plan may find it either way while
   decisions may wait (Waits), and leaving otherwise. *)
OnCache(f) == \E c \in CacheDisks : crow[f][c].st \in {"ok", "stale"}

PlanStep ==
    /\ pc.ph = "plan"
    /\ \E past \in SUBSET {d \in Disks : cdisk[d] = "missing"},
          res \in (IF Waits THEN SUBSET {f \in Files : OnCache(f)} ELSE {{}}) :
         /\ (Grace \/ past = {})
         \* §13.4, §19: a copy on a cache disk counts toward no level, its file resident or leaving: a resident file
         \* gets no job (cache.mirror and cache.replicate_early left out), one that leaves its copies on data disks; and
         \* a cache disk is never a candidate.
         /\ LET counts(f, d) ==
                    /\ d \notin CacheDisks
                    /\ \/ crow[f][d].st = "ok" /\ (cdisk[d] = "online" \/ d \notin past)
                       \/ crow[f][d].st = "pending" /\ cdisk[d] = "online" /\ ~Leaving(f)
                       \* A pending copy whose operation only a reprise can conclude counts as an ok copy there would.
                       \/ crow[f][d].st = "pending" /\ rjob[f][d].running /\ (cdisk[d] = "online" \/ d \notin past)
                \* ... and is never canceled (flaw 2 of README.md, fixed).
                cancel(f, d) == crow[f][d].st = "pending" /\ ~counts(f, d) /\ ~rjob[f][d].running
                count(f) == Cardinality({d \in Disks : counts(f, d)})
                sources(f) == {d \in Disks : crow[f][d].st = "ok" /\ crow[f][d].gone = "no" /\ cdisk[d] = "online"}
                held(f) == {Phys[e] : e \in {x \in Disks : crow[f][x].st # "none"}}
                cands(f) == {d \in Disks \ CacheDisks : cdisk[d] = "online" /\ crow[f][d].st = "none" /\ Phys[d] \notin held(f)}
                queue(f) == /\ cfile[f].on /\ ~cfile[f].conflict /\ ~Leaving(f) /\ f \notin res
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
    \* With the view, its parts are adopted before any job runs (AdoptParts).
    /\ pc' = IF ViewActs = {} THEN [ph |-> "copy", i |-> 1] ELSE [ph |-> "adopt"]
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
          THEN \* the pass ends; with the SSD cache, once its transfers ran (XferPick); with the view, once its parts are
               \* settled and its operations taken in (TakeIn)
               /\ IF CacheDisks # {} THEN pc' = [ph |-> "xfer", f |-> 1] /\ UNCHANGED <<sess, look>>
                  ELSE IF ViewActs = {} THEN AfterTakeIn
                  ELSE pc' = [ph |-> "settle"] /\ UNCHANGED <<sess, look>>
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
                                                  part |-> q, reason |-> "none", link |-> FALSE]]
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
(* §19 The SSD cache's transfer (internal/jobs/transfer.go, trash.Session.Transfer): after the copies, each file that
   leaves the cache is planned again alone, from the catalog as it stands (watch.CacheTransfers): once the copies on
   data disks that count reach its level, each ok, verified since it last changed, at its path and on a disk online,
   with no job and no pending copy, its copy on a cache disk, ok, at its path and on a disk online, gets a transfer.
   It waits while an operation of the view no pass has taken in touches its path (touchedByView). Under the file's lock,
   a user copy there hands its part to a copy on a data disk first, in one transaction (catalog.HandOver); then the copy
   goes to the cache's trash as surplus, a move checked against its row (§17.1), and is purged from there at once, a
   journaled unlink (§17.4). The model takes the look at the copy and its move as one step, as for the daemon's other
   moves (README.md). *)

Data == Disks \ CacheDisks
(* The copies on data disks that count toward the file's level (§13.4), and its copies on a cache disk that may go. *)
XCounted(f) == {d \in Data : crow[f][d].st = "ok"}
XCached(f) == {c \in CacheDisks : crow[f][c].st = "ok" /\ crow[f][c].gone = "no" /\ cdisk[c] = "online"}
XDue(f) ==
    /\ cfile[f].on /\ ~cfile[f].conflict /\ ~Leaving(f)
    /\ ~\E d \in Disks : crow[f][d].st = "pending" \/ rjob[f][d].on
    /\ Cardinality(XCounted(f)) >= Level
    /\ \A d \in XCounted(f) : crow[f][d].verified /\ crow[f][d].gone = "no" /\ cdisk[d] = "online"
    /\ XCached(f) # {}
(* transferHeir: a copy ok on a data disk that can be read, a user copy first, then the first seen, then by position. *)
XHeirs(f) == {d \in Data : crow[f][d].st = "ok" /\ cdisk[d] = "online"}
XHeir(f) ==
    LET H == XHeirs(f)
        U == {d \in H : crow[f][d].by = "user"}
        S == IF U # {} THEN U ELSE H
        m == Min({crow[f][d].seen : d \in S})
    IN Min({d \in S : crow[f][d].seen = m})
(* What the transfer looks at right before its purge (trash.Session.dataCopiesHold; README.md, flaw 8, fixed): each copy
   on a data disk that the catalog calls ok, as its row says, by its inode, size and times, on a disk the session can
   read, and one at least. *)
XHeld(f) ==
    /\ XCounted(f) # {}
    /\ \A d \in XCounted(f) : Ok(d) /\ vol[d][M(f)] = crow[f][d].obj /\ crow[f][d].obj # 0
                           /\ objs[crow[f][d].obj].gen = crow[f][d].gen /\ objs[crow[f][d].obj].cg = crow[f][d].cg

XferPick ==
    /\ pc.ph = "xfer" /\ pc.f <= NFiles
    /\ LET f == pc.f
           next == [ph |-> "xfer", f |-> f + 1]
       IN IF ~XDue(f) \/ OpenAt(f) # {}
          THEN /\ pc' = next /\ UNCHANGED <<crow, sess, cdisk>>
          ELSE LET c == Min(XCached(f)) IN
               IF ~InSess(c) THEN /\ pc' = next /\ UNCHANGED <<crow, sess, cdisk>>       \* disk-unavailable
               ELSE IF ~Ok(c) THEN /\ Lose(c) /\ pc' = next /\ UNCHANGED crow
               ELSE IF crow[f][c].by = "user"
               THEN \* no heir: changed, and the copy on the cache stays
                    IF XHeirs(f) = {} THEN /\ pc' = next /\ UNCHANGED <<crow, sess, cdisk>>
                    ELSE /\ crow' = [crow EXCEPT ![f][XHeir(f)].by = "user", ![f][c].by = "atoll"]
                         /\ pc' = [ph |-> "xjournal", f |-> f, c |-> c]
                         /\ UNCHANGED <<sess, cdisk>>
               ELSE /\ pc' = [ph |-> "xjournal", f |-> f, c |-> c] /\ UNCHANGED <<crow, sess, cdisk>>
    /\ UNCHANGED <<phys, cfile, ops, nextOp, rjob, tjob, tick, look, cnt, nver, ghost>>

(* Not due yet: its residency, replication.window, or a battery (§20). *)
XferSkip ==
    /\ pc.ph = "xfer" /\ pc.f <= NFiles /\ Waits /\ XDue(pc.f)
    /\ pc' = [ph |-> "xfer", f |-> pc.f + 1]
    /\ UNCHANGED <<phys, cat, sess, look, cnt, nver, ghost>>

XferDone ==
    /\ pc.ph = "xfer" /\ pc.f > NFiles
    /\ IF ViewActs = {} THEN AfterTakeIn
       ELSE pc' = [ph |-> "settle"] /\ UNCHANGED <<sess, look>>
    /\ UNCHANGED <<phys, cat, cnt, nver, ghost>>

(* §17.1 step 1: the operation trash, surplus, planned, and its trash row, in one transaction (catalog.BeginTrash). *)
XferJournal ==
    /\ pc.ph = "xjournal"
    /\ LET f == pc.f
           c == pc.c
       \* the view's operations read again in this transaction (README.md, flaw 10, fixed)
       IN IF nextOp > MaxOps \/ OpenAt(f) # {}
          THEN /\ pc' = [ph |-> "xfer", f |-> f + 1] /\ UNCHANGED <<ops, nextOp>>     \* the model's bound
          ELSE /\ ops' = [ops EXCEPT ![nextOp] = [k |-> "trash", d |-> c, f |-> f, key |-> crow[f][c].obj, st |-> "planned",
                                                  part |-> 0, reason |-> "surplus", link |-> FALSE]]
               /\ nextOp' = nextOp + 1
               /\ pc' = [ph |-> "xstep", f |-> f, p |-> nextOp, s |-> "journaled"]
    /\ UNCHANGED <<phys, cfile, crow, cdisk, rjob, tjob, tick, sess, look, cnt, nver, ghost>>

(* §17.1 steps 2 to 4: the copy compared with its row right before the move, then the move without replacing
   anything. Invariant 2: the copy that leaves the library is an Atoll copy, and its file keeps a user copy. *)
XferMove ==
    /\ pc.ph = "xstep" /\ pc.s = "journaled"
    /\ LET p == pc.p
           f == ops[p].f
           d == ops[p].d
           key == ops[p].key
           untouched == /\ vol[d][M(f)] = key /\ key # 0 /\ crow[f][d].obj = key
                        /\ objs[key].gen = crow[f][d].gen /\ objs[key].cg = crow[f][d].cg
       IN /\ Ok(d)
          /\ IF ~untouched
             THEN \* changed: nothing moved, undone at once
                  /\ ops' = [ops EXCEPT ![p].st = "undone"]
                  /\ pc' = [ph |-> "xfer", f |-> f + 1]
                  /\ UNCHANGED <<vol, ghost>>
             ELSE /\ vol' = [vol EXCEPT ![d][M(f)] = 0, ![d][X(p)] = key]
                  /\ ghost' = Flag(ghost, "inv2", crow[f][d].by = "user" \/
                                   ~\E e \in Disks \ {d} : crow[f][e].st = "ok" /\ crow[f][e].by = "user")
                  /\ pc' = [pc EXCEPT !.s = "moved"]
                  /\ UNCHANGED ops
    /\ UNCHANGED <<objs, dur, dstat, mgen, cfile, crow, cdisk, nextOp, rjob, tjob, tick, sess, look, cnt, nver>>

(* §17.1 step 5: fsync of the file at its new name and of the trash's folder, then of the source's folder. *)
XferSync ==
    /\ pc.ph = "xstep" /\ pc.s \in {"moved", "tsynced"}
    /\ LET p == pc.p
           d == ops[p].d
           n == IF pc.s = "moved" THEN X(p) ELSE M(ops[p].f)
       IN /\ Ok(d)
          /\ dur' = [dur EXCEPT ![d][n] = vol[d][n]]
          /\ objs' = SyncObjs(objs, {vol[d][n]} \ {0})
    /\ pc' = [pc EXCEPT !.s = IF pc.s = "moved" THEN "tsynced" ELSE "ssynced"]
    /\ UNCHANGED <<vol, dstat, mgen, cat, sess, look, cnt, nver, ghost>>

(* §17.1 step 6: the operation done and the copy's row removed (catalog.FinishTrash); then its purge. *)
XferCommit ==
    /\ pc.ph = "xstep" /\ pc.s = "ssynced"
    /\ LET p == pc.p
           d == ops[p].d
       IN /\ TrashFinish(d, p)
          /\ ghost' = Flag(ghost, "inv7", ~Durable(d, X(p), ops[p].key) \/ dur[d][M(ops[p].f)] = ops[p].key)
    /\ pc' = [ph |-> "xpurge", f |-> pc.f, p |-> pc.p]
    /\ UNCHANGED <<phys, cdisk, nextOp, tick, sess, look, cnt, nver>>

(* §17.4: the purge of the entry, an operation unlink journaled (catalog.BeginFromTrash), whose file_key is the entry's. *)
XferPurgeJournal ==
    /\ pc.ph = "xpurge"
    /\ LET p == pc.p
           f == pc.f
       IN IF nextOp > MaxOps
          THEN /\ pc' = [ph |-> "xfer", f |-> f + 1] /\ UNCHANGED <<ops, nextOp>>     \* the model's bound
          ELSE /\ ops' = [ops EXCEPT ![nextOp] = [k |-> "unlink", d |-> ops[p].d, f |-> f, key |-> ops[p].key, st |-> "planned",
                                                  part |-> p, reason |-> "surplus", link |-> FALSE]]
               /\ nextOp' = nextOp + 1
               /\ pc' = [ph |-> "xstep", f |-> f, p |-> nextOp, s |-> "pjournaled"]
    /\ UNCHANGED <<phys, cfile, crow, cdisk, rjob, tjob, tick, sess, look, cnt, nver, ghost>>

(* ops.PurgeEntry: the file in the trash looked at by its inode, then unlinked: another file there stays, undone. The
   copies on data disks are looked at right before (XHeld), which the model takes in the same step as the unlink, as
   it takes the look and the act of the daemon's moves: one that is not as its row says, or on a disk that cannot be
   read, undoes the purge, the entry kept in the cache's trash (README.md, flaw 8, fixed: the purge once leaned on the
   catalog alone). *)
XferPurge ==
    /\ pc.ph = "xstep" /\ pc.s = "pjournaled"
    /\ LET q == pc.p
           d == ops[q].d
           n == X(ops[q].part)
       IN /\ Ok(d)
          /\ IF Seen(d, n, ops[q].key) = "other" \/ ~XHeld(pc.f)
             THEN /\ ops' = [ops EXCEPT ![q].st = "undone"]
                  /\ pc' = [ph |-> "xfer", f |-> pc.f + 1]
                  /\ UNCHANGED vol
             ELSE /\ vol' = [vol EXCEPT ![d][n] = 0]
                  /\ pc' = [pc EXCEPT !.s = "unlinked"]
                  /\ UNCHANGED ops
    /\ UNCHANGED <<objs, dur, dstat, mgen, cfile, crow, cdisk, nextOp, rjob, tjob, tick, sess, look, cnt, nver, ghost>>

(* The folder of the entry synced, then the commit (catalog.FinishPurge). *)
XferPurgeSync ==
    /\ pc.ph = "xstep" /\ pc.s = "unlinked"
    /\ LET q == pc.p
           d == ops[q].d
       IN /\ Ok(d)
          /\ dur' = [dur EXCEPT ![d][X(ops[q].part)] = vol[d][X(ops[q].part)]]
    /\ pc' = [pc EXCEPT !.s = "psynced"]
    /\ UNCHANGED <<objs, vol, dstat, mgen, cat, sess, look, cnt, nver, ghost>>

XferPurgeCommit ==
    /\ pc.ph = "xstep" /\ pc.s = "psynced"
    /\ ops' = [ops EXCEPT ![pc.p].st = "done"]
    /\ ghost' = Flag(ghost, "inv7", dur[ops[pc.p].d][X(ops[pc.p].part)] = ops[pc.p].key)
    /\ pc' = [ph |-> "xfer", f |-> pc.f + 1]
    /\ UNCHANGED <<phys, cfile, crow, cdisk, nextOp, rjob, tjob, tick, sess, look, cnt, nver>>

(* A disk that cannot be relied on at any step: its operation stays planned, the disk missing (§17.1). *)
XferLost ==
    /\ pc.ph = "xstep" /\ ~Ok(ops[pc.p].d)
    /\ Lose(ops[pc.p].d)
    /\ pc' = [ph |-> "xfer", f |-> pc.f + 1]
    /\ UNCHANGED <<phys, cfile, crow, ops, nextOp, rjob, tjob, tick, look, cnt, nver, ghost>>

-----------------------------------------------------------------------------
(* §17 The link guard (trash.Session.Guard, solver.PlanLinks, ops/linkguard.go): once everything else of a pass ran,
   on each disk it walked and can write to, each file's link at <workdir>/links/<file id> is decided against the
   catalog: one that leads to its file's copy stays, or with the guard off goes, but for a copy the last look did not
   find there, whose data the link may hold alone; one whose data is another copy found there, or an entry of the
   trash, goes (kind unlink); any other's data lost every name Atoll knows, as a deletion or a replacement made around
   Atoll leaves it, and goes to the trash from the link (kind trash), overwritten when its file still has a copy on that
   disk, deleted otherwise, but for data the library's name still holds, as a file a rule now ignores keeps it, whose
   link only goes. Then the copy found there without its link gets one (kind link). Each operation journaled, made, the
   links folder synced, then committed (catalog.BeginLinks, EndLinks); a link made or removed moves its file's change
   time, which the copy's row takes when it recorded the one just before (§17.1, step 6). The look before each and the
   act are one step, as for the daemon's moves (README.md); the path a link goes to the trash at is the file's, as the
   last copy-gone event of the disk gives it once the file's row went. *)

GuardOn == Guard = "on" \/ (Guard = "off" /\ cnt.off = 0)
(* The inodes of the entries of a disk's trash, as the catalog has them (catalog.TrashKeys). *)
TrashKeys(d) == {ops[p].key : p \in {q \in OpIds : ops[q].k = "trash" /\ ops[q].d = d /\ ops[q].st = "done"}}
(* The inodes of the copies of a disk that the last look found there. *)
CopyKeys(d) == {crow[g][d].obj : g \in {h \in Files : crow[h][d].st \in {"ok", "stale"} /\ crow[h][d].gone = "no"}}
(* What becomes of the link of file f on disk d (PlanLinks). *)
LinkAct(d, f) ==
    LET l == vol[d][Ln(f)]
        r == crow[f][d]
        mine == r.st \in {"ok", "stale"} /\ r.obj # 0 /\ r.obj = l
    IN CASE l = 0 -> "none"
         [] mine /\ (GuardOn \/ r.gone # "no") -> "keep"
         [] mine \/ l \in CopyKeys(d) \/ l \in TrashKeys(d) -> "unlink"
         [] OTHER -> "trash"
(* Whether the copy of file f on disk d gets a link: the guard on, found there with its inode, and no link there. *)
LinkMakes(d, f) ==
    LET r == crow[f][d] IN GuardOn /\ r.st \in {"ok", "stale"} /\ r.gone = "no" /\ r.obj # 0 /\ vol[d][Ln(f)] = 0
GuardNextFile(d, f) ==
    IF f < NFiles THEN [ph |-> "guard", d |-> d, f |-> f + 1, s |-> "link"] ELSE [ph |-> "guard", d |-> d + 1, f |-> 1, s |-> "link"]
GuardMake(d, f) == [ph |-> "guard", d |-> d, f |-> f, s |-> "make"]

GuardPick ==
    /\ pc.ph = "guard" /\ pc.d <= NDisks
    /\ LET d == pc.d
           f == pc.f
           l == vol[d][Ln(f)]
           act == LinkAct(d, f)
           nextd == [ph |-> "guard", d |-> d + 1, f |-> 1, s |-> "link"]
       IN IF ~InSess(d) \/ cdisk[d] # "online" THEN /\ pc' = nextd /\ UNCHANGED <<ops, nextOp, sess, cdisk>>
          ELSE IF ~Ok(d) THEN /\ Lose(d) /\ pc' = nextd /\ UNCHANGED <<ops, nextOp>>
          ELSE IF pc.s = "link" /\ act \in {"unlink", "trash"} /\ nextOp <= MaxOps
          THEN IF act = "unlink" \/ vol[d][M(f)] = l
               THEN /\ ops' = [ops EXCEPT ![nextOp] = [NoOp EXCEPT !.k = "unlink", !.d = d, !.f = f, !.key = l, !.st = "planned",
                                                        !.link = TRUE]]
                    /\ nextOp' = nextOp + 1
                    /\ pc' = [ph |-> "gstep", d |-> d, f |-> f, p |-> nextOp, s |-> "ujournaled", pr |-> 0]
                    /\ UNCHANGED <<sess, cdisk>>
               ELSE /\ ops' = [ops EXCEPT ![nextOp] = [NoOp EXCEPT !.k = "trash", !.d = d, !.f = f, !.key = l, !.st = "planned",
                                                        !.link = TRUE,
                                                        !.reason = IF crow[f][d].st # "none" THEN "overwritten" ELSE "deleted"]]
                    /\ nextOp' = nextOp + 1
                    /\ pc' = [ph |-> "gstep", d |-> d, f |-> f, p |-> nextOp, s |-> "tjournaled", pr |-> 0]
                    /\ UNCHANGED <<sess, cdisk>>
          ELSE IF pc.s = "link" THEN /\ pc' = GuardMake(d, f) /\ UNCHANGED <<ops, nextOp, sess, cdisk>>
          ELSE IF LinkMakes(d, f) /\ nextOp <= MaxOps
          THEN /\ ops' = [ops EXCEPT ![nextOp] = [NoOp EXCEPT !.k = "link", !.d = d, !.f = f, !.key = crow[f][d].obj, !.st = "planned",
                                                  !.link = TRUE]]
               /\ nextOp' = nextOp + 1
               /\ pc' = [ph |-> "gstep", d |-> d, f |-> f, p |-> nextOp, s |-> "mjournaled", pr |-> 0]
               /\ UNCHANGED <<sess, cdisk>>
          ELSE /\ pc' = GuardNextFile(d, f) /\ UNCHANGED <<ops, nextOp, sess, cdisk>>
    /\ UNCHANGED <<phys, cfile, crow, rjob, tjob, tick, look, cnt, nver, ghost>>

GuardEnd ==
    /\ pc.ph = "guard" /\ pc.d > NDisks
    /\ pc' = Idle /\ sess' = NoSess /\ look' = NoLooks
    /\ UNCHANGED <<phys, cat, cnt, nver, ghost>>

(* ops.DropLink: the link must lead to the operation's inode, which must have another name; one gone already is fine;
   one that leads to another file, or that is its data's last name (ErrLastName), stays, undone. *)
GuardDrop ==
    /\ pc.ph = "gstep" /\ pc.s = "ujournaled"
    /\ LET p == pc.p
           d == pc.d
           f == pc.f
           key == ops[p].key
           seen == Seen(d, Ln(f), key)
       IN /\ Ok(d)
          /\ IF seen = "absent"
             THEN /\ pc' = [pc EXCEPT !.s = "unlinked", !.pr = 0] /\ UNCHANGED <<objs, vol, ops>>
             ELSE IF seen = "other" \/ ~\E n \in Names \ {Ln(f)} : vol[d][n] = key
             THEN /\ ops' = [ops EXCEPT ![p].st = "undone"]
                  /\ pc' = GuardMake(d, f)
                  /\ UNCHANGED <<objs, vol>>
             ELSE /\ vol' = [vol EXCEPT ![d][Ln(f)] = 0]
                  /\ objs' = [objs EXCEPT ![key].cg = @ + 1]
                  /\ pc' = [pc EXCEPT !.s = "unlinked", !.pr = objs[key].cg + 1]
                  /\ UNCHANGED ops
    /\ UNCHANGED <<dur, dstat, mgen, cfile, crow, cdisk, nextOp, rjob, tjob, tick, sess, look, cnt, nver, ghost>>

(* ops.MakeLink: the copy's name must still hold the operation's inode, and the link's be free; the link made, its
   change time moved for every name. *)
GuardLink ==
    /\ pc.ph = "gstep" /\ pc.s = "mjournaled"
    /\ LET p == pc.p
           d == pc.d
           f == pc.f
           key == ops[p].key
       IN /\ Ok(d)
          /\ IF vol[d][M(f)] # key \/ vol[d][Ln(f)] # 0
             THEN /\ ops' = [ops EXCEPT ![p].st = "undone"]      \* ErrGone, ErrPathTaken: nothing made
                  /\ pc' = GuardNextFile(d, f)
                  /\ UNCHANGED <<objs, vol>>
             ELSE /\ vol' = [vol EXCEPT ![d][Ln(f)] = key]
                  /\ objs' = [objs EXCEPT ![key].cg = @ + 1]
                  /\ pc' = [pc EXCEPT !.s = "linked", !.pr = objs[key].cg + 1]
                  /\ UNCHANGED ops
    /\ UNCHANGED <<dur, dstat, mgen, cfile, crow, cdisk, nextOp, rjob, tjob, tick, sess, look, cnt, nver, ghost>>

(* The file at its new name synced (syncEntry), then the links folder (LinksFolder.Sync). *)
GuardSync ==
    /\ pc.ph = "gstep" /\ pc.s \in {"unlinked", "linked"}
    /\ LET d == pc.d
           n == Ln(pc.f)
       IN /\ Ok(d)
          /\ dur' = [dur EXCEPT ![d][n] = vol[d][n]]
          /\ objs' = SyncObjs(objs, {vol[d][n]} \ {0})
    /\ pc' = [pc EXCEPT !.s = IF pc.s = "linked" THEN "lsynced" ELSE "usynced"]
    /\ UNCHANGED <<vol, dstat, mgen, cat, sess, look, cnt, nver, ghost>>

(* catalog.EndLinks: done, and the copy's row takes the change time the link gave it when it recorded the one before:
   pr is that change time, the one before plus 1, 0 for none. *)
GuardCommit ==
    /\ pc.ph = "gstep" /\ pc.s \in {"usynced", "lsynced"}
    /\ LET p == pc.p
           d == pc.d
           f == pc.f
           key == ops[p].key
       IN /\ ops' = [ops EXCEPT ![p].st = "done"]
          /\ crow' = IF pc.pr > 0 /\ crow[f][d].st \in {"ok", "stale"} /\ crow[f][d].obj = key /\ crow[f][d].cg + 1 = pc.pr
                     THEN [crow EXCEPT ![f][d].cg = pc.pr] ELSE crow
          /\ ghost' = Flag(ghost, "inv7", IF pc.s = "lsynced" THEN ~Durable(d, Ln(f), key) ELSE dur[d][Ln(f)] = key)
          /\ pc' = IF pc.s = "lsynced" THEN GuardNextFile(d, f) ELSE GuardMake(d, f)
    /\ UNCHANGED <<phys, cfile, cdisk, nextOp, rjob, tjob, tick, sess, look, cnt, nver>>

(* ops.LinkToTrash, as ToTrash moves a copy (§17.1): the link must still lead to the operation's inode, then the move
   without replacing anything, the look after, the syncs of the target and of the links folder, and the commit
   (catalog.FinishTrash), which records nothing of a copy. *)
GuardTrash ==
    /\ pc.ph = "gstep" /\ pc.s = "tjournaled"
    /\ LET p == pc.p
           d == pc.d
           f == pc.f
           key == ops[p].key
           replacing == ops[p].reason = "deleted" /\ objs[key].ver = ghost.latest[f] /\ ghost.latest[f] \notin ghost.udel
       IN /\ Ok(d)
          /\ IF vol[d][Ln(f)] # key
             THEN \* nothing moved, resolved at once by the table: undone
                  /\ ops' = [ops EXCEPT ![p].st = "undone"]
                  /\ pc' = GuardMake(d, f)
                  /\ UNCHANGED <<objs, vol, ghost>>
             ELSE /\ vol' = [vol EXCEPT ![d][Ln(f)] = 0, ![d][X(p)] = key]
                  /\ objs' = [objs EXCEPT ![key].cg = @ + 1]
                  /\ ghost' = Flag(ghost, "inv10-replacement", replacing)
                  /\ pc' = [pc EXCEPT !.s = "tmoved"]
                  /\ UNCHANGED ops
    /\ UNCHANGED <<dur, dstat, mgen, cfile, crow, cdisk, nextOp, rjob, tjob, tick, sess, look, cnt, nver>>

GuardTrashSync ==
    /\ pc.ph = "gstep" /\ pc.s \in {"tmoved", "ttsynced"}
    /\ LET p == pc.p
           d == pc.d
           n == IF pc.s = "tmoved" THEN X(p) ELSE Ln(pc.f)
       IN /\ Ok(d)
          /\ dur' = [dur EXCEPT ![d][n] = vol[d][n]]
          /\ objs' = SyncObjs(objs, {vol[d][n]} \ {0})
    /\ pc' = [pc EXCEPT !.s = IF pc.s = "tmoved" THEN "ttsynced" ELSE "tssynced"]
    /\ UNCHANGED <<vol, dstat, mgen, cat, sess, look, cnt, nver, ghost>>

GuardTrashCommit ==
    /\ pc.ph = "gstep" /\ pc.s = "tssynced"
    /\ LET p == pc.p
           d == pc.d
       IN /\ ops' = [ops EXCEPT ![p].st = "done"]
          /\ ghost' = Flag(ghost, "inv7", ~Durable(d, X(p), ops[p].key) \/ dur[d][Ln(pc.f)] = ops[p].key)
    /\ pc' = GuardMake(pc.d, pc.f)
    /\ UNCHANGED <<phys, cfile, crow, cdisk, nextOp, rjob, tjob, tick, sess, look, cnt, nver>>

(* A disk that cannot be relied on at any step: its operation stays planned, the disk missing. *)
GuardLost ==
    /\ pc.ph = "gstep" /\ ~Ok(pc.d)
    /\ Lose(pc.d)
    /\ pc' = [ph |-> "guard", d |-> pc.d + 1, f |-> 1, s |-> "link"]
    /\ UNCHANGED <<phys, cfile, crow, ops, nextOp, rjob, tjob, tick, look, cnt, nver, ghost>>

-----------------------------------------------------------------------------
(* §21.2, Copies written in parallel and Taken in: what a pass makes of the view's operations. The pass waits at the
   paths of those still planned (PlannedAt), and a copy the view sent to the trash is gone by the user (WalkStart,
   DecideFile); these are the steps of its own. *)

(* Before any job runs (jobs.adoptParts): each part the view handed over, done and not taken in, whose operation create
   is still planned, goes to the replicate job of its file on its disk that is queued with no part and no chunk yet, in
   one transaction (catalog.AdoptViewParts): the job names the part, the view's operation is undone, the job's own
   naming the part from then on, and the view's row is taken in; one part a job, the first. The job then runs the copy
   pipeline from it as a resume does, every chunk compared with the original (CopyData). *)
Adoptable(v) ==
    LET f == vops[v].f
        d == vops[v].d
    IN /\ vops[v].k = "part" /\ vops[v].st = "done" /\ ~vops[v].taken /\ ops[vops[v].p].st = "planned"
       /\ cfile[f].on /\ rjob[f][d].on /\ ~rjob[f][d].running /\ rjob[f][d].part = 0 /\ rjob[f][d].hver = 0

AdoptParts ==
    /\ pc.ph = "adopt"
    /\ LET A == {v \in VIds : Adoptable(v) /\
                   \A u \in VIds : (Adoptable(u) /\ vops[u].f = vops[v].f /\ vops[u].d = vops[v].d) => v <= u}
       IN /\ rjob' = [f \in Files |-> [d \in Disks |->
                 IF \E v \in A : vops[v].f = f /\ vops[v].d = d
                 THEN [rjob[f][d] EXCEPT !.part = vops[CHOOSE v \in A : vops[v].f = f /\ vops[v].d = d].p]
                 ELSE rjob[f][d]]]
          /\ ops' = [p \in OpIds |-> IF \E v \in A : vops[v].p = p THEN [ops[p] EXCEPT !.st = "undone"] ELSE ops[p]]
          /\ vops' = [v \in VIds |-> IF v \in A THEN [vops[v] EXCEPT !.taken = TRUE] ELSE vops[v]]
    /\ pc' = [ph |-> "copy", i |-> 1]
    /\ UNCHANGED <<phys, cfile, crow, cdisk, nextOp, tjob, tick, sess, look, cnt, nver, ghost, nextV, vpc, vsess, vpart, vfile>>

(* Once the jobs ran (jobs.settleParts): a part the view handed over that no job took is dropped, on a disk the pass
   looked at and can write to, when its file is not in the catalog and no other operation of the view is open at its
   path, or when it is and has no pending copy on that disk whose job holds no part yet: the part removed, its
   operation undone and its row taken in (catalog.DismissViewPart). One whose file the catalog does not hold yet, while
   the view's operations at its path are open, waits. *)
Settleable(v) ==
    LET f == vops[v].f
        d == vops[v].d
    IN /\ vops[v].k = "part" /\ vops[v].st = "done" /\ ~vops[v].taken /\ InSess(d) /\ Ok(d)
       /\ IF ~cfile[f].on THEN OpenAt(f) = {}
          ELSE ~(crow[f][d].st = "pending" /\ rjob[f][d].on /\ rjob[f][d].part = 0)

SettleParts ==
    /\ pc.ph = "settle"
    /\ LET S == {v \in VIds : Settleable(v)}
           drop(d, n) == \E v \in S : vops[v].d = d /\ n = T(vops[v].p)
       IN /\ vol' = [d \in Disks |-> [n \in Names |-> IF drop(d, n) THEN 0 ELSE vol[d][n]]]
          /\ dur' = [d \in Disks |-> [n \in Names |-> IF drop(d, n) THEN 0 ELSE dur[d][n]]]
          /\ ops' = [p \in OpIds |-> IF \E v \in S : vops[v].p = p THEN [ops[p] EXCEPT !.st = "undone"] ELSE ops[p]]
          /\ vops' = [v \in VIds |-> IF v \in S THEN [vops[v] EXCEPT !.taken = TRUE] ELSE vops[v]]
    /\ pc' = [ph |-> "takein"]
    /\ UNCHANGED <<objs, dstat, mgen, cfile, crow, cdisk, nextOp, rjob, tjob, tick, sess, look, cnt, nver, ghost, nextV, vpc,
                   vsess, vpart, vfile>>

(* view.TakenIn: whether the catalog alone presents at a path what the view presents with its operations. Nothing with
   nothing; a disk the view presents with the catalog's original there, or with the copy there in sync, at the size and
   time a look this pass finds there, on a disk it looked at (view.agrees, inSyncOn). The model compares a look with a
   row by its object and gen, and two copies by the version their rows hold, as a replica keeps its source's size and
   modification time. *)
Agrees(f) ==
    LET vd == ViewDisk(f)
        cd == CatDisk(f)
        live == IF vd = 0 THEN 0 ELSE vol[vd][M(f)]
        inSync == crow[f][vd].st = "ok" /\ crow[f][vd].gone = "no" /\ crow[f][vd].obj = live /\ crow[f][vd].gen = objs[live].gen
    IN IF vd = 0 THEN cd = 0
       ELSE /\ InSess(vd) /\ Ok(vd)
            /\ IF live = 0 \/ cd = 0 THEN live = 0 /\ cd = 0
               ELSE IF cd = vd THEN inSync
               ELSE crow[f][cd].gver = objs[live].ver /\ inSync

(* §21.2 Taken in, at the end of the pass: each operation of the view that is done, in the order of their ids, once the
   catalog alone presents at its path what the view presents with all of them; one not taken in holds back the later
   ones at its path. Then the pass ends. *)
TakeIn ==
    /\ pc.ph = "takein"
    /\ LET held(f) == LET H == {v \in OpenAt(f) : vops[v].st # "done"} IN IF H = {} THEN MaxVOps + 1 ELSE Min(H)
           \* what the view holds open to write stays open until closed (README.md, flaw 10, fixed)
           T0 == {v \in VIds : vops[v].k \in {"create", "write", "unlink"} /\ vops[v].st = "done" /\ ~vops[v].taken
                               /\ v < held(vops[v].f) /\ Agrees(vops[v].f)
                               /\ ~(vfile.open /\ vfile.f = vops[v].f)}
       IN vops' = [v \in VIds |-> IF v \in T0 THEN [vops[v] EXCEPT !.taken = TRUE] ELSE vops[v]]
    /\ AfterTakeIn
    /\ UNCHANGED <<phys, cat, cnt, nver, ghost, nextV, vpc, vsess, vpart, vfile>>

-----------------------------------------------------------------------------
(* §21.2 Atoll's own view, which writes: atoll mount, a process of its own beside the daemon, one operation at a time
   (the writer's lock), each made on one disk and journaled in view_ops before it runs, with its operations of ops in
   the same transaction (invariant 12): a new file at its logical path on a data disk, its part written in parallel to
   its replica's target, a write on the copy the view presents, and a deletion that sends that copy to the trash at once,
   deleted (§17.1). Stopped short, it loses what it held in memory, and resolves at its next start what it left planned,
   before it mounts; a command's or the daemon's reprise leaves its operations alone. *)

(* A user's write through the view reaches its disk durably, with what came before on that disk, as UserAct says. *)
VUserAct(d, vol1, objs1, gone, f, newv, named) ==
    LET objs2 == SyncObjs(objs1, OnDisk(objs1, d))
        dur2 == [dur EXCEPT ![d] = vol1[d]]
    IN /\ vol' = vol1 /\ objs' = objs2 /\ dur' = dur2
       /\ nver' = newv
       /\ cnt' = [cnt EXCEPT !.user = @ + 1]
       /\ ghost' = [ghost EXCEPT !.written = @ \cup {newv}, !.latest = [@ EXCEPT ![f] = newv],
              !.udest = IF UserLost(objs2, dur2, vol1, gone) THEN @ \cup {gone} ELSE @,
              !.vlast = IF named THEN [@ EXCEPT ![f] = newv] ELSE @]

(* The latest version a user wrote of a file, which no user deleted, held by an object. *)
VLatestIn(f, o) == o # 0 /\ objs[o].ver = ghost.latest[f] /\ ghost.latest[f] \notin ghost.udel

(* §21.2, at its start: the view's own mount check opens the root of each disk it finds mounted, then it resolves what
   it left planned (VRepriseOp). *)
VStart ==
    /\ ViewActs # {} /\ vpc = VDown
    /\ vsess' = ViewCheck
    /\ vpc' = [ph |-> "reprise", v |-> 1]
    /\ UNCHANGED <<phys, cat, pc, sess, look, cnt, nver, ghost, vops, nextV, vpart, vfile>>

(* Its mount check again, every 30 s: a disk lost and mounted again is another mount, whose old handles answer EIO.
   A check that finds there the disk of an operation the view left planned resolves it, as the reprise of a start
   would, but for a part the view still writes or closes (writer.Recheck, the maintainer, 2026-11-22; README.md, flaw 5,
   fixed). Its disk is lost and there again on a mount of its own, as the maintainer asked on 2026-11-23: an operation
   stays planned only where VOk failed, its disk gone or on another mount (VDelLost), and is Up again only on another
   mount (Return); a mount check answered from a page cache, and the confirmation past it, are not modelled. *)
VHeld(v) == \E e \in Disks : vpart[e].st # "none" /\ vpart[e].v = v
VResolvable == {v \in VIds : vops[v].st = "planned" /\ ~VHeld(v) /\ Up(vops[v].d)}
VCheck ==
    /\ vpc = VIdle /\ (vsess # ViewCheck \/ VResolvable # {})
    /\ vsess' = ViewCheck
    /\ vpc' = IF VResolvable # {} THEN [ph |-> "reprise", v |-> 1] ELSE vpc
    /\ UNCHANGED <<phys, cat, pc, sess, look, cnt, nver, ghost, vops, nextV, vpart, vfile>>

(* atoll mount stopped short, as by kill -9: its parts and open files go with it, its journal stays. *)
VStop ==
    /\ cnt.stop < MaxStop /\ vpc # VDown
    /\ vpc' = VDown /\ vsess' = NoSess /\ vpart' = NoVParts /\ vfile' = NoVFile
    /\ cnt' = [cnt EXCEPT !.stop = @ + 1]
    /\ UNCHANGED <<phys, cat, pc, sess, look, nver, ghost, vops, nextV>>

(* §17.2's table for a deletion of the view (ops.ResolveViewTrash; README.md, flaws 3 and 4, fixed), compared with a
   row: hasRow and rgen, what it says of the copy's size and modification time (gen). At once after a failed move, the
   look right before it; at a reprise, the copy's row in the catalog, when one records that object. The trash's name
   alone holding the copy's object is the copy only with that gen, and with no row its inode is all there is; both names
   leading to it are one file, edited since or not, as no row can say otherwise. *)
VTrashLook(d, v, hasRow, rgen) ==
    LET o == vops[v]
        sr == Seen(d, M(o.f), o.key)
        edited == ~hasRow \/ objs[o.key].gen # rgen
        tg0 == Seen(d, X(o.p), o.key)
        tg == IF tg0 = "same" /\ sr # "same" /\ hasRow /\ edited THEN "other" ELSE tg0
    IN [tg |-> tg, sr |-> sr, edited |-> edited]
VTrashOut(d, v, hasRow, rgen) ==
    LET s == VTrashLook(d, v, hasRow, rgen) IN IF s.tg = "same" /\ ~(s.sr = "same" /\ s.edited) THEN "done" ELSE "undone"
(* What the catalog says of the copy a deletion of the view moves, at a reprise. *)
VRowHas(v) == LET o == vops[v] IN crow[o.f][o.d].st \in {"ok", "stale"} /\ crow[o.f][o.d].obj = o.key
VRowGen(v) == crow[vops[v].f][vops[v].d].gen
VTrashResolveNames(d, v, hasRow, rgen) ==
    LET f == vops[v].f
        p == vops[v].p
        s == VTrashLook(d, v, hasRow, rgen)
        v0 == vol[d]
        v1 == CASE s.tg = "same" /\ s.sr = "same" /\ s.edited -> [v0 EXCEPT ![X(p)] = 0]
                [] s.tg = "same" /\ s.sr = "same" -> [v0 EXCEPT ![M(f)] = 0]
                [] s.tg = "other" /\ s.sr = "absent" -> [v0 EXCEPT ![M(f)] = v0[X(p)], ![X(p)] = 0]
                [] OTHER -> v0
    IN /\ vol' = [vol EXCEPT ![d] = v1]
       /\ dur' = [dur EXCEPT ![d][M(f)] = v1[M(f)], ![d][X(p)] = v1[X(p)]]
       /\ objs' = SyncObjs(objs, {v1[M(f)], v1[X(p)]} \ {0})

(* §21.2, The view's journal: what the view left planned, in the order of the ids, on each disk it holds a handle on;
   one on another disk stays planned, for its next start (writer.Reprise). A file made is done when something is at its
   name, and undone otherwise. A part being written is no copy of a file closed: its part goes, and it is undone. A
   deletion is resolved by the table of §17.2, in two steps, the names then the commit (writer.resolveTrash; README.md,
   flaw 3, fixed: the code once called it done on the inode of the trash's name alone). A part the view still holds in
   memory, which only a check while it runs can meet (VCheck), is its writer's. *)
VRepriseOp ==
    /\ vpc.ph = "reprise" /\ vpc.v <= MaxVOps
    /\ LET v == vpc.v
           o == vops[v]
           d == o.d
           f == o.f
           next == [ph |-> "reprise", v |-> v + 1]
       IN IF o.st # "planned" \/ ~VOk(d) \/ VHeld(v)
          THEN /\ vpc' = next /\ UNCHANGED <<objs, vol, dur, ops, vops, ghost>>
          ELSE CASE o.k = "create" ->
                      /\ vops' = [vops EXCEPT ![v].st = IF vol[d][M(f)] # 0 THEN "done" ELSE "undone"]
                      /\ vpc' = next /\ UNCHANGED <<objs, vol, dur, ops, ghost>>
                 [] o.k = "part" ->
                      /\ vol' = [vol EXCEPT ![d][T(o.p)] = 0]
                      /\ dur' = [dur EXCEPT ![d][T(o.p)] = 0]
                      /\ ops' = [ops EXCEPT ![o.p].st = "undone"]
                      /\ vops' = [vops EXCEPT ![v].st = "undone"]
                      /\ vpc' = next /\ UNCHANGED <<objs, ghost>>
                 [] o.k = "unlink" ->
                      /\ VTrashResolveNames(d, v, VRowHas(v), VRowGen(v))
                      /\ vpc' = [ph |-> "vrepcommit", v |-> v, out |-> VTrashOut(d, v, VRowHas(v), VRowGen(v))]
                      /\ UNCHANGED <<ops, vops, ghost>>
                 [] OTHER -> /\ vpc' = next /\ UNCHANGED <<objs, vol, dur, ops, vops, ghost>>
    /\ UNCHANGED <<dstat, mgen, cfile, crow, cdisk, nextOp, rjob, tjob, tick, pc, sess, look, cnt, nver, nextV, vsess, vpart, vfile>>

VRepriseCommit ==
    /\ vpc.ph = "vrepcommit"
    /\ LET v == vpc.v
           o == vops[v]
       IN /\ ops' = [ops EXCEPT ![o.p].st = vpc.out]
          /\ vops' = [vops EXCEPT ![v].st = vpc.out]
          /\ ghost' = Flag(ghost, "inv7", vpc.out = "done" /\
                                          (~Durable(o.d, X(o.p), o.key) \/ o.key \in {vol[o.d][M(o.f)], dur[o.d][M(o.f)]}))
    /\ vpc' = [ph |-> "reprise", v |-> vpc.v + 1]
    /\ UNCHANGED <<phys, cfile, crow, cdisk, nextOp, rjob, tjob, tick, pc, sess, look, cnt, nver, nextV, vsess, vpart, vfile>>

VRepriseEnd ==
    /\ vpc.ph = "reprise" /\ vpc.v > MaxVOps
    /\ vpc' = VIdle
    /\ UNCHANGED <<phys, cat, pc, sess, look, cnt, nver, ghost, vops, nextV, vsess, vpart, vfile>>

(* §21.2 What is new: a file made at its logical path on a data disk the view can write to, where it presents
   nothing; journaled, then made, as a user's file, then done (writer.Create). The model takes make and done as one
   step, the reprise finding the file made done either way. A name taken there is EEXIST, undone; a disk that cannot
   be relied on leaves it planned. With Parallel, and no file of the catalog at its path, its part comes next. *)
PartTargets(f, a) ==
    LET c == {e \in Disks : e # a /\ Phys[e] # Phys[a] /\ cdisk[e] = "online" /\ VOk(e) /\ vpart[e].st = "none"}
    IN IF Level < 2 \/ c = {} THEN {} ELSE {Min(c)}

VCreate(f) ==
    /\ "create" \in ViewActs /\ vpc = VIdle /\ ~vfile.open /\ cnt.user < MaxUser /\ nextV <= MaxVOps
    /\ ViewDisk(f) = 0
    \* §19 Landing: with the SSD cache, a new file goes to a cache disk (one with room, which the model leaves out)
    /\ \E d \in (IF CacheDisks = {} THEN Disks ELSE CacheDisks) :
         /\ cdisk[d] = "online" /\ VOk(d)
         /\ vops' = [vops EXCEPT ![nextV] = [NoVOp EXCEPT !.k = "create", !.d = d, !.f = f, !.st = "planned"]]
    /\ nextV' = nextV + 1
    /\ vpc' = [ph |-> "create", v |-> nextV]
    /\ UNCHANGED <<phys, cat, pc, sess, look, cnt, nver, ghost, vsess, vpart, vfile>>

VCreateMake ==
    /\ vpc.ph = "create"
    /\ LET v == vpc.v
           d == vops[v].d
           f == vops[v].f
           n == Len(objs) + 1
           e == nver + 1
       IN IF ~VOk(d)
          THEN /\ vpc' = VIdle /\ UNCHANGED <<objs, vol, dur, nver, cnt, ghost, vops, vfile>>
          ELSE IF vol[d][M(f)] # 0
          THEN /\ vops' = [vops EXCEPT ![v].st = "undone"]
               /\ vpc' = VIdle /\ UNCHANGED <<objs, vol, dur, nver, cnt, ghost, vfile>>
          ELSE /\ VUserAct(d, [vol EXCEPT ![d][M(f)] = n],
                           Append(objs, [d |-> d, ver |-> e, dver |-> e, gen |-> 0, cg |-> 0, atoll |-> FALSE]), 0, f, e, TRUE)
               /\ vops' = [vops EXCEPT ![v].st = "done"]
               /\ vfile' = [open |-> TRUE, f |-> f, d |-> d, o |-> n]
               \* a file that lands on the SSD cache is written there alone, never in parallel (§19)
               /\ vpc' = IF Parallel /\ ~cfile[f].on /\ d \notin CacheDisks THEN [ph |-> "parts", f |-> f, todo |-> PartTargets(f, d)]
                         ELSE VIdle
    /\ UNCHANGED <<dstat, mgen, cat, pc, sess, look, nextV, vsess, vpart>>

(* §21.2 Each part journaled before it is made: an operation create of ops on the target, planned, its part
   <op id>.part, and the view's row kind part that names it, in one transaction (catalog.BeginViewPart); then made as a
   copy's part is, empty as the new file is, synced, and its inode recorded (ops.MakePart, catalog.SetCreateKey), which
   the model takes as one step. A disk that cannot be relied on leaves it planned, for the view's reprise; with no ids
   left, the replica is the ordinary job's. *)
VPartBegin ==
    /\ vpc.ph = "parts" /\ vpc.todo # {}
    /\ LET e == Min(vpc.todo)
           f == vpc.f
       IN /\ vpart[e].st = "none"
          /\ IF nextOp > MaxOps \/ nextV > MaxVOps \/ ~VOk(e)
             THEN /\ vpc' = [vpc EXCEPT !.todo = @ \ {e}] /\ UNCHANGED <<ops, nextOp, vops, nextV, vpart>>
             ELSE /\ ops' = [ops EXCEPT ![nextOp] = [k |-> "create", d |-> e, f |-> f, key |-> 0, st |-> "planned", part |-> nextOp,
                                                      reason |-> "none", link |-> FALSE]]
                  /\ nextOp' = nextOp + 1
                  /\ vops' = [vops EXCEPT ![nextV] = [NoVOp EXCEPT !.k = "part", !.d = e, !.f = f, !.p = nextOp, !.st = "planned"]]
                  /\ nextV' = nextV + 1
                  /\ vpart' = [vpart EXCEPT ![e] = [NoVPart EXCEPT !.st = "begun", !.p = nextOp, !.v = nextV]]
                  /\ UNCHANGED vpc
    /\ UNCHANGED <<phys, cfile, crow, cdisk, rjob, tjob, tick, pc, sess, look, cnt, nver, ghost, vsess, vfile>>

VPartMake ==
    /\ vpc.ph = "parts" /\ vpc.todo # {}
    /\ LET e == Min(vpc.todo)
           pt == vpart[e]
           n == Len(objs) + 1
       IN /\ pt.st = "begun"
          /\ IF VOk(e)
             THEN /\ objs' = Append(objs, [d |-> e, ver |-> objs[vfile.o].ver, dver |-> objs[vfile.o].ver, gen |-> 0, cg |-> 0,
                                           atoll |-> TRUE])
                  /\ vol' = [vol EXCEPT ![e][T(pt.p)] = n]
                  /\ dur' = [dur EXCEPT ![e][T(pt.p)] = n]
                  /\ ops' = [ops EXCEPT ![pt.p].key = n]
                  /\ vpart' = [vpart EXCEPT ![e] = [pt EXCEPT !.st = "live", !.o = n]]
             ELSE /\ vpart' = [vpart EXCEPT ![e] = NoVPart]
                  /\ UNCHANGED <<objs, vol, dur, ops>>
          /\ vpc' = [vpc EXCEPT !.todo = @ \ {e}]
    /\ UNCHANGED <<dstat, mgen, cfile, crow, cdisk, nextOp, rjob, tjob, tick, pc, sess, look, cnt, nver, ghost, vops, nextV, vsess, vfile>>

VPartsDone ==
    /\ vpc.ph = "parts" /\ vpc.todo = {}
    /\ vpc' = VIdle
    /\ UNCHANGED <<phys, cat, pc, sess, look, cnt, nver, ghost, vops, nextV, vsess, vpart, vfile>>

(* The program writes into the file it holds open through the view: into its object, wherever its names are, an edit
   in place, durably as a user acts; each live part takes the write too, or fails, as one that falls behind, whose write
   fails, or that would fill its disk past min_free (parallel.mirror). A write that fails on the original drops every
   part. *)
VData ==
    /\ vfile.open /\ vpc = VIdle /\ cnt.user < MaxUser
    /\ LET d == vfile.d
           f == vfile.f
           o == vfile.o
           v == nver + 1
           live == {e \in Disks : vpart[e].st = "live"}
       IN IF ~VOk(d)
          THEN /\ vpart' = [e \in Disks |-> IF vpart[e].st \in {"live", "failed"} THEN [vpart[e] EXCEPT !.st = "drop"] ELSE vpart[e]]
               /\ UNCHANGED <<objs, vol, dur, nver, cnt, ghost>>
          ELSE \E F \in SUBSET live :
                 /\ \A e \in live : ~VOk(e) => e \in F
                 /\ LET objs1 == [x \in DOMAIN objs |->
                            IF x = o THEN [objs[x] EXCEPT !.ver = v, !.dver = v, !.gen = @ + 1]
                            ELSE IF \E e \in live \ F : vpart[e].o = x THEN [objs[x] EXCEPT !.ver = v]
                            ELSE objs[x]]
                    IN VUserAct(d, vol, objs1, objs[o].ver, f, v, vol[d][M(f)] = o)
                 /\ vpart' = [e \in Disks |-> IF e \in F THEN [vpart[e] EXCEPT !.st = "failed"] ELSE vpart[e]]
    /\ UNCHANGED <<dstat, mgen, cat, pc, sess, look, vops, nextV, vpc, vsess, vfile>>

(* The program closes the file (parallel.release): each live part is to be sealed, against the original's size then,
   any other dropped; with the original's disk lost, its size cannot be told, and every part is dropped. *)
VClose ==
    /\ vfile.open /\ vpc = VIdle
    /\ vpart' = [e \in Disks |->
           CASE vpart[e].st = "live" /\ VOk(vfile.d) -> [vpart[e] EXCEPT !.st = "closing", !.size = Size(objs[vfile.o].ver)]
             [] vpart[e].st \in {"live", "failed"} -> [vpart[e] EXCEPT !.st = "drop"]
             [] OTHER -> vpart[e]]
    /\ vfile' = NoVFile
    /\ UNCHANGED <<phys, cat, pc, sess, look, cnt, nver, ghost, vops, nextV, vpc, vsess>>

(* What closing a file's parts does apart from the program (parallel.finish, seal, dropPart): a part that holds the
   original's size is synced, then its row is done, handed over to the passes, its operation create left planned so
   that tmp/ keeps it; one dropped is removed and its operations undone, or, on a disk that cannot be relied on, left
   planned for the view's reprise. *)
VPartSeal(e) ==
    /\ vpc # VDown /\ vpart[e].st = "closing"
    /\ LET pt == vpart[e] IN
       IF VOk(e) /\ Size(objs[pt.o].ver) = pt.size
       THEN /\ objs' = SyncObjs(objs, {pt.o})
            /\ vpart' = [vpart EXCEPT ![e].st = "sealed"]
       ELSE /\ vpart' = [vpart EXCEPT ![e].st = "drop"]
            /\ UNCHANGED objs
    /\ UNCHANGED <<vol, dur, dstat, mgen, cat, pc, sess, look, cnt, nver, ghost, vops, nextV, vpc, vsess, vfile>>

VPartWritten(e) ==
    /\ vpc # VDown /\ vpart[e].st = "sealed"
    /\ vops' = [vops EXCEPT ![vpart[e].v].st = "done"]
    /\ vpart' = [vpart EXCEPT ![e] = NoVPart]
    /\ UNCHANGED <<phys, cat, pc, sess, look, cnt, nver, ghost, nextV, vpc, vsess, vfile>>

VPartDrop(e) ==
    /\ vpc # VDown /\ vpart[e].st = "drop"
    /\ LET pt == vpart[e] IN
       IF VOk(e)
       THEN /\ vol' = [vol EXCEPT ![e][T(pt.p)] = 0]
            /\ dur' = [dur EXCEPT ![e][T(pt.p)] = 0]
            /\ ops' = [ops EXCEPT ![pt.p].st = "undone"]
            /\ vops' = [vops EXCEPT ![pt.v].st = "undone"]
       ELSE UNCHANGED <<vol, dur, ops, vops>>
    /\ vpart' = [vpart EXCEPT ![e] = NoVPart]
    /\ UNCHANGED <<objs, dstat, mgen, cfile, crow, cdisk, nextOp, rjob, tjob, tick, pc, sess, look, cnt, nver, ghost, nextV, vpc,
                   vsess, vfile>>

(* §21.2 A file that exists, opened to be written through the view, on the copy it presents alone, and its write
   journaled once, done at once, unless an operation of the view open at that path already presents it from there
   (writer.Open, journalWrite). *)
VWriteOpen(f) ==
    /\ "write" \in ViewActs /\ vpc = VIdle /\ ~vfile.open /\ cnt.user < MaxUser
    /\ LET d == ViewDisk(f)
           latest == IF OpenAt(f) = {} THEN 0 ELSE Max(OpenAt(f))
           again == latest # 0 /\ vops[latest].k \in {"create", "write"} /\ vops[latest].d = d
       IN /\ d # 0 /\ VOk(d) /\ vol[d][M(f)] # 0
          /\ again \/ nextV <= MaxVOps
          \* it waits while a move of Atoll's own of that copy is planned (README.md, flaw 10, fixed)
          /\ ~(\E p \in OpIds : ops[p].k = "trash" /\ ops[p].st = "planned" /\ ops[p].d = d
                                                          /\ ops[p].f = f /\ ~ViewNamed(p))
          /\ vops' = IF again THEN vops ELSE [vops EXCEPT ![nextV] = [NoVOp EXCEPT !.k = "write", !.d = d, !.f = f, !.st = "done"]]
          /\ nextV' = IF again THEN nextV ELSE nextV + 1
          /\ vfile' = [open |-> TRUE, f |-> f, d |-> d, o |-> vol[d][M(f)]]
    /\ UNCHANGED <<phys, cat, pc, sess, look, cnt, nver, ghost, vpc, vsess, vpart>>

(* §21.2 A deletion: the parts of the file being written at that path dropped (abandonAt), the copy the view presents
   looked at, then its operation trash of ops, deleted, and the view's row unlink that names it, in one transaction
   (catalog.BeginViewTrash), then the steps of §17.1 (ops.ToTrash): the look right before the move, which the model
   takes apart from the move so that a user may act between them, the move without replacing anything, or link, its
   syncs, a look at the source and unlink, then the look after the move, the syncs and the commit. *)
VDelete(f) ==
    /\ "delete" \in ViewActs /\ vpc = VIdle /\ cnt.user < MaxUser /\ nextOp <= MaxOps /\ nextV <= MaxVOps
    /\ LET d == ViewDisk(f)
           key == IF d = 0 THEN 0 ELSE vol[d][M(f)]
       IN /\ d # 0 /\ VOk(d) /\ key # 0
          /\ ops' = [ops EXCEPT ![nextOp] = [k |-> "trash", d |-> d, f |-> f, key |-> key, st |-> "planned", part |-> 0,
                                              reason |-> "deleted", link |-> FALSE]]
          /\ vops' = [vops EXCEPT ![nextV] = [NoVOp EXCEPT !.k = "unlink", !.d = d, !.f = f, !.p = nextOp, !.key = key,
                                                !.gen = objs[key].gen, !.cg = objs[key].cg, !.st = "planned"]]
          /\ vpart' = IF vfile.open /\ vfile.f = f
                      THEN [e \in Disks |-> IF vpart[e].st \in {"live", "failed"} THEN [vpart[e] EXCEPT !.st = "drop"] ELSE vpart[e]]
                      ELSE vpart
          /\ cnt' = [cnt EXCEPT !.user = @ + 1]
          /\ ghost' = [ghost EXCEPT !.udel = @ \cup {objs[key].ver}, !.vlast = [@ EXCEPT ![f] = 0]]
    /\ nextOp' = nextOp + 1
    /\ nextV' = nextV + 1
    /\ vpc' = [ph |-> "del", v |-> nextV, s |-> "journaled"]
    /\ UNCHANGED <<phys, cfile, crow, cdisk, rjob, tjob, tick, pc, sess, look, nver, vsess, vfile>>

VDelLook ==
    /\ vpc.ph = "del" /\ vpc.s = "journaled"
    /\ LET o == vops[vpc.v] IN
       /\ VOk(o.d)
       /\ IF vol[o.d][M(o.f)] = o.key /\ objs[o.key].gen = o.gen /\ objs[o.key].cg = o.cg
          THEN /\ vpc' = [vpc EXCEPT !.s = "looked"] /\ UNCHANGED <<ops, vops>>
          ELSE \* changed: nothing moves, undone (§17.1, step 4)
               /\ ops' = [ops EXCEPT ![o.p].st = "undone"]
               /\ vops' = [vops EXCEPT ![vpc.v].st = "undone"]
               /\ vpc' = VIdle
    /\ UNCHANGED <<phys, cfile, crow, cdisk, nextOp, rjob, tjob, tick, pc, sess, look, cnt, nver, ghost, nextV, vsess, vpart, vfile>>

VDelMove ==
    /\ vpc.ph = "del" /\ vpc.s = "looked"
    /\ LET o == vops[vpc.v]
           d == o.d
           src == vol[d][M(o.f)]
       IN /\ VOk(d)
          /\ IF src = 0
             THEN /\ ops' = [ops EXCEPT ![o.p].st = "undone"]
                  /\ vops' = [vops EXCEPT ![vpc.v].st = "undone"]
                  /\ vpc' = VIdle
                  /\ UNCHANGED vol
             ELSE IF d \in LinkDisks
             THEN /\ vol' = [vol EXCEPT ![d][X(o.p)] = src]
                  /\ vpc' = [vpc EXCEPT !.s = "linked"]
                  /\ UNCHANGED <<ops, vops>>
             ELSE /\ vol' = [vol EXCEPT ![d][M(o.f)] = 0, ![d][X(o.p)] = src]
                  /\ vpc' = [vpc EXCEPT !.s = "after"]
                  /\ UNCHANGED <<ops, vops>>
    /\ UNCHANGED <<objs, dur, dstat, mgen, cfile, crow, cdisk, nextOp, rjob, tjob, tick, pc, sess, look, cnt, nver, ghost, nextV,
                   vsess, vpart, vfile>>

VDelLinkSync ==
    /\ vpc.ph = "del" /\ vpc.s = "linked"
    /\ LET o == vops[vpc.v] IN
       /\ VOk(o.d)
       /\ dur' = [dur EXCEPT ![o.d][X(o.p)] = vol[o.d][X(o.p)]]
       /\ objs' = SyncObjs(objs, {vol[o.d][X(o.p)]} \ {0})
    /\ vpc' = [vpc EXCEPT !.s = "lsynced"]
    /\ UNCHANGED <<vol, dstat, mgen, cat, pc, sess, look, cnt, nver, ghost, vops, nextV, vsess, vpart, vfile>>

(* The look at the source's name before the unlink: inode, size and modification time, as the link moved the change
   time. Gone by itself, the link is the file's name now; another file or an edit, the link goes, undone. *)
VDelUnlink ==
    /\ vpc.ph = "del" /\ vpc.s = "lsynced"
    /\ LET o == vops[vpc.v]
           d == o.d
           src == vol[d][M(o.f)]
       IN /\ VOk(d)
          /\ IF src = 0
             THEN /\ vpc' = [vpc EXCEPT !.s = "after"] /\ UNCHANGED <<vol, ops, vops>>
             ELSE IF src # o.key \/ objs[o.key].gen # o.gen
             THEN /\ vol' = [vol EXCEPT ![d][X(o.p)] = 0]
                  /\ ops' = [ops EXCEPT ![o.p].st = "undone"]
                  /\ vops' = [vops EXCEPT ![vpc.v].st = "undone"]
                  /\ vpc' = VIdle
             ELSE /\ vol' = [vol EXCEPT ![d][M(o.f)] = 0]
                  /\ vpc' = [vpc EXCEPT !.s = "after"]
                  /\ UNCHANGED <<ops, vops>>
    /\ UNCHANGED <<objs, dur, dstat, mgen, cfile, crow, cdisk, nextOp, rjob, tjob, tick, pc, sess, look, cnt, nver, ghost, nextV,
                   vsess, vpart, vfile>>

(* §17.1, step 4: afterwards the trash's name must hold the copy, by its inode, size and modification time. Otherwise
   the move is resolved at once by the table of §17.2, as the daemon resolves one (writer.afterFailedTrash; README.md, flaw
   4, fixed: the code once undid it as it was). *)
VDelAfter ==
    /\ vpc.ph = "del" /\ vpc.s = "after"
    /\ LET v == vpc.v
           o == vops[v]
           d == o.d
       IN /\ VOk(d)
          /\ IF vol[d][X(o.p)] = o.key /\ objs[o.key].gen = o.gen
             THEN /\ vpc' = [vpc EXCEPT !.s = "moved"] /\ UNCHANGED <<objs, vol, dur, ops, vops, ghost>>
             ELSE /\ VTrashResolveNames(d, v, TRUE, o.gen)
                  /\ vpc' = [ph |-> "del", v |-> v, s |-> "resolved", out |-> VTrashOut(d, v, TRUE, o.gen)]
                  /\ UNCHANGED <<ops, vops, ghost>>
    /\ UNCHANGED <<dstat, mgen, cfile, crow, cdisk, nextOp, rjob, tjob, tick, pc, sess, look, cnt, nver, nextV, vsess, vpart, vfile>>

VDelResolved ==
    /\ vpc.ph = "del" /\ vpc.s = "resolved"
    /\ LET o == vops[vpc.v] IN
       /\ ops' = [ops EXCEPT ![o.p].st = vpc.out]
       /\ vops' = [vops EXCEPT ![vpc.v].st = vpc.out]
       /\ ghost' = Flag(Flag(ghost, "inv7", vpc.out = "done" /\ ~Durable(o.d, X(o.p), o.key)),
                        "inv10-replacement", VLatestIn(o.f, vol[o.d][X(o.p)]))
    /\ vpc' = VIdle
    /\ UNCHANGED <<phys, cfile, crow, cdisk, nextOp, rjob, tjob, tick, pc, sess, look, cnt, nver, nextV, vsess, vpart, vfile>>

(* §17.1 step 5: fsync of the file at its new name and of the trash's folder, then of the source's folder. *)
VDelSync ==
    /\ vpc.ph = "del" /\ vpc.s \in {"moved", "tsynced"}
    /\ LET o == vops[vpc.v]
           n == IF vpc.s = "moved" THEN X(o.p) ELSE M(o.f)
       IN /\ VOk(o.d)
          /\ dur' = [dur EXCEPT ![o.d][n] = vol[o.d][n]]
          /\ objs' = SyncObjs(objs, {vol[o.d][n]} \ {0})
    /\ vpc' = [vpc EXCEPT !.s = IF vpc.s = "moved" THEN "tsynced" ELSE "ssynced"]
    /\ UNCHANGED <<vol, dstat, mgen, cat, pc, sess, look, cnt, nver, ghost, vops, nextV, vsess, vpart, vfile>>

(* The view's operation done, and its operation trash with it (catalog.FinishViewOp); the copy's row is the pass's to
   remove, as gone by the user. *)
VDelFinish ==
    /\ vpc.ph = "del" /\ vpc.s = "ssynced"
    /\ LET o == vops[vpc.v] IN
       /\ ops' = [ops EXCEPT ![o.p].st = "done"]
       /\ vops' = [vops EXCEPT ![vpc.v].st = "done"]
       /\ ghost' = Flag(ghost, "inv7", ~Durable(o.d, X(o.p), o.key) \/ dur[o.d][M(o.f)] = o.key)
    /\ vpc' = VIdle
    /\ UNCHANGED <<phys, cfile, crow, cdisk, nextOp, rjob, tjob, tick, pc, sess, look, cnt, nver, nextV, vsess, vpart, vfile>>

(* A disk that cannot be relied on at any step of a deletion: its operations stay planned, for the view's reprise. *)
VDelLost ==
    /\ vpc.ph = "del" /\ vpc.s \in {"journaled", "looked", "linked", "lsynced", "after", "moved", "tsynced"}
    /\ ~VOk(vops[vpc.v].d)
    /\ vpc' = VIdle
    /\ UNCHANGED <<phys, cat, pc, sess, look, cnt, nver, ghost, vops, nextV, vsess, vpart, vfile>>

ViewUser ==
    \/ \E f \in Files : VCreate(f) \/ VWriteOpen(f) \/ VDelete(f)
    \/ VData

ViewSys ==
    \/ VStart \/ VCheck \/ VRepriseOp \/ VRepriseCommit \/ VRepriseEnd
    \/ VCreateMake \/ VPartBegin \/ VPartMake \/ VPartsDone \/ VClose
    \/ \E e \in Disks : VPartSeal(e) \/ VPartWritten(e) \/ VPartDrop(e)
    \/ VDelLook \/ VDelMove \/ VDelLinkSync \/ VDelUnlink \/ VDelAfter \/ VDelResolved \/ VDelSync \/ VDelFinish \/ VDelLost

-----------------------------------------------------------------------------

Atoll ==
    \/ PassStart
    \/ RepriseOp \/ RepriseCommit \/ RepriseTidy \/ RepriseLost \/ RepriseEnd
    \/ WalkStart \/ WalkEnd \/ WalkDone
    \/ DecideFile \/ DecideSkip \/ DecideDone
    \/ ModifyFile \/ ModifySkip \/ ModifyDone
    \/ TrashPick \/ TrashMove \/ TrashLinkSync \/ TrashUnlink \/ TrashResolve \/ TrashResolved \/ TrashSync \/ TrashCommit
    \/ TrashLost
    \/ PlanStep
    \/ CopyPick \/ CopyPart \/ CopyKey \/ CopySourceLost \/ CopyData \/ CopyRecheck \/ CopyVerify
    \/ CopyRename \/ CopyUnlink \/ CopyRecord \/ CopySync \/ CopyCommit \/ CopyLost
    \/ XferPick \/ XferSkip \/ XferDone \/ XferJournal \/ XferMove \/ XferSync \/ XferCommit
    \/ XferPurgeJournal \/ XferPurge \/ XferPurgeSync \/ XferPurgeCommit \/ XferLost
    \/ GuardPick \/ GuardEnd \/ GuardDrop \/ GuardLink \/ GuardSync \/ GuardCommit \/ GuardTrash \/ GuardTrashSync
    \/ GuardTrashCommit \/ GuardLost

(* The daemon's passes, with what they make of the view's operations. *)
AtollAll == (Atoll /\ UNCHANGED vw) \/ AdoptParts \/ SettleParts \/ TakeIn

Next == AtollAll \/ (World /\ UNCHANGED vw) \/ PowerCut \/ VStop \/ ViewSys \/ ViewUser

Spec == Init /\ [][Next]_vars

(* For liveness: Atoll keeps running its passes, the view its own steps and the program closes what it opened, and a
   disk lost is mounted again in time. *)
FairSpec == Spec /\ WF_vars(AtollAll) /\ WF_vars(ViewSys) /\ \A d \in Disks : WF_vars(Return(d) /\ UNCHANGED vw)

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

(* 2. Never remove the last user copy of a file: the one removal no command asks for, the SSD cache's transfer (§6.1,
   §19), moves out of the library only an Atoll copy, its user part handed to a copy on a data disk first, so that the
   file keeps a user copy (flag set by XferMove). *)
Inv2_LastUserCopyKept == "inv2" \notin ghost.flags

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
    /\ \A d \in Disks, p \in OpIds :
        /\ (vol[d][T(p)] # 0 \/ dur[d][T(p)] # 0) => (ops[p].k = "create" /\ ops[p].d = d)
        /\ (vol[d][X(p)] # 0 \/ dur[d][X(p)] # 0) => (ops[p].k = "trash" /\ ops[p].d = d)
    \* and every link of the guard, by an operation link of that disk and file
    /\ \A d \in Disks, f \in Files : Guard # "none" =>
        ((vol[d][Ln(f)] # 0 \/ dur[d][Ln(f)] # 0) => \E p \in OpIds : ops[p].k = "link" /\ ops[p].d = d /\ ops[p].f = f)

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

(* 14. Atoll's own view presents the in-sync copy with the oldest creation time, never a stale one (§21.2): a file a
   user last wrote through the view, and did not delete since, reads through it as what was written, while every disk
   is there and online in the catalog and the file is in no conflict. A change made directly on a disk is the passes' to
   bring to the view, and leaves no claim (UserAct). What reads through the view is what ReadDisk opens. *)
ViewPresentsLatest ==
    \A f \in Files : LET d == ReadDisk(f) IN
        (ghost.vlast[f] > 0 /\ ~cfile[f].conflict /\ AllOnline)
        => (d # 0 /\ vol[d][M(f)] # 0 /\ objs[vol[d][M(f)]].ver = ghost.vlast[f])

TypeOK ==
    /\ dstat \in [Disks -> {"up", "gone"}]
    /\ cdisk \in [Disks -> {"online", "missing"}]
    /\ \A f \in Files, d \in Disks : crow[f][d].st \in {"none", "ok", "stale", "pending"}
    /\ \A p \in OpIds : ops[p].st \in {"none", "planned", "done", "undone"}
    /\ nextOp \in 1..(MaxOps + 1)
    /\ \A v \in VIds : vops[v].st \in {"none", "planned", "done", "undone"}
    /\ nextV \in 1..(MaxVOps + 1)

-----------------------------------------------------------------------------
(* Liveness, under FairSpec, with Waits = FALSE and no disk lost for good: each file reaches its level, and no
   operation stays planned. *)

Settled ==
    /\ \A f \in Files : (cfile[f].on /\ ~cfile[f].conflict) => Cardinality({d \in Disks : crow[f][d].st = "ok"}) >= Level
    /\ \A p \in OpIds : ops[p].st # "planned"
    \* with the SSD cache, every file left it, its copies on data disks (§19), but one whose copy on a data disk waits to be
    \* read again, unverified, as a reprise may record it (§14.2): a scrub, which the model leaves out, verifies it
    /\ \A f \in Files, c \in CacheDisks :
           (cfile[f].on /\ ~cfile[f].conflict /\ ~\E d \in Disks \ CacheDisks : crow[f][d].st = "ok" /\ ~crow[f][d].verified)
           => crow[f][c].st = "none"

EventuallySettled == <>[]Settled

(* With the view: every operation of the view that happened is taken in, by a pass, at last (§21.2). *)
ViewTaken == \A v \in VIds : (vops[v].k # "none" /\ vops[v].st # "undone") => vops[v].taken

EventuallyViewTaken == <>[]ViewTaken

(* With the link guard turned off, every link goes at last, its data to the trash first when it has no other name
   (§17, "Turned off"). *)
Unlinked == (Guard = "off" /\ cnt.off = 1) => \A d \in Disks, f \in Files : vol[d][Ln(f)] = 0

EventuallyUnlinked == <>[]Unlinked

=============================================================================
