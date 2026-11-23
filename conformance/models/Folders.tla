------------------------------- MODULE Folders -------------------------------
(* Folder renames across disks, their jobs, chains, cycles and exchanges: a model of its own beside Rename.tla
   (specs.md §15.3, §15.12, §17.2, and the write 3 of §6.1, a copy or a folder renamed, journaled first). A folder
   renamed directly on one disk, whose copies, and the files below it that only other disks hold, follow it: the
   decision of the folder, its entries, a job for each other disk, and the job run there as one operation; files
   renamed in a chain, one freeing another's name, and in a cycle, which only an exchange of two names in one step
   (renameat2 with RENAME_EXCHANGE) gets round; with Atoll killed between any two steps, the power cut, a disk lost
   and mounted again, and a user acting on any disk at any moment.

   What it keeps: folder names below the root (D, E, F) and file names in a folder (x, y); on each disk the folders at
   the root and the files in each folder, by object (an inode with its creation time), as the kernel shows them and as
   a power cut would leave them; the catalog's files and their copies (where each sits, its file's path or old_lpath,
   and the kind of clock its last look started), the entries of the folders at the root, the folder jobs of each disk
   (kind = rename, file_id NULL) and the journal (rename, of a file or a folder, and mkdir). Atoll is one process: the
   mount check, the reprise of each disk's planned operations (§17.2: a file renamed by the identity of §15.1, a folder
   by its inode, and the commit of a folder's, FinishFolderRename, as a reprise gives it), the walk of each disk and
   its confirmation with the verdicts there and the folders found renamed (§15.10, §15.12, watch.moveFolders), the
   decision Rename by its greatest fixed point, then the folders found renamed (jobs.decideFolder: the wait for the
   copies of that disk below the old name, whether copies came with it, the files that only other disks hold
   following it, its entries, the jobs, and a job moved on by a further rename); then on each writable disk, by
   position (§15.12, step 5): the folder jobs, each in the order of jobs.folderOrder, the first of a cycle dropped, run
   when ready (jobs.folderReady, landsBadly) as one operation rename of the folder, journaled, the look right before,
   RENAME_NOREPLACE, the look after, the sync and the commit, or resolved at once by the table of §17.2
   (trash.Session.AfterFailedMove), and dropped when not ready, its copies then following one by one; then the
   renames by old path, round after round until one moves nothing, a copy below a job still queued waiting for it, each
   as Rename.tla's with its missing parent made in one step; and, once a round moves nothing, the copies left
   path-taken whose targets lead round a cycle, the first of them by place and the copy at its target exchanged
   (jobs.exchange, ops.Disk.ExchangeFiles): both journaled, the look right before at both names, the exchange, the look
   after at both, the sync of both folders and the two commits.

   What it leaves out: the file's content beyond a version (edits, modifications and conflicts are Replication.tla's),
   the trash (a copy gone has its row go: Kept), refused names, NTFS and its names (a disk outside ExDisks only lacks
   the exchange), folders below folders, a folder's mode (Rename.tla's), Atoll's own empty folders (Rename.tla's),
   the guards and the debounce (everything is due), the link variant of a move (Rename.tla's), a copy moved to two
   targets by two disks, more than two disks, and the time of folders. *)
EXTENDS Naturals, FiniteSets, Sequences, TLC

CONSTANTS
    NFolders,   \* folder names at the root, by number: D, E, F
    NNames,     \* file names in a folder, by number: x, y
    NFiles,     \* file ids the catalog may hold
    StartPath,  \* [1..NFiles -> path, or None for no file at the start]
    StartOn,    \* [1..NFiles -> SUBSET Disks]: the disks that hold each file at the start
    StartDirs,  \* [Disks -> SUBSET 1..NFolders]: the folders on each disk at the start
    ExDisks,    \* the disks whose driver exchanges two names (ext4, XFS, btrfs; not NTFS)
    MaxUser,    \* how many times a user acts
    MaxCrash,   \* how many times Atoll is killed
    MaxCut,     \* how many power cuts
    MaxDrop,    \* how many times a disk is lost
    MaxOps,     \* how many operations Atoll may journal
    UserActs,   \* some of "mvdir", "swapdir", "mv", "swap", "create", "delete", "replace", "mkdir", "rmdir"
    Fixes       \* the remedies proposed for the flaws found here and still open, by name (README.md)

NDisks == 2
Disks == 1..NDisks
FNames == 1..NFolders
Names == 1..NNames
Paths == FNames \X Names
None == <<0, 0>>
FileIds == 1..NFiles
OpIds == 1..MaxOps
Acts == {"mvdir", "swapdir", "mv", "swap", "create", "delete", "replace", "mkdir", "rmdir"}
FixNames == {"exchange-back", "folder-back"}

ASSUME /\ NFolders \in 1..3 /\ NNames \in 1..2 /\ NFiles \in 1..3
       /\ StartPath \in [FileIds -> Paths \cup {None}] /\ StartOn \in [FileIds -> SUBSET Disks]
       /\ StartDirs \in [Disks -> SUBSET FNames]
       /\ \A f \in FileIds : (StartPath[f] = None) <=> (StartOn[f] = {})
       /\ \A f \in FileIds, d \in Disks : d \in StartOn[f] => StartPath[f][1] \in StartDirs[d]
       /\ \A f, g \in FileIds : f # g /\ StartPath[f] # None => StartPath[f] # StartPath[g]
       /\ ExDisks \subseteq Disks /\ UserActs \subseteq Acts /\ Fixes \subseteq FixNames

PathLess(p, q) == p[1] < q[1] \/ (p[1] = q[1] /\ p[2] < q[2])
MinPath(S) == CHOOSE p \in S : \A q \in S : p = q \/ PathLess(p, q)
Min(S) == CHOOSE x \in S : \A y \in S : x <= y
NextIn(d, S) == LET c == {e \in S : e > d} IN IF c = {} THEN NDisks + 1 ELSE Min(c)

NoKids == [n \in Names |-> 0]
(* A copy's row: st none or ok, obj its file_key, old its old_lpath (None for none), the verdict of this pass's walk
   with the name it was found moved to, and gk the kind of the clock its last look started (gone_kind with gone_ns):
   none, moved or gone. *)
NoCopy == [st |-> "none", obj |-> 0, old |-> None, v |-> "none", to |-> None, gk |-> "none"]
NoEntry == [obj |-> 0, by |-> "none"]
(* An operation: rename of a file (p, q paths) or of a folder (p, q <<F, 0>>), or mkdir of a folder (p <<F, 0>>). *)
NoOp == [k |-> "none", d |-> 0, p |-> None, q |-> None, key |-> 0, st |-> "none"]
Idle == [ph |-> "idle"]
NoSess == [d \in Disks |-> 0]

VARIABLES
    objs,    \* files and folders: [kind, d, ver, dver (files: content now, after a power cut), kids, dkids (folders), by]
    root,    \* [Disks -> [FNames -> folder or 0]]: the folders at the root, as the kernel shows them
    droot,   \* the same, as a power cut leaves them
    dstat,   \* [Disks -> {"up", "gone"}]
    mgen,    \* [Disks -> Nat]: the mount id
    cf,      \* the catalog's files: [FileIds -> [on, path]]
    cc,      \* their copies: [FileIds -> [Disks -> row]]
    ce,      \* the entries of the folders at the root: [Disks -> [FNames -> [obj, by]]]
    fj,      \* the folder jobs: [Disks -> set of <<From, To>>], each for its dst_disk
    moves,   \* the folders this pass's walk found renamed on each disk: [Disks -> set of <<From, To>>]
    cdisk,   \* each disk's state from the last mount check
    ops,     \* the journal
    nextOp,
    pc,      \* where Atoll's process is
    sess,    \* the mount each disk had when this session confirmed it, plus 1; 0 for none
    cnt,     \* how many of each event of the world so far
    nver,    \* the last version a user wrote
    ghost    \* for the invariants: versions written and destroyed by users, the moves users made, and flags

vars == <<objs, root, droot, dstat, mgen, cf, cc, ce, fj, moves, cdisk, ops, nextOp, pc, sess, cnt, nver, ghost>>
disk == <<objs, root, droot, dstat, mgen>>
cat == <<cf, cc, ce, fj, moves, cdisk, ops, nextOp>>

-----------------------------------------------------------------------------
(* Helpers *)

Up(d) == dstat[d] = "up"
InSess(d) == d \in Disks /\ sess[d] # 0
Ok(d) == Up(d) /\ sess[d] = mgen[d] + 1
Session == {d \in Disks : InSess(d)}
Lose(d) == /\ sess' = [sess EXCEPT ![d] = 0]
           /\ cdisk' = [cdisk EXCEPT ![d] = "missing"]
Flag(g, name, cond) == IF cond THEN [g EXCEPT !.flags = @ \cup {name}] ELSE g

(* What a name holds on a disk, as the kernel shows it: a folder at the root for <<F, 0>>, a file for a path. *)
At(d, nm) == IF nm[2] = 0 THEN root[d][nm[1]]
             ELSE IF root[d][nm[1]] = 0 THEN 0 ELSE objs[root[d][nm[1]]].kids[nm[2]]
