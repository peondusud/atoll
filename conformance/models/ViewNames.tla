------------------------------ MODULE ViewNames ------------------------------
(* Atoll's own view: its rename, its replacement onto a name it presents, and its folders renamed or removed on every
   disk at once (specs.md §21.2; internal/view/writer, writer.go Rename, renameFile, replace, renameFolder, Rmdir, and
   reprise.go), a model of its own beside Replication.tla's view and ViewMove.tla: what the view journals and does,
   step by step, what its start resolves of what a stop left planned and its check every 30 s of what a disk lost left
   planned, what it presents, as view.Resolve lays its operations over the catalog, and how the daemon's passes wait at
   them, take what they did to the other disk, and take them in; with atoll mount stopped short (kill -9), a disk lost
   and mounted again, and a user acting directly on a disk, between any two steps.

   What it keeps: folder names at the root (D, E) and file names in a folder (x, y); on each disk the folders at the
   root and the files in each, by object (an inode, with the version it holds), and its trash; the catalog's files (a
   path, on or off, in conflict or not), their copies (ok, a deletion to make, a copy to make again, or a refresh to
   make; where each sits, its old_lpath) and the entries of the folders; the view's journal, view_ops (kind, disk,
   paths, the inode a file's operation names, a replacement's copy sent to the trash and its operation trash, state,
   taken in), the view's process and its handles. A pass: on each disk its walk, which waits at the paths of the view's
   operations planned there (jobs.viewPlannedOn), finds each copy there, moved (a rename, its file's path following,
   the other copies behind), replaced (a modification, the other copies to refresh) or gone (a deletion, its other
   copies to the trash, when deletes.direct is propagate or the copy went through the view's deletion; kept otherwise,
   the copy made again), the folders found renamed, below which the files that only other disks hold follow, and the
   folder entries, each change decided once a later walk of that disk finds it still (the debounce) and no copy of its
   file on another disk waits; then on each disk its folder jobs, each as one operation, and what follows, a copy at a
   time: a copy behind renamed, one to delete to the trash, one to make again or to refresh, a conflict when its name
   holds another file; then the take-in of the view's operations done (view.TakenIn).

   What it leaves out: the daemon's own journal and its crashes (each of its operations is one step here; Folders.tla
   and Replication.tla have their steps), the guards and the settle, what a user makes new being recorded, the power
   cut (every name is durable as it is made), refused and colliding names, folders below folders, the view's other
   operations (Replication.tla's and ViewMove.tla's), and more than two disks. *)
EXTENDS Naturals, FiniteSets, Sequences, TLC

CONSTANTS
    NFolders,   \* folder names at the root, by number: D, E
    NNames,     \* file names in a folder, by number: x, y
    NFiles,     \* file ids the catalog holds
    StartPath,  \* [1..NFiles -> path]: each file's path at the start
    StartOn,    \* [1..NFiles -> SUBSET Disks]: the disks that hold each file at the start
    StartDirs,  \* [Disks -> SUBSET 1..NFolders]: the folders on each disk at the start
    Direct,     \* deletes.direct: "propagate" or "restore"
    ViewActs,   \* some of "rename", "replace", "mvdir", "rmdir": what the view is asked
    MaxView,    \* how many operations the view may journal
    UserActs,   \* some of "mv", "create", "delete", "save", "write", "mkdir", "rmdir", "mvdir": a user on a disk
    MaxUser,    \* how many times a user acts directly on a disk
    MaxStop,    \* how many times atoll mount is stopped short
    MaxDrop,    \* how many times a disk is lost
    Fixes       \* the remedies proposed for the flaws found here and still open, by name (README.md); {} is specs.md as it is

NDisks == 2
Disks == 1..NDisks
FNames == 1..NFolders
Names == 1..NNames
Paths == FNames \X Names
None == <<0, 0>>
FileIds == 1..NFiles
VIds == 1..MaxView
FixNames == {"replace-gone", "folder-key", "view-moot", "job-view-held"}

ASSUME /\ NFolders \in 1..2 /\ NNames \in 1..2 /\ NFiles \in 1..2
       /\ StartPath \in [FileIds -> Paths] /\ StartOn \in [FileIds -> (SUBSET Disks) \ {{}}]
       /\ StartDirs \in [Disks -> SUBSET FNames]
       /\ \A f \in FileIds, d \in Disks : d \in StartOn[f] => StartPath[f][1] \in StartDirs[d]
       /\ \A f, g \in FileIds : f # g => StartPath[f] # StartPath[g]
       /\ Direct \in {"propagate", "restore"}
       /\ ViewActs \subseteq {"rename", "replace", "mvdir", "rmdir"}
       /\ UserActs \subseteq {"mv", "create", "delete", "save", "write", "mkdir", "rmdir", "mvdir"}
       /\ Fixes \subseteq FixNames

Min(S) == CHOOSE x \in S : \A y \in S : x <= y
NoKids == [n \in Names |-> 0]
NoCopy == [st |-> "none", obj |-> 0, old |-> None]
(* An operation of the view: kind, the disk it names (a folder's, the first that holds it), its two paths (a folder's
   <<F, 0>>), the inode a file's names (key), a replacement's copy sent to the trash with the version it held at the
   look (tkey, tver) and that operation trash's state (ts), its state, whether a pass took it in, and for a folder the
   folders its holders held at the old name then, each with its disk, and whether the view itself made what it names on
   a disk (real and did, for the invariants alone: the code records no inode of a folder). *)
NoV == [k |-> "none", d |-> 0, p |-> None, q |-> None, key |-> 0, tkey |-> 0, tver |-> 0, st |-> "none", ts |-> "none",
        taken |-> FALSE, real |-> {}, did |-> FALSE]
VDown == [ph |-> "down"]
VIdle == [ph |-> "idle"]
PIdle == [ph |-> "idle"]

VARIABLES
    objs,    \* files and folders: [kind, d, ver, kids]
    root,    \* [Disks -> [FNames -> folder or 0]]
    trash,   \* [Disks -> SUBSET objects]: what each disk's trash holds
    dstat,   \* [Disks -> {"up", "gone"}]
    mgen,    \* [Disks -> Nat]: the mount id
    cf,      \* the catalog's files: [FileIds -> [on, path, st]]
    cc,      \* their copies: [FileIds -> [Disks -> [st, obj, old]]]
    ce,      \* the entries of the folders: [Disks -> [FNames -> folder or 0]]
    fj,      \* the folder jobs: [Disks -> set of <<From, To, n>>], n what the view had done when it was decided (DecidedN)
    fseen,   \* [Disks -> set of <<From, To>>]: the folders the last walk of each disk found renamed, not yet due
    vseen,   \* [Disks -> set of <<file, verdict, to, by>>]: the copies the last walk found changed, not yet due
    vops,    \* the view's journal
    nextV,
    vpc,     \* where atoll mount is: down, idle, an operation's step, or its reprise
    vsess,   \* the mount each disk had when the view opened its handle there, plus 1; 0 for none
    pc,      \* where the daemon's pass is
    cnt,     \* how many of each event of the world so far
    nver,    \* the last version a user wrote
    ghost    \* for the invariants

vars == <<objs, root, trash, dstat, mgen, cf, cc, ce, fj, fseen, vseen, vops, nextV, vpc, vsess, pc, cnt, nver, ghost>>
disk == <<objs, root, trash, dstat, mgen>>
cat == <<cf, cc, ce, fj, fseen, vseen>>
view == <<vops, nextV, vpc, vsess>>

-----------------------------------------------------------------------------
(* Helpers *)

