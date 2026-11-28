------------------------------ MODULE ViewNames ------------------------------
(* Atoll's own view: its rename, its replacement onto a name it presents, and its folders renamed or removed on every
   disk at once (specs.md §21.2; internal/view/writer, writer.go Rename, renameFile, replace, renameFolder, Rmdir, and
   reprise.go), a model of its own beside Replication.tla's view and ViewMove.tla: what the view journals and does,
   step by step, what its start resolves of what a stop left planned and its check every 30 s of what a disk lost left
   planned, what it presents, as view.Resolve lays its operations over the catalog, and how the daemon's passes wait at
   them, take what they did to the other disk, and take them in; with atoll mount stopped short (kill -9), a disk lost
   and mounted again, and a user acting directly on a disk, between any two steps.

   What it keeps: folder names at the root (D, E) and file names in a folder (x, y); on each disk the folders at the
   root and the files in each, by object (an inode, with the version it holds, a folder Atoll made for its own rename
   marked so), and its trash; the catalog's files (a
   path, on or off, in conflict or not), their copies (ok, a deletion to make, a copy to make again, or a refresh to
   make; where each sits, its old_lpath) and the entries of the folders; the view's journal, view_ops (kind, disk,
   paths, the inode a file's operation names or a folder's its holder's folder, a replacement's copy sent to the trash
   and its operation trash, state, taken in), the view's process and its handles. A pass: on each disk its walk, which waits at the paths of the view's
   operations planned there (jobs.viewPlannedOn), finds each copy there, moved (a rename, its file's path following,
   the other copies behind), replaced (a modification, the other copies to refresh) or gone (a deletion, its other
   copies to the trash, when deletes.direct is propagate or the copy went through the view's deletion; kept otherwise,
   the copy made again), the folders found renamed, below which the files that only other disks hold follow, and the
   folder entries, each change decided once a later walk of that disk finds it still (the debounce) and no copy of its
   file on another disk waits, a folder found renamed or a copy found moved against a folder's rename of the view
   that stands waiting too (flaw 23); then a folder's rename of the view no walk finds, decided as a folder renamed
   on its disk (jobs.viewFolderMoves); then on each disk its walk found, on that mount, its folder jobs, each as one
   operation, the rename of the view that stands which that disk missed, made at once (jobs.backRenames), and what
   follows, a copy at a time: a copy behind renamed, unless it would go against a folder's
   rename of the view that stands (jobs.viewKeeps), one to delete to the trash, one to make again or to refresh, a
   conflict when its name holds another file, a folder of Atoll's own its rename leaves empty removed; then the
   take-in of the view's operations the pass read when it began, done then (jobs.readViewOps, view.TakenIn), a
   folder's rename waiting for every disk that holds it, for a disk that missed it, and while the catalog holds a
   file below its old name (jobs.missedViewOps).

   What it leaves out: the daemon's own journal and its crashes (each of its operations is one step here; Folders.tla
   and Replication.tla have their steps), the guards and the settle, two copies of a file found moved to two names
   in one pass, which the code decides together, the second a file of its own (solver.Decide, ActOtherTarget), where
   the walks here decide them a disk at a time, what a user makes new being recorded, the power cut (every name is
   durable as it is made), refused and colliding names, folders below folders, the view's other operations
   (Replication.tla's and ViewMove.tla's), and more than two disks. *)
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

ASSUME /\ NFolders \in 1..2 /\ NNames \in 1..2 /\ NFiles \in 1..2
       /\ StartPath \in [FileIds -> Paths] /\ StartOn \in [FileIds -> (SUBSET Disks) \ {{}}]
       /\ StartDirs \in [Disks -> SUBSET FNames]
       /\ \A f \in FileIds, d \in Disks : d \in StartOn[f] => StartPath[f][1] \in StartDirs[d]
       /\ \A f, g \in FileIds : f # g => StartPath[f] # StartPath[g]
       /\ Direct \in {"propagate", "restore"}
       /\ ViewActs \subseteq {"rename", "replace", "mvdir", "rmdir"}
       /\ UserActs \subseteq {"mv", "create", "delete", "save", "write", "mkdir", "rmdir", "mvdir"}
       \* flaws 18 to 23's and observation 5's remedies are specs.md's, the code's and the model's; no flaw open
       /\ Fixes = {}

Min(S) == CHOOSE x \in S : \A y \in S : x <= y
NoKids == [n \in Names |-> 0]
NoCopy == [st |-> "none", obj |-> 0, old |-> None]
(* An operation of the view: kind, the disk it names (a folder's, below), its two paths (a folder's <<F, 0>>), the
   inode a file's names (key), a replacement's copy sent to the trash with the version it held at the look (tkey, tver)
   and that operation trash's state (ts), its state, whether a pass took it in, and for a folder the folders its holders
   held at the old name then, each with its disk, and whether the view itself made what it names on a disk (real and
   did, for the invariants alone). *)