(* The same, as a power cut would leave it. *)
DAt(d, p) == IF droot[d][p[1]] = 0 THEN 0 ELSE objs[droot[d][p[1]]].dkids[p[2]]
Held(v) == \E d \in Disks, p \in Paths : DAt(d, p) # 0 /\ objs[DAt(d, p)].dver = v

Place(f, d) == IF cc[f][d].old # None THEN cc[f][d].old ELSE cf[f].path
Ok2(f, d) == cc[f][d].st = "ok"
Planned(d) == {o \in OpIds : ops[o].st = "planned" /\ ops[o].d = d}
(* A name at or below a name of an operation left planned on its disk, which a walk takes as unknown (§15.12). *)
PlannedAt(d, p) == \E o \in Planned(d) : \E nm \in {ops[o].p, ops[o].q} : nm = p \/ (nm[2] = 0 /\ nm[1] = p[1])
Free(S) == CHOOSE g \in S : \A h \in S : g <= h

SyncDir(os, o) == IF o = 0 THEN os ELSE [os EXCEPT ![o].dkids = os[o].kids]
(* A user acts durably, with what came before on that disk. *)
DiskSynced(os, d) == [o \in DOMAIN os |-> IF os[o].d = d THEN [os[o] EXCEPT !.dver = os[o].ver, !.dkids = os[o].kids] ELSE os[o]]

(* The file at p goes to q, on one disk whose folders of both exist; and two names exchanged. *)
MoveName(os, d, p, q) ==
    LET s == root[d][p[1]]
        t == root[d][q[1]]
        o == os[s].kids[p[2]]
    IN [os EXCEPT ![s].kids[p[2]] = 0, ![t].kids[q[2]] = o]