Up(d) == dstat[d] = "up"
VUp(d) == Up(d) /\ vsess[d] = mgen[d] + 1
Flag(g, name, cond) == IF cond THEN [g EXCEPT !.flags = @ \cup {name}] ELSE g
AtIn(os, rt, d, nm) == IF nm[2] = 0 THEN rt[d][nm[1]]
                       ELSE IF rt[d][nm[1]] = 0 THEN 0 ELSE os[rt[d][nm[1]]].kids[nm[2]]
At(d, nm) == AtIn(objs, root, d, nm)
Place(f, d) == IF cc[f][d].old # None THEN cc[f][d].old ELSE cf[f].path
AtOrBelow(x, h) == x = h \/ (h[2] = 0 /\ x[1] = h[1])
NewDir(d) == [kind |-> "dir", d |-> d, ver |-> 0, kids |-> NoKids]
NewFile(d, v) == [kind |-> "file", d |-> d, ver |-> v, kids |-> NoKids]

(* The disk once a file at p moves to q on d, the folder of q made first when the disk lacks it (ops.userParent). *)
MoveTo(d, p, q) ==
    LET made == root[d][q[1]] = 0
        os1 == IF made THEN Append(objs, NewDir(d)) ELSE objs
        rt1 == IF made THEN [root EXCEPT ![d][q[1]] = Len(objs) + 1] ELSE root
        o == At(d, p)
    IN [objs |-> [os1 EXCEPT ![root[d][p[1]]].kids[p[2]] = 0, ![rt1[d][q[1]]].kids[q[2]] = o], root |-> rt1]

(* The disk once a new object o goes to a free name p on d, its folder made first when the disk lacks it. *)
PutNew(os, d, p, o) ==
    LET made == root[d][p[1]] = 0
        os1 == IF made THEN Append(os, NewDir(d)) ELSE os
        rt1 == IF made THEN [root EXCEPT ![d][p[1]] = Len(os) + 1] ELSE root
    IN [objs |-> [os1 EXCEPT ![rt1[d][p[1]]].kids[p[2]] = o], root |-> rt1]

-----------------------------------------------------------------------------
(* What the view presents (view.Resolve, view.Tree): the view's operations no pass has taken in and not undone, the
   newest deciding, over the catalog's rows. A file renamed or replaced onto a name presents what its disk holds there,
   as a look finds it; its old name presents nothing; below a folder renamed, the catalog's rows below the old name, or
   below the new one once a pass has recorded the rename (view.Tree.present: the first of those paths with rows);
   below a folder renamed from or removed, nothing. The catalog presents a file at its path from its original, here the
   first disk whose copy is ok at that path. *)

OpenOps == {v \in VIds : vops[v].st \in {"planned", "done"} /\ ~vops[v].taken}

RECURSIVE Rev(_)
Rev(sq) == IF sq = <<>> THEN <<>> ELSE Rev(Tail(sq)) \o <<Head(sq)>>

(* What a path resolves to: none, a disk's look (d), or the catalog's rows at the paths ps, in that order. *)
RECURSIVE Res(_, _, _, _)
Res(q, i, os, later) ==
    IF i = 0 THEN [k |-> "cat", d |-> 0, ps |-> <<q>> \o Rev(later)]
    ELSE IF i \notin os THEN Res(q, i - 1, os, later)
    ELSE LET o == vops[i] IN
         CASE o.k \in {"rename", "replace"} /\ q = o.q -> [k |-> "disk", d |-> o.d, ps |-> <<>>]
           [] o.k \in {"rename", "replace"} /\ q = o.p -> [k |-> "none", d |-> 0, ps |-> <<>>]
           [] o.k = "mvdir" /\ q[1] = o.q[1] -> Res(<<o.p[1], q[2]>>, i - 1, os, later \o <<q>>)
           [] o.k \in {"mvdir", "rmdir"} /\ q[1] = o.p[1] -> [k |-> "none", d |-> 0, ps |-> <<>>]
           [] OTHER -> Res(q, i - 1, os, later)
Resolved(q, os) == Res(q, MaxView, os, <<>>)

CopyAtPath(f, e) == cc[f][e].st = "ok" /\ cc[f][e].old = None
CatFiles(q) == {f \in FileIds : cf[f].on /\ cf[f].path = q /\ \E e \in Disks : CopyAtPath(f, e)}
NoP == [k |-> "none", d |-> 0, o |-> 0]