NoV == [k |-> "none", d |-> 0, p |-> None, q |-> None, key |-> 0, tkey |-> 0, tver |-> 0, st |-> "none", ts |-> "none",
        taken |-> FALSE, real |-> {}, did |-> FALSE]
(* d and key, of a folder's rename, are the holder its first rename moves the folder on and that folder, which the
   journal names before that rename, its first holder and 0 until then (view_ops.disk_id and file_key, flaw 19's
   remedy). *)
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
(* ghost.ur: on each disk, the folder a user renamed there directly last, from and to, while nothing else renamed
   it since. *)
NoUr == <<0, 0, 0>>
ClearUr(g, d, k) == IF g.ur[d][1] = k THEN [g EXCEPT !.ur[d] = NoUr] ELSE g
AtIn(os, rt, d, nm) == IF nm[2] = 0 THEN rt[d][nm[1]]
                       ELSE IF rt[d][nm[1]] = 0 THEN 0 ELSE os[rt[d][nm[1]]].kids[nm[2]]
At(d, nm) == AtIn(objs, root, d, nm)
Place(f, d) == IF cc[f][d].old # None THEN cc[f][d].old ELSE cf[f].path
AtOrBelow(x, h) == x = h \/ (h[2] = 0 /\ x[1] = h[1])
(* own: a folder Atoll made for its own rename, which it removes once its own rename empties it (trash.Session
   RemoveEmptied, catalog.OwnFolder); a user's, or one the view made on a user's behalf, it never removes. *)
NewDir(d) == [kind |-> "dir", d |-> d, ver |-> 0, kids |-> NoKids, own |-> FALSE]
NewFile(d, v) == [kind |-> "file", d |-> d, ver |-> v, kids |-> NoKids, own |-> FALSE]

(* The disk once a file at p moves to q on d, the folder of q made first when the disk lacks it (ops.userParent), or
   Atoll's own when own says it is Atoll's rename (ops.makeParents). *)
MoveToBy(d, p, q, own) ==
    LET made == root[d][q[1]] = 0
        os1 == IF made THEN Append(objs, [NewDir(d) EXCEPT !.own = own]) ELSE objs
        rt1 == IF made THEN [root EXCEPT ![d][q[1]] = Len(objs) + 1] ELSE root
        o == At(d, p)
    IN [objs |-> [os1 EXCEPT ![root[d][p[1]]].kids[p[2]] = 0, ![rt1[d][q[1]]].kids[q[2]] = o], root |-> rt1]
MoveTo(d, p, q) == MoveToBy(d, p, q, FALSE)

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
                      IN [kind |-> "dir", d |-> d, ver |-> 0, kids |-> [n \in Names |-> StartAt(d, <<F, n>>)],
                          own |-> FALSE]
                 ELSE LET f == (o - 2 * NFolders - 1) \div 2 + 1
                          d == ((o - 2 * NFolders - 1) % 2) + 1
                      IN [kind |-> "file", d |-> d, ver |-> f, kids |-> NoKids, own |-> FALSE]]
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
    /\ ghost = [written |-> FileIds, udest |-> {}, flags |-> {}, direct |-> FALSE, ur |-> [d \in Disks |-> NoUr]]

-----------------------------------------------------------------------------
(* The world: a user directly on a disk, through mergerfs or the disk itself; a disk lost and mounted again; atoll
   mount stopped short. *)

HeldIn(os, rt, tr, v) == \E d \in Disks :
    \/ \E p \in Paths : AtIn(os, rt, d, p) # 0 /\ os[AtIn(os, rt, d, p)].ver = v
    \/ \E o \in tr[d] : os[o].ver = v

UserOnUr(os, rt, gone, newv, ur) ==
    /\ objs' = os /\ root' = rt
    /\ nver' = IF newv = 0 THEN nver ELSE newv
    /\ cnt' = [cnt EXCEPT !.user = @ + 1]
    /\ ghost' = [ghost EXCEPT !.written = IF newv = 0 THEN @ ELSE @ \cup {newv},
                              !.udest = IF gone # 0 /\ ~HeldIn(os, rt, trash, gone) THEN @ \cup {gone} ELSE @,
                              !.direct = TRUE, !.ur = ur]
    /\ UNCHANGED <<trash, dstat, mgen, cat, view, pc>>