SwapNames(os, d, p, q) ==
    LET s == root[d][p[1]]
        t == root[d][q[1]]
        a == os[s].kids[p[2]]
        b == os[t].kids[q[2]]
    IN [os EXCEPT ![s].kids[p[2]] = b, ![t].kids[q[2]] = a]

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
                          k == [n \in Names |-> StartAt(d, <<F, n>>)]
                      IN [kind |-> "dir", d |-> d, ver |-> 0, dver |-> 0, kids |-> k, dkids |-> k, by |-> "user"]
                 ELSE LET f == (o - 2 * NFolders - 1) \div 2 + 1
                          d == ((o - 2 * NFolders - 1) % 2) + 1
                      IN [kind |-> "file", d |-> d, ver |-> f, dver |-> f, kids |-> NoKids, dkids |-> NoKids, by |-> "user"]]
    /\ root = [d \in Disks |-> [F \in FNames |-> IF F \in StartDirs[d] THEN DirObj(d, F) ELSE 0]]
    /\ droot = root
    /\ dstat = [d \in Disks |-> "up"]
    /\ mgen = [d \in Disks |-> 0]
    /\ cf = [f \in FileIds |-> [on |-> StartPath[f] # None, path |-> IF StartPath[f] # None THEN StartPath[f] ELSE <<1, 1>>]]
    /\ cc = [f \in FileIds |-> [d \in Disks |->
              IF d \in StartOn[f] THEN [NoCopy EXCEPT !.st = "ok", !.obj = FileObj(f, d)] ELSE NoCopy]]
    /\ ce = [d \in Disks |-> [F \in FNames |-> IF F \in StartDirs[d] THEN [obj |-> DirObj(d, F), by |-> "user"] ELSE NoEntry]]
    /\ fj = [d \in Disks |-> {}]
    /\ moves = [d \in Disks |-> {}]
    /\ cdisk = [d \in Disks |-> "online"]
    /\ ops = [o \in OpIds |-> NoOp]
    /\ nextOp = 1
    /\ pc = Idle
    /\ sess = NoSess
    /\ cnt = [user |-> 0, crash |-> 0, cut |-> 0, drop |-> 0]
    /\ nver = NFiles
    /\ ghost = [written |-> {f \in FileIds : StartPath[f] # None}, udest |-> {}, flags |-> {}, umoved |-> {}]

-----------------------------------------------------------------------------
(* The world. A user acts on one disk, durably, with what came before on that disk, as in Rename.tla; a folder a
   file needs is made first. umoved: each object a user moved, with the path it went to, for RenamesAreUsers. *)

UserDo(d, objs1, root1, gone, newv, moved) ==
    LET objs2 == DiskSynced(objs1, d)
        droot2 == [droot EXCEPT ![d] = root1[d]]
        held(v) == \E e \in Disks, p \in Paths :
                       LET fo == droot2[e][p[1]] IN
                       fo # 0 /\ objs2[fo].dkids[p[2]] # 0 /\ objs2[objs2[fo].dkids[p[2]]].dver = v
    IN /\ objs' = objs2 /\ root' = root1 /\ droot' = droot2
       /\ nver' = IF newv = 0 THEN nver ELSE newv
       /\ cnt' = [cnt EXCEPT !.user = @ + 1]
       /\ ghost' = [ghost EXCEPT !.written = IF newv = 0 THEN @ ELSE @ \cup {newv},
                                 !.udest = IF gone # 0 /\ ~held(gone) THEN @ \cup {gone} ELSE @,
                                 !.umoved = @ \cup moved]
       /\ UNCHANGED <<dstat, mgen, cat, pc, sess>>

Allowed(a) == a \in UserActs /\ cnt.user < MaxUser
NewDir(d) == [kind |-> "dir", d |-> d, ver |-> 0, dver |-> 0, kids |-> NoKids, dkids |-> NoKids, by |-> "user"]
NewFile(d, v) == [kind |-> "file", d |-> d, ver |-> v, dver |-> v, kids |-> NoKids, dkids |-> NoKids, by |-> "user"]
FilesIn(o, G) == {<<objs[o].kids[n], <<G, n>>>> : n \in {m \in Names : objs[o].kids[m] # 0}}

(* §15.3, §15.12: mv D E on one disk, with all it holds. *)
UMvDir(d, F, G) ==
    /\ Allowed("mvdir") /\ Up(d) /\ F # G /\ root[d][F] # 0 /\ root[d][G] = 0
    /\ UserDo(d, objs, [root EXCEPT ![d][G] = root[d][F], ![d][F] = 0], 0, 0, FilesIn(root[d][F], G))

(* Two folders swap their names, through a third. *)
USwapDir(d, F, G) ==
    /\ Allowed("swapdir") /\ Up(d) /\ F < G /\ root[d][F] # 0 /\ root[d][G] # 0
    /\ UserDo(d, objs, [root EXCEPT ![d][G] = root[d][F], ![d][F] = root[d][G]], 0, 0,
              FilesIn(root[d][F], G) \cup FilesIn(root[d][G], F))

UMv(d, p, q) ==
    /\ Allowed("mv") /\ Up(d) /\ p # q /\ At(d, p) # 0 /\ At(d, q) = 0
    /\ LET made == root[d][q[1]] = 0
           objs1 == IF made THEN Append(objs, NewDir(d)) ELSE objs
           root1 == IF made THEN [root EXCEPT ![d][q[1]] = Len(objs) + 1] ELSE root
           s == root[d][p[1]]
           t == root1[d][q[1]]
       IN UserDo(d, [objs1 EXCEPT ![s].kids[p[2]] = 0, ![t].kids[q[2]] = At(d, p)], root1, 0, 0, {<<At(d, p), q>>})

(* Two files swap their names, through a third, or with RENAME_EXCHANGE. *)
USwap(d, p, q) ==
    /\ Allowed("swap") /\ Up(d) /\ PathLess(p, q) /\ At(d, p) # 0 /\ At(d, q) # 0
    /\ UserDo(d, SwapNames(objs, d, p, q), root, 0, 0, {<<At(d, p), q>>, <<At(d, q), p>>})

(* A new file where the name is free on that disk: a new release imported at the old name, say. *)
UCreate(d, p) ==
    /\ Allowed("create") /\ Up(d) /\ At(d, p) = 0
    /\ LET made == root[d][p[1]] = 0
           objs1 == IF made THEN Append(objs, NewDir(d)) ELSE objs
           root1 == IF made THEN [root EXCEPT ![d][p[1]] = Len(objs) + 1] ELSE root
           n == Len(objs1) + 1
           t == root1[d][p[1]]
       IN UserDo(d, [Append(objs1, NewFile(d, nver + 1)) EXCEPT ![t].kids[p[2]] = n], root1, 0, nver + 1, {})

UDelete(d, p) ==
    /\ Allowed("delete") /\ Up(d) /\ At(d, p) # 0
    /\ UserDo(d, [objs EXCEPT ![root[d][p[1]]].kids[p[2]] = 0], root, objs[At(d, p)].ver, 0, {})

(* An atomic save, rename(tmp, file): a new file at the name, the one it replaces gone. *)
UReplace(d, p) ==
    /\ Allowed("replace") /\ Up(d) /\ At(d, p) # 0
    /\ UserDo(d, [Append(objs, NewFile(d, nver + 1)) EXCEPT ![root[d][p[1]]].kids[p[2]] = Len(objs) + 1], root,
              objs[At(d, p)].ver, nver + 1, {})

UMkdir(d, F) ==
    /\ Allowed("mkdir") /\ Up(d) /\ root[d][F] = 0
    /\ UserDo(d, Append(objs, NewDir(d)), [root EXCEPT ![d][F] = Len(objs) + 1], 0, 0, {})

URmdir(d, F) ==
    /\ Allowed("rmdir") /\ Up(d) /\ root[d][F] # 0 /\ objs[root[d][F]].kids = NoKids
    /\ UserDo(d, objs, [root EXCEPT ![d][F] = 0], 0, 0, {})

Drop(d) ==
    /\ cnt.drop < MaxDrop /\ Up(d)
    /\ dstat' = [dstat EXCEPT ![d] = "gone"]
    /\ cnt' = [cnt EXCEPT !.drop = @ + 1]
    /\ UNCHANGED <<objs, root, droot, mgen, cat, pc, sess, nver, ghost>>

Return(d) ==
    /\ dstat[d] = "gone"
    /\ dstat' = [dstat EXCEPT ![d] = "up"]
    /\ mgen' = [mgen EXCEPT ![d] = @ + 1]
    /\ root' = [root EXCEPT ![d] = droot[d]]
    /\ objs' = [o \in DOMAIN objs |-> IF objs[o].d = d THEN [objs[o] EXCEPT !.ver = objs[o].dver, !.kids = objs[o].dkids] ELSE objs[o]]
    /\ UNCHANGED <<droot, cat, pc, sess, cnt, nver, ghost>>

Crash ==
    /\ cnt.crash < MaxCrash /\ pc # Idle
    /\ pc' = Idle /\ sess' = NoSess
    /\ cnt' = [cnt EXCEPT !.crash = @ + 1]
    /\ UNCHANGED <<disk, cat, nver, ghost>>

PowerCut ==
    /\ cnt.cut < MaxCut
    /\ root' = droot
    /\ objs' = [o \in DOMAIN objs |-> [objs[o] EXCEPT !.ver = objs[o].dver, !.kids = objs[o].dkids]]
    /\ pc' = Idle /\ sess' = NoSess
    /\ cnt' = [cnt EXCEPT !.cut = @ + 1]
    /\ UNCHANGED <<droot, dstat, mgen, cat, nver, ghost>>

World ==
    \/ \E d \in Disks, F, G \in FNames : UMvDir(d, F, G) \/ USwapDir(d, F, G)
    \/ \E d \in Disks, p, q \in Paths : UMv(d, p, q) \/ USwap(d, p, q)
    \/ \E d \in Disks, p \in Paths : UCreate(d, p) \/ UDelete(d, p) \/ UReplace(d, p)
    \/ \E d \in Disks, F \in FNames : UMkdir(d, F) \/ URmdir(d, F)
    \/ \E d \in Disks : Drop(d) \/ Return(d)
    \/ Crash \/ PowerCut

-----------------------------------------------------------------------------
(* The commits that move rows. *)

(* FinishRename: the copy of that inode on that disk, where its row says it sits, takes the target: old_lpath NULL when
   it is its file's path, the target otherwise, its clock stopped (catalog.renameRow). *)
RenameRows(d, op) ==
    [f \in FileIds |-> [e \in Disks |->
        IF e = d /\ Ok2(f, e) /\ cc[f][e].obj = op.key /\ Place(f, e) = op.p
        THEN [cc[f][e] EXCEPT !.old = IF cf[f].path = op.q THEN None ELSE op.q, !.gk = "none"]
        ELSE cc[f][e]]]

(* FinishFolderRename's rows (catalog.folderRows): each copy of that disk behind below the old name with no clock
   running, and in a reprise also one whose clock says moved, and one at its own file's path below it with another copy
   at its own path elsewhere, which a rename back left so (§17.2), takes the new name below the new folder. *)
FolderRows(d, F, G, reprise) ==
    [f \in FileIds |-> [e \in Disks |->
        LET r == cc[f][e]
            behind == r.old # None /\ r.old[1] = F /\ (r.gk = "none" \/ (reprise /\ r.gk = "moved"))
            own == reprise /\ r.old = None /\ cf[f].path[1] = F /\ r.gk \in {"none", "moved"}
                   /\ \E x \in Disks \ {e} : Ok2(f, x) /\ cc[f][x].old = None
            next == <<G, Place(f, e)[2]>>
        IN IF e = d /\ Ok2(f, e) /\ cf[f].on /\ (behind \/ own)
           THEN [r EXCEPT !.old = IF next = cf[f].path THEN None ELSE next, !.gk = "none"]
           ELSE r]]

Entries(d, F, G) == [ce EXCEPT ![d][G] = ce[d][F], ![d][F] = NoEntry]

(* A further rename moves on the jobs whose target was that folder; one whose two names become equal goes
   (catalog.moveFolderJobs). *)
MovedJobs(F, G) == [e \in Disks |-> {IF j[2] = F THEN <<j[1], G>> ELSE j : j \in fj[e]} \ {<<G, G>>}]

-----------------------------------------------------------------------------
(* §15.12 steps 1 and 2: the mount check, recorded, the session's disks, and the reprise of each. *)

PassStart ==
    /\ pc = Idle
    /\ cdisk' = [d \in Disks |-> IF Up(d) THEN "online" ELSE "missing"]
    /\ sess' = [d \in Disks |-> IF Up(d) THEN mgen[d] + 1 ELSE 0]
    /\ pc' = [ph |-> "reprise", d |-> NextIn(0, {d \in Disks : Up(d)})]
    /\ UNCHANGED <<disk, cf, cc, ce, fj, moves, ops, nextOp, cnt, nver, ghost>>

(* §17.2, ops.Disk.recoverRename: the way to the target synced, then both names looked at, a file by the identity of
   §15.1, a folder by its inode; the target holding it finishes, the source's name going if it holds it too, and the
   folders synced; anything else is undone, both folders synced. Names and commit in one step. A folder's finishes as
   FinishFolderRename(op, 0) does: its rows as a reprise moves them, its entries, and the job of that disk at that
   old name dropped. *)
Fin(d, o) == At(d, ops[o].q) = ops[o].key
Resolved(d, o) ==
    LET op == ops[o]
        fin == Fin(d, o)
        both == At(d, op.q) = op.key /\ At(d, op.p) = op.key
        objs1 == IF op.p[2] # 0 /\ both THEN [objs EXCEPT ![root[d][op.p[1]]].kids[op.p[2]] = 0] ELSE objs
        objs2 == IF op.p[2] = 0 THEN objs1 ELSE SyncDir(SyncDir(objs1, root[d][op.q[1]]), root[d][op.p[1]])
    IN [fin |-> fin, objs |-> objs2]

ResolveRows(d, o, fin) ==
    LET op == ops[o] IN
    IF ~fin THEN [cc |-> cc, ce |-> ce, fj |-> fj]
    ELSE IF op.p[2] # 0 THEN [cc |-> RenameRows(d, op), ce |-> ce, fj |-> fj]
    ELSE [cc |-> FolderRows(d, op.p[1], op.q[1], TRUE), ce |-> Entries(d, op.p[1], op.q[1]),
          fj |-> [fj EXCEPT ![d] = {j \in @ : j[1] # op.p[1]}]]

RepOp ==
    /\ pc.ph = "reprise" /\ pc.d <= NDisks /\ Ok(pc.d) /\ Planned(pc.d) # {}
    /\ LET d == pc.d
           o == Min(Planned(d))
           r == Resolved(d, o)
           rows == ResolveRows(d, o, r.fin)
       IN /\ objs' = r.objs
          /\ droot' = [droot EXCEPT ![d] = root[d]]
          /\ ops' = [ops EXCEPT ![o].st = IF r.fin THEN "done" ELSE "undone"]
          /\ cc' = rows.cc /\ ce' = rows.ce /\ fj' = rows.fj
    /\ UNCHANGED <<root, dstat, mgen, cf, moves, cdisk, nextOp, pc, sess, cnt, nver, ghost>>

RepNext ==
    /\ pc.ph = "reprise" /\ pc.d <= NDisks /\ (~Ok(pc.d) \/ Planned(pc.d) = {})
    /\ IF Ok(pc.d) THEN UNCHANGED <<sess, cdisk>> ELSE Lose(pc.d)
    /\ pc' = [ph |-> "reprise", d |-> NextIn(pc.d, Session)]
    /\ UNCHANGED <<disk, cf, cc, ce, fj, moves, ops, nextOp, cnt, nver, ghost>>

RepEnd ==
    /\ pc.ph = "reprise" /\ pc.d > NDisks
    /\ pc' = [ph |-> "walk", d |-> NextIn(0, Session)]
    /\ UNCHANGED <<disk, cat, sess, cnt, nver, ghost>>

-----------------------------------------------------------------------------
(* §15.12 step 3, §15.10: the walk of each disk of the session, then its confirmation, then the verdicts, the folders
   found renamed and what is new, in one transaction. A folder row whose folder is not at its name, found at a name
   with no row, and whose old name holds no file at home, is a folder renamed (watch.moveFolders): its row stays, and
   its new name and all below it get none, until it is decided. A copy's verdict: unknown at a name of an operation
   planned there; there; moved when its place does not hold it and another name does; gone when its place holds what
   another row's copy moved there, or nothing; replaced when its place holds another file. *)

WalkStart ==
    /\ pc.ph = "walk" /\ pc.d <= NDisks
    /\ pc' = [ph |-> "walkend", d |-> pc.d, mg |-> mgen[pc.d], real |-> Up(pc.d),
              fs |-> [p \in Paths |-> IF Up(pc.d) THEN At(pc.d, p) ELSE 0],
              ds |-> [F \in FNames |-> IF Up(pc.d) THEN root[pc.d][F] ELSE 0]]
    /\ UNCHANGED <<disk, cat, sess, cnt, nver, ghost>>

WalkCommit(d, fs, ds) ==
    LET known(o) == \E f \in FileIds : Ok2(f, d) /\ cc[f][d].obj = o
        home(F) == \E f \in FileIds : Ok2(f, d) /\ Place(f, d)[1] = F /\ fs[Place(f, d)] = cc[f][d].obj
        mv == {m \in FNames \X FNames :
                  LET F == m[1]
                      G == m[2]
                  IN /\ ce[d][F].obj # 0 /\ ds[F] # ce[d][F].obj /\ ds[G] = ce[d][F].obj /\ ce[d][G].obj = 0
                     /\ ~home(F) /\ ~PlannedAt(d, <<F, 0>>) /\ ~PlannedAt(d, <<G, 0>>)}
        tops == {m[2] : m \in mv}
        kept == {m[1] : m \in mv}
        movedTo(f) == {q \in Paths : q # Place(f, d) /\ fs[q] = cc[f][d].obj}
        \* the place holds another row's copy, moved there or at home there, as a half-resolved exchange leaves it: the
        \* code reads that copy gone, and decides Kept (internal/jobs flaws test of 2026-11-23, README.md, observation 3)
        claimed(p, f) == \E g \in FileIds : g # f /\ Ok2(g, d) /\ fs[p] = cc[g][d].obj /\ ~PlannedAt(d, Place(g, d))
        verdict(f) ==
            LET pl == Place(f, d) IN
            IF PlannedAt(d, pl) THEN "unknown"
            ELSE IF fs[pl] = cc[f][d].obj THEN "there"
            ELSE IF movedTo(f) # {} THEN "moved"
            ELSE IF fs[pl] # 0 /\ ~claimed(pl, f) THEN "replaced"
            ELSE "gone"
        clock(v, old) == CASE v = "moved" -> "moved" [] v = "gone" -> "gone" [] v = "unknown" -> old [] OTHER -> "none"
        cc1 == [f \in FileIds |-> [e \in Disks |->
                  IF e # d \/ ~Ok2(f, e) THEN cc[f][e]
                  ELSE LET v == verdict(f) IN
                       [cc[f][e] EXCEPT !.v = v, !.to = IF v = "moved" THEN CHOOSE q \in movedTo(f) : TRUE ELSE None,
                                        !.gk = clock(v, cc[f][e].gk)]]]
        newAt(p) == fs[p] # 0 /\ ~known(fs[p]) /\ ~PlannedAt(d, p) /\ p[1] \notin tops
        atPath(p) == {f \in FileIds : cf[f].on /\ cf[f].path = p}
        \* a copy new to the catalog of the file at that path, when that file has none on this disk
        joins(f, p) == newAt(p) /\ f \in atPath(p) /\ cc1[f][d].st = "none"
        cc2 == [f \in FileIds |-> [e \in Disks |->
                  IF e = d /\ \E p \in Paths : joins(f, p)
                  THEN [NoCopy EXCEPT !.st = "ok", !.obj = fs[CHOOSE p \in Paths : joins(f, p)], !.v = "there"]
                  ELSE cc1[f][e]]]
        \* a file new to the catalog, where no file row holds the path
        fresh == {p \in Paths : newAt(p) /\ atPath(p) = {} /\ \E g \in FileIds : ~cf[g].on}
        pnew == IF fresh = {} THEN None ELSE MinPath(fresh)
        g == IF pnew = None THEN 0 ELSE Free({h \in FileIds : ~cf[h].on})
    IN /\ cc' = IF pnew = None THEN cc2
                ELSE [cc2 EXCEPT ![g] = [e \in Disks |-> IF e = d THEN [NoCopy EXCEPT !.st = "ok", !.obj = fs[pnew], !.v = "there"]
                                                         ELSE NoCopy]]
       /\ cf' = IF pnew = None THEN cf ELSE [cf EXCEPT ![g] = [on |-> TRUE, path |-> pnew]]
       /\ ce' = [ce EXCEPT ![d] = [F \in FNames |->
                   IF F \in kept \/ F \in tops \/ PlannedAt(d, <<F, 0>>) THEN ce[d][F]
                   ELSE IF ds[F] = 0 THEN NoEntry
                   ELSE IF ds[F] = ce[d][F].obj THEN ce[d][F]
                   ELSE [obj |-> ds[F], by |-> "user"]]]
       /\ moves' = [moves EXCEPT ![d] = mv]

WalkEnd ==
    /\ pc.ph = "walkend"
    /\ LET d == pc.d IN
       IF Up(d) /\ mgen[d] = pc.mg /\ InSess(d)
       THEN /\ WalkCommit(d, pc.fs, pc.ds)
            /\ ghost' = Flag(ghost, "inv4", ~pc.real)
            /\ UNCHANGED <<sess, cdisk>>
       ELSE /\ Lose(d)
            /\ UNCHANGED <<cf, cc, ce, moves, ghost>>
    /\ pc' = [ph |-> "walk", d |-> NextIn(pc.d, Session)]
    /\ UNCHANGED <<disk, fj, ops, nextOp, cnt, nver>>

WalkDone ==
    /\ pc.ph = "walk" /\ pc.d > NDisks
    /\ pc' = [ph |-> "decide"]
    /\ UNCHANGED <<disk, cat, sess, cnt, nver, ghost>>

-----------------------------------------------------------------------------
(* §15.12 step 4, the decisions about the files, in one step: a file with a copy moved takes its target, the target of
   its first moved copy by position, by the greatest fixed point: start with every file with a copy moved, drop any
   whose target a file row outside the set holds, or a file of the set that stays at its own path, or one first by
   path wants too, and repeat. Its copy moved there loses old_lpath and its clock, a copy gone has its row go, the
   others get old_lpath where they sit, a disk away included. A file left with no free target is path-taken: its
   moved copy keeps its clock. A copy gone of another file has its row go (Kept), and a file with no copy left goes.
   First, as the rows stand before any decision (jobs.readFolders), whether copies came with each folder found
   renamed: a copy of that disk sits below its new name with no clock, or one found moved there sat below its old
   name (jobs.isRealRename). *)

MovedOn(f) == {d \in Session : Ok2(f, d) /\ cc[f][d].v = "moved"}
Tgt(f) == cc[f][Min(MovedOn(f))].to
Keep(S) == {f \in S :
               /\ ~\E g \in FileIds \ S : g # f /\ cf[g].on /\ cf[g].path = Tgt(f)
               /\ ~\E g \in S : g # f /\ cf[g].path = Tgt(f) /\ Tgt(g) = cf[g].path
               /\ ~\E h \in S : h # f /\ Tgt(h) = Tgt(f) /\ Tgt(f) # cf[f].path /\ PathLess(cf[h].path, cf[f].path)}
RECURSIVE Fix(_)
Fix(S) == IF Keep(S) = S THEN S ELSE Fix(Keep(S))

RealMove(d, m) ==
    \/ \E f \in FileIds : Ok2(f, d) /\ Place(f, d)[1] = m[2] /\ cc[f][d].gk = "none"
    \/ \E f \in FileIds : Ok2(f, d) /\ cc[f][d].v = "moved" /\ cc[f][d].to[1] = m[2] /\ Place(f, d)[1] = m[1]

DecideFiles ==
    /\ pc.ph = "decide"
    /\ LET S0 == {f \in FileIds : cf[f].on /\ MovedOn(f) # {}}
           R == Fix(S0)
           row(f, d) ==
               LET r == cc[f][d] IN
               IF r.st # "ok" THEN r
               ELSE IF ~InSess(d) THEN IF f \in R THEN [r EXCEPT !.old = IF Place(f, d) = Tgt(f) THEN None ELSE Place(f, d)] ELSE r
               ELSE IF f \in R
               THEN CASE r.v = "moved" /\ r.to = Tgt(f) -> [r EXCEPT !.old = None, !.gk = "none"]
                      [] r.v = "gone" -> NoCopy
                      [] r.v = "moved" -> r
                      [] OTHER -> [r EXCEPT !.old = IF Place(f, d) = Tgt(f) THEN None ELSE Place(f, d)]
               ELSE IF r.v = "gone" THEN NoCopy
               ELSE r
           cc1 == [f \in FileIds |-> [d \in Disks |-> row(f, d)]]
           left(f) == \E d \in Disks : cc1[f][d].st = "ok"
           \* a copy behind whose old_lpath a rename back clears: a folder job above it goes, as it would carry it away
           \* again (catalog.dropJobsAbove, §15.12)
           cleared(d) == {cc[f][d].old : f \in {g \in R : Ok2(g, d) /\ cc[g][d].old # None /\ cc[g][d].v # "moved"
                                                          /\ Place(g, d) = Tgt(g)}}
       IN /\ cc' = [f \in FileIds |-> IF left(f) THEN cc1[f] ELSE [d \in Disks |-> NoCopy]]
          /\ fj' = [d \in Disks |-> {j \in fj[d] : ~\E c \in cleared(d) : c[1] = j[1]}]
          /\ cf' = [f \in FileIds |->
                IF ~left(f) THEN [cf[f] EXCEPT !.on = FALSE]
                ELSE IF f \in R THEN [cf[f] EXCEPT !.path = Tgt(f)] ELSE cf[f]]
          /\ pc' = [ph |-> "folders",
                    left |-> UNION {{<<d, m, RealMove(d, m)>> : m \in moves[d]} : d \in Session},
                    \* what no pass can take from below a folder: replaced, or moved to a name another file holds
                    stay |-> {<<f, d>> \in FileIds \X Disks : Ok2(f, d) /\ (cc[f][d].v = "replaced" \/ (cc[f][d].v = "moved" /\ f \notin R))},
                    moved |-> S0]
          \* a file takes only a path a user moved one of its copies to; a copy that caught up with its file's own path
          \* is no rename
          /\ ghost' = Flag(ghost, "rename-reverted",
                           \E f \in R : Tgt(f) # cf[f].path /\ <<cc[f][Min(MovedOn(f))].obj, Tgt(f)>> \notin ghost.umoved)
    /\ UNCHANGED <<disk, ce, moves, cdisk, ops, nextOp, sess, cnt, nver>>

(* The folders found renamed, each in its own step, in the order of their jobs (jobs.movesInOrder): one that lands in
   another's old name after that one. A folder whose disk still holds copies below its old name, but for those no pass
   can take from there, waits. One no copy came with is only its entries, and the jobs it moves on. Otherwise, in one
   transaction: each file below the old name with copies on other disks only, none moved in this pass, follows it if
   its new path is free, its copies getting old_lpath where they sit; the entries; the jobs moved on; and a job for
   each other disk holding a copy below the old name. *)
DecideFolder ==
    /\ pc.ph = "folders" /\ pc.left # {}
    /\ LET waits(x, y) == x[1] = y[1] /\ x[2][2] = y[2][1] /\ x # y
           ready == {x \in pc.left : ~\E y \in pc.left : waits(x, y)}
           pool == IF ready # {} THEN ready ELSE pc.left
           x == CHOOSE x \in pool : \A y \in pool : x[1] < y[1] \/ (x[1] = y[1] /\ x[2][1] <= y[2][1])
           d == x[1]
           F == x[2][1]
           G == x[2][2]
           below == {f \in FileIds : Ok2(f, d) /\ Place(f, d)[1] = F /\ <<f, d>> \notin pc.stay}
           followers == {g \in FileIds : cf[g].on /\ cf[g].path[1] = F /\ ~Ok2(g, d) /\ g \notin pc.moved
                                         /\ \E e \in Disks : Ok2(g, e)}
           to(g) == <<G, cf[g].path[2]>>
           follows == {g \in followers : ~\E h \in FileIds : h # g /\ cf[h].on /\ cf[h].path = to(g)}
           cf1 == [g \in FileIds |-> IF g \in follows THEN [cf[g] EXCEPT !.path = to(g)] ELSE cf[g]]
           cc1 == [g \in FileIds |-> [e \in Disks |->
                     IF g \in follows /\ Ok2(g, e) THEN [cc[g][e] EXCEPT !.old = IF Place(g, e) = to(g) THEN None ELSE Place(g, e)]
                     ELSE cc[g][e]]]
           others == {e \in Disks \ {d} : \E g \in FileIds : cc1[g][e].st = "ok" /\ cf1[g].on
                                                          /\ (IF cc1[g][e].old # None THEN cc1[g][e].old ELSE cf1[g].path)[1] = F}
           jobs == MovedJobs(F, G)
       IN /\ pc' = [pc EXCEPT !.left = @ \ {x}]
          /\ IF below # {}
             THEN UNCHANGED <<cf, cc, ce, fj>>
             ELSE IF ~x[3]
             THEN /\ ce' = Entries(d, F, G) /\ fj' = jobs
                  /\ UNCHANGED <<cf, cc>>
             ELSE /\ cf' = cf1 /\ cc' = cc1 /\ ce' = Entries(d, F, G)
                  /\ fj' = [e \in Disks |-> IF e \in others THEN jobs[e] \cup {<<F, G>>} ELSE jobs[e]]
    /\ UNCHANGED <<disk, moves, cdisk, ops, nextOp, sess, cnt, nver, ghost>>

DecideDone ==
    /\ pc.ph = "folders" /\ pc.left = {}
    /\ cc' = [f \in FileIds |-> [d \in Disks |-> [cc[f][d] EXCEPT !.v = "none", !.to = None]]]
    /\ moves' = [d \in Disks |-> {}]
    /\ pc' = [ph |-> "fjobs", d |-> NextIn(0, Session), done |-> {}]
    /\ UNCHANGED <<disk, cf, ce, fj, cdisk, ops, nextOp, sess, cnt, nver, ghost>>

-----------------------------------------------------------------------------
(* §15.12 step 5, on each writable disk by position: first its folder jobs (jobs.runFolderJobs). A job that waits for
   another of the disk goes after it: its new name is that one's old name. A cycle: the first of it goes. A job runs
   when its folder is there, its new name absent, and no copy at its own file's path below it would land on a name a
   file row holds (jobs.folderReady, landsBadly); otherwise it goes, and the copies follow one by one. *)

NextDisk(d) == [ph |-> "fjobs", d |-> NextIn(d, Session), done |-> {}]

LandsBadly(d, j) ==
    \E f \in FileIds : Ok2(f, d) /\ cc[f][d].old = None /\ cf[f].on /\ cf[f].path[1] = j[1]
                      /\ \E g \in FileIds : g # f /\ cf[g].on /\ cf[g].path = <<j[2], cf[f].path[2]>>

WorkEnd ==
    /\ pc.ph = "fjobs" /\ pc.d > NDisks
    /\ pc' = Idle /\ sess' = NoSess
    /\ UNCHANGED <<disk, cat, cnt, nver, ghost>>

(* A disk that cannot be relied on at any step of its work: what is under way stays planned, the disk missing. *)
WorkLost ==
    /\ \/ pc.ph \in {"fjobs", "round"} /\ pc.d <= NDisks /\ InSess(pc.d)
       \/ pc.ph = "fstep" /\ pc.s # "synced"
       \/ pc.ph = "rstep" /\ pc.s # "ssynced"
       \/ pc.ph = "xstep" /\ pc.s \notin {"synced", "ca"}
    /\ ~Ok(pc.d)
    /\ Lose(pc.d)
    /\ pc' = NextDisk(pc.d)
    /\ UNCHANGED <<disk, cf, cc, ce, fj, moves, ops, nextOp, cnt, nver, ghost>>

WorkSkip ==
    /\ pc.ph = "fjobs" /\ pc.d <= NDisks /\ ~InSess(pc.d)
    /\ pc' = NextDisk(pc.d)
    /\ UNCHANGED <<disk, cat, sess, cnt, nver, ghost>>

FJPick ==
    /\ pc.ph = "fjobs" /\ pc.d <= NDisks /\ Ok(pc.d)
    /\ LET d == pc.d
           J == fj[d] \ pc.done
           free == {j \in J : ~\E k \in J : k # j /\ j[2] = k[1]}
           pool == IF free # {} THEN free ELSE J
           j == CHOOSE j \in pool : \A k \in pool : j[1] <= k[1]
           ready == free # {} /\ root[d][j[1]] # 0 /\ root[d][j[2]] = 0 /\ ~LandsBadly(d, j)
       IN IF J = {}
          THEN /\ pc' = [ph |-> "round", d |-> d, done |-> {}, moved |-> FALSE, stuck |-> {}]
               /\ UNCHANGED <<fj, ops, nextOp>>
          ELSE IF ~ready
          THEN /\ fj' = [fj EXCEPT ![d] = @ \ {j}]
               /\ pc' = [pc EXCEPT !.done = @ \cup {j}]
               /\ UNCHANGED <<ops, nextOp>>
          ELSE IF nextOp > MaxOps
          THEN /\ pc' = NextDisk(d) /\ UNCHANGED <<fj, ops, nextOp>>
          ELSE /\ ops' = [ops EXCEPT ![nextOp] = [k |-> "rename", d |-> d, p |-> <<j[1], 0>>, q |-> <<j[2], 0>>,
                                                  key |-> root[d][j[1]], st |-> "planned"]]
               /\ nextOp' = nextOp + 1
               /\ pc' = [ph |-> "fstep", d |-> d, j |-> j, o |-> nextOp, s |-> "journaled", mv |-> 0, done |-> pc.done]
               /\ UNCHANGED fj
    /\ UNCHANGED <<disk, cf, cc, ce, moves, cdisk, sess, cnt, nver, ghost>>

(* Whatever stops the folder's operation on a disk that can still be relied on is resolved at once by the table of
   §17.2, as ops.Disk.recoverRename does (trash.Session.AfterFailedMove): finished, its commit is a reprise's. The job
   goes either way here: undone, its copies follow one by one. *)
FResolve(nameAltered) ==
    LET d == pc.d
        r == Resolved(d, pc.o)
        rows == ResolveRows(d, pc.o, r.fin)
    IN /\ objs' = r.objs
       /\ droot' = [droot EXCEPT ![d] = root[d]]
       /\ ops' = [ops EXCEPT ![pc.o].st = IF r.fin THEN "done" ELSE "undone"]
       /\ cc' = rows.cc /\ ce' = rows.ce
       /\ fj' = [rows.fj EXCEPT ![d] = @ \ {pc.j}]
       /\ ghost' = Flag(ghost, "name-altered", nameAltered /\ ~r.fin /\ root[d][pc.j[2]] = pc.mv)
       /\ pc' = [ph |-> "fjobs", d |-> d, done |-> pc.done \cup {pc.j}]

(* The look right before: the folder at its name, by its inode. *)
FJLook ==
    /\ pc.ph = "fstep" /\ pc.s = "journaled" /\ Ok(pc.d)
    /\ IF root[pc.d][pc.j[1]] = ops[pc.o].key
       THEN /\ pc' = [pc EXCEPT !.s = "looked"] /\ UNCHANGED <<objs, droot, cc, ce, fj, ops, ghost>>
       ELSE FResolve(FALSE)
    /\ UNCHANGED <<root, dstat, mgen, cf, moves, cdisk, nextOp, sess, cnt, nver>>

(* RENAME_NOREPLACE: rename(2) moves what the name holds now, whatever it is; EEXIST or ENOENT moves nothing. *)
FJMove ==
    /\ pc.ph = "fstep" /\ pc.s = "looked" /\ Ok(pc.d)
    /\ LET d == pc.d
           F == pc.j[1]
           G == pc.j[2]
       IN IF root[d][G] # 0 \/ root[d][F] = 0
          THEN /\ FResolve(FALSE) /\ UNCHANGED root
          ELSE /\ root' = [root EXCEPT ![d][G] = root[d][F], ![d][F] = 0]
               /\ pc' = [pc EXCEPT !.s = "moved", !.mv = root[d][F]]
               /\ UNCHANGED <<objs, droot, cc, ce, fj, ops, ghost>>
    /\ UNCHANGED <<dstat, mgen, cf, moves, cdisk, nextOp, sess, cnt, nver>>

(* The look after: the new name holds the folder, by its inode; otherwise the table at once, which leaves what is
   there where it is for a rename. With folder-back, the remedy proposed for flaw 14 (README.md): what the move itself
   took from the old name, a folder that is not the one journaled, goes back there first when that name is free,
   without replacing anything, the root synced, as a file's rename puts back what it took (flaw 7); the operation is
   then undone, and the job goes, its copies following one by one. *)
FJAfter ==
    /\ pc.ph = "fstep" /\ pc.s = "moved" /\ Ok(pc.d)
    /\ LET d == pc.d
           F == pc.j[1]
           G == pc.j[2]
           back == "folder-back" \in Fixes /\ root[d][G] = pc.mv /\ pc.mv # ops[pc.o].key /\ root[d][F] = 0
       IN IF root[d][G] = ops[pc.o].key
          THEN /\ pc' = [pc EXCEPT !.s = "after"] /\ UNCHANGED <<root, objs, droot, cc, ce, fj, ops, ghost>>
          ELSE IF back
          THEN /\ root' = [root EXCEPT ![d][F] = root[d][G], ![d][G] = 0]
               /\ droot' = [droot EXCEPT ![d] = [root[d] EXCEPT ![F] = root[d][G], ![G] = 0]]
               /\ ops' = [ops EXCEPT ![pc.o].st = "undone"]
               /\ fj' = [fj EXCEPT ![d] = @ \ {pc.j}]
               /\ pc' = [ph |-> "fjobs", d |-> d, done |-> pc.done \cup {pc.j}]
               /\ UNCHANGED <<objs, cc, ce, ghost>>
          ELSE /\ FResolve(TRUE) /\ UNCHANGED root
    /\ UNCHANGED <<dstat, mgen, cf, moves, cdisk, nextOp, sess, cnt, nver>>

(* The folder synced at its new name, then the root, which holds both names here. *)
FJSync ==
    /\ pc.ph = "fstep" /\ pc.s = "after" /\ Ok(pc.d)
    /\ droot' = [droot EXCEPT ![pc.d] = root[pc.d]]
    /\ pc' = [pc EXCEPT !.s = "synced"]
    /\ UNCHANGED <<objs, root, dstat, mgen, cat, sess, cnt, nver, ghost>>

(* In one transaction: the operation done, each copy behind below the old name with no clock running following it, the
   entries, and the job gone (catalog.FinishFolderRename). *)
FJCommit ==
    /\ pc.ph = "fstep" /\ pc.s = "synced"
    /\ LET d == pc.d
           F == pc.j[1]
           G == pc.j[2]
           key == ops[pc.o].key
       IN /\ ops' = [ops EXCEPT ![pc.o].st = "done"]
          /\ cc' = FolderRows(d, F, G, FALSE)
          /\ ce' = Entries(d, F, G)
          /\ fj' = [fj EXCEPT ![d] = @ \ {pc.j}]
          /\ ghost' = Flag(ghost, "inv7", ~((root[d][G] = key => droot[d][G] = key) /\ (root[d][F] # key => droot[d][F] # key)))
          /\ pc' = [ph |-> "fjobs", d |-> d, done |-> pc.done \cup {pc.j}]
    /\ UNCHANGED <<disk, cf, moves, cdisk, nextOp, sess, cnt, nver>>


-----------------------------------------------------------------------------
(* Then the renames, by old path, round after round until one moves nothing (jobs.runRenames): a copy below a job
   still queued, or landing below one, waits for it. The look at both names (§15.12, step 2): the copy where it sits
   and nothing at the target goes on, its missing parent made first, an operation mkdir in one step here; nothing where
   it sits and the copy at the target is already-there, its row follows; the copy where it sits and something at the
   target is path-taken; anything else stays. *)

Behind(d) == {f \in FileIds : cf[f].on /\ Ok2(f, d) /\ cc[f][d].old # None}
BelowJob(d, f) == \E j \in fj[d] : cc[f][d].old[1] = j[1] \/ cf[f].path[1] = j[1]

(* jobs.findSwap: of the copies left path-taken, by place, the first whose targets lead round a cycle back to it, and
   the copy at its target. *)
AtPlace(d, S, p) == {g \in S : Place(g, d) = p}
RECURSIVE Leads(_, _, _, _, _)
Leads(d, S, start, f, n) ==
    IF n = 0 THEN FALSE
    ELSE LET nx == AtPlace(d, S, cf[f].path) IN
         nx # {} /\ LET g == CHOOSE g \in nx : TRUE IN g = start \/ Leads(d, S, start, g, n - 1)
Cyclic(d, S) == {f \in S : Leads(d, S, f, f, Cardinality(S))}

(* The end of a round: another when this one moved anything; otherwise the exchange of a cycle where the driver
   can, its first operation journaled; otherwise the next disk. *)
RoundEnd ==
    /\ pc.ph = "round" /\ Ok(pc.d)
    /\ {f \in Behind(pc.d) \ pc.done : ~BelowJob(pc.d, f)} = {}
    /\ LET d == pc.d
           C == Cyclic(d, pc.stuck)
           a == CHOOSE a \in C : \A b \in C : a = b \/ PathLess(Place(a, d), Place(b, d))
           b == CHOOSE b \in AtPlace(d, pc.stuck, cf[a].path) : TRUE
       IN IF pc.moved
          THEN /\ pc' = [ph |-> "round", d |-> d, done |-> {}, moved |-> FALSE, stuck |-> {}]
               /\ UNCHANGED <<ops, nextOp>>
          ELSE IF d \in ExDisks /\ C # {} /\ nextOp + 1 <= MaxOps
          THEN /\ ops' = [ops EXCEPT ![nextOp] = [k |-> "rename", d |-> d, p |-> Place(a, d), q |-> cf[a].path,
                                                  key |-> cc[a][d].obj, st |-> "planned"]]
               /\ nextOp' = nextOp + 1
               /\ pc' = [ph |-> "xstep", d |-> d, a |-> a, b |-> b, oa |-> nextOp, ob |-> 0, s |-> "ja", mv |-> <<0, 0>>]
          ELSE /\ pc' = NextDisk(d) /\ UNCHANGED <<ops, nextOp>>
    /\ UNCHANGED <<disk, cf, cc, ce, fj, moves, cdisk, sess, cnt, nver, ghost>>

RPick ==
    /\ pc.ph = "round" /\ Ok(pc.d)
    /\ LET d == pc.d
           B == {f \in Behind(d) \ pc.done : ~BelowJob(d, f)}
       IN /\ B # {}
          /\ LET f == CHOOSE f \in B : \A g \in B : f = g \/ PathLess(cc[f][d].old, cc[g][d].old)
                 r == cc[f][d]
                 tgt == cf[f].path
                 src == At(d, r.old)
                 there == At(d, tgt)
             IN IF src = r.obj /\ there = 0
                THEN IF nextOp > MaxOps
                     THEN /\ pc' = [pc EXCEPT !.done = @ \cup {f}] /\ UNCHANGED <<objs, root, droot, cc, ce, ops, nextOp>>
                     ELSE IF root[d][tgt[1]] = 0
                     THEN \* the missing parent, journaled, made, synced and committed, Atoll's own
                          /\ objs' = Append(objs, [NewDir(d) EXCEPT !.by = "atoll"])
                          /\ root' = [root EXCEPT ![d][tgt[1]] = Len(objs) + 1]
                          /\ droot' = [droot EXCEPT ![d][tgt[1]] = Len(objs) + 1]
                          /\ ops' = [ops EXCEPT ![nextOp] = [k |-> "mkdir", d |-> d, p |-> <<tgt[1], 0>>, q |-> None,
                                                             key |-> Len(objs) + 1, st |-> "done"]]
                          /\ nextOp' = nextOp + 1
                          /\ ce' = [ce EXCEPT ![d][tgt[1]] = [obj |-> Len(objs) + 1, by |-> "atoll"]]
                          /\ UNCHANGED <<pc, cc>>
                     ELSE /\ ops' = [ops EXCEPT ![nextOp] = [k |-> "rename", d |-> d, p |-> r.old, q |-> tgt, key |-> r.obj, st |-> "planned"]]
                          /\ nextOp' = nextOp + 1
                          /\ pc' = [ph |-> "rstep", d |-> d, f |-> f, o |-> nextOp, s |-> "journaled", mv |-> 0,
                                    done |-> pc.done \cup {f}, moved |-> pc.moved, stuck |-> pc.stuck]
                          /\ UNCHANGED <<objs, root, droot, cc, ce>>
                ELSE IF src = 0 /\ there = r.obj
                THEN /\ cc' = [cc EXCEPT ![f][d].old = None, ![f][d].gk = "none"]
                     /\ pc' = [pc EXCEPT !.done = @ \cup {f}, !.moved = TRUE]
                     /\ UNCHANGED <<objs, root, droot, ce, ops, nextOp>>
                ELSE IF src = r.obj
                THEN /\ pc' = [pc EXCEPT !.done = @ \cup {f}, !.stuck = @ \cup {f}]
                     /\ UNCHANGED <<objs, root, droot, cc, ce, ops, nextOp>>
                ELSE /\ pc' = [pc EXCEPT !.done = @ \cup {f}]
                     /\ UNCHANGED <<objs, root, droot, cc, ce, ops, nextOp>>
    /\ UNCHANGED <<dstat, mgen, cf, fj, moves, cdisk, sess, cnt, nver, ghost>>

BackToRound(moved) == [ph |-> "round", d |-> pc.d, done |-> pc.done, moved |-> pc.moved \/ moved, stuck |-> pc.stuck]

(* A copy's rename resolved at once by the table of §17.2, as ops.Disk.RenameFile's caller does. *)
RResolve(nameAltered) ==
    LET d == pc.d
        r == Resolved(d, pc.o)
        rows == ResolveRows(d, pc.o, r.fin)
    IN /\ objs' = r.objs
       /\ droot' = [droot EXCEPT ![d] = root[d]]
       /\ ops' = [ops EXCEPT ![pc.o].st = IF r.fin THEN "done" ELSE "undone"]
       /\ cc' = rows.cc
       /\ ghost' = Flag(ghost, "name-altered", nameAltered)
       /\ pc' = BackToRound(r.fin)

(* The look right before, the move without replacing anything, and the look after, which must find the copy by its
   identity, or the table at once, but that what the move itself took from the copy's old name goes back there first
   when that is free (ops.putBack, flaw 7 of Rename.tla); the syncs, target then source; the commit. *)
RLook ==
    /\ pc.ph = "rstep" /\ pc.s = "journaled" /\ Ok(pc.d)
    /\ LET op == ops[pc.o] IN
       IF At(pc.d, op.p) = op.key /\ At(pc.d, op.q) = 0
       THEN /\ pc' = [pc EXCEPT !.s = "looked"] /\ UNCHANGED <<objs, droot, cc, ops, ghost>>
       ELSE RResolve(FALSE)
    /\ UNCHANGED <<root, dstat, mgen, cf, ce, fj, moves, cdisk, nextOp, sess, cnt, nver>>

RMove ==
    /\ pc.ph = "rstep" /\ pc.s = "looked" /\ Ok(pc.d)
    /\ LET d == pc.d
           op == ops[pc.o]
       IN IF At(d, op.p) = 0 \/ At(d, op.q) # 0 \/ root[d][op.q[1]] = 0
          THEN RResolve(FALSE)
          ELSE /\ objs' = MoveName(objs, d, op.p, op.q)
               /\ pc' = [pc EXCEPT !.s = "after", !.mv = At(d, op.p)]
               /\ UNCHANGED <<droot, cc, ops, ghost>>
    /\ UNCHANGED <<root, dstat, mgen, cf, ce, fj, moves, cdisk, nextOp, sess, cnt, nver>>

RAfter ==
    /\ pc.ph = "rstep" /\ pc.s = "after" /\ Ok(pc.d)
    /\ LET d == pc.d
           op == ops[pc.o]
           taken == At(d, op.q) = pc.mv /\ pc.mv # op.key /\ pc.mv # 0
           back == taken /\ At(d, op.p) = 0
       IN IF At(d, op.q) = op.key
          THEN /\ pc' = [pc EXCEPT !.s = "moved"] /\ UNCHANGED <<objs, droot, cc, ops, ghost>>
          ELSE IF back
          THEN \* put back, synced there, then the table: the copy is at neither name, undone
               /\ objs' = SyncDir(SyncDir(MoveName(objs, d, op.q, op.p), root[d][op.p[1]]), root[d][op.q[1]])
               /\ droot' = [droot EXCEPT ![d] = root[d]]
               /\ ops' = [ops EXCEPT ![pc.o].st = "undone"]
               /\ pc' = BackToRound(FALSE)
               /\ UNCHANGED <<cc, ghost>>
          ELSE RResolve(taken)
    /\ UNCHANGED <<root, dstat, mgen, cf, ce, fj, moves, cdisk, nextOp, sess, cnt, nver>>

RSync ==
    /\ pc.ph = "rstep" /\ pc.s \in {"moved", "tsynced"} /\ Ok(pc.d)
    /\ LET op == ops[pc.o]
           n == IF pc.s = "moved" THEN op.q ELSE op.p
       IN objs' = SyncDir(objs, root[pc.d][n[1]])
    /\ pc' = [pc EXCEPT !.s = IF pc.s = "moved" THEN "tsynced" ELSE "ssynced"]
    /\ UNCHANGED <<root, droot, dstat, mgen, cat, sess, cnt, nver, ghost>>

(* A rename's names are durable as Atoll left them, for as long as nobody changed them since (invariant 7). *)
NamesDurable(d, p, q, key) == (At(d, q) = key => DAt(d, q) = key) /\ (At(d, p) # key => DAt(d, p) # key)

RCommit ==
    /\ pc.ph = "rstep" /\ pc.s = "ssynced"
    /\ LET op == ops[pc.o] IN
       /\ ops' = [ops EXCEPT ![pc.o].st = "done"]
       /\ cc' = [cc EXCEPT ![pc.f][pc.d].old = IF cf[pc.f].path = op.q THEN None ELSE op.q, ![pc.f][pc.d].gk = "none"]
       /\ ghost' = Flag(ghost, "inv7", ~NamesDurable(pc.d, op.p, op.q, op.key))
    /\ pc' = BackToRound(TRUE)
    /\ UNCHANGED <<disk, cf, ce, fj, moves, cdisk, nextOp, sess, cnt, nver>>


-----------------------------------------------------------------------------
(* A cycle (§15.12, jobs.exchange, ops.Disk.ExchangeFiles): a, the first by place, and b, the copy at its target,
   exchange their names. Both journaled as renames, b with the place of a as its target; the look right before at both
   names, each as its row says; the exchange, which swaps what both names hold then, ENOENT when either is empty; the
   look after, each name holding the other's copy by its identity; the sync of both folders, one journal transaction
   on the filesystems that exchange; and each commit, a then b, as a rename's. Whatever stops it on a disk that can
   still be relied on is resolved at once, each operation by the table of §17.2 as a reprise resolves it, and the
   disk's renames end there for this pass (jobs.exchange gives false). *)

PA == Place(pc.a, pc.d)
PB == Place(pc.b, pc.d)

XJournalB ==
    /\ pc.ph = "xstep" /\ pc.s = "ja"
    /\ ops' = [ops EXCEPT ![nextOp] = [k |-> "rename", d |-> pc.d, p |-> PB, q |-> PA, key |-> cc[pc.b][pc.d].obj, st |-> "planned"]]
    /\ nextOp' = nextOp + 1
    /\ pc' = [pc EXCEPT !.ob = nextOp, !.s = "jb"]
    /\ UNCHANGED <<disk, cf, cc, ce, fj, moves, cdisk, sess, cnt, nver, ghost>>

(* Both operations resolved at once, a then b, each by the names it finds as a reprise resolves it (Fin, with the
   one that finishes leaving the other's row at its target's name: a half-resolved exchange, which the next look reads
   as the user's own swap would read, README.md, observation 3); the folders synced. *)
XResolved ==
    LET d == pc.d
        A == ops[pc.oa]
        Bo == ops[pc.ob]
        planned(o) == o # 0 /\ ops[o].st = "planned"
        finA2 == planned(pc.oa) /\ Fin(d, pc.oa)
        finB == planned(pc.ob) /\ Fin(d, pc.ob)
        cc1 == IF finA2 THEN RenameRows(d, A) ELSE cc
        cc2 == IF finB THEN [f \in FileIds |-> [e \in Disks |->
                   IF e = d /\ cc1[f][e].st = "ok" /\ cc1[f][e].obj = Bo.key
                      /\ (IF cc1[f][e].old # None THEN cc1[f][e].old ELSE cf[f].path) = Bo.p
                   THEN [cc1[f][e] EXCEPT !.old = IF cf[f].path = Bo.q THEN None ELSE Bo.q, !.gk = "none"]
                   ELSE cc1[f][e]]]
               ELSE cc1
        dirs == {root[d][A.p[1]], root[d][A.q[1]]} \ {0}
    IN [cc |-> cc2, finA |-> finA2, finB |-> finB,
        objs |-> [o \in DOMAIN objs |-> IF o \in dirs THEN [objs[o] EXCEPT !.dkids = objs[o].kids] ELSE objs[o]]]

XResolve ==
    LET r == XResolved
        d == pc.d
        set(o, fin) == IF o # 0 /\ ops[o].st = "planned" THEN IF fin THEN "done" ELSE "undone" ELSE ops[o].st
        \* what the exchange moved that is no copy of it, left at the name the exchange gave it
        stray == /\ pc.mv # <<0, 0>>
                 /\ \/ (pc.mv[1] # 0 /\ pc.mv[1] # ops[pc.oa].key /\ At(d, PB) = pc.mv[1])
                    \/ (pc.mv[2] # 0 /\ pc.mv[2] # ops[pc.ob].key /\ At(d, PA) = pc.mv[2])
    IN /\ objs' = r.objs
       /\ droot' = [droot EXCEPT ![d] = root[d]]
       /\ ops' = [o \in OpIds |-> IF o = pc.oa THEN [ops[o] EXCEPT !.st = set(o, r.finA)]
                                  ELSE IF o = pc.ob THEN [ops[o] EXCEPT !.st = set(o, r.finB)]
                                  ELSE ops[o]]
       /\ cc' = r.cc
       /\ ghost' = Flag(ghost, "name-altered", stray)
       /\ pc' = NextDisk(d)

XLook ==
    /\ pc.ph = "xstep" /\ pc.s = "jb" /\ Ok(pc.d)
    /\ IF At(pc.d, PA) = ops[pc.oa].key /\ At(pc.d, PB) = ops[pc.ob].key
       THEN /\ pc' = [pc EXCEPT !.s = "looked"] /\ UNCHANGED <<objs, droot, cc, ops, ghost>>
       ELSE XResolve
    /\ UNCHANGED <<root, dstat, mgen, cf, ce, fj, moves, cdisk, nextOp, sess, cnt, nver>>

(* With exchange-back, the other remedy proposed: as a rename puts back what it took from its old name (flaw 7), what
   the exchange took from either name that is not the copy the look found there goes back, by a second exchange,
   before the table is asked. *)
XSwap ==
    /\ pc.ph = "xstep" /\ pc.s = "looked" /\ Ok(pc.d)
    /\ LET d == pc.d IN
       IF At(d, PA) = 0 \/ At(d, PB) = 0
       THEN XResolve
       ELSE /\ objs' = SwapNames(objs, d, PA, PB)
            /\ pc' = [pc EXCEPT !.s = "swapped", !.mv = <<At(d, PA), At(d, PB)>>]
            /\ UNCHANGED <<droot, cc, ops, ghost>>
    /\ UNCHANGED <<root, dstat, mgen, cf, ce, fj, moves, cdisk, nextOp, sess, cnt, nver>>

XAfter ==
    /\ pc.ph = "xstep" /\ pc.s = "swapped" /\ Ok(pc.d)
    /\ LET d == pc.d
           ok == At(d, PB) = ops[pc.oa].key /\ At(d, PA) = ops[pc.ob].key
           \* exchange-back: the two names still hold what the exchange gave them, one of which is no copy of it
           undo == "exchange-back" \in Fixes /\ At(d, PB) = pc.mv[1] /\ At(d, PA) = pc.mv[2]
       IN IF ok
          THEN /\ pc' = [pc EXCEPT !.s = "after"] /\ UNCHANGED <<objs, droot, cc, ops, ghost>>
          ELSE IF undo
          THEN \* exchanged back, synced; both operations then find each copy at its place: undone
               /\ objs' = LET os == SwapNames(objs, d, PA, PB) IN
                          SyncDir(SyncDir(os, root[d][PA[1]]), root[d][PB[1]])
               /\ droot' = [droot EXCEPT ![d] = root[d]]
               /\ ops' = [ops EXCEPT ![pc.oa].st = "undone", ![pc.ob].st = "undone"]
               /\ pc' = NextDisk(d)
               /\ UNCHANGED <<cc, ghost>>
          ELSE XResolve
    /\ UNCHANGED <<root, dstat, mgen, cf, ce, fj, moves, cdisk, nextOp, sess, cnt, nver>>

XSync ==
    /\ pc.ph = "xstep" /\ pc.s = "after" /\ Ok(pc.d)
    /\ objs' = SyncDir(SyncDir(objs, root[pc.d][PA[1]]), root[pc.d][PB[1]])
    /\ pc' = [pc EXCEPT !.s = "synced"]
    /\ UNCHANGED <<root, droot, dstat, mgen, cat, sess, cnt, nver, ghost>>

XCommitA ==
    /\ pc.ph = "xstep" /\ pc.s = "synced"
    /\ LET op == ops[pc.oa] IN
       /\ ops' = [ops EXCEPT ![pc.oa].st = "done"]
       /\ cc' = RenameRows(pc.d, op)
       /\ ghost' = Flag(ghost, "inv7", ~(At(pc.d, op.q) = op.key => DAt(pc.d, op.q) = op.key))
    /\ pc' = [pc EXCEPT !.s = "ca"]
    /\ UNCHANGED <<disk, cf, ce, fj, moves, cdisk, nextOp, sess, cnt, nver>>

(* b found where its row says, by its inode: a's row says that place too now (§17.2, "while an exchange is half
   recorded, two rows say they sit at one place, and only one has the operation's inode"). *)
XCommitB ==
    /\ pc.ph = "xstep" /\ pc.s = "ca"
    /\ LET op == ops[pc.ob] IN
       /\ ops' = [ops EXCEPT ![pc.ob].st = "done"]
       /\ cc' = RenameRows(pc.d, op)
       /\ ghost' = Flag(ghost, "inv7", ~(At(pc.d, op.q) = op.key => DAt(pc.d, op.q) = op.key))
    /\ pc' = [ph |-> "round", d |-> pc.d, done |-> {}, moved |-> FALSE, stuck |-> {}]
    /\ UNCHANGED <<disk, cf, ce, fj, moves, cdisk, nextOp, sess, cnt, nver>>


-----------------------------------------------------------------------------

Atoll ==
    \/ PassStart \/ RepOp \/ RepNext \/ RepEnd
    \/ WalkStart \/ WalkEnd \/ WalkDone \/ DecideFiles \/ DecideFolder \/ DecideDone
    \/ WorkEnd \/ WorkLost \/ WorkSkip
    \/ FJPick \/ FJLook \/ FJMove \/ FJAfter \/ FJSync \/ FJCommit
    \/ RoundEnd \/ RPick \/ RLook \/ RMove \/ RAfter \/ RSync \/ RCommit
    \/ XJournalB \/ XLook \/ XSwap \/ XAfter \/ XSync \/ XCommitA \/ XCommitB

Next == Atoll \/ World

Spec == Init /\ [][Next]_vars

FairSpec == Spec /\ WF_vars(Atoll) /\ \A d \in Disks : WF_vars(Return(d))

-----------------------------------------------------------------------------
(* Invariants. *)

(* No version a user wrote is lost: each is durably at a path of some disk, missing disks included, unless a user's
   own action took its last copy. *)
NoUserDataLost == \A v \in ghost.written \ ghost.udest : Held(v)

(* 4. Nothing a walk of a disk gone found is recorded. *)
Inv4_UnknownNeverAbsent == "inv4" \notin ghost.flags

(* 7. A rename, of a file or a folder, is done only once its target durably holds what it moved and its old name no
   longer does, as long as nobody changed them since: no row is ever said to be at a name it is not at (§17.2). *)
Inv7_RenameDurable == "inv7" \notin ghost.flags

(* 15. Never alter a file name: Atoll's move never leaves a file or a folder it did not mean to move at the name it
   gave it; a folder's operation carries what is below it, by design (§15.12). *)
NamesKept == "name-altered" \notin ghost.flags

(* §15.12, A disk away: a file takes only a path a user moved one of its copies to. *)
RenamesAreUsers == "rename-reverted" \notin ghost.flags

(* 13 on one disk: no object is the copy of two files. *)
OneRowAnObject ==
    \A d \in Disks, f, g \in FileIds :
        (f # g /\ Ok2(f, d) /\ Ok2(g, d)) => cc[f][d].obj # cc[g][d].obj

TypeOK ==
    /\ \A o \in OpIds : ops[o].st \in {"none", "planned", "done", "undone"}
    /\ nextOp \in 1..(MaxOps + 1)
    /\ \A d \in Disks : fj[d] \subseteq FNames \X FNames

(* Liveness: every copy follows its file's rename, no folder job is left, and no operation stays planned. *)
Followed ==
    /\ \A f \in FileIds, d \in Disks : (cf[f].on /\ Ok2(f, d)) => cc[f][d].old = None
    /\ \A d \in Disks : fj[d] = {}
    /\ \A o \in OpIds : ops[o].st # "planned"

EventuallyFollowed == <>[]Followed

=============================================================================
