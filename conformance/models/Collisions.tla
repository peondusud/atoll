------------------------------ MODULE Collisions ------------------------------
(* Name collisions on an NTFS disk (specs.md §15.7, §15.8, §12.4): a model of its own. Two disks: S, the source,
   whose filesystem holds every name (ext4), and N, an NTFS disk, the target of each file's replica. A path is a folder
   at the root and a file in it; folder names 1 and 2 share an lpath_key (Films, films), 3 has its own, and file names
   1 and 2 share one when SameNames (a.mkv, A.mkv). A path collides with a name on N when, at some component, their
   prefixes have the same key and different bytes (§15.7). N's driver finds a name by its bytes, as every driver Atoll
   writes to does (ntfs3, the type ntfs, ntfs-3g and lowntfs-3g as the mount check takes them, a short name aside,
   §15.8), or by its key (Fold), as ntfs3 and the type ntfs with nocase and lowntfs-3g with ignore_case would, which the
   mount check refuses (§10): what a lookup finds is never what decides, the listing right before each write is.

   What it keeps: on S, each file's path; on N, the folders at the root, each with who gave it its name, a user directly
   or Atoll, and the files in each, each a copy of a file of the catalog; the catalog's files, their path, the copy on
   N (none, pending or ok, where it sits), the entries of N's folders, the replica jobs queued for N and N's folder
   jobs. A user renames a file on S, or a folder there, which the passes take to N; makes a new file through Atoll's
   own view, which lands on N when N takes its name by the catalog, as the view asks (writer.refusedOn), on S
   otherwise; makes a folder directly on N. A pass: its walk of both disks, which records N's folders as entries,
   takes in what the view made and decides each rename on S, a file's leaving N's copy behind, a folder's giving N a
   job; then the plan, file by file in order, which places the replica of each file N lacks when no name collides by
   what the catalog has (catalog.KeysTaken: the files with a copy on N at their name, pending ones included, the names
   copies still sit at, and N's entries, for the folder then for the file) and by what the plan holds itself
   (solver.ownNames: the folders above a pending copy, the name a copy is to take while it sits at its old one, with
   the folder above it, and what this plan placed), the path free. What it queues runs at any time after: a replica
   job (jobs.copier: LookAtTarget, its missing folder made, the part moved to the file's path as it is then); the
   rename of a copy behind (jobs.runRenames: refusal by the catalog's keys but for its old name, its folder made); a
   folder's job (jobs.runFolderJob: refusal of the new name by the catalog's keys, the folder its entry records at the
   old name, the new free). Atoll records each folder it makes as an entry. Right before each name Atoll makes on N,
   the view's included, it lists the folder the name goes in, and each folder on the way, and refuses the name when
   one holds another of its key, whoever made it (disk-key-look, DiskCollides: the maintainer's remedy of flaws 24 and
   25, 2026-11-25, README.md).

   What it leaves out: everything else of a pass and of a copy (Replication.tla's, Copies.tla's), the steps of each
   operation and their reprise, crashes, disks lost, deletions, conflicts of content (Conflicts.tla), names a disk
   refuses (Nested.tla), folders below folders, a user's own files on N, the trash. *)
EXTENDS Integers, FiniteSets, TLC

CONSTANTS
    NFolders,   \* folder names at the root, by number: 1 and 2 share a key, 3 has its own
    NNames,     \* file names in a folder, by number
    SameNames,  \* file names 1 and 2 share a key
    Fold,       \* N's driver finds a name by its key, as ntfs3 a pair in two cases; FALSE: by its bytes
    NFiles,     \* file ids
    StartPath,  \* [FileIds -> path or None]: each file's path on S at the start; None, a file the view may make
    StartN,     \* SUBSET FileIds: the files with a copy on N at the start, at their path
    Acts,       \* some of "mv", "mvdir", "view", "mkdir": what a user does
    MaxActs,    \* how many times
    Fixes       \* the remedies proposed for the flaws found here and still open, by name (README.md); {} is specs.md as it is

FNames == 1..NFolders
Names == 1..NNames
Paths == FNames \X Names
None == <<0, 0>>
FileIds == 1..NFiles
\* flaws 24 and 25 are fixed, with disk-key-look, what every instance does (README.md): no remedy is proposed
FixNames == {}

ASSUME /\ NFolders \in 2..3 /\ NNames \in 1..2 /\ SameNames \in BOOLEAN /\ Fold \in BOOLEAN /\ NFiles \in 1..3
       /\ StartPath \in [FileIds -> Paths \cup {None}] /\ StartN \subseteq FileIds
       /\ \A f \in StartN : StartPath[f] # None
       /\ \A f, g \in FileIds : f # g /\ StartPath[f] # None => StartPath[f] # StartPath[g]
       /\ Acts \subseteq {"mv", "mvdir", "view", "mkdir"} /\ MaxActs \in Nat
       /\ Fixes \subseteq FixNames

(* The keys (§15.8): a folder's, a file name's, a path's. *)
FKey(F) == IF F \in {1, 2} THEN 1 ELSE F
NKey(a) == IF SameNames /\ a \in {1, 2} THEN 1 ELSE a
CollideF(F, G) == F # G /\ FKey(F) = FKey(G)
SameKey(p, q) == FKey(p[1]) = FKey(q[1]) /\ NKey(p[2]) = NKey(q[2])
(* A path collides with a name of N: at the folder, or at the whole path (§15.7). *)
CollidesPath(p, q) == p # q /\ (CollideF(p[1], q[1]) \/ SameKey(p, q))

VARIABLES
    sp,     \* [FileIds -> path or None]: where S holds each file
    nd,     \* [FNames -> "none", "user" or "atoll"]: N's folders, and who gave each its name
    nf,     \* [FNames -> [Names -> [o |-> file id or 0, by |-> "user" or "atoll"]]]: N's files
    cp,     \* [FileIds -> path or None]: the catalog's path of each file
    cn,     \* [FileIds -> [st |-> "none", "pending" or "ok", at |-> path]]: its copy on N, and where it sits
    ent,    \* SUBSET FNames: N's entries
    rq,     \* SUBSET FileIds: the replica jobs queued for N
    fjob,   \* SUBSET (FNames \X FNames): N's folder jobs
    smv,    \* SUBSET (FNames \X FNames): the folders a user renamed on S, for the next walk
    vnew,   \* SUBSET FileIds: the files the view made on N that no walk took in yet
    acts,   \* how many times a user acted
    ghost   \* for the invariants

vars == <<sp, nd, nf, cp, cn, ent, rq, fjob, smv, vnew, acts, ghost>>

NoFile == [o |-> 0, by |-> "atoll"]
NoCopy == [st |-> "none", at |-> None]
Flag(name, cond) == IF cond THEN ghost \cup {name} ELSE ghost

Init ==
    /\ sp = StartPath
    /\ cp = StartPath
    /\ cn = [f \in FileIds |-> IF f \in StartN THEN [st |-> "ok", at |-> StartPath[f]] ELSE NoCopy]
    /\ nd = [F \in FNames |-> IF \E f \in StartN : StartPath[f][1] = F THEN "atoll" ELSE "none"]
    /\ nf = [F \in FNames |-> [a \in Names |->
               IF \E f \in StartN : StartPath[f] = <<F, a>>
               THEN [o |-> CHOOSE f \in StartN : StartPath[f] = <<F, a>>, by |-> "atoll"] ELSE NoFile]]
    /\ ent = {F \in FNames : \E f \in StartN : StartPath[f][1] = F}
    /\ rq = {} /\ fjob = {} /\ smv = {} /\ vnew = {}
    /\ acts = 0
    /\ ghost = {}

-----------------------------------------------------------------------------
(* N's driver: the folder a lookup of a name finds, 0 for none. *)
FindF(F) ==
    IF nd[F] # "none" THEN F
    ELSE IF Fold /\ \E G \in FNames : CollideF(G, F) /\ nd[G] # "none"
         THEN CHOOSE G \in FNames : CollideF(G, F) /\ nd[G] # "none"
         ELSE 0
FindN(F, a) ==
    IF nf[F][a].o # 0 THEN a
    ELSE IF Fold /\ \E b \in Names : b # a /\ NKey(b) = NKey(a) /\ nf[F][b].o # 0
         THEN CHOOSE b \in Names : b # a /\ NKey(b) = NKey(a) /\ nf[F][b].o # 0
         ELSE 0

(* What the catalog says N holds (catalog.KeyTakenExcept): the files with a copy on N at their name, a pending one
   included, the names copies behind still sit at, and N's entries; but except and what is below it. *)
Sits(g) == cn[g].st # "none" /\ cn[g].at = cp[g]
Behind(g) == cn[g].st = "ok" /\ cp[g] # None /\ cn[g].at # cp[g]
CatFolders(except) ==
    {F \in ent : F # except[1] \/ except[2] # 0} \ (IF except[2] = 0 THEN {except[1]} ELSE {})
CatPaths(except) ==
    ({cp[g] : g \in {g \in FileIds : Sits(g)}} \cup {cn[g].at : g \in {g \in FileIds : Behind(g)}})
    \ {q \in Paths : q = except \/ (except[2] = 0 /\ q[1] = except[1])}
(* except: a path a rename frees, or <<F, 0>> for a folder's. *)
TakenByCat(p, except) ==
    \/ \E G \in CatFolders(except) : CollideF(G, p[1])
    \/ \E q \in CatPaths(except) : q # p /\ SameKey(p, q)

(* What a plan holds itself (solver.ownNames), and what it placed: the folders above a pending copy, the name a copy
   behind is to take and its folder. *)
OwnFolders == {cp[g][1] : g \in {g \in FileIds : cn[g].st = "pending" \/ Behind(g)}}
OwnPaths == {cp[g] : g \in {g \in FileIds : Behind(g)}}
PlanRefuses(f, placed) ==
    LET p == cp[f] IN
    \/ TakenByCat(p, None)
    \/ \E G \in OwnFolders \cup {q[1] : q \in placed} : CollideF(G, p[1])
    \/ \E q \in OwnPaths \cup placed : q # p /\ SameKey(p, q)
PathFree(f, placed) ==
    /\ ~\E g \in FileIds \ {f} : cn[g].st # "none" /\ (cn[g].at = cp[f] \/ cp[g] = cp[f])
    /\ cp[f] \notin placed

(* Atoll writes a name on N: a folder, or a file in a folder. It collides with what N holds, whoever made it. *)
WritesF(F) == \E G \in FNames : CollideF(G, F) /\ nd[G] # "none"
(* but the name o, which the rename that writes this one frees: a case-only rename does not collide with its own old
   name (§15.7) *)
WritesN(F, a, o) == \E b \in Names : b # a /\ NKey(b) = NKey(a) /\ nf[F][b].o # 0 /\ <<F, b>> # o
WritesFile(F, a, o) == WritesF(F) \/ WritesN(F, a, o)

(* disk-key-look, the maintainer's remedy of flaws 24 and 25 (2026-11-25): right before Atoll makes a name on an NTFS
   disk, it lists the folder the name goes in, and each folder on the way (ops.lookThrough, ops.refuseColliding), and
   refuses when one holds another name with the key of the one it holds or makes there, whoever made it, but o, the
   name a case-only rename frees: the disk is the truth, what no row says yet included. The listing decides whether a
   name is there, never what a lookup finds. *)
DiskCollides(F, a, o) ==
    \/ \E G \in FNames : CollideF(G, F) /\ nd[G] # "none"
    \/ nd[F] # "none" /\ \E b \in Names : b # a /\ NKey(b) = NKey(a) /\ nf[F][b].o # 0 /\ <<F, b>> # o

-----------------------------------------------------------------------------
(* The world: a user. *)
UMv(f, q) ==
    /\ "mv" \in Acts /\ acts < MaxActs /\ sp[f] # None /\ q # sp[f]
    /\ ~\E g \in FileIds : sp[g] = q
    /\ sp' = [sp EXCEPT ![f] = q]
    /\ acts' = acts + 1
    /\ UNCHANGED <<nd, nf, cp, cn, ent, rq, fjob, smv, vnew, ghost>>

UMvDir(F, G) ==
    /\ "mvdir" \in Acts /\ acts < MaxActs /\ F # G
    /\ \E f \in FileIds : sp[f] # None /\ sp[f][1] = F
    /\ ~\E f \in FileIds : sp[f] # None /\ sp[f][1] = G
    /\ sp' = [f \in FileIds |-> IF sp[f] # None /\ sp[f][1] = F THEN <<G, sp[f][2]>> ELSE sp[f]]
    /\ smv' = smv \cup {<<F, G>>}
    /\ acts' = acts + 1
    /\ UNCHANGED <<nd, nf, cp, cn, ent, rq, fjob, vnew, ghost>>

(* A user makes a folder directly on N: refused by a driver that finds another by its key. *)
UMkdir(F) ==
    /\ "mkdir" \in Acts /\ acts < MaxActs /\ FindF(F) = 0
    /\ nd' = [nd EXCEPT ![F] = "user"]
    /\ acts' = acts + 1
    /\ UNCHANGED <<sp, nf, cp, cn, ent, rq, fjob, smv, vnew, ghost>>

(* A new file through Atoll's own view, at a name the view presents nothing at: on N when N takes the name by what the
   catalog says (writer.refusedOn) and by what N's folders list right before (disk-key-look; writer.elsewhere), its
   folder made there when the driver finds none; on S otherwise. *)
UView(f, p) ==
    /\ "view" \in Acts /\ acts < MaxActs /\ sp[f] = None /\ cp[f] = None /\ f \notin vnew
    /\ ~\E g \in FileIds : cp[g] = p \/ sp[g] = p \/ (g \in vnew /\ cn[g].at = p)
    /\ acts' = acts + 1
    /\ IF ~TakenByCat(p, None) /\ ~DiskCollides(p[1], p[2], None)
       THEN LET G == IF FindF(p[1]) = 0 THEN p[1] ELSE FindF(p[1]) IN
            /\ FindN(G, p[2]) = 0
            /\ nd' = IF FindF(p[1]) = 0 THEN [nd EXCEPT ![p[1]] = "atoll"] ELSE nd
            /\ nf' = [nf EXCEPT ![G][p[2]] = [o |-> f, by |-> "atoll"]]
            /\ cn' = [cn EXCEPT ![f] = [st |-> "ok", at |-> p]]
            /\ vnew' = vnew \cup {f}
            /\ ghost' = Flag("wrote-colliding", WritesFile(G, p[2], None))
            /\ UNCHANGED <<sp, cp, ent, rq, fjob, smv>>
       ELSE /\ sp' = [sp EXCEPT ![f] = p]
            /\ UNCHANGED <<nd, nf, cp, cn, ent, rq, fjob, smv, vnew, ghost>>

-----------------------------------------------------------------------------
(* A pass. Its walk: N's folders recorded as entries; what the view made taken in; the renames on S decided, a file's
   leaving N's copy behind, its job a rename; a folder's moving N's copies below it with it in the catalog, behind,
   and giving N a job when N has its entry. *)
PassWalk ==
    /\ LET newPath(f) == IF f \in vnew THEN cn[f].at ELSE IF sp[f] # None THEN sp[f] ELSE cp[f]
       IN /\ cp' = [f \in FileIds |-> newPath(f)]
          /\ fjob' = fjob \cup {m \in smv : m[1] \in ent}
    /\ ent' = {F \in FNames : nd[F] # "none"}
    /\ smv' = {} /\ vnew' = {}
    /\ UNCHANGED <<sp, nd, nf, cn, rq, acts, ghost>>

(* The plan, file by file in order: a replica of each file N lacks, when no name collides and the path is free. *)
RECURSIVE PlanFrom(_, _, _)
PlanFrom(i, placed, put) ==
    IF i > NFiles THEN put
    ELSE IF /\ cp[i] # None /\ sp[i] # None /\ cn[i].st = "none" /\ i \notin rq
            /\ ~PlanRefuses(i, placed) /\ PathFree(i, placed)
         THEN PlanFrom(i + 1, placed \cup {cp[i]}, put \cup {i})
         ELSE PlanFrom(i + 1, placed, put)
PassPlan ==
    /\ LET put == PlanFrom(1, {}, {}) IN
       /\ put # {}
       /\ cn' = [f \in FileIds |-> IF f \in put THEN [st |-> "pending", at |-> cp[f]] ELSE cn[f]]
       /\ rq' = rq \cup put
    /\ UNCHANGED <<sp, nd, nf, cp, ent, fjob, smv, vnew, acts, ghost>>

(* A replica job (jobs.copier): the target is the file's path as it is then; the look at it (ops.LookAtTarget), which
   lists each folder on the way, and its folder's listing right before it is made, and the file's folder's right before
   the part takes its name, refuse a name with the key of another there (disk-key-look), name-refused, and the job
   goes; otherwise a lookup of its folder, made when there is none and recorded, then of its name; what the lookup
   finds there is a conflict and the job goes; the part is moved to the name. *)
Replica(f) ==
    /\ f \in rq
    /\ LET p == cp[f]
           G == IF FindF(p[1]) = 0 THEN p[1] ELSE FindF(p[1])
           refused == DiskCollides(p[1], p[2], None)
       IN /\ rq' = rq \ {f}
          /\ IF refused \/ FindN(G, p[2]) # 0
             THEN /\ cn' = [cn EXCEPT ![f] = NoCopy]
                  /\ UNCHANGED <<nd, nf, ent, ghost>>
             ELSE /\ nd' = IF FindF(p[1]) = 0 THEN [nd EXCEPT ![p[1]] = "atoll"] ELSE nd
                  /\ ent' = IF FindF(p[1]) = 0 THEN ent \cup {p[1]} ELSE ent
                  /\ nf' = [nf EXCEPT ![G][p[2]] = [o |-> f, by |-> "atoll"]]
                  /\ cn' = [cn EXCEPT ![f] = [st |-> "ok", at |-> p]]
                  /\ ghost' = Flag("wrote-colliding", WritesFile(G, p[2], None))
    /\ UNCHANGED <<sp, cp, fjob, smv, vnew, acts>>

(* The rename of a copy behind on N (jobs.runRenames): refused when the catalog's keys hold the new name, but for its
   old one, or when N's folders list a name of its key right before, but its old one (disk-key-look); its folder made
   when the driver finds none, and recorded; its name free by a lookup. *)
RenameCopy(f) ==
    /\ Behind(f) /\ ~\E m \in fjob : m[2] = cp[f][1]
    /\ LET p == cp[f]
           o == cn[f].at
           G == IF FindF(p[1]) = 0 THEN p[1] ELSE FindF(p[1])
       IN /\ ~TakenByCat(p, o) /\ ~DiskCollides(p[1], p[2], o)
          \* free, or a case-only rename onto its own old name, which the driver finds by its key
          /\ FindN(G, p[2]) = 0 \/ (G = o[1] /\ FindN(G, p[2]) = o[2])
          /\ nd' = IF FindF(p[1]) = 0 THEN [nd EXCEPT ![p[1]] = "atoll"] ELSE nd
          /\ ent' = IF FindF(p[1]) = 0 THEN ent \cup {p[1]} ELSE ent
          /\ nf' = [nf EXCEPT ![o[1]][o[2]] = NoFile, ![G][p[2]] = [o |-> f, by |-> "atoll"]]
          /\ cn' = [cn EXCEPT ![f].at = p]
          /\ ghost' = Flag("wrote-colliding", WritesFile(G, p[2], o))
    /\ UNCHANGED <<sp, cp, rq, fjob, smv, vnew, acts>>

(* A folder's job on N (jobs.runFolderJob): refused when the catalog's keys hold the new name, but for the old one, or
   when N's root lists a name of its key right before, but the old one (disk-key-look); run when the folder its entry
   records is at the old name and the new is free by a lookup; dropped otherwise, its copies following one by one. *)
FolderJob(F, G) ==
    /\ <<F, G>> \in fjob
    /\ fjob' = fjob \ {<<F, G>>}
    /\ IF ~(\E H \in CatFolders(<<F, 0>>) : CollideF(H, G))
          /\ ~(\E H \in FNames \ {F} : CollideF(H, G) /\ nd[H] # "none")
          /\ nd[F] # "none" /\ F \in ent /\ FindF(G) = 0
       THEN /\ nd' = [nd EXCEPT ![G] = "atoll", ![F] = "none"]
            /\ nf' = [nf EXCEPT ![G] = nf[F], ![F] = [a \in Names |-> NoFile]]
            /\ ent' = (ent \ {F}) \cup {G}
            /\ cn' = [g \in FileIds |-> IF cn[g].st = "ok" /\ cn[g].at[1] = F THEN [cn[g] EXCEPT !.at = <<G, @[2]>>]
                                        ELSE cn[g]]
            /\ ghost' = Flag("wrote-colliding", \E H \in FNames \ {F} : CollideF(H, G) /\ nd[H] # "none")
       ELSE UNCHANGED <<nd, nf, ent, cn, ghost>>
    /\ UNCHANGED <<sp, cp, rq, smv, vnew, acts>>

Next ==
    \/ \E f \in FileIds, q \in Paths : UMv(f, q)
    \/ \E F, G \in FNames : UMvDir(F, G)
    \/ \E F \in FNames : UMkdir(F)
    \/ \E f \in FileIds, p \in Paths : UView(f, p)
    \/ PassWalk \/ PassPlan
    \/ \E f \in FileIds : Replica(f) \/ RenameCopy(f)
    \/ \E F, G \in FNames : FolderJob(F, G)

Spec == Init /\ [][Next]_vars

-----------------------------------------------------------------------------
TypeOK ==
    /\ \A f \in FileIds : cn[f].st \in {"none", "pending", "ok"}
    /\ \A F \in FNames : nd[F] \in {"none", "user", "atoll"}
    /\ ghost \subseteq {"wrote-colliding"}

(* §15.7: no NTFS disk holds two names with one key that Atoll wrote. *)
NoAtollPair ==
    /\ ~\E F, G \in FNames : CollideF(F, G) /\ nd[F] = "atoll" /\ nd[G] = "atoll"
    /\ ~\E F \in FNames, a, b \in Names : a # b /\ NKey(a) = NKey(b)
                                          /\ nf[F][a].o # 0 /\ nf[F][b].o # 0
                                          /\ nf[F][a].by = "atoll" /\ nf[F][b].by = "atoll"

(* §15.7: Atoll never writes a name that collides with one N holds, whoever made it, one no walk found yet included. *)
WritesNoCollision == "wrote-colliding" \notin ghost

(* Invariant 6: a copy Atoll wrote on N is at its name, to the byte, where the catalog says it sits. *)
CopiesAtTheirNames == \A f \in FileIds : cn[f].st = "ok" => nf[cn[f].at[1]][cn[f].at[2]].o = f=============================================================================