UserOn(os, rt, gone, newv) == UserOnUr(os, rt, gone, newv, ghost.ur)

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
    /\ UserOnUr(objs, [root EXCEPT ![d][G] = root[d][F], ![d][F] = 0], 0, 0,
                [ghost.ur EXCEPT ![d] = <<root[d][F], F, G>>])

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
   cannot be relied on failed. Until a rename went through, the journal names, in a transaction of its own before each,
   the holder and its folder by inode (catalog.NameViewFolder, flaw 19's remedy; one step with the look here). Done once
   it reached one; with none, undone, or left planned when a disk failed so. *)
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
           named == VUp(h) /\ root[h][F] # 0 /\ vpc.moved = 0
       IN /\ CASE vsess[h] = 0 \/ (VUp(h) /\ root[h][F] = 0) ->
                    /\ vpc' = [vpc EXCEPT !.left = rest, !.missed = @ \/ vsess[h] = 0] /\ UNCHANGED root
               [] ~VUp(h) \/ root[h][G] # 0 ->
                    /\ vpc' = [vpc EXCEPT !.left = rest, !.lost = @ \/ ~VUp(h), !.missed = @ \/ ~VUp(h)]
                    /\ UNCHANGED root
               [] OTHER ->
                    /\ root' = [root EXCEPT ![h][G] = root[h][F], ![h][F] = 0]
                    /\ vpc' = [vpc EXCEPT !.left = rest, !.moved = @ + 1]
          /\ vops' = [vops EXCEPT ![vpc.v].did = @ \/ moves,
                                  ![vpc.v].d = IF named THEN h ELSE @,
                                  ![vpc.v].key = IF named THEN root[h][F] ELSE @]
          /\ ghost' = IF moves THEN ClearUr(ghost, h, root[h][F]) ELSE ghost
    /\ UNCHANGED <<objs, trash, dstat, mgen, cat, nextV, vsess, pc, cnt, nver>>

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
       THEN \* flaw 19's remedy (README.md): the folder the journal names by its inode, held at the new name on
            \* the disk it names; none named, nothing was renamed (writer.resolve)
            LET there == o.key # 0 /\ root[d][o.q[1]] = o.key
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
(* A copy that went through the view to the trash, whose file's deletion propagates whatever deletes.direct says: a
   deletion's (Replication.tla's, none here) and the copy a replacement sent there, as §21.2 says of "a copy the view
   sent to the trash" (jobs.sentToTrash, flaw 18's remedy). *)
ThroughView(d, o) == \E v \in OpenOps : vops[v].k = "replace" /\ vops[v].d = d /\ vops[v].tkey = o

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
(* Flaw 23's remedy (README.md; jobs.viewFolderMoves, viewKeeps, backRenames): the last folder's rename of the view at
   its names, done and not taken in, stands while its disk, which the pass walked, or of a disk this pass has not
   walked yet, would walk, holds the folder it names at the new name; a folder found renamed, but one the view made
   there, and a copy found moved, from at or below that new name to at or below its old name, is its disk having
   missed it, and waits, as observation 5's ViewKept has a copy's rename wait; the pass makes that rename there
   (BackRename). *)
HolderSeen(h) == IF pc.ph = "walk" THEN (\E w \in pc.wk : w[1] = h) \/ (h >= pc.d /\ Up(h))
                 ELSE \E w \in pc.wk : w[1] = h
StandsV(v) ==
    /\ v \in OpenOps /\ vops[v].k = "mvdir" /\ vops[v].st = "done" /\ vops[v].key # 0
    /\ HolderSeen(vops[v].d) /\ root[vops[v].d][vops[v].q[1]] = vops[v].key
    /\ ~\E w \in OpenOps : w > v /\ \E x \in {vops[w].p, vops[w].q} \ {None} : x[1] \in {vops[v].p[1], vops[v].q[1]}
BackWaits(from, to) == \E v \in OpenOps : StandsV(v) /\ from[1] = vops[v].q[1] /\ to[1] = vops[v].p[1]
FMoves(d) == {m \in FMovesFound(d) \cap fseen[d] :
                 /\ ~\E f \in FileIds : cf[f].on /\ cf[f].path[1] = m[1] /\ OtherWaits(f, d)
                 /\ ~BackWaits(<<m[1], 0>>, <<m[2], 0>>)}

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
              [] vd.v = "moved" /\ BackWaits(Place(f, d), vd.to) -> same
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

(* Observation 5's remedy (README.md; jobs.viewFolderMoves): a folder's rename of the view, done, no pass has taken in,
   whose disk holds the folder it names, by its inode, at the new name, where that disk's entry already holds it and
   none holds one at the old name, as no walk of that disk found it at the old name in between, while the catalog holds
   a file below the old name, or another disk a copy or an entry there with no job of that rename, and the walk of
   that disk in this pass (walked) finds no folder renamed at either name: taken to the disks it missed as a folder
   renamed on that disk. *)
