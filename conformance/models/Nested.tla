------------------------------- MODULE Nested -------------------------------
(* Folders below folders, and names a disk refuses, across two disks (specs.md §15.3, §15.8, §15.12): a model of its
   own beside Folders.tla, whose paths are a folder at the root and a file in it. Here a path is a folder's, one or two
   names deep (D, D/E), each folder holding one file, x; a folder renamed directly on one disk, inside another or out of
   one, its subtree with it; a folder made; a file moved to another folder; and on a disk, names its filesystem refuses
   (§15.8, as NTFS refuses a name with ":"). The daemon's passes, as Folders.tla has them, each operation of Atoll one
   step here (journaled, made and committed at once: Folders.tla and Rename.tla have their steps): the walk of each disk,
   its verdicts and the folders found renamed (watch.moveFolders: a folder row whose folder is not at its name, found at
   a name with no row, not below the old one, the old name holding no file at home); the decisions about the files; the
   folders found renamed, in the order of their jobs, each waiting while its disk still holds copies below the old name,
   its entries rewritten below it (catalog.rewriteEntries), the jobs moved on by a further rename at or below their
   target (catalog.moveFolderJobs), and a job for each other disk with a copy or an entry below it; then on each disk its
   folder jobs, in the order of jobs.folderOrder, the first of a cycle dropped, each run as one operation when ready
   (jobs.folderReady: the folder its entry records at the old name, the new name absent, not below the old, its name not
   refused, its missing parents made), dropped otherwise, its copies then following one by one; the renames of copies
   behind, round after round, a copy below a job queued waiting for it, its missing folders made, a name the disk
   refuses leaving it behind (§15.12, Refused names); and a copy behind whose name its disk refuses, with another copy
   of its file at its path on a disk walked in this pass, sent to its trash, `reason = renamed`, by a trash job the
   pass after (findWishes), the file then lacking that replica. With Atoll killed between any two steps, a disk lost
   and mounted again, and a user acting on either disk at any moment.

   What it leaves out: the steps of each operation and their reprise (Folders.tla's and Rename.tla's), the power cut,
   content beyond a version, edits and conflicts (Replication.tla's), deletions and the guards, the debounce (every
   change is due at the walk that finds it), swaps and cycles of files and their exchange (Folders.tla's), the plan
   and the copies it makes, more than two disks and two levels of folders, a folder's mode, and Atoll's own folders
   left empty. *)
EXTENDS Naturals, FiniteSets, Sequences, TLC

CONSTANTS
    NNames,     \* folder names, by number: D, E
    NFiles,     \* file ids the catalog may hold
    StartDirs,  \* [Disks -> SUBSET FPaths]: the folders on each disk at the start
    StartFile,  \* [FileIds -> path, or None]: the folder each file is in at the start
    StartOn,    \* [FileIds -> SUBSET Disks]
    Refuse,     \* [Disks -> SUBSET Names]: the names each disk's filesystem refuses
    UserActs,   \* some of "mvdir", "mkdir", "mv"
    UserDisks,  \* the disks a user acts on
    MaxUser,
    MaxCrash,
    MaxDrop,
    Fixes       \* the remedies proposed for the flaws found here and still open, by name (README.md); {} is specs.md as it is

NDisks == 2
Disks == 1..NDisks
Names == 1..NNames
FPaths == {<<a>> : a \in Names} \cup {<<a, b>> : a, b \in Names}
None == <<>>
FileIds == 1..NFiles
FixNames == {}

ASSUME /\ NNames \in 1..2 /\ NFiles \in 1..3
       /\ StartDirs \in [Disks -> SUBSET FPaths]
       /\ StartFile \in [FileIds -> FPaths \cup {None}] /\ StartOn \in [FileIds -> SUBSET Disks]
       /\ \A f \in FileIds : (StartFile[f] = None) <=> (StartOn[f] = {})
       /\ \A f \in FileIds : \A d \in StartOn[f] : StartFile[f] \in StartDirs[d]
       /\ \A f, g \in FileIds : f # g /\ StartFile[f] # None => StartFile[f] # StartFile[g]
       /\ Refuse \in [Disks -> SUBSET Names]
       /\ UserActs \subseteq {"mvdir", "mkdir", "mv"} /\ UserDisks \subseteq Disks
       /\ Fixes \subseteq FixNames

Min(S) == CHOOSE x \in S : \A y \in S : x <= y
PathLess(p, q) == Len(p) < Len(q) \/ (Len(p) = Len(q) /\ \E i \in 1..Len(p) : p[i] < q[i] /\ \A j \in 1..(i - 1) : p[j] = q[j])
MinPath(S) == CHOOSE p \in S : \A q \in S : p = q \/ PathLess(p, q)
Below(q, p) == Len(q) > Len(p) /\ SubSeq(q, 1, Len(p)) = p
AtOrBelow(q, p) == q = p \/ Below(q, p)
Parent(p) == SubSeq(p, 1, Len(p) - 1)
Rewrite(q, P, Q) == IF AtOrBelow(q, P) THEN Q \o SubSeq(q, Len(P) + 1, Len(q)) ELSE q
Refused(d, p) == \E i \in 1..Len(p) : p[i] \in Refuse[d]

NoCopy == [st |-> "none", obj |-> 0, old |-> None, v |-> "none", to |-> None]
Idle == [ph |-> "idle"]

VARIABLES
    ov,      \* the objects: their versions, a folder's 0 (a sequence, each new object appended)
    dirs,    \* [Disks -> [FPaths -> folder or 0]]
    fl,      \* [Disks -> [FPaths -> file or 0]]: the file x in the folder at that path
    trash,   \* [Disks -> SUBSET objects]
    dstat,   \* [Disks -> {"up", "gone"}]
    cf,      \* the catalog's files: [FileIds -> [on, path]]
    cc,      \* their copies: [FileIds -> [Disks -> [st, obj, old, v, to]]]
    ce,      \* the entries of the folders: [Disks -> [FPaths -> folder or 0]]
    fj,      \* the folder jobs: [Disks -> set of <<From, To>>]
    tj,      \* the trash jobs, reason renamed, of a copy behind: [Disks -> SUBSET FileIds]
    moves,   \* the folders this pass's walk found renamed: [Disks -> set of <<From, To>>]
    pc,
    cnt,
    ghost    \* written versions, the moves users made, and flags

vars == <<ov, dirs, fl, trash, dstat, cf, cc, ce, fj, tj, moves, pc, cnt, ghost>>
disk == <<ov, dirs, fl, trash, dstat>>
cat == <<cf, cc, ce, fj, tj, moves>>

Up(d) == dstat[d] = "up"
Ok2(f, d) == cc[f][d].st = "ok"
Place(f, d) == IF cc[f][d].old # None THEN cc[f][d].old ELSE cf[f].path
Flag(g, name, cond) == IF cond THEN [g EXCEPT !.flags = @ \cup {name}] ELSE g

(* A folder's subtree moved from P to Q in a map of paths. *)
MovedMap(m, P, Q) == [r \in FPaths |->
    IF \E q \in FPaths : AtOrBelow(q, P) /\ Rewrite(q, P, Q) = r
    THEN m[CHOOSE q \in FPaths : AtOrBelow(q, P) /\ Rewrite(q, P, Q) = r]
    ELSE IF AtOrBelow(r, P) THEN 0 ELSE m[r]]

(* The folder above p is there on d, the root for a name at the root. *)
ParentThere(d, p) == IF Len(p) = 1 THEN TRUE ELSE dirs[d][Parent(p)] # 0

(* What rename(2) of a folder does on d, P to Q: refused when Q holds anything, its parent is missing, Q is at or below
   P, or the disk refuses a name of it; and here when its subtree would go past two names. *)
CanMoveDir(d, P, Q) ==
    /\ dirs[d][P] # 0 /\ dirs[d][Q] = 0 /\ ~AtOrBelow(Q, P) /\ ~Refused(d, Q)
    /\ ParentThere(d, Q)
    /\ \A q \in FPaths : AtOrBelow(q, P) /\ dirs[d][q] # 0 => Len(Rewrite(q, P, Q)) <= 2

(* The start. Objects 1..|FPaths|*2 are each disk's folders, by disk then path; then each file's copy on each disk. *)
PathList == CHOOSE s \in [1..Cardinality(FPaths) -> FPaths] : \A i, j \in DOMAIN s : i < j => PathLess(s[i], s[j])
PathNo(p) == CHOOSE i \in DOMAIN PathList : PathList[i] = p
NP == Cardinality(FPaths)
DirObj(d, p) == (d - 1) * NP + PathNo(p)
FileObj(f, d) == 2 * NP + (f - 1) * 2 + d

Init ==
    /\ ov = [o \in 1..(2 * NP + 2 * NFiles) |-> IF o <= 2 * NP THEN 0 ELSE (o - 2 * NP - 1) \div 2 + 1]
    /\ dirs = [d \in Disks |-> [p \in FPaths |-> IF p \in StartDirs[d] THEN DirObj(d, p) ELSE 0]]
    /\ fl = [d \in Disks |-> [p \in FPaths |->
              IF \E f \in FileIds : StartFile[f] = p /\ d \in StartOn[f]
              THEN FileObj(CHOOSE f \in FileIds : StartFile[f] = p /\ d \in StartOn[f], d) ELSE 0]]
    /\ trash = [d \in Disks |-> {}]
    /\ dstat = [d \in Disks |-> "up"]
    /\ cf = [f \in FileIds |-> [on |-> StartFile[f] # None, path |-> IF StartFile[f] # None THEN StartFile[f] ELSE <<1>>]]
    /\ cc = [f \in FileIds |-> [d \in Disks |->
              IF d \in StartOn[f] THEN [NoCopy EXCEPT !.st = "ok", !.obj = FileObj(f, d)] ELSE NoCopy]]
    /\ ce = [d \in Disks |-> [p \in FPaths |-> IF p \in StartDirs[d] THEN DirObj(d, p) ELSE 0]]
    /\ fj = [d \in Disks |-> {}]
    /\ tj = [d \in Disks |-> {}]
    /\ moves = [d \in Disks |-> {}]
    /\ pc = Idle
    /\ cnt = [user |-> 0, crash |-> 0, drop |-> 0]
    /\ ghost = [written |-> {f \in FileIds : StartFile[f] # None}, umoved |-> {}, flags |-> {}]

-----------------------------------------------------------------------------
(* The world: a user on one disk, as its filesystem lets them; a disk lost and mounted again; Atoll killed. umoved:
   each file object a user moved, directly or with its folder, with the path it went to. *)

Allowed(a, d) == a \in UserActs /\ cnt.user < MaxUser /\ d \in UserDisks
User(d) ==
    /\ cnt' = [cnt EXCEPT !.user = @ + 1]
    /\ UNCHANGED <<trash, dstat, cat, pc>>

UMvDir(d, P, Q) ==
    /\ Allowed("mvdir", d) /\ Up(d) /\ CanMoveDir(d, P, Q)
    /\ dirs' = [dirs EXCEPT ![d] = MovedMap(dirs[d], P, Q)]
    /\ fl' = [fl EXCEPT ![d] = MovedMap(fl[d], P, Q)]
    /\ ghost' = [ghost EXCEPT !.umoved = @ \cup {<<fl[d][q], Rewrite(q, P, Q)>> : q \in {r \in FPaths : AtOrBelow(r, P) /\ fl[d][r] # 0}}]
    /\ User(d) /\ UNCHANGED ov

UMkdir(d, P) ==
    /\ Allowed("mkdir", d) /\ Up(d) /\ dirs[d][P] = 0 /\ ~Refused(d, P) /\ ParentThere(d, P)
    /\ ov' = Append(ov, 0)
    /\ dirs' = [dirs EXCEPT ![d][P] = Len(ov) + 1]
    /\ User(d) /\ UNCHANGED <<fl, ghost>>

UMv(d, p, q) ==
    /\ Allowed("mv", d) /\ Up(d) /\ p # q /\ fl[d][p] # 0 /\ dirs[d][q] # 0 /\ fl[d][q] = 0
    /\ fl' = [fl EXCEPT ![d][p] = 0, ![d][q] = fl[d][p]]
    /\ ghost' = [ghost EXCEPT !.umoved = @ \cup {<<fl[d][p], q>>}]
    /\ User(d) /\ UNCHANGED <<ov, dirs>>

Drop(d) ==
    /\ cnt.drop < MaxDrop /\ Up(d)
    /\ dstat' = [dstat EXCEPT ![d] = "gone"]
    /\ cnt' = [cnt EXCEPT !.drop = @ + 1]
    /\ UNCHANGED <<ov, dirs, fl, trash, cat, pc, ghost>>

Return(d) ==
    /\ dstat[d] = "gone"
    /\ dstat' = [dstat EXCEPT ![d] = "up"]
    /\ UNCHANGED <<ov, dirs, fl, trash, cat, pc, cnt, ghost>>

Crash ==
    /\ cnt.crash < MaxCrash /\ pc # Idle
    /\ pc' = Idle
    /\ moves' = [d \in Disks |-> {}]
    /\ cnt' = [cnt EXCEPT !.crash = @ + 1]
    /\ UNCHANGED <<disk, cf, cc, ce, fj, tj, ghost>>

World ==
    \/ \E d \in Disks, P, Q \in FPaths : UMvDir(d, P, Q)
    \/ \E d \in Disks, P \in FPaths : UMkdir(d, P)
    \/ \E d \in Disks, p, q \in FPaths : UMv(d, p, q)
    \/ \E d \in Disks : Drop(d) \/ Return(d)
    \/ Crash

-----------------------------------------------------------------------------
(* §15.12 step 3: the walk of each disk up, its verdicts, the folders found renamed, its entries and what is new, in
   one step. *)

PassStart ==
    /\ pc = Idle
    /\ pc' = [ph |-> "walk", d |-> 1, walked |-> {}]
    /\ UNCHANGED <<disk, cat, cnt, ghost>>

WalkDisk ==
    /\ pc.ph = "walk" /\ pc.d <= NDisks
    /\ LET d == pc.d
           known(o) == \E f \in FileIds : Ok2(f, d) /\ cc[f][d].obj = o
           home(P) == \E f \in FileIds : Ok2(f, d) /\ AtOrBelow(Place(f, d), P) /\ fl[d][Place(f, d)] = cc[f][d].obj
           mv == {m \in FPaths \X FPaths :
                     /\ ce[d][m[1]] # 0 /\ dirs[d][m[1]] # ce[d][m[1]]
                     /\ dirs[d][m[2]] = ce[d][m[1]] /\ ce[d][m[2]] = 0
                     /\ ~Below(m[2], m[1]) /\ ~home(m[1])}
           tops == {m[2] : m \in mv}
           kept(P) == \E m \in mv : AtOrBelow(P, m[1]) \/ AtOrBelow(P, m[2])
           movedTo(f) == {q \in FPaths : q # Place(f, d) /\ fl[d][q] = cc[f][d].obj}
           verdict(f) == IF fl[d][Place(f, d)] = cc[f][d].obj THEN "there"
                         ELSE IF movedTo(f) # {} THEN "moved" ELSE "gone"
           cc1 == [f \in FileIds |-> [e \in Disks |->
                     IF e # d \/ ~Ok2(f, e) THEN cc[f][e]
                     ELSE [cc[f][e] EXCEPT !.v = verdict(f),
                                           !.to = IF verdict(f) = "moved" THEN MinPath(movedTo(f)) ELSE None]]]
           newAt(p) == fl[d][p] # 0 /\ ~known(fl[d][p]) /\ ~\E t \in tops : AtOrBelow(p, t)
           atPath(p) == {f \in FileIds : cf[f].on /\ cf[f].path = p}
           joins(f, p) == newAt(p) /\ f \in atPath(p) /\ ~Ok2(f, d)
           cc2 == [f \in FileIds |-> [e \in Disks |->
                     IF e = d /\ \E p \in FPaths : joins(f, p)
                     THEN [NoCopy EXCEPT !.st = "ok", !.obj = fl[d][CHOOSE p \in FPaths : joins(f, p)], !.v = "there"]
                     ELSE cc1[f][e]]]
           fresh == {p \in FPaths : newAt(p) /\ atPath(p) = {} /\ \E g \in FileIds : ~cf[g].on}
           pnew == IF fresh = {} THEN None ELSE MinPath(fresh)
           g == IF pnew = None THEN 0 ELSE Min({h \in FileIds : ~cf[h].on})
       IN IF Up(d)
          THEN /\ cc' = IF pnew = None THEN cc2
                        ELSE [cc2 EXCEPT ![g] = [e \in Disks |-> IF e = d THEN [NoCopy EXCEPT !.st = "ok", !.obj = fl[d][pnew], !.v = "there"]
                                                                 ELSE NoCopy]]
               /\ cf' = IF pnew = None THEN cf ELSE [cf EXCEPT ![g] = [on |-> TRUE, path |-> pnew]]
               /\ ce' = [ce EXCEPT ![d] = [P \in FPaths |-> IF kept(P) THEN ce[d][P] ELSE dirs[d][P]]]
               /\ moves' = [moves EXCEPT ![d] = mv]
               /\ pc' = [pc EXCEPT !.d = d + 1, !.walked = @ \cup {d}]
          ELSE /\ pc' = [pc EXCEPT !.d = d + 1]
               /\ UNCHANGED <<cf, cc, ce, moves>>
    /\ UNCHANGED <<disk, fj, tj, cnt, ghost>>

-----------------------------------------------------------------------------
(* §15.12 step 4, the decisions about the files, as Folders.tla's: a file with a copy moved takes its target, the
   target of its first moved copy, by the greatest fixed point; its moved copy loses old_lpath, a copy gone has its row
   go, the others get old_lpath where they sit. A job above a copy whose old_lpath a rename back clears goes. *)

MovedOn(f) == {d \in pc.walked : Ok2(f, d) /\ cc[f][d].v = "moved"}
Tgt(f) == cc[f][Min(MovedOn(f))].to
Keep(S) == {f \in S :
               /\ ~\E g \in FileIds \ S : g # f /\ cf[g].on /\ cf[g].path = Tgt(f)
               /\ ~\E g \in S : g # f /\ cf[g].path = Tgt(f) /\ Tgt(g) = cf[g].path
               /\ ~\E h \in S : h # f /\ Tgt(h) = Tgt(f) /\ Tgt(f) # cf[f].path /\ PathLess(cf[h].path, cf[f].path)}
RECURSIVE Fix(_)
Fix(S) == IF Keep(S) = S THEN S ELSE Fix(Keep(S))

(* Whether copies came with a folder found renamed, as the rows stand before any decision (jobs.isRealRename). *)
RealMove(d, m) ==
    \E f \in FileIds : Ok2(f, d) /\ ((AtOrBelow(Place(f, d), m[2]) /\ cc[f][d].v = "there")
                                     \/ (cc[f][d].v = "moved" /\ AtOrBelow(cc[f][d].to, m[2]) /\ AtOrBelow(Place(f, d), m[1])))

DecideFiles ==
    /\ pc.ph = "walk" /\ pc.d > NDisks
    /\ LET S0 == {f \in FileIds : cf[f].on /\ MovedOn(f) # {}}
           R == Fix(S0)
           row(f, d) ==
               LET r == cc[f][d] IN
               IF r.st # "ok" THEN r
               ELSE IF d \notin pc.walked THEN IF f \in R THEN [r EXCEPT !.old = IF Place(f, d) = Tgt(f) THEN None ELSE Place(f, d)] ELSE r
               ELSE IF f \in R
               THEN CASE r.v = "moved" /\ r.to = Tgt(f) -> [r EXCEPT !.old = None]
                      [] r.v = "gone" -> NoCopy
                      [] r.v = "moved" -> r
                      [] OTHER -> [r EXCEPT !.old = IF Place(f, d) = Tgt(f) THEN None ELSE Place(f, d)]
               ELSE IF r.v = "gone" THEN NoCopy
               ELSE r
           cc1 == [f \in FileIds |-> [d \in Disks |-> row(f, d)]]
           left(f) == \E d \in Disks : cc1[f][d].st = "ok"
           cleared(d) == {cc[f][d].old : f \in {h \in R : Ok2(h, d) /\ cc[h][d].old # None /\ cc[h][d].v # "moved"
                                                          /\ Place(h, d) = Tgt(h)}}
       IN /\ cc' = [f \in FileIds |-> IF left(f) THEN cc1[f] ELSE [d \in Disks |-> NoCopy]]
          /\ fj' = [d \in Disks |-> {j \in fj[d] : ~\E c \in cleared(d) : AtOrBelow(c, j[1])}]
          /\ cf' = [f \in FileIds |->
                IF ~left(f) THEN [cf[f] EXCEPT !.on = FALSE]
                ELSE IF f \in R THEN [cf[f] EXCEPT !.path = Tgt(f)] ELSE cf[f]]
          /\ pc' = [ph |-> "folders", walked |-> pc.walked,
                    left |-> UNION {{<<d, m, RealMove(d, m)>> : m \in moves[d]} : d \in pc.walked},
                    stay |-> {<<f, d>> \in FileIds \X Disks : Ok2(f, d) /\ cc[f][d].v = "moved" /\ f \notin R},
                    moved |-> S0]
          /\ ghost' = Flag(ghost, "rename-reverted",
                           \E f \in R : Tgt(f) # cf[f].path /\ <<cc[f][Min(MovedOn(f))].obj, Tgt(f)>> \notin ghost.umoved)
    /\ UNCHANGED <<disk, ce, tj, moves, cnt>>

(* The folders found renamed, each in its own step, in the order of their jobs: one that lands in another's old name,
   at or below it, after that one. A folder whose disk still holds copies below its old name, but for those no pass
   can take from there, waits. One no copy came with is only its entries and the jobs it moves on. Otherwise, in one
   transaction (catalog.FolderRow): each file below the old name with copies on other disks only, none moved in this
   pass, follows it if its new path is free; the entries below the old name rewritten below the new one; the jobs whose
   target is at or below the old name moved on, one whose two names become equal going; and a job for each other disk
   holding a copy or an entry at or below the old name. *)
RewriteMap(m, P, Q) == [r \in FPaths |->
    IF \E q \in FPaths : AtOrBelow(q, P) /\ Rewrite(q, P, Q) = r /\ m[q] # 0
    THEN m[CHOOSE q \in FPaths : AtOrBelow(q, P) /\ Rewrite(q, P, Q) = r /\ m[q] # 0]
    ELSE IF AtOrBelow(r, P) THEN 0 ELSE m[r]]
MovedJobs(P, Q) == [e \in Disks |->
    {j \in {IF AtOrBelow(k[2], P) THEN <<k[1], Rewrite(k[2], P, Q)>> ELSE k : k \in fj[e]} : j[1] # j[2]}]

DecideFolder ==
    /\ pc.ph = "folders" /\ pc.left # {}
    /\ LET waits(x, y) == x[1] = y[1] /\ x # y /\ AtOrBelow(x[2][2], y[2][1])
           ready == {x \in pc.left : ~\E y \in pc.left : waits(x, y)}
           pool == IF ready # {} THEN ready ELSE pc.left
           x == CHOOSE x \in pool : \A y \in pool : x = y \/ x[1] < y[1] \/ (x[1] = y[1] /\ PathLess(x[2][1], y[2][1]))
           d == x[1]
           P == x[2][1]
           Q == x[2][2]
           below == {f \in FileIds : Ok2(f, d) /\ AtOrBelow(Place(f, d), P) /\ <<f, d>> \notin pc.stay}
           to(g) == Rewrite(cf[g].path, P, Q)
           followers == {g \in FileIds : cf[g].on /\ AtOrBelow(cf[g].path, P) /\ ~Ok2(g, d) /\ g \notin pc.moved
                                         /\ \E e \in Disks : Ok2(g, e)}
           follows == {g \in followers : Len(to(g)) <= 2 /\ ~\E h \in FileIds : h # g /\ cf[h].on /\ cf[h].path = to(g)}
           cf1 == [g \in FileIds |-> IF g \in follows THEN [cf[g] EXCEPT !.path = to(g)] ELSE cf[g]]
           cc1 == [g \in FileIds |-> [e \in Disks |->
                     IF g \in follows /\ Ok2(g, e) THEN [cc[g][e] EXCEPT !.old = IF Place(g, e) = to(g) THEN None ELSE Place(g, e)]
                     ELSE cc[g][e]]]
           others == {e \in Disks \ {d} :
                        \/ \E g \in FileIds : cc1[g][e].st = "ok" /\ cf1[g].on
                                              /\ AtOrBelow(IF cc1[g][e].old # None THEN cc1[g][e].old ELSE cf1[g].path, P)
                        \/ \E r \in FPaths : AtOrBelow(r, P) /\ ce[e][r] # 0}
           jobs == MovedJobs(P, Q)
       IN /\ pc' = [pc EXCEPT !.left = @ \ {x}]
          /\ IF below # {}
             THEN UNCHANGED <<cf, cc, ce, fj>>
             ELSE IF ~x[3]
             THEN /\ ce' = [ce EXCEPT ![d] = RewriteMap(ce[d], P, Q)] /\ fj' = jobs
                  /\ UNCHANGED <<cf, cc>>
             ELSE /\ cf' = cf1 /\ cc' = cc1 /\ ce' = [ce EXCEPT ![d] = RewriteMap(ce[d], P, Q)]
                  /\ fj' = [e \in Disks |-> IF e \in others THEN jobs[e] \cup {<<P, Q>>} ELSE jobs[e]]
    /\ UNCHANGED <<disk, tj, moves, cnt, ghost>>

DecideDone ==
    /\ pc.ph = "folders" /\ pc.left = {}
    /\ cc' = [f \in FileIds |-> [d \in Disks |-> [cc[f][d] EXCEPT !.v = "none", !.to = None]]]
    /\ moves' = [d \in Disks |-> {}]
    /\ pc' = [ph |-> "work", d |-> 1, walked |-> pc.walked, done |-> {}, stage |-> "trash", moved |-> FALSE]
    /\ UNCHANGED <<disk, cf, ce, fj, tj, cnt, ghost>>

-----------------------------------------------------------------------------
(* §15.12 step 5, on each disk up, by position: its trash jobs, then its folder jobs, then the renames of copies
   behind, then the trash jobs its refused copies want. *)

NextDisk == [ph |-> "work", d |-> pc.d + 1, walked |-> pc.walked, done |-> {}, stage |-> "trash", moved |-> FALSE]
GoodAt(f, d) == \E e \in (pc.walked \ {d}) : Ok2(f, e) /\ cc[f][e].old = None /\ Up(e) /\ fl[e][cf[f].path] = cc[f][e].obj

WorkEnd ==
    /\ pc.ph = "work" /\ pc.d > NDisks
    /\ pc' = Idle
    /\ UNCHANGED <<disk, cat, cnt, ghost>>

WorkSkip ==
    /\ pc.ph = "work" /\ pc.d <= NDisks /\ ~Up(pc.d)
    /\ pc' = NextDisk
    /\ UNCHANGED <<disk, cat, cnt, ghost>>

(* A trash job of a copy behind, reason renamed: the copy still where its row says, its file's name still refused
   there, and another copy at the file's path on a disk walked in this pass; it goes to the trash, its row with it.
   Otherwise the job goes. *)
TrashJob ==
    /\ pc.ph = "work" /\ pc.d <= NDisks /\ Up(pc.d) /\ pc.stage = "trash"
    /\ LET d == pc.d IN
       IF tj[d] = {}
       THEN /\ pc' = [pc EXCEPT !.stage = "fjobs"] /\ UNCHANGED <<fl, trash, cf, cc, tj, ghost>>
       ELSE LET f == Min(tj[d])
                r == cc[f][d]
                stands == cf[f].on /\ Ok2(f, d) /\ r.old # None /\ fl[d][r.old] = r.obj /\ Refused(d, cf[f].path) /\ GoodAt(f, d)
            IN /\ tj' = [tj EXCEPT ![d] = @ \ {f}]
               /\ IF stands
                  THEN /\ fl' = [fl EXCEPT ![d][r.old] = 0]
                       /\ trash' = [trash EXCEPT ![d] = @ \cup {r.obj}]
                       /\ cc' = [cc EXCEPT ![f][d] = NoCopy]
                       /\ ghost' = Flag(ghost, "last-copy", ~GoodAt(f, d))
                  ELSE UNCHANGED <<fl, trash, cc, ghost>>
               /\ UNCHANGED <<cf, pc>>
    /\ UNCHANGED <<ov, dirs, dstat, ce, fj, moves, cnt>>

(* jobs.waitsFor: a job goes after another when, run first, it would land in that one's old folder, or below its new
   name, or carry that one's old folder away. *)
WaitsFor(j, k) == AtOrBelow(j[2], k[1]) \/ Below(j[2], k[2]) \/ (Below(k[1], j[1]) /\ ~AtOrBelow(k[2], j[1]) /\ ~Below(k[2], j[2]))
RECURSIVE Reaches(_, _, _, _)
Reaches(J, start, j, n) == n > 0 /\ \E k \in J : WaitsFor(j, k) /\ (k = start \/ Reaches(J, start, k, n - 1))
LandsBadly(d, j) ==
    \E f \in FileIds : Ok2(f, d) /\ cc[f][d].old = None /\ cf[f].on /\ AtOrBelow(cf[f].path, j[1])
                      /\ \E g \in FileIds : g # f /\ cf[g].on /\ cf[g].path = Rewrite(cf[f].path, j[1], j[2])

(* A folder's job, as one operation: the missing parents of its new name made, its folder renamed, the copies of that
   disk behind below the old name following it, the entries rewritten; or, not ready, dropped, the copies following
   one by one. *)
FolderJob ==
    /\ pc.ph = "work" /\ pc.d <= NDisks /\ Up(pc.d) /\ pc.stage = "fjobs"
    /\ LET d == pc.d
           J == fj[d] \ pc.done
           free == {j \in J : ~\E k \in J : k # j /\ WaitsFor(j, k)}
           cyc == {j \in J : Reaches(J, j, j, Cardinality(J))}
           j == IF free # {} THEN CHOOSE j \in free : \A k \in free : j = k \/ PathLess(j[1], k[1])
                ELSE CHOOSE j \in cyc : \A k \in cyc : j = k \/ PathLess(j[1], k[1])
           F == j[1]
           G == j[2]
           parentOk == ParentThere(d, G) \/ ~Refused(d, Parent(G))
           \* a target past two names, as a further rename at or below it can leave it, is past this model's
           \* filesystem, which refuses it as a branch's limit would (fitsBelow)
           ready == /\ free # {} /\ G \in FPaths /\ dirs[d][F] # 0 /\ (ce[d][F] = 0 \/ ce[d][F] = dirs[d][F])
                    /\ dirs[d][G] = 0 /\ ~AtOrBelow(G, F) /\ ~Refused(d, G) /\ parentOk /\ ~LandsBadly(d, j)
                    /\ \A q \in FPaths : AtOrBelow(q, F) /\ dirs[d][q] # 0 => Len(Rewrite(q, F, G)) <= 2
           made == Len(G) = 2 /\ dirs[d][Parent(G)] = 0
           dirs1 == IF made THEN [dirs EXCEPT ![d][Parent(G)] = Len(ov) + 1] ELSE dirs
           rows == [f \in FileIds |-> [e \in Disks |->
                      LET r == cc[f][e] IN
                      \* a row past two names once rewritten, of a copy no longer where it says, as the folder below
                      \* its old name is gone from that disk, keeps its place, which is no more true than the code's
                      \* rewritten one, and the next walk finds the copy where it is either way
                      IF e = d /\ Ok2(f, e) /\ r.old # None /\ AtOrBelow(r.old, F) /\ Rewrite(r.old, F, G) \in FPaths
                      THEN [r EXCEPT !.old = IF Rewrite(r.old, F, G) = cf[f].path THEN None ELSE Rewrite(r.old, F, G)]
                      ELSE r]]
       IN IF J = {}
          THEN /\ pc' = [pc EXCEPT !.stage = "round", !.done = {}, !.moved = FALSE]
               /\ UNCHANGED <<ov, dirs, fl, cc, ce, fj, ghost>>
          ELSE /\ fj' = [fj EXCEPT ![d] = @ \ {j}]
               /\ pc' = [pc EXCEPT !.done = @ \cup {j}]
               /\ IF ready
                  THEN /\ ov' = IF made THEN Append(ov, 0) ELSE ov
                       /\ dirs' = [dirs1 EXCEPT ![d] = MovedMap(dirs1[d], F, G)]
                       /\ fl' = [fl EXCEPT ![d] = MovedMap(fl[d], F, G)]
                       /\ cc' = rows
                       /\ ce' = [ce EXCEPT ![d] = RewriteMap(IF made THEN [ce[d] EXCEPT ![Parent(G)] = Len(ov) + 1] ELSE ce[d], F, G)]
                       \* everything below moves with it, whatever it is (§15.12): what lands below the new name lands
                       \* there as the user's folder rename it carries out puts it, a subfolder a user renamed there too
                       /\ ghost' = [ghost EXCEPT !.umoved = @ \cup {<<fl[d][q], Rewrite(q, F, G)>> :
                                                                     q \in {r \in FPaths : AtOrBelow(r, F) /\ fl[d][r] # 0}}]
                  ELSE UNCHANGED <<ov, dirs, fl, cc, ce, ghost>>
    /\ UNCHANGED <<trash, dstat, cf, tj, moves, cnt>>

(* The renames of copies behind, by old path, round after round until one moves nothing: a copy below a job still
   queued, or landing at or below its old name, waits for it. A name the disk refuses leaves it behind (§15.12, step
   1, Refused names). Otherwise the look: the copy where it sits and nothing at the target goes there, the target's
   missing folders made first, Atoll's own; nothing where it sits and the copy at the target, already-there; anything
   else stays. *)
Behind(d) == {f \in FileIds : cf[f].on /\ Ok2(f, d) /\ cc[f][d].old # None}
BelowJob(d, f) == \E j \in fj[d] : AtOrBelow(cc[f][d].old, j[1]) \/ AtOrBelow(cf[f].path, j[1])

RenameCopy ==
    /\ pc.ph = "work" /\ pc.d <= NDisks /\ Up(pc.d) /\ pc.stage = "round"
    /\ LET d == pc.d
           B == {f \in Behind(d) \ pc.done : ~BelowJob(d, f)}
       IN IF B = {}
          THEN IF pc.moved
               THEN /\ pc' = [pc EXCEPT !.done = {}, !.moved = FALSE] /\ UNCHANGED <<ov, dirs, fl, cc, ce>>
               ELSE /\ pc' = [pc EXCEPT !.stage = "wishes"] /\ UNCHANGED <<ov, dirs, fl, cc, ce>>
          ELSE LET f == CHOOSE f \in B : \A g \in B : f = g \/ PathLess(cc[f][d].old, cc[g][d].old)
                   r == cc[f][d]
                   tgt == cf[f].path
                   src == fl[d][r.old]
                   there == fl[d][tgt]
                   missing == {p \in FPaths : AtOrBelow(tgt, p) /\ dirs[d][p] = 0}
                   ov1 == ov \o [i \in 1..Cardinality(missing) |-> 0]
                   mk == [p \in FPaths |-> IF p \in missing
                                           THEN Len(ov) + (IF Len(p) = 1 \/ <<p[1]>> \notin missing THEN 1 ELSE 2)
                                           ELSE dirs[d][p]]
               IN /\ pc' = [pc EXCEPT !.done = @ \cup {f},
                                      !.moved = @ \/ (~Refused(d, tgt) /\ ((src = r.obj /\ there = 0) \/ (src = 0 /\ there = r.obj)))]
                  /\ IF Refused(d, tgt)
                     THEN UNCHANGED <<ov, dirs, fl, cc, ce>>
                     ELSE IF src = r.obj /\ there = 0
                     THEN /\ ov' = ov1
                          /\ dirs' = [dirs EXCEPT ![d] = mk]
                          /\ ce' = [ce EXCEPT ![d] = [p \in FPaths |-> IF p \in missing THEN mk[p] ELSE ce[d][p]]]
                          /\ fl' = [fl EXCEPT ![d][r.old] = 0, ![d][tgt] = r.obj]
                          /\ cc' = [cc EXCEPT ![f][d].old = None]
                     ELSE IF src = 0 /\ there = r.obj
                     THEN /\ cc' = [cc EXCEPT ![f][d].old = None] /\ UNCHANGED <<ov, dirs, fl, ce>>
                     ELSE UNCHANGED <<ov, dirs, fl, cc, ce>>
    /\ UNCHANGED <<trash, dstat, cf, fj, tj, moves, cnt, ghost>>

(* findWishes: a copy behind whose file's name its disk refuses, with another copy at the file's path on a disk walked
   in this pass, gets its trash job, which runs the next pass (it has no job yet). *)
Wishes ==
    /\ pc.ph = "work" /\ pc.d <= NDisks /\ Up(pc.d) /\ pc.stage = "wishes"
    /\ LET d == pc.d IN
       tj' = [tj EXCEPT ![d] = @ \cup {f \in Behind(d) : Refused(d, cf[f].path) /\ d \in pc.walked /\ GoodAt(f, d)}]
    /\ pc' = NextDisk
    /\ UNCHANGED <<disk, cf, cc, ce, fj, moves, cnt, ghost>>

-----------------------------------------------------------------------------

Atoll == PassStart \/ WalkDisk \/ DecideFiles \/ DecideFolder \/ DecideDone
         \/ WorkEnd \/ WorkSkip \/ TrashJob \/ FolderJob \/ RenameCopy \/ Wishes

Next == Atoll \/ World

Spec == Init /\ [][Next]_vars

FairSpec == Spec /\ WF_vars(Atoll) /\ \A d \in Disks : WF_vars(Return(d))

-----------------------------------------------------------------------------
(* Invariants. *)

(* No version a user wrote is lost: each is at a path, or in a trash, of some disk. *)
Held(v) == \E d \in Disks : (\E p \in FPaths : fl[d][p] # 0 /\ ov[fl[d][p]] = v) \/ (\E o \in trash[d] : ov[o] = v)
NoUserDataLost == \A v \in ghost.written : Held(v)

(* 2: a copy goes to the trash for a name its disk refuses only while another copy of its file is at the file's path,
   so that the file keeps a copy where its catalog says. *)
LastCopyKept == "last-copy" \notin ghost.flags

(* §15.12: a file takes only a path a user moved one of its copies to. *)
RenamesAreUsers == "rename-reverted" \notin ghost.flags

(* 13 on one disk: no object is the copy of two files. *)
OneRowAnObject ==
    \A d \in Disks, f, g \in FileIds : (f # g /\ Ok2(f, d) /\ Ok2(g, d)) => cc[f][d].obj # cc[g][d].obj

TypeOK ==
    /\ \A d \in Disks : tj[d] \subseteq FileIds /\ \A j \in fj[d] : j[1] \in FPaths /\ Len(j[2]) \in 1..4
    /\ \A f \in FileIds : cf[f].path \in FPaths

(* Liveness: every copy follows its file, at its path on its disk, but one whose file's name its disk refuses, which
   stays behind or goes to the trash; no job is left; and each file is where its catalog says on a disk. *)
Followed ==
    /\ \A f \in FileIds, d \in Disks :
          (cf[f].on /\ Ok2(f, d) /\ ~Refused(d, cf[f].path)) => (cc[f][d].old = None /\ fl[d][cf[f].path] = cc[f][d].obj)
    /\ \A d \in Disks : fj[d] = {} /\ tj[d] = {}
    /\ \A f \in FileIds : cf[f].on => \E d \in Disks : Ok2(f, d) /\ cc[f][d].old = None /\ fl[d][cf[f].path] = cc[f][d].obj
EventuallyFollowed == <>[]Followed

=============================================================================