(* The first of a source's catalog paths that holds a file, None for none. *)
CatPath(ps) == LET with == {i \in 1..Len(ps) : CatFiles(ps[i]) # {}} IN IF with = {} THEN None ELSE ps[Min(with)]

PresentFile(p, os) ==
    LET s == Resolved(p, os) IN
    IF s.k = "none" THEN NoP
    ELSE IF s.k = "disk" THEN IF At(s.d, p) # 0 THEN [k |-> "file", d |-> s.d, o |-> At(s.d, p)] ELSE NoP
    ELSE IF CatPath(s.ps) = None THEN NoP
    ELSE LET f == Min(CatFiles(CatPath(s.ps)))
             e == Min({x \in Disks : CopyAtPath(f, x)})
         IN [k |-> "file", d |-> e, o |-> cc[f][e].obj]

(* The first of a folder's catalog paths whose entry a disk holds, None for none. *)
DirPath(ps) == LET with == {i \in 1..Len(ps) : \E e \in Disks : ce[e][ps[i][1]] # 0} IN IF with = {} THEN None ELSE ps[Min(with)]
PresentFolder(F, os) == LET s == Resolved(<<F, 0>>, os) IN s.k = "cat" /\ DirPath(s.ps) # None

(* The disks that hold a folder the view presents, as the catalog's entries say of every path it resolves to
   (writer.folderHolders). *)
HoldersOf(F) == LET s == Resolved(<<F, 0>>, OpenOps) IN
                IF s.k # "cat" THEN {} ELSE {e \in Disks : \E i \in 1..Len(s.ps) : ce[e][s.ps[i][1]] # 0}

-----------------------------------------------------------------------------
(* The start. Objects 1..2*NFolders are each disk's folders, by disk then name; then each file's copy on each disk. *)

DirObj(d, F) == (d - 1) * NFolders + F
FileObj(f, d) == 2 * NFolders + (f - 1) * 2 + d
NObj0 == 2 * NFolders + 2 * NFiles
StartAt(d, p) == IF \E f \in FileIds : StartPath[f] = p /\ d \in StartOn[f]
                 THEN FileObj(CHOOSE f \in FileIds : StartPath[f] = p /\ d \in StartOn[f], d) ELSE 0

Init ==
    /\ objs = [o \in 1..NObj0 |->
                 IF o <= 2 * NFolders
                 THEN LET d == (o - 1) \div NFolders + 1
                          F == ((o - 1) % NFolders) + 1
                      IN [kind |-> "dir", d |-> d, ver |-> 0, kids |-> [n \in Names |-> StartAt(d, <<F, n>>)]]
                 ELSE LET f == (o - 2 * NFolders - 1) \div 2 + 1
                          d == ((o - 2 * NFolders - 1) % 2) + 1
                      IN [kind |-> "file", d |-> d, ver |-> f, kids |-> NoKids]]
    /\ root = [d \in Disks |-> [F \in FNames |-> IF F \in StartDirs[d] THEN DirObj(d, F) ELSE 0]]
    /\ trash = [d \in Disks |-> {}]
    /\ dstat = [d \in Disks |-> "up"]
    /\ mgen = [d \in Disks |-> 0]
    /\ cf = [f \in FileIds |-> [on |-> TRUE, path |-> StartPath[f], st |-> "ok"]]
    /\ cc = [f \in FileIds |-> [d \in Disks |->
              IF d \in StartOn[f] THEN [NoCopy EXCEPT !.st = "ok", !.obj = FileObj(f, d)] ELSE NoCopy]]
    /\ ce = [d \in Disks |-> [F \in FNames |-> IF F \in StartDirs[d] THEN DirObj(d, F) ELSE 0]]
    /\ fj = [d \in Disks |-> {}]
    /\ fseen = [d \in Disks |-> {}]
    /\ vseen = [d \in Disks |-> {}]
    /\ vops = [v \in VIds |-> NoV]
    /\ nextV = 1
    /\ vpc = VIdle
    /\ vsess = [d \in Disks |-> 1]
    /\ pc = PIdle
    /\ cnt = [user |-> 0, stop |-> 0, drop |-> 0]
    /\ nver = NFiles
    /\ ghost = [written |-> FileIds, udest |-> {}, flags |-> {}, direct |-> FALSE]

-----------------------------------------------------------------------------
(* The world: a user directly on a disk, through mergerfs or the disk itself; a disk lost and mounted again; atoll
   mount stopped short. *)

HeldIn(os, rt, tr, v) == \E d \in Disks :
    \/ \E p \in Paths : AtIn(os, rt, d, p) # 0 /\ os[AtIn(os, rt, d, p)].ver = v
    \/ \E o \in tr[d] : os[o].ver = v

UserOn(os, rt, gone, newv) ==
    /\ objs' = os /\ root' = rt
    /\ nver' = IF newv = 0 THEN nver ELSE newv
    /\ cnt' = [cnt EXCEPT !.user = @ + 1]
    /\ ghost' = [ghost EXCEPT !.written = IF newv = 0 THEN @ ELSE @ \cup {newv},
                              !.udest = IF gone # 0 /\ ~HeldIn(os, rt, trash, gone) THEN @ \cup {gone} ELSE @,
                              !.direct = TRUE]
    /\ UNCHANGED <<trash, dstat, mgen, cat, view, pc>>

UAllowed(a) == a \in UserActs /\ cnt.user < MaxUser

UMv(d, p, q) ==
    /\ UAllowed("mv") /\ Up(d) /\ p # q /\ At(d, p) # 0 /\ At(d, q) = 0
    /\ LET m == MoveTo(d, p, q) IN UserOn(m.objs, m.root, 0, 0)

UCreate(d, p) ==
    /\ UAllowed("create") /\ Up(d) /\ At(d, p) = 0
    /\ LET m == PutNew(Append(objs, NewFile(d, nver + 1)), d, p, Len(objs) + 1) IN UserOn(m.objs, m.root, 0, nver + 1)

UDelete(d, p) ==
    /\ UAllowed("delete") /\ Up(d) /\ At(d, p) # 0
    /\ UserOn([objs EXCEPT ![root[d][p[1]]].kids[p[2]] = 0], root, objs[At(d, p)].ver, 0)

(* An atomic save: a new file at the name, the one it replaces gone. *)
USave(d, p) ==
    /\ UAllowed("save") /\ Up(d) /\ At(d, p) # 0
    /\ UserOn([Append(objs, NewFile(d, nver + 1)) EXCEPT ![root[d][p[1]]].kids[p[2]] = Len(objs) + 1], root,
              objs[At(d, p)].ver, nver + 1)

(* A write in place: the same inode, a new version, the one it overwrites gone unless held elsewhere. *)
UWrite(d, p) ==
    /\ UAllowed("write") /\ Up(d) /\ At(d, p) # 0
    /\ UserOn([objs EXCEPT ![At(d, p)].ver = nver + 1], root, objs[At(d, p)].ver, nver + 1)

UMkdir(d, F) ==
    /\ UAllowed("mkdir") /\ Up(d) /\ root[d][F] = 0
    /\ UserOn(Append(objs, NewDir(d)), [root EXCEPT ![d][F] = Len(objs) + 1], 0, 0)

URmdir(d, F) ==
    /\ UAllowed("rmdir") /\ Up(d) /\ root[d][F] # 0 /\ objs[root[d][F]].kids = NoKids
    /\ UserOn(objs, [root EXCEPT ![d][F] = 0], 0, 0)

UMvDir(d, F, G) ==
    /\ UAllowed("mvdir") /\ Up(d) /\ F # G /\ root[d][F] # 0 /\ root[d][G] = 0
    /\ UserOn(objs, [root EXCEPT ![d][G] = root[d][F], ![d][F] = 0], 0, 0)

Drop(d) ==
    /\ cnt.drop < MaxDrop /\ Up(d)
    /\ dstat' = [dstat EXCEPT ![d] = "gone"]
    /\ cnt' = [cnt EXCEPT !.drop = @ + 1]
    /\ UNCHANGED <<objs, root, trash, mgen, cat, view, pc, nver, ghost>>

Return(d) ==
    /\ dstat[d] = "gone"
    /\ dstat' = [dstat EXCEPT ![d] = "up"]
    /\ mgen' = [mgen EXCEPT ![d] = @ + 1]
    /\ UNCHANGED <<objs, root, trash, cat, view, pc, cnt, nver, ghost>>

VStop ==
    /\ cnt.stop < MaxStop /\ vpc.ph # "down"
    /\ vpc' = VDown /\ vsess' = [d \in Disks |-> 0]
    /\ cnt' = [cnt EXCEPT !.stop = @ + 1]
    /\ UNCHANGED <<disk, cat, vops, nextV, pc, nver, ghost>>

World ==
    \/ \E d \in Disks, p, q \in Paths : UMv(d, p, q)
    \/ \E d \in Disks, p \in Paths : UCreate(d, p) \/ UDelete(d, p) \/ USave(d, p) \/ UWrite(d, p)
    \/ \E d \in Disks, F, G \in FNames : UMkdir(d, F) \/ URmdir(d, F) \/ UMvDir(d, F, G)
    \/ \E d \in Disks : Drop(d) \/ Return(d)
    \/ VStop

-----------------------------------------------------------------------------
(* The view's journal marked: done or undone, its operation trash with it, and whether that is what the disks hold (for
   the invariants): a file's rename or replacement done when its new name holds the inode it names on its disk; a
   folder's, one operation of every disk that holds it, "done once it reached one" (§21.2), when the new name of one of
   its holders holds the folder that was at the old name there; undone when it is not so. *)

DoneTrue(os, rt, v) ==
    LET o == vops[v] IN
    CASE o.k = "rename" -> AtIn(os, rt, o.d, o.q) = o.key
      \* a replacement whose source is gone once its copy replaced is in the trash has that left alone to finish
      \* (resolveReplace: "what was replaced is in the trash, which is all there is to finish")
      [] o.k = "replace" -> AtIn(os, rt, o.d, o.q) = o.key \/ (o.tkey \in trash[o.d] /\ AtIn(os, rt, o.d, o.p) # o.key)
      [] o.k = "mvdir" -> \E r \in o.real : rt[r[1]][o.q[1]] = r[2]
      [] OTHER -> TRUE

(* did: this step made what the operation names, the view's own move. A verdict on what the view did not make, as
   a reprise's, is done only when it is so; undone is wrong only of what the view made and is still so, as long as no
   user acted directly: a user's own move, before or after, is no operation of the view's, and once a user changed a
   name the view's verdict by names cannot always tell, which the passes then follow as the user's. *)
SetSt(v, st, os, rt, did) ==
    /\ vops' = [vops EXCEPT ![v].st = st, ![v].ts = IF vops[v].ts = "planned" THEN st ELSE vops[v].ts,
                            ![v].did = vops[v].did \/ did]
    /\ ghost' = Flag(Flag(ghost, "done-wrong", st = "done" /\ ~(vops[v].did \/ did) /\ ~DoneTrue(os, rt, v)),
                     "undone-wrong", st = "undone" /\ (vops[v].did \/ did) /\ DoneTrue(os, rt, v) /\ ~ghost.direct)

-----------------------------------------------------------------------------
(* The view's operations, each under the writer's lock, each journaled before it runs (writer.journal,
   catalog.BeginViewOp, BeginViewTrash), one at a time. A step on a disk whose handle cannot be relied on leaves the
   operation planned, for the view's reprise or its check (writer.finish, ErrDiskLost). *)

VAct(k) == k \in ViewActs /\ nextV <= MaxView /\ vpc = VIdle
VStep(s) == vpc.ph = "op" /\ vpc.s = s
VNew(rec, s, left) ==
    /\ vops' = [vops EXCEPT ![nextV] = rec]
    /\ nextV' = nextV + 1
    /\ vpc' = [ph |-> "op", v |-> nextV, s |-> s, left |-> left, moved |-> 0, lost |-> FALSE, missed |-> FALSE]

(* Rename of a file onto a name that presents nothing, in a folder the view presents (writer.Rename, renameFile): on
   the disk the view presents it from, its inode looked at, journaled; then ops.Disk.RenameUser, without replacing, its
   missing folder made first; done, or undone when the name no longer holds that inode or the new one is taken. *)
VRename(p, q) ==
    /\ VAct("rename") /\ p # q
    /\ LET w == PresentFile(p, OpenOps) IN
       /\ w.k = "file" /\ VUp(w.d) /\ At(w.d, p) # 0
       /\ PresentFile(q, OpenOps).k = "none" /\ PresentFolder(q[1], OpenOps)
       /\ VNew([NoV EXCEPT !.k = "rename", !.d = w.d, !.p = p, !.q = q, !.key = At(w.d, p), !.st = "planned"], "rename", {})
    /\ UNCHANGED <<disk, cat, vsess, pc, cnt, nver, ghost>>

VRenameDo ==
    /\ VStep("rename")
    /\ LET v == vpc.v
           o == vops[v]
           d == o.d
       IN IF ~VUp(d)
          THEN /\ vpc' = VIdle /\ UNCHANGED <<objs, root, vops, ghost>>
          ELSE IF At(d, o.p) = o.key /\ At(d, o.q) = 0
          THEN LET m == MoveTo(d, o.p, o.q) IN
               /\ objs' = m.objs /\ root' = m.root
               /\ SetSt(v, "done", m.objs, m.root, TRUE)
               /\ vpc' = VIdle
          ELSE /\ SetSt(v, "undone", objs, root, FALSE) /\ vpc' = VIdle /\ UNCHANGED <<objs, root>>
    /\ UNCHANGED <<trash, dstat, mgen, cat, nextV, vsess, pc, cnt, nver>>

(* Replacement (writer.replace): onto a name presented from the same disk; the copy there looked at, the operation
   replace and its trash journaled, `overwritten`; the copy sent to the trash against that look (ops.Disk.ToTrash),
   resolved at once by the table of §17.2 when it fails (afterFailedTrash): not moved, undone; then the rename without
   replacing: done, or, when it fails, left planned, for the view's reprise, whatever failed. *)
VReplace(p, q) ==
    /\ VAct("replace") /\ p # q
    /\ LET w == PresentFile(p, OpenOps)
           t == PresentFile(q, OpenOps)
       IN /\ w.k = "file" /\ t.k = "file" /\ t.d = w.d /\ VUp(w.d) /\ At(w.d, p) # 0 /\ At(w.d, q) # 0
          /\ VNew([NoV EXCEPT !.k = "replace", !.d = w.d, !.p = p, !.q = q, !.key = At(w.d, p), !.tkey = At(w.d, q),
                              !.tver = objs[At(w.d, q)].ver, !.st = "planned", !.ts = "planned"], "trash", {})
    /\ UNCHANGED <<disk, cat, vsess, pc, cnt, nver, ghost>>

VReplaceTrash ==
    /\ VStep("trash")
    /\ LET v == vpc.v
           o == vops[v]
           d == o.d
       IN IF ~VUp(d)
          THEN /\ vpc' = VIdle /\ UNCHANGED <<objs, trash, vops, ghost>>
          ELSE IF At(d, o.q) = o.tkey /\ objs[o.tkey].ver = o.tver
          THEN /\ objs' = [objs EXCEPT ![root[d][o.q[1]]].kids[o.q[2]] = 0]
               /\ trash' = [trash EXCEPT ![d] = @ \cup {o.tkey}]
               /\ vpc' = [vpc EXCEPT !.s = "onto"]
               /\ UNCHANGED <<vops, ghost>>
          ELSE /\ SetSt(v, "undone", objs, root, FALSE) /\ vpc' = VIdle /\ UNCHANGED <<objs, trash>>
    /\ UNCHANGED <<root, dstat, mgen, cat, nextV, vsess, pc, cnt, nver>>

VReplaceOnto ==
    /\ VStep("onto")
    /\ LET v == vpc.v
           o == vops[v]
           d == o.d
       IN IF VUp(d) /\ At(d, o.p) = o.key /\ At(d, o.q) = 0
          THEN LET m == MoveTo(d, o.p, o.q) IN
               /\ objs' = m.objs /\ root' = m.root
               /\ SetSt(v, "done", m.objs, m.root, TRUE)
          ELSE UNCHANGED <<objs, root, vops, ghost>>
    /\ vpc' = VIdle
    /\ UNCHANGED <<trash, dstat, mgen, cat, nextV, vsess, pc, cnt, nver>>

(* A folder renamed on every disk that holds it (writer.renameFolder), onto a name that presents nothing: one
   operation, of the first disk that holds it, journaled with no inode; then on each holder in turn, its folder looked
   at and renamed without replacing: a disk with no handle or no folder there skipped, one whose name is taken or that
   cannot be relied on failed. Done once it reached one; with none, undone, or left planned when a disk failed so. *)
VMvDir(F, G) ==
    /\ VAct("mvdir") /\ F # G
    /\ PresentFolder(F, OpenOps) /\ ~PresentFolder(G, OpenOps)
    /\ \A n \in Names : PresentFile(<<G, n>>, OpenOps).k = "none"
    /\ HoldersOf(F) # {}
    /\ LET h == Min(HoldersOf(F)) IN
       VNew([NoV EXCEPT !.k = "mvdir", !.d = h, !.p = <<F, 0>>, !.q = <<G, 0>>, !.st = "planned",
                        !.real = {<<e, root[e][F]>> : e \in {x \in HoldersOf(F) : root[x][F] # 0}}],
            "dir", HoldersOf(F))
    /\ UNCHANGED <<disk, cat, vsess, pc, cnt, nver, ghost>>

VMvDirStep ==
    /\ VStep("dir") /\ vpc.left # {}
    /\ LET o == vops[vpc.v]
           h == Min(vpc.left)
           F == o.p[1]
           G == o.q[1]
           rest == vpc.left \ {h}
           moves == vsess[h] # 0 /\ VUp(h) /\ root[h][F] # 0 /\ root[h][G] = 0
       IN /\ CASE vsess[h] = 0 \/ (VUp(h) /\ root[h][F] = 0) ->
                    /\ vpc' = [vpc EXCEPT !.left = rest, !.missed = @ \/ vsess[h] = 0] /\ UNCHANGED root
               [] ~VUp(h) \/ root[h][G] # 0 ->
                    /\ vpc' = [vpc EXCEPT !.left = rest, !.lost = @ \/ ~VUp(h), !.missed = @ \/ ~VUp(h)]
                    /\ UNCHANGED root
               [] OTHER ->
                    /\ root' = [root EXCEPT ![h][G] = root[h][F], ![h][F] = 0]
                    /\ vpc' = [vpc EXCEPT !.left = rest, !.moved = @ + 1]
          /\ vops' = [vops EXCEPT ![vpc.v].did = @ \/ moves]
    /\ UNCHANGED <<objs, trash, dstat, mgen, cat, nextV, vsess, pc, cnt, nver, ghost>>

VMvDirEnd ==
    /\ VStep("dir") /\ vpc.left = {}
    /\ IF vpc.moved = 0 /\ vpc.lost
       THEN UNCHANGED <<vops, ghost>>
       ELSE SetSt(vpc.v, IF vpc.moved = 0 THEN "undone" ELSE "done", objs, root, FALSE)
    /\ vpc' = VIdle
    /\ UNCHANGED <<disk, cat, nextV, vsess, pc, cnt, nver>>

(* A folder that presents nothing below it removed (writer.Rmdir): one operation, of its first holder; then from each
   holder where it is empty; a disk with no handle, no folder or something in it skipped; one that cannot be relied on
   ends it, planned. *)
VRmdir(F) ==
    /\ VAct("rmdir")
    /\ PresentFolder(F, OpenOps) /\ \A n \in Names : PresentFile(<<F, n>>, OpenOps).k = "none"
    /\ HoldersOf(F) # {}
    /\ VNew([NoV EXCEPT !.k = "rmdir", !.d = Min(HoldersOf(F)), !.p = <<F, 0>>, !.st = "planned"], "rm", HoldersOf(F))
    /\ UNCHANGED <<disk, cat, vsess, pc, cnt, nver, ghost>>

VRmdirStep ==
    /\ VStep("rm") /\ vpc.left # {}
    /\ LET o == vops[vpc.v]
           h == Min(vpc.left)
           F == o.p[1]
       IN IF vsess[h] # 0 /\ ~VUp(h)
          THEN /\ vpc' = VIdle /\ UNCHANGED root
          ELSE /\ root' = IF vsess[h] # 0 /\ root[h][F] # 0 /\ objs[root[h][F]].kids = NoKids
                          THEN [root EXCEPT ![h][F] = 0] ELSE root
               /\ vpc' = [vpc EXCEPT !.left = @ \ {h}]
    /\ UNCHANGED <<objs, trash, dstat, mgen, cat, vops, nextV, vsess, pc, cnt, nver, ghost>>

VRmdirEnd ==
    /\ VStep("rm") /\ vpc.left = {}
    /\ SetSt(vpc.v, "done", objs, root, FALSE)
    /\ vpc' = VIdle
    /\ UNCHANGED <<disk, cat, nextV, vsess, pc, cnt, nver>>

-----------------------------------------------------------------------------
(* What a stop left planned (§21.2, The view's journal; reprise.go, resolve and resolveReplace): at the view's start,
   every operation planned whose disk it has a handle on; at its check every 30 s, once a disk is back on a mount of
   its own, the operations planned there. A rename is done when its new name holds the inode it names, undone
   otherwise; a folder's when its disk holds a folder at the new name; a removal is done. A replacement: its trash
   resolved first, by the table (not in the trash: undone); then done when the new name holds the inode, or the rename
   made now when the old name still holds it, left planned when that fails; done otherwise. One operation a step. *)

Resolve(v) ==
    LET o == vops[v]
        d == o.d
    IN IF o.k = "rename"
       THEN /\ SetSt(v, IF At(d, o.q) = o.key THEN "done" ELSE "undone", objs, root, FALSE)
            /\ UNCHANGED <<objs, root>>
       ELSE IF o.k = "replace"
       THEN IF o.tkey \notin trash[d]
            THEN /\ SetSt(v, "undone", objs, root, FALSE) /\ UNCHANGED <<objs, root>>
            ELSE IF At(d, o.q) # o.key /\ At(d, o.p) = o.key
            THEN IF At(d, o.q) = 0
                 THEN LET m == MoveTo(d, o.p, o.q) IN
                      /\ objs' = m.objs /\ root' = m.root
                      /\ SetSt(v, "done", m.objs, m.root, TRUE)
                 ELSE UNCHANGED <<objs, root, vops, ghost>>
            ELSE /\ SetSt(v, "done", objs, root, FALSE) /\ UNCHANGED <<objs, root>>
       ELSE IF o.k = "mvdir"
       THEN \* with folder-key, the remedy proposed for flaw 19 (README.md): its first holder's folder inode, which
            \* the operation records as a file's does (file_key), held at the new name there; or another holder's new
            \* name holding a folder and its old name none
            LET F == o.p[1]
                G == o.q[1]
                keyed == \E r \in o.real : r[1] = d /\ root[d][G] = r[2]
                others == \E r \in o.real : r[1] # d /\ VUp(r[1]) /\ root[r[1]][G] # 0 /\ root[r[1]][F] = 0
                there == IF "folder-key" \in Fixes THEN keyed \/ others ELSE root[d][G] # 0
            IN /\ SetSt(v, IF there THEN "done" ELSE "undone", objs, root, FALSE)
               /\ UNCHANGED <<objs, root>>
       ELSE /\ SetSt(v, "done", objs, root, FALSE) /\ UNCHANGED <<objs, root>>

VStart ==
    /\ vpc = VDown
    /\ vsess' = [d \in Disks |-> IF Up(d) THEN mgen[d] + 1 ELSE 0]
    /\ vpc' = [ph |-> "reprise", todo |-> {v \in VIds : vops[v].st = "planned"}]
    /\ UNCHANGED <<disk, cat, vops, nextV, pc, cnt, nver, ghost>>

VCheck ==
    /\ vpc = VIdle
    /\ \E d \in Disks : (Up(d) /\ vsess[d] # mgen[d] + 1) \/ (~Up(d) /\ vsess[d] # 0)
    /\ LET back == {d \in Disks : Up(d) /\ vsess[d] # mgen[d] + 1} IN
       /\ vsess' = [d \in Disks |-> IF Up(d) THEN mgen[d] + 1 ELSE 0]
       /\ vpc' = [ph |-> "reprise", todo |-> {v \in VIds : vops[v].st = "planned" /\ vops[v].d \in back}]
    /\ UNCHANGED <<disk, cat, vops, nextV, pc, cnt, nver, ghost>>

VRepriseOp ==
    /\ vpc.ph = "reprise" /\ vpc.todo # {}
    /\ LET v == Min(vpc.todo) IN
       /\ IF VUp(vops[v].d) /\ vops[v].st = "planned" THEN Resolve(v) ELSE UNCHANGED <<objs, root, vops, ghost>>
       /\ vpc' = [vpc EXCEPT !.todo = @ \ {v}]
    /\ UNCHANGED <<trash, dstat, mgen, cat, nextV, vsess, pc, cnt, nver>>

VRepriseEnd ==
    /\ vpc.ph = "reprise" /\ vpc.todo = {}
    /\ vpc' = VIdle
    /\ UNCHANGED <<disk, cat, vops, nextV, vsess, pc, cnt, nver, ghost>>

ViewAsked ==
    \/ \E p, q \in Paths : VRename(p, q) \/ VReplace(p, q)
    \/ \E F, G \in FNames : VMvDir(F, G) \/ VRmdir(F)

ViewSteps ==
    \/ VRenameDo \/ VReplaceTrash \/ VReplaceOnto \/ VMvDirStep \/ VMvDirEnd \/ VRmdirStep \/ VRmdirEnd
    \/ VStart \/ VCheck \/ VRepriseOp \/ VRepriseEnd

-----------------------------------------------------------------------------
(* A pass of the daemon (§15.3, §15.4, §15.5, §15.12, §21.2 Taken in), each disk up in turn: its walk, then what
   follows there, a copy at a time, then the take-in. A name of an operation of the view planned on that disk, and what
   is below a folder's, is left as it is (jobs.viewPlannedOn). *)

Waited(d, p) == \E v \in VIds :
    /\ vops[v].st = "planned" /\ vops[v].d = d
    /\ \/ p \in {vops[v].p, vops[v].q}
       \/ vops[v].k \in {"mvdir", "rmdir"} /\ p[1] \in {vops[v].p[1], vops[v].q[1]}

RowObj(d, o) == \E g \in FileIds : cc[g][d].st # "none" /\ cc[g][d].obj = o
(* A copy that went through the view's deletion to the trash, whose file's deletion propagates whatever deletes.direct
   says (jobs.readViewOps marks only a deletion's, Replication.tla's, none here). With replace-gone, the remedy proposed
   for flaw 18 (README.md), the copy a replacement sent to the trash too, as §21.2 says of "a copy the view sent to the
   trash". *)
ThroughView(d, o) == "replace-gone" \in Fixes /\ \E v \in OpenOps : vops[v].k = "replace" /\ vops[v].d = d /\ vops[v].tkey = o

(* A copy's verdict on the walk of d; a change, decided once the walk before found it too (pass.isDue). *)
Verdict(f, d) ==
    LET pl == Place(f, d)
        o == cc[f][d].obj
        cand == {q \in Paths : At(d, q) = o} \ {pl}
        to == CHOOSE q \in cand : TRUE
    IN IF Waited(d, pl) \/ Waited(d, cf[f].path) THEN [v |-> "unknown", to |-> None, by |-> 0]
       ELSE IF At(d, pl) = o THEN [v |-> "there", to |-> None, by |-> 0]
       ELSE IF cand # {} THEN IF Waited(d, to) THEN [v |-> "unknown", to |-> None, by |-> 0]
                              ELSE [v |-> "moved", to |-> to, by |-> 0]
       ELSE IF At(d, pl) # 0 /\ ~RowObj(d, At(d, pl)) THEN [v |-> "replaced", to |-> None, by |-> At(d, pl)]
       ELSE [v |-> "gone", to |-> None, by |-> 0]

Changed(f, d) == cf[f].on /\ cc[f][d].st = "ok" /\ Verdict(f, d).v \in {"moved", "replaced", "gone"}
SeenKey(f, d) == <<f, Verdict(f, d).v, Verdict(f, d).to, Verdict(f, d).by>>
Due(f, d) == SeenKey(f, d) \in vseen[d]

(* A copy of a file on another disk than d at a name that disk's walk waits at, or waited at in this pass, its verdict
   unknown, or with a change its walk found first, not yet due: the file's decision waits (solver.Decide, folderWaits),
   as the code decides once every walk of the pass ran; of a disk this pass has not walked yet, what its walk would
   find now. *)
WalkWaits(e) == {p \in Paths : Waited(e, p)}
OtherWaits(f, d) == \E e \in Disks \ {d} :
    /\ cc[f][e].st # "none"
    /\ IF e < d THEN Place(f, e) \in pc.w[e] \/ cf[f].path \in pc.w[e] \/ \E k \in vseen[e] : k[1] = f
       ELSE Place(f, e) \in WalkWaits(e) \/ cf[f].path \in WalkWaits(e) \/ (Changed(f, e) /\ ~Due(f, e))

(* The folders the walk of d finds renamed: an entry whose folder is not at its name, found at a name with none; one
   decided, as FMoves, once the walk before found it too (the debounce, pass.isDue) and no file below it waits
   (jobs.decideFolder: MadeBelow, folderWaits); its clock runs until then. *)
FMovesFound(d) == {m \in FNames \X FNames :
                 /\ m[1] # m[2] /\ ce[d][m[1]] # 0 /\ root[d][m[1]] # ce[d][m[1]]
                 /\ root[d][m[2]] = ce[d][m[1]] /\ ce[d][m[2]] = 0
                 /\ ~Waited(d, <<m[1], 0>>) /\ ~Waited(d, <<m[2], 0>>)}
FMoves(d) == {m \in FMovesFound(d) \cap fseen[d] :
                 ~\E f \in FileIds : cf[f].on /\ cf[f].path[1] = m[1] /\ OtherWaits(f, d)}

Behind(f, d, q) == [e \in Disks |->
    IF e # d /\ cc[f][e].st = "ok" THEN [cc[f][e] EXCEPT !.old = IF Place(f, e) = q THEN None ELSE Place(f, e)]
    ELSE cc[f][e]]

(* What the walk of d decides of a file: [on, path, st, rows]. *)
WalkFile(f, d) ==
    LET r == cc[f][d]
        vd == Verdict(f, d)
        same == [on |-> cf[f].on, path |-> cf[f].path, st |-> cf[f].st, rows |-> cc[f]]
        takenBy(q) == \E g \in FileIds \ {f} : cf[g].on /\ cf[g].path = q
        mv == {m \in FMoves(d) : cf[f].path[1] = m[1]}
    IN IF ~cf[f].on \/ OtherWaits(f, d) THEN same
       ELSE IF r.st # "ok"
       THEN \* no copy here: a folder renamed here that it is below takes it with it
            IF mv # {} /\ ~Waited(d, cf[f].path) /\ ~takenBy(<<(CHOOSE m \in mv : TRUE)[2], cf[f].path[2]>>)
            THEN LET q == <<(CHOOSE m \in mv : TRUE)[2], cf[f].path[2]>> IN
                 [same EXCEPT !.path = q, !.rows = Behind(f, d, q)]
            ELSE same
       ELSE CASE vd.v \in {"unknown", "there"} \/ ~Due(f, d) -> same
              [] vd.v = "moved" /\ r.old = None ->
                   IF takenBy(vd.to) THEN same
                   ELSE [same EXCEPT !.path = vd.to, !.rows = [Behind(f, d, vd.to) EXCEPT ![d] = r]]
              [] vd.v = "moved" /\ vd.to = cf[f].path ->
                   [same EXCEPT !.rows = [cc[f] EXCEPT ![d].old = None]]
              [] vd.v = "moved" -> same
              [] vd.v = "replaced" ->
                   [same EXCEPT !.rows = [e \in Disks |->
                       IF e = d THEN [r EXCEPT !.obj = vd.by]
                       ELSE IF cc[f][e].st = "ok" THEN [cc[f][e] EXCEPT !.st = "stale"] ELSE cc[f][e]]]
              [] OTHER ->
                   IF Direct = "propagate" \/ ThroughView(d, r.obj)
                   THEN LET rows1 == [e \in Disks |->
                                        IF e = d THEN NoCopy
                                        ELSE IF cc[f][e].st \in {"ok", "stale"} THEN [cc[f][e] EXCEPT !.st = "del"]
                                        ELSE IF cc[f][e].st = "pending" THEN NoCopy ELSE cc[f][e]]
                        IN [same EXCEPT !.on = \E e \in Disks : rows1[e].st # "none", !.rows = rows1]
                   ELSE [same EXCEPT !.rows = [cc[f] EXCEPT ![d] = [NoCopy EXCEPT !.st = "pending"]]]

(* What a decision carries out of the view: its operations done by then; one planned then, or journaled since, is not
   (n: the smallest id of those). *)
DecidedN == Min({v \in VIds : vops[v].st = "planned"} \cup {nextV})

PassStart ==
    /\ pc = PIdle
    /\ pc' = [ph |-> "walk", d |-> 1, n |-> DecidedN, w |-> [e \in Disks |-> {}]]
    /\ UNCHANGED <<disk, cat, view, cnt, nver, ghost>>

PassWalk ==
    /\ pc.ph = "walk"
    /\ LET d == pc.d
           W == [f \in FileIds |-> WalkFile(f, d)]
       IN /\ IF Up(d)
             THEN /\ cf' = [f \in FileIds |-> [on |-> W[f].on, path |-> W[f].path, st |-> W[f].st]]
                  /\ cc' = [f \in FileIds |-> W[f].rows]
                  \* a folder found renamed and not yet due keeps its entry, and its new name gets none
                  /\ LET held == {F \in FNames : \E m \in FMovesFound(d) \ FMoves(d) : F \in {m[1], m[2]}}
                     IN ce' = [ce EXCEPT ![d] = [F \in FNames |->
                                 IF Waited(d, <<F, 0>>) \/ F \in held THEN ce[d][F] ELSE root[d][F]]]
                  /\ fseen' = [fseen EXCEPT ![d] = FMovesFound(d) \ FMoves(d)]
                  \* a change's clock runs from the walk that first found it until a decision takes it
                  /\ vseen' = [vseen EXCEPT ![d] = {SeenKey(f, d) : f \in {g \in FileIds :
                                 Changed(g, d) /\ W[g] = [on |-> cf[g].on, path |-> cf[g].path, st |-> cf[g].st, rows |-> cc[g]]}}]
                  \* a further rename moves on the jobs whose target was that folder, one whose two names become
                  \* equal going (catalog.moveFolderJobs); each other disk holding a folder found renamed gets its
                  \* job (jobs.decideFolder)
                  /\ LET moved(j) == IF \E m \in FMoves(d) : m[1] = j[2]
                                     THEN <<j[1], (CHOOSE m \in FMoves(d) : m[1] = j[2])[2], j[3]>> ELSE j
                         new(e) == IF e = d THEN {} ELSE {<<m[1], m[2], DecidedN>> : m \in {x \in FMoves(d) : ce[e][x[1]] # 0}}
                     IN fj' = [e \in Disks |-> {j \in {moved(x) : x \in fj[e]} \cup new(e) : j[1] # j[2]}]
             ELSE UNCHANGED cat
          /\ pc' = IF d < NDisks THEN [pc EXCEPT !.d = d + 1, !.w[d] = IF Up(d) THEN WalkWaits(d) ELSE {}]
                   ELSE [ph |-> "follow", d |-> 1, done |-> {}, n |-> pc.n]
    /\ UNCHANGED <<disk, view, cnt, nver, ghost>>

Work(f, d) == \/ cc[f][d].st = "ok" /\ cc[f][d].old # None
              \/ cc[f][d].st \in {"del", "pending", "stale"}

(* An operation of the view, taken in or not, at, below or above a name, of an id at or past n: with job-view-held,
   the remedy proposed for flaw 22 (README.md), a folder's job decided before one, planned when the pass decided it or
   journaled since, goes, read in the transaction that would journal its rename, for the next pass to decide anew from
   what the disks hold; a file's rename waits for one planned when the pass began or journaled since; one done before,
   or that is the same rename, is what the job carries out. *)
ViewHolds(from, to, n) == "job-view-held" \in Fixes /\ \E v \in VIds :
    /\ v >= n /\ vops[v].st \in {"planned", "done"}
    /\ ~(vops[v].k \in {"mvdir", "rename"} /\ vops[v].p = from /\ vops[v].q = to)
    /\ \E nm \in {from, to}, x \in {vops[v].p, vops[v].q} \ {None} : AtOrBelow(nm, x) \/ AtOrBelow(x, nm)

(* A folder's job, first, as one operation (jobs.runFolderJob): run when the old name holds a folder and the new is
   free, the copies of that disk behind below it then at their file's path; dropped otherwise, its copies following
   one by one. A folder the view gave its name at or after n, on d: what a job decided before must not take from
   there. *)
GivenSince(d, F, n) == \E v \in VIds : v >= n /\ vops[v].k = "mvdir" /\ vops[v].st \in {"planned", "done"}
                                     /\ vops[v].q[1] = F /\ \E r \in vops[v].real : r[1] = d /\ r[2] = root[d][F]
FolderJob(d) ==
    /\ pc.ph = "follow" /\ pc.d <= NDisks /\ Up(pc.d) /\ pc.d = d /\ fj[d] # {}
    /\ LET j == CHOOSE j \in fj[d] : TRUE
           F == j[1]
           G == j[2]
           ready == root[d][F] # 0 /\ root[d][F] = ce[d][F] /\ root[d][G] = 0 /\ ~ViewHolds(<<F, 0>>, <<G, 0>>, j[3])
       IN /\ fj' = [fj EXCEPT ![d] = @ \ {j}]
          /\ IF ready
             THEN /\ root' = [root EXCEPT ![d][G] = root[d][F], ![d][F] = 0]
                  /\ cc' = [f \in FileIds |-> [cc[f] EXCEPT ![d] =
                              IF cc[f][d].st = "ok" /\ cc[f][d].old # None /\ cc[f][d].old[1] = F
                                 /\ cf[f].path = <<G, cc[f][d].old[2]>>
                              THEN [cc[f][d] EXCEPT !.old = None] ELSE cc[f][d]]]
                  /\ ghost' = Flag(ghost, "name-taken-back", GivenSince(d, F, j[3]))
             ELSE UNCHANGED <<root, cc, ghost>>
    /\ UNCHANGED <<objs, trash, dstat, mgen, cf, ce, fseen, vseen, view, pc, cnt, nver>>
Source(f, d) == {e \in Disks \ {d} : CopyAtPath(f, e) /\ Up(e) /\ At(e, cf[f].path) = cc[f][e].obj}

(* What follows on d for one file, as one step: a copy behind renamed to its file's path when that is free; a copy to
   delete sent to the trash when it is as its row says; one to make again or to refresh made from a copy in sync, the
   one it refreshes sent to the trash first, or the file in conflict when its name holds another file. *)
FollowFile(f, d) ==
    LET r == cc[f][d]
        path == cf[f].path
        src == Source(f, d)
    IN CASE r.st = "ok" ->
              IF At(d, r.old) = r.obj /\ At(d, path) = 0 /\ ~Waited(d, path) /\ ~Waited(d, r.old)
                 /\ ~ViewHolds(r.old, path, pc.n)
              THEN LET m == MoveTo(d, r.old, path) IN
                   /\ objs' = m.objs /\ root' = m.root
                   /\ cc' = [cc EXCEPT ![f][d].old = None]
                   /\ UNCHANGED <<trash, cf>>
              ELSE UNCHANGED <<objs, root, trash, cf, cc>>
         [] r.st = "del" ->
              /\ IF At(d, Place(f, d)) = r.obj /\ ~Waited(d, Place(f, d))
                 THEN /\ objs' = [objs EXCEPT ![root[d][Place(f, d)[1]]].kids[Place(f, d)[2]] = 0]
                      /\ trash' = [trash EXCEPT ![d] = @ \cup {r.obj}]
                 ELSE UNCHANGED <<objs, trash>>
              /\ cc' = [cc EXCEPT ![f][d] = NoCopy]
              /\ cf' = [cf EXCEPT ![f].on = \E e \in Disks \ {d} : cc[f][e].st # "none"]
              /\ UNCHANGED root
         [] OTHER ->     \* pending or stale
              IF src = {} \/ Waited(d, path)
              THEN UNCHANGED <<objs, root, trash, cf, cc>>
              ELSE LET e == Min(src)
                       v == objs[cc[f][e].obj].ver
                       mine == r.st = "stale" /\ r.old = None /\ At(d, path) = r.obj
                       objs1 == IF mine THEN [objs EXCEPT ![root[d][path[1]]].kids[path[2]] = 0] ELSE objs
                       free == mine \/ At(d, path) = 0
                       n == Len(objs1) + 1
                       m == PutNew(Append(objs1, NewFile(d, v)), d, path, n)
                   IN IF free
                      THEN /\ objs' = m.objs /\ root' = m.root
                           /\ trash' = IF mine THEN [trash EXCEPT ![d] = @ \cup {r.obj}] ELSE trash
                           /\ cc' = [cc EXCEPT ![f][d] = [NoCopy EXCEPT !.st = "ok", !.obj = n]]
                           /\ UNCHANGED cf
                      ELSE /\ cf' = [cf EXCEPT ![f].st = "conflict"]
                           /\ UNCHANGED <<objs, root, trash, cc>>

PassFollow ==
    /\ pc.ph = "follow" /\ pc.d <= NDisks /\ Up(pc.d) /\ fj[pc.d] = {}
    /\ \E f \in FileIds \ pc.done : Work(f, pc.d)
    /\ LET f == Min({g \in FileIds \ pc.done : Work(g, pc.d)}) IN
       /\ FollowFile(f, pc.d)
       /\ pc' = [pc EXCEPT !.done = @ \cup {f}]
    /\ UNCHANGED <<ce, fj, fseen, vseen, dstat, mgen, view, cnt, nver, ghost>>

PassFollowNext ==
    /\ pc.ph = "follow" /\ pc.d <= NDisks
    /\ ~Up(pc.d) \/ (fj[pc.d] = {} /\ ~\E f \in FileIds \ pc.done : Work(f, pc.d))
    /\ pc' = IF pc.d < NDisks THEN [pc EXCEPT !.d = pc.d + 1, !.done = {}] ELSE [ph |-> "takein"]
    /\ UNCHANGED <<disk, cat, view, cnt, nver, ghost>>

(* view.TakenIn: each operation done, in the order of their ids, once the catalog alone presents at each path it
   touched what the view presents with them all; one not taken in holds back each later one at, below or above one of
   its paths. A path the view presents from a disk is looked at there, which it must be up for; the catalog may present
   it from another copy, once it holds the one the view wrote in sync there (view.inSyncOn). *)
Agrees(x) ==
    IF x[2] = 0 THEN PresentFolder(x[1], OpenOps) = PresentFolder(x[1], {})
    ELSE LET s == Resolved(x, OpenOps)
             w == PresentFile(x, OpenOps)
             c == PresentFile(x, {})
         IN IF s.k = "disk"
            THEN Up(s.d) /\ ((w.k = "none" /\ c.k = "none")
                             \/ (w.k = "file" /\ c.k = "file"
                                 /\ ((c.d = w.d /\ c.o = w.o)
                                     \/ \E f \in CatFiles(x) : CopyAtPath(f, w.d) /\ cc[f][w.d].obj = w.o)))
            ELSE w.k = c.k /\ (w.k = "none" \/ w.d = c.d)

RECURSIVE TakeSet(_, _, _)
TakeSet(i, held, taken) ==
    IF i > MaxView THEN taken
    ELSE IF i \notin OpenOps THEN TakeSet(i + 1, held, taken)
    ELSE LET o == vops[i]
             ps == {o.p, o.q} \ {None}
             blocked == \E x \in ps, h \in held : AtOrBelow(x, h) \/ AtOrBelow(h, x)
             \* with view-moot, the remedy proposed for flaw 20 (README.md): a rename or a replacement whose new name
             \* its disk no longer holds the inode of, as a user moved or removed it directly since, once a pass
             \* walked that disk, taken in as it is, the catalog then presenting what the disks hold
             \* and a folder's whose old name a disk that is there holds a folder at, which no job of that disk will
             \* move: another than the ones it named, as a user made one there directly since, or one it named left
             \* empty there, its copies having followed one by one
             moot == "view-moot" \in Fixes /\
                     \/ o.k \in {"rename", "replace"} /\ Up(o.d) /\ At(o.d, o.q) # o.key
                     \/ o.k = "mvdir" /\ \E e \in Disks :
                            /\ Up(e) /\ root[e][o.p[1]] # 0
                            /\ (root[e][o.p[1]] \notin {r[2] : r \in o.real} \/ objs[root[e][o.p[1]]].kids = NoKids)
                            /\ ~\E j \in fj[e] : j[1] = o.p[1]
                     \* or whose new name none of its holders holds the folder it named at any more
                     \/ o.k = "mvdir" /\ \A r \in o.real : Up(r[1]) /\ root[r[1]][o.q[1]] # r[2]
         IN IF ~blocked /\ o.st = "done" /\ (moot \/ \A x \in ps : Agrees(x))
            THEN TakeSet(i + 1, held, taken \cup {i})
            ELSE TakeSet(i + 1, held \cup ps, taken)

PassTakeIn ==
    /\ pc.ph = "takein"
    /\ LET T == TakeSet(1, {}, {}) IN vops' = [v \in VIds |-> IF v \in T THEN [vops[v] EXCEPT !.taken = TRUE] ELSE vops[v]]
    /\ pc' = PIdle
    /\ UNCHANGED <<disk, cat, nextV, vpc, vsess, cnt, nver, ghost>>

Pass == PassStart \/ PassWalk \/ (\E d \in Disks : FolderJob(d)) \/ PassFollow \/ PassFollowNext \/ PassTakeIn

Next == ViewAsked \/ ViewSteps \/ Pass \/ World

Spec == Init /\ [][Next]_vars

FairSpec == Spec /\ WF_vars(ViewSteps) /\ WF_vars(Pass) /\ \A d \in Disks : WF_vars(Return(d))

-----------------------------------------------------------------------------
(* Invariants. *)

TypeOK ==
    /\ \A v \in VIds : vops[v].st \in {"none", "planned", "done", "undone"}
    /\ \A f \in FileIds, d \in Disks : cc[f][d].st \in {"none", "ok", "del", "pending", "stale"}
    /\ \A d \in Disks : fj[d] \subseteq FNames \X FNames \X (1..(MaxView + 1))

(* No version a user wrote is lost: each is at a path or in the trash of some disk, but for what a user's own write,
   save or deletion destroyed. *)
Held(v) == HeldIn(objs, root, trash, v)
NoUserDataLost == \A v \in ghost.written \ ghost.udest : Held(v)

(* §21.2, The view's journal: an operation is done only once what it names happened on its disk, and undone only
   when it did not, so that the view presents, and a pass waits at, what is there. *)
ViewJournalTrue == ghost.flags \cap {"done-wrong", "undone-wrong"} = {}

(* §21.2, "a disk always holds a path at the path the view presents it at": while the view runs, between its
   operations, and as long as no user acted directly on a disk, a file the view presents can be opened there: what it
   presents from a disk is at that path on it, and of a file it presents from the catalog, a copy at its path on a disk
   that is there is at the path presented, as the view's open gives way to the next copy that holds the file at its
   path (flaw 9's remedy); once nothing of the view is planned, as what a planned operation presents is what it is
   to do. No instance checks it: it fails for the window of one pass (README.md, observation 4). *)
Openable(p) ==
    LET s == Resolved(p, OpenOps)
        w == PresentFile(p, OpenOps)
    IN \/ w.k = "none"
       \/ s.k = "disk" /\ (~Up(w.d) \/ At(w.d, p) # 0)
       \/ s.k = "cat" /\ LET f == Min(CatFiles(CatPath(s.ps)))
                             holding == {e \in Disks : CopyAtPath(f, e) /\ Up(e)}
                         IN holding = {} \/ \E e \in holding : At(e, p) # 0
ViewReadable == (vpc = VIdle /\ ~ghost.direct /\ \A v \in VIds : vops[v].st # "planned") => \A p \in Paths : Openable(p)

(* Atoll's own work never takes from a folder the name a user gave it through the view after Atoll decided that work:
   a job a pass decided before a rename of the view never carries the folder back from the name the view gave it. *)
ViewNamesKept == "name-taken-back" \notin ghost.flags

(* Liveness: no operation of the view stays planned, and each done is taken in at last. *)
EventuallySettled == <>[](\A v \in VIds : vops[v].st # "planned" /\ (vops[v].st = "done" => vops[v].taken))

=============================================================================