ViewBackOn(e, P, rows, paths) ==
    \/ ce[e][P] # 0
    \/ \E f \in FileIds : rows[f][e].st \in {"ok", "del", "stale"}
                          /\ (IF rows[f][e].old # None THEN rows[f][e].old ELSE paths[f])[1] = P
ViewBack(v, walked) ==
    LET o == vops[v]
        d == o.d
        P == o.p[1]
        Q == o.q[1]
    IN /\ v \in OpenOps /\ o.k = "mvdir" /\ o.st = "done" /\ o.key # 0 /\ d \in walked
       /\ ~\E w \in OpenOps : w > v /\ \E x \in {vops[w].p, vops[w].q} \ {None} : x[1] \in {P, Q}
       /\ root[d][Q] = o.key /\ ce[d][Q] = o.key /\ ce[d][P] = 0
       /\ ~\E m \in FMovesFound(d) \cup fseen[d] : {m[1], m[2]} \cap {P, Q} # {}
       /\ \/ \E f \in FileIds : cf[f].on /\ cf[f].path[1] = P
          \/ \E e \in Disks \ {d} : /\ ViewBackOn(e, P, cc, [f \in FileIds |-> cf[f].path])
                                   /\ ~\E j \in fj[e] : j[1] = P /\ j[2] = Q

PassStart ==
    /\ pc = PIdle
    \* vb: what the remedy finds before this pass decides anything, which it takes as the code does once the walks ran;
    \* wk: the disks this pass walked, each with the mount it walked
    \* top, dn: the operations of the view the pass read at its start, those done then (jobs.readViewOps)
    /\ pc' = [ph |-> "walk", d |-> 1, n |-> DecidedN, w |-> [e \in Disks |-> {}], wk |-> {}, top |-> nextV,
              dn |-> {v \in VIds : vops[v].st = "done"},
              vb |-> {v \in VIds : ViewBack(v, {e \in Disks : Up(e)})}]
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
          /\ LET wk == IF Up(d) THEN pc.wk \cup {<<d, mgen[d]>>} ELSE pc.wk
             IN pc' = IF d < NDisks THEN [pc EXCEPT !.d = d + 1, !.w[d] = IF Up(d) THEN WalkWaits(d) ELSE {}, !.wk = wk]
                      ELSE [ph |-> "view", n |-> pc.n, w |-> [pc.w EXCEPT ![d] = IF Up(d) THEN WalkWaits(d) ELSE {}],
                            wk |-> wk, vb |-> pc.vb, top |-> pc.top, dn |-> pc.dn]
    /\ UNCHANGED <<disk, view, cnt, nver, ghost>>

(* Observation 5's remedy, once every walk of the pass is decided: a rename ViewBack found before the decisions, and
   finds still, is decided as a folder renamed on its disk (jobs.decideFolder), unless a copy of that disk still sits
   below the old name or a file below it waits; the files below the old name follow it, but one with a copy on that
   disk elsewhere than at its new path, whose copy decides it, or whose new path another file holds; the copies of
   that disk already at their new path no longer behind, the others behind; a job whose target was the old name moves
   on, and each other disk holding a copy or an entry there gets one, unless it has it. One the decisions made it find
   waits for the next pass, whose walks read the rows as they are then; until then, as while one waits, no copy is
   taken from below the new name to below the old (ViewKept). *)
ViewBackWaits(v) ==
    LET o == vops[v]
        d == o.d
        P == o.p[1]
    IN \/ \E f \in FileIds : cc[f][d].st \in {"ok", "del", "stale"} /\ Place(f, d)[1] = P
       \/ \E f \in FileIds : /\ cf[f].on /\ cf[f].path[1] = P
                             /\ \E e \in Disks \ {d} :
                                   /\ cc[f][e].st # "none"
                                   /\ Place(f, e) \in pc.w[e] \/ cf[f].path \in pc.w[e] \/ \E k \in vseen[e] : k[1] = f

ViewBackFile(f, d, P, Q) ==
    LET q == <<Q, cf[f].path[2]>>
        here == cc[f][d]
        follows == /\ cf[f].on /\ cf[f].path[1] = P
                   /\ ~\E g \in FileIds \ {f} : cf[g].on /\ cf[g].path = q
                   /\ here.st = "none" \/ (here.st = "ok" /\ here.old = q)
    IN IF follows
       THEN [path |-> q,
             rows |-> [Behind(f, d, q) EXCEPT ![d] = IF here.st = "ok" THEN [here EXCEPT !.old = None] ELSE here]]
       ELSE [path |-> cf[f].path, rows |-> cc[f]]

(* Flaw 23's remedy: a disk this pass walked that missed a rename standing, holding at its old name, and nothing at
   its new, the folder its entry records at either name, gets that rename, made by this pass (pc.bj), never queued
   as a job that could outlive it. *)
Missed(v, e) ==
    /\ StandsV(v) /\ vops[v].d # e /\ \E w \in pc.wk : w[1] = e
    /\ root[e][vops[v].p[1]] # 0 /\ root[e][vops[v].q[1]] = 0
    /\ root[e][vops[v].p[1]] \in {ce[e][vops[v].p[1]], ce[e][vops[v].q[1]]}
    /\ ~\E j \in fj[e] : j[1] = vops[v].p[1] /\ j[2] = vops[v].q[1]
BackRenames(e) == {<<vops[v].p[1], vops[v].q[1]>> : v \in {v \in VIds : Missed(v, e)}}

PassViewBack ==
    /\ pc.ph = "view"
    /\ LET found == {v \in VIds : ViewBack(v, {w[1] : w \in pc.wk})}
           due == {v \in found \cap pc.vb : ~ViewBackWaits(v)}
       IN IF due = {}
          THEN /\ pc' = [ph |-> "follow", d |-> 1, done |-> {}, n |-> pc.n, wk |-> pc.wk,
                         bj |-> [e \in Disks |-> BackRenames(e)], top |-> pc.top, dn |-> pc.dn]
               /\ UNCHANGED <<cf, cc, ce, fj, fseen, vseen>>
          ELSE LET v == Min(due)
                   d == vops[v].d
                   P == vops[v].p[1]
                   Q == vops[v].q[1]
                   W == [f \in FileIds |-> ViewBackFile(f, d, P, Q)]
                   rows == [f \in FileIds |-> W[f].rows]
                   paths == [f \in FileIds |-> W[f].path]
                   moved(j) == IF j[2] = P THEN <<j[1], Q, j[3]>> ELSE j
                   kept(e) == {j \in {moved(x) : x \in fj[e]} : j[1] # j[2]}
                   new(e) == IF e # d /\ ViewBackOn(e, P, rows, paths) /\ ~\E j \in kept(e) : j[1] = P /\ j[2] = Q
                             THEN {<<P, Q, DecidedN>>} ELSE {}
               IN /\ cf' = [f \in FileIds |-> [cf[f] EXCEPT !.path = paths[f]]]
                  /\ cc' = rows
                  /\ fj' = [e \in Disks |-> kept(e) \cup new(e)]
                  /\ pc' = [ph |-> "follow", d |-> 1, done |-> {}, n |-> pc.n, wk |-> pc.wk,
                            bj |-> [e \in Disks |-> BackRenames(e)], top |-> pc.top, dn |-> pc.dn]
                  /\ UNCHANGED <<ce, fseen, vseen>>
    /\ UNCHANGED <<disk, view, cnt, nver, ghost>>

Work(f, d) == \/ cc[f][d].st = "ok" /\ cc[f][d].old # None
              \/ cc[f][d].st \in {"del", "pending", "stale"}

(* A pass works on a disk only on the mount its walk in that pass found: one missing at its walk, or lost since, is so
   for the rest of the pass (trash.Session: MarkMissing, Lose), whatever mount it is back on meanwhile. *)
Workable(d) == Up(d) /\ <<d, mgen[d]>> \in pc.wk

(* Flaw 22's remedy (README.md), read in the transaction that would journal Atoll's rename (catalog.BeginRename,
   ViewHold): an operation of the view no pass has taken in, at, below or above a name, other than the same rename,
   which is what the job carries out; for a file's rename by a pass, only one planned when the pass began or journaled
   since, of an id at or past n, as one done before is what the pass decided from. A folder's job goes for it, for a
   later pass to decide anew from what the disks hold; a copy's rename waits. The job's own n, what the view had done
   when it was decided, serves ViewNamesKept alone: the code keeps no such mark, and the hold asks of every operation
   not taken in. *)
Same(v, from, to) == vops[v].k \in {"mvdir", "rename"} /\ vops[v].p = from /\ vops[v].q = to
ViewHolds(from, to, n) == \E v \in OpenOps :
    /\ v >= n
    /\ ~Same(v, from, to)
    \* observation 5's remedy: what the rename carries out, an operation of the view not taken in, is held back only
    \* by later ones, as it undoes the earlier ones at its names (catalog.viewHoldsRename)
    /\ ~\E s \in OpenOps : s > v /\ Same(s, from, to)
    /\ \E nm \in {from, to}, x \in {vops[v].p, vops[v].q} \ {None} : AtOrBelow(nm, x) \/ AtOrBelow(x, nm)

(* A folder's job, first, as one operation (jobs.runFolderJob): run when the old name holds a folder and the new is
   free, the copies of that disk behind below it then at their file's path; dropped otherwise, its copies following
   one by one. A folder the view gave its name at or after n, on d: what a job decided before must not take from
   there. *)
GivenSince(d, F, n) == \E v \in VIds : v >= n /\ vops[v].k = "mvdir" /\ vops[v].st \in {"planned", "done"}
                                     /\ vops[v].q[1] = F /\ \E r \in vops[v].real : r[1] = d /\ r[2] = root[d][F]
FolderJob(d) ==
    /\ pc.ph = "follow" /\ pc.d <= NDisks /\ Workable(pc.d) /\ pc.d = d /\ fj[d] # {}
    /\ LET j == CHOOSE j \in fj[d] : TRUE
           F == j[1]
           G == j[2]
           \* the folder its entry records at the old name, or one there where no entry is, as jobs.folderReady
           \* takes it (catalog.EntryKeyAt: no entry, no inode to compare)
           known == root[d][F] = ce[d][F] \/ ce[d][F] = 0
           ready == root[d][F] # 0 /\ known /\ root[d][G] = 0 /\ ~ViewHolds(<<F, 0>>, <<G, 0>>, 1)
       IN /\ fj' = [fj EXCEPT ![d] = @ \ {j}]
          /\ IF ready
             THEN /\ root' = [root EXCEPT ![d][G] = root[d][F], ![d][F] = 0]
                  \* in the job's transaction, the copies behind below F get their place below G, and the entry
                  \* follows (catalog.FinishFolderRename: folderRows, rewriteEntries)
                  /\ cc' = [f \in FileIds |-> [cc[f] EXCEPT ![d] =
                              IF cc[f][d].st = "ok" /\ cc[f][d].old # None /\ cc[f][d].old[1] = F
                              THEN [cc[f][d] EXCEPT !.old = IF cf[f].path = <<G, cc[f][d].old[2]>> THEN None
                                                            ELSE <<G, cc[f][d].old[2]>>]
                              ELSE cc[f][d]]]
                  /\ ce' = IF ce[d][F] # 0 THEN [ce EXCEPT ![d][G] = ce[d][F], ![d][F] = 0] ELSE ce
                  /\ ghost' = ClearUr(Flag(ghost, "name-taken-back", GivenSince(d, F, j[3])), d, root[d][F])
             ELSE UNCHANGED <<root, cc, ce, ghost>>
    /\ UNCHANGED <<objs, trash, dstat, mgen, cf, fseen, vseen, view, pc, cnt, nver>>
Source(f, d) == {e \in Disks \ {d} : CopyAtPath(f, e) /\ Up(e) /\ At(e, cf[f].path) = cc[f][e].obj}

(* Observation 5's remedy (README.md; jobs.viewKept): a copy's rename by a pass waits while it would take the copy from
   below the new name of the last folder's rename of the view at its names, done and not taken in, to below its old
   name, as long as the disk that rename names holds there the folder it names, as this pass's walk of that disk found:
   the rename stands on that disk, and the catalog comes to it. *)
ViewKept(from, to) == \E v \in OpenOps :
    /\ vops[v].k = "mvdir" /\ vops[v].st = "done" /\ vops[v].key # 0
    /\ from[1] = vops[v].q[1] /\ to[1] = vops[v].p[1]
    /\ \E w \in pc.wk : w[1] = vops[v].d
    /\ root[vops[v].d][vops[v].q[1]] = vops[v].key
    /\ ~\E w \in OpenOps : w > v /\ \E x \in {vops[w].p, vops[w].q} \ {None} : x[1] \in {from[1], to[1]}

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
                 /\ ~ViewKept(r.old, path)
              THEN LET m == MoveToBy(d, r.old, path, TRUE)
                       left == m.root[d][r.old[1]]
                   IN
                   /\ objs' = m.objs
                   \* a folder of Atoll's own its rename leaves empty goes
                   /\ root' = IF m.objs[left].own /\ m.objs[left].kids = NoKids
                              THEN [m.root EXCEPT ![d][r.old[1]] = 0] ELSE m.root
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

(* Flaw 23's remedy: a rename of a disk that missed a folder's rename of the view, made in the pass that found it,
   once its folder jobs ran: on the folder at the old name its entry records at either name, the new name free, the
   rename still standing and nothing of the view holding it back, as a folder's job runs (jobs.runFolderJob, at once
   in jobs.backRenames); otherwise it is not made, and a later pass finds it again if it is still to make. *)
BackRename(d) ==
    /\ pc.ph = "follow" /\ pc.d <= NDisks /\ Workable(pc.d) /\ pc.d = d /\ fj[d] = {} /\ pc.bj[d] # {}
    /\ LET j == CHOOSE j \in pc.bj[d] : TRUE
           F == j[1]
           G == j[2]
           ready == /\ root[d][F] # 0 /\ root[d][G] = 0 /\ root[d][F] \in {ce[d][F], ce[d][G]}
                    /\ BackWaits(<<G, 0>>, <<F, 0>>) /\ ~ViewHolds(<<F, 0>>, <<G, 0>>, 1)
       IN /\ pc' = [pc EXCEPT !.bj[d] = @ \ {j}]
          /\ IF ready
             THEN /\ root' = [root EXCEPT ![d][G] = root[d][F], ![d][F] = 0]
                  /\ ghost' = ClearUr(Flag(ghost, "back-undid-user", ghost.ur[d] = <<root[d][F], G, F>>), d,
                                      root[d][F])
                  /\ cc' = [f \in FileIds |-> [cc[f] EXCEPT ![d] =
                              IF cc[f][d].st = "ok" /\ cc[f][d].old # None /\ cc[f][d].old[1] = F
                              THEN [cc[f][d] EXCEPT !.old = IF cf[f].path = <<G, cc[f][d].old[2]>> THEN None
                                                            ELSE <<G, cc[f][d].old[2]>>]
                              ELSE cc[f][d]]]
                  /\ ce' = IF ce[d][F] # 0 THEN [ce EXCEPT ![d][G] = ce[d][F], ![d][F] = 0] ELSE ce
             ELSE UNCHANGED <<root, cc, ce, ghost>>
    /\ UNCHANGED <<objs, trash, dstat, mgen, cf, fj, fseen, vseen, view, cnt, nver>>

PassFollow ==
    /\ pc.ph = "follow" /\ pc.d <= NDisks /\ Workable(pc.d) /\ fj[pc.d] = {} /\ pc.bj[pc.d] = {}
    /\ \E f \in FileIds \ pc.done : Work(f, pc.d)
    /\ LET f == Min({g \in FileIds \ pc.done : Work(g, pc.d)}) IN
       /\ FollowFile(f, pc.d)
       /\ pc' = [pc EXCEPT !.done = @ \cup {f}]
    /\ UNCHANGED <<ce, fj, fseen, vseen, dstat, mgen, view, cnt, nver, ghost>>

PassFollowNext ==
    /\ pc.ph = "follow" /\ pc.d <= NDisks
    /\ ~Workable(pc.d) \/ (fj[pc.d] = {} /\ pc.bj[pc.d] = {} /\ ~\E f \in FileIds \ pc.done : Work(f, pc.d))
    /\ pc' = IF pc.d < NDisks THEN [pc EXCEPT !.d = pc.d + 1, !.done = {}] ELSE [ph |-> "takein", top |-> pc.top, dn |-> pc.dn]
    /\ UNCHANGED <<disk, cat, view, cnt, nver, ghost>>

(* view.TakenIn: each operation done, in the order of their ids, once the catalog alone presents at each path it
   touched what the view presents with them all; one not taken in holds back each later one at, below or above one of
   its paths. A path the view presents from a disk is looked at there, which it must be up for; the catalog may present
   it from another copy, once it holds the one the view wrote in sync there (view.inSyncOn). *)
Agrees(x, os) ==
    IF x[2] = 0 THEN PresentFolder(x[1], os) = PresentFolder(x[1], {})
    ELSE LET s == Resolved(x, os)
             w == PresentFile(x, os)
             c == PresentFile(x, {})
         IN IF s.k = "disk"
            THEN Up(s.d) /\ ((w.k = "none" /\ c.k = "none")
                             \/ (w.k = "file" /\ c.k = "file"
                                 /\ ((c.d = w.d /\ c.o = w.o)
                                     \/ \E f \in CatFiles(x) : CopyAtPath(f, w.d) /\ cc[f][w.d].obj = w.o)))
            ELSE w.k = c.k /\ (w.k = "none" \/ w.d = c.d)

(* The operations the pass read at its start, not taken in then: what it takes in and lays over the catalog; one
   journaled since waits for the next pass, and one planned then too, as jobs.takeInViewOps reads them. *)
PassOps == {v \in OpenOps : v < pc.top}
RECURSIVE TakeSet(_, _, _)
TakeSet(i, held, taken) ==
    IF i > MaxView THEN taken
    ELSE IF i \notin PassOps THEN TakeSet(i + 1, held, taken)
    ELSE LET o == vops[i]
             ps == {o.p, o.q} \ {None}
             blocked == \E x \in ps, h \in held : AtOrBelow(x, h) \/ AtOrBelow(h, x)
             \* flaw 20 and 21's remedy (README.md; jobs.mootViewOps): an operation a user undid directly is taken in
             \* as it is once a pass walked its disks, the catalog then presenting what the disks hold: a rename or a
             \* replacement whose new name its disk no longer holds the inode of; a folder's rename whose new name
             \* holds the folder it renamed on no disk whose entry holds it at either name, on its first holder the
             \* one it records by its inode, on another any folder; and a folder's whose old name a disk holds a
             \* folder at that no job of that disk will move, on its first holder another than the one it records, or
             \* one the catalog holds nothing of that disk below
             F == o.p[1]
             G == o.q[1]
             holders == {e \in Disks : ce[e][F] # 0 \/ ce[e][G] # 0} \cup {o.d}
             gone(e) == IF e = o.d /\ o.key # 0 THEN root[e][G] # o.key ELSE root[e][G] = 0
             emptyHere(e) == ~\E f \in FileIds : cc[f][e].st \in {"ok", "del", "stale"} /\ Place(f, e)[1] = F
             again(e) == /\ Up(e) /\ root[e][F] # 0 /\ ~\E j \in fj[e] : j[1] = F
                         /\ ((e = o.d /\ o.key # 0 /\ root[e][F] # o.key) \/ emptyHere(e))
             moot == \/ o.k \in {"rename", "replace"} /\ Up(o.d) /\ At(o.d, o.q) # o.key
                     \/ o.k = "mvdir" /\ \A e \in holders : Up(e) /\ gone(e)
                     \/ o.k = "mvdir" /\ \E e \in Disks : again(e)
             \* observation 5's remedy (README.md; jobs.missedViewOps): the last folder's rename of the view at its
             \* names, made on its disk, waits while another disk holds at the old name, and none at the new, the
             \* folder its entry records at either name: that disk missed it, and the pass takes it there
             missed == /\ o.k = "mvdir" /\ o.key # 0 /\ Up(o.d) /\ root[o.d][G] = o.key
                       /\ ~\E w \in PassOps : w > i /\ \E x \in {vops[w].p, vops[w].q} \ {None} : x[1] \in {F, G}
                       /\ \E e \in Disks \ {o.d} : /\ Up(e) /\ root[e][F] # 0 /\ root[e][G] = 0
                                                  /\ root[e][F] \in {ce[e][F], ce[e][G]}
             \* and a folder's rename waits for every disk that holds the folder, by the catalog's entries at either
             \* name, to be looked at, not only those the pass could read: one away may have missed it
             away == o.k = "mvdir" /\ \E e \in holders : ~Up(e)
             \* flaw 23's remedy: nor while the catalog holds a file below its old name, which the view presents at
             \* the new one, as the take-in compares the folder's names alone
             below == o.k = "mvdir" /\ \E f \in FileIds : cf[f].on /\ cf[f].path[1] = F
         IN IF ~blocked /\ o.st = "done" /\ i \in pc.dn /\ ~missed /\ ~away /\ (moot \/ (~below /\ \A x \in ps : Agrees(x, PassOps)))
            THEN TakeSet(i + 1, held, taken \cup {i})
            ELSE TakeSet(i + 1, held \cup ps, taken)

PassTakeIn ==
    /\ pc.ph = "takein"
    /\ LET T == TakeSet(1, {}, {}) IN vops' = [v \in VIds |-> IF v \in T THEN [vops[v] EXCEPT !.taken = TRUE] ELSE vops[v]]
    /\ pc' = PIdle
    /\ UNCHANGED <<disk, cat, nextV, vpc, vsess, cnt, nver, ghost>>

Pass == PassStart \/ PassWalk \/ PassViewBack \/ (\E d \in Disks : FolderJob(d) \/ BackRename(d)) \/ PassFollow
        \/ PassFollowNext \/ PassTakeIn

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
   path (flaw 9's remedy), or below a folder's old name on a disk that missed that folder's rename, which a read opens
   there (observation 4's remedy, the maintainer, 2026-11-28); once nothing of the view is planned, as what a planned
   operation presents is what it is to do. Every safety instance of the module checks it since (README.md). *)
Openable(p) ==
    LET s == Resolved(p, OpenOps)
        w == PresentFile(p, OpenOps)
    IN \/ w.k = "none"
       \/ s.k = "disk" /\ (~Up(w.d) \/ At(w.d, p) # 0)
       \* the presented path, or, for reading, the file's name below a folder's old name on a disk that missed that
       \* folder's rename (view.Tree.Open, observation 4, the maintainer's remedy of 2026-11-28)
       \/ s.k = "cat" /\ LET f == Min(CatFiles(CatPath(s.ps)))
                             holding == {e \in Disks : CopyAtPath(f, e) /\ Up(e)}
                         IN holding = {} \/ \E e \in holding : \E i \in 1..Len(s.ps) : At(e, s.ps[i]) # 0
ViewReadable == (vpc = VIdle /\ ~ghost.direct /\ \A v \in VIds : vops[v].st # "planned") => \A p \in Paths : Openable(p)

(* Atoll's own work never takes from a folder the name a user gave it through the view after Atoll decided that work:
   a job a pass decided before a rename of the view never carries the folder back from the name the view gave it. *)
ViewNamesKept == "name-taken-back" \notin ghost.flags

(* The rename a pass makes on a disk that missed a folder's rename of the view (flaw 23's remedy) never takes a folder
   back from the name a user gave it directly on that disk, nothing having renamed it since. It does not hold, and no
   instance checks it: a user's rename back on one disk while the view's rename stands is put back, as the code cannot
   tell it from that disk having missed the view's rename (README.md, flaw 23). *)
BackKeepsUser == "back-undid-user" \notin ghost.flags

(* A folder's rename through the view is kept: once all is still (nothing of the view planned or not taken in, no pass
   under way, every disk there, no job, no change a walk would find or found and has not decided, nothing to follow),
   and as long as no user acted directly, the catalog holds no file below the old name of the last folder's rename the
   view made, the one no later operation of the view touches, as the disks it missed then followed it (observation
   5). *)
Still == /\ pc = PIdle /\ vpc = VIdle
         /\ \A v \in VIds : vops[v].st # "planned" /\ (vops[v].st = "done" => vops[v].taken)
         /\ \A d \in Disks : Up(d) /\ fj[d] = {} /\ fseen[d] = {} /\ vseen[d] = {} /\ FMovesFound(d) = {}
         /\ ~\E f \in FileIds, d \in Disks : Work(f, d) \/ Changed(f, d)
LastFolderRename(v) ==
    /\ vops[v].k = "mvdir" /\ vops[v].st = "done"
    /\ ~\E w \in VIds : /\ w > v /\ vops[w].st \in {"planned", "done"}
                        /\ \E x \in {vops[w].p, vops[w].q} \ {None} : x[1] \in {vops[v].p[1], vops[v].q[1]}
ViewNamesSettled == (Still /\ ~ghost.direct) =>
    \A v \in VIds : LastFolderRename(v) => ~\E f \in FileIds : cf[f].on /\ cf[f].path[1] = vops[v].p[1]

(* Liveness: no operation of the view stays planned, and each done is taken in at last. *)
EventuallySettled == <>[](\A v \in VIds : vops[v].st # "planned" /\ (vops[v].st = "done" => vops[v].taken))

=============================================================================
