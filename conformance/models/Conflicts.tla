------------------------------- MODULE Conflicts -------------------------------
(* A file in conflict and its resolution (specs.md §15.7, §16.1, §21.2): a model of its own. One file, a copy on each
   of two disks, each copy an object (an inode) holding a version, as Replication.tla has them; the file in conflict
   from the start, as after concurrent changes, at a disk's adoption or after catalog rebuild (§9), or once a repair
   finds that a corrupt copy that changed holds another content (§16.1). What it keeps: on each disk, the object at
   the file's path and its version, and the objects in the trash; the catalog's file (conflict, pending once resolved,
   or ok once replicated), the original it presents (files.original_disk) and each copy's row (what it last recorded of
   the object, or none); Atoll's own view, which presents the copy the catalog names (view.presentFile: the original of
   a file in conflict, the copy it presented before), and a handle a program holds open on that copy to write; and
   `atoll conflicts resolve --keep`, step by step (trash.Session.Resolve): its checks, its look at each copy as it is
   now, its event (catalog.BeginResolve) before anything moves, the kept copy's row brought to what its disk holds,
   then each other copy in turn: on a disk that cannot be reached it stays, one no longer there loses its row, any
   other has its row brought to what its disk holds and goes to the trash, `overwritten`, a move checked against that
   row right before it (trash.Session.Send); once none is left, the file pending from the kept copy, which the next
   pass copies to the other disk. With Corrupt, the file starts corrupt instead, disk 1's copy rotted, and its repair
   by chunk (§16.1): a copy that changed since it was found corrupt settled before anything is written, a conflict
   when it holds another version, nothing written; otherwise its bad chunks written from the healthy copy, the copy
   compared with its row right before the first write. A user writes into a copy in place, replaces it by an atomic
   save, or deletes it, on either disk at any moment; through the view, opens the copy it presents, writes and closes;
   a disk is lost and mounted again.

   What it leaves out: the passes but their copy (Replication.tla's), the scrub's other steps and the repair by file,
   a chunk's bytes (a version stands for the content), more copies and disks, the steps of a move and their reprise,
   the power cut. *)
EXTENDS Integers, FiniteSets, Sequences, TLC

CONSTANTS
    StartVer,   \* [Disks -> version]: the version each disk's copy holds at the start, the file in conflict
    StartOrig,  \* the disk whose copy the view presented before the conflict
    Corrupt,    \* the start is a file corrupt instead: disk 1's copy rotted, disk 2's healthy, a repair to run (§16.1)
    UserActs,   \* some of "write", "save", "delete": a user directly on a disk
    MaxUser,
    MaxView,    \* how many writes through the view
    MaxDrop,    \* how many times a disk is lost
    Fixes       \* the remedies proposed for the flaws found here and still open, by name (README.md); {} is specs.md as it is

NDisks == 2
Disks == 1..NDisks
\* flaw 26 is open, its remedy proposed here, the maintainer's to choose (README.md)
FixNames == {"resolve-view-free"}

ASSUME /\ StartVer \in [Disks -> 1..2] /\ StartVer[1] # StartVer[2] /\ StartOrig \in Disks /\ Corrupt \in BOOLEAN
       /\ UserActs \subseteq {"write", "save", "delete"} /\ MaxUser \in Nat /\ MaxView \in Nat /\ MaxDrop \in Nat
       /\ Fixes \subseteq FixNames

VARIABLES
    objs,   \* <<[ver, disk, at]>>: each object, its version, its disk, and where it is: "path", "trash" or "gone"
    up,     \* [Disks -> BOOLEAN]
    fst,    \* the file's state: "conflict", "pending" or "ok"
    orig,   \* the disk the catalog names as the file's original (0 for none)
    row,    \* [Disks -> object id or 0]: the copy each row records, as its identity and change count say
    rver,   \* [Disks -> version or 0]: the version each row records
    rs,     \* the resolve under way: [ph, keep, seen, sv, i, stayed]
    rp,     \* the repair of disk 1's copy under way: "idle" or "write"
    vh,     \* the view's handle: [o |-> object or 0, n |-> writes]
    nver,   \* the last version written
    cnt,    \* [user, drop]
    ghost   \* [written, udest, said, flags]

vars == <<objs, up, fst, orig, row, rver, rs, rp, vh, nver, cnt, ghost>>

AtPath(d) == IF \E o \in DOMAIN objs : objs[o].disk = d /\ objs[o].at = "path"
             THEN CHOOSE o \in DOMAIN objs : objs[o].disk = d /\ objs[o].at = "path" ELSE 0
RIdle == [ph |-> "idle", keep |-> 0, seen |-> [d \in Disks |-> 0], sv |-> [d \in Disks |-> 0], i |-> 0, stayed |-> FALSE]
Flag(g, name, cond) == IF cond THEN [g EXCEPT !.flags = @ \cup {name}] ELSE g

(* With Corrupt, disk 1's copy holds version 0, rot of version 1, which no user wrote, and both rows say version 1. *)
Init ==
    /\ objs = [o \in Disks |-> [ver |-> IF Corrupt THEN o - 1 ELSE StartVer[o], disk |-> o, at |-> "path"]]
    /\ up = [d \in Disks |-> TRUE]
    /\ fst = IF Corrupt THEN "corrupt" ELSE "conflict"
    /\ orig = StartOrig
    /\ row = [d \in Disks |-> d]
    /\ rver = IF Corrupt THEN [d \in Disks |-> 1] ELSE StartVer
    /\ rs = RIdle
    /\ rp = "idle"
    /\ vh = [o |-> 0, n |-> 0]
    /\ nver = 2
    /\ cnt = [user |-> 0, drop |-> 0]
    /\ ghost = [written |-> IF Corrupt THEN {1} ELSE {StartVer[1], StartVer[2]}, udest |-> {}, said |-> {}, sent |-> {},
                flags |-> {}]

-----------------------------------------------------------------------------
(* The world. A user, directly on a disk: a write in place (a new version, the same object), an atomic save (a new
   object at the path, the one it replaces gone), a deletion. *)
UAllowed(a) == a \in UserActs /\ cnt.user < MaxUser
UWrite(d) ==
    /\ UAllowed("write") /\ up[d] /\ AtPath(d) # 0
    /\ LET o == AtPath(d) IN
       /\ objs' = [objs EXCEPT ![o].ver = nver + 1]
       /\ ghost' = [ghost EXCEPT !.written = @ \cup {nver + 1}, !.udest = @ \cup {objs[o].ver}]
    /\ nver' = nver + 1 /\ cnt' = [cnt EXCEPT !.user = @ + 1]
    /\ UNCHANGED <<up, fst, orig, row, rver, rs, vh, rp>>
USave(d) ==
    /\ UAllowed("save") /\ up[d] /\ AtPath(d) # 0
    /\ LET o == AtPath(d) IN
       /\ objs' = Append([objs EXCEPT ![o].at = "gone"], [ver |-> nver + 1, disk |-> d, at |-> "path"])
       /\ ghost' = [ghost EXCEPT !.written = @ \cup {nver + 1}, !.udest = @ \cup {objs[o].ver}]
    /\ nver' = nver + 1 /\ cnt' = [cnt EXCEPT !.user = @ + 1]
    /\ UNCHANGED <<up, fst, orig, row, rver, rs, vh, rp>>
UDelete(d) ==
    /\ UAllowed("delete") /\ up[d] /\ AtPath(d) # 0
    /\ LET o == AtPath(d) IN
       /\ objs' = [objs EXCEPT ![o].at = "gone"]
       /\ ghost' = [ghost EXCEPT !.udest = @ \cup {objs[o].ver}]
    /\ cnt' = [cnt EXCEPT !.user = @ + 1]
    /\ UNCHANGED <<up, fst, orig, row, rver, rs, vh, nver, rp>>
Drop(d) ==
    /\ cnt.drop < MaxDrop /\ up[d]
    /\ up' = [up EXCEPT ![d] = FALSE] /\ cnt' = [cnt EXCEPT !.drop = @ + 1]
    /\ UNCHANGED <<objs, fst, orig, row, rver, rs, vh, nver, ghost, rp>>
Return(d) ==
    /\ ~up[d] /\ up' = [up EXCEPT ![d] = TRUE]
    /\ UNCHANGED <<objs, fst, orig, row, rver, rs, vh, nver, cnt, ghost, rp>>

(* Atoll's own view: the copy it presents (view.presentFile): of a file in conflict the original it presented before,
   of another the first copy whose row holds it on a disk there; a program opens it to write, writes into the object
   it opened wherever that object is then, and closes it. *)
Presented ==
    IF fst \in {"conflict", "corrupt"}
    THEN IF row[orig] # 0 /\ up[orig] THEN orig ELSE 0
    ELSE IF \E d \in Disks : row[d] # 0 /\ up[d] THEN CHOOSE d \in Disks : row[d] # 0 /\ up[d] /\ \A e \in Disks : e < d => ~(row[e] # 0 /\ up[e])
         ELSE 0
VOpen ==
    /\ vh.o = 0 /\ vh.n < MaxView /\ Presented # 0 /\ AtPath(Presented) # 0 /\ objs[AtPath(Presented)].disk = Presented
    /\ vh' = [vh EXCEPT !.o = AtPath(Presented)]
    /\ ghost' = Flag(ghost, "presents-sent", objs[AtPath(Presented)].ver \in ghost.sent)
    /\ UNCHANGED <<objs, up, fst, orig, row, rver, rs, nver, cnt, rp>>
VWrite ==
    /\ vh.o # 0 /\ vh.n < MaxView /\ up[objs[vh.o].disk]
    /\ objs' = [objs EXCEPT ![vh.o].ver = nver + 1]
    /\ nver' = nver + 1
    /\ vh' = [vh EXCEPT !.n = @ + 1]
    \* into a file a user's own save or deletion took from its name, what it writes is that user's to lose
    /\ ghost' = [ghost EXCEPT !.written = @ \cup {nver + 1},
                              !.udest = @ \cup {objs[vh.o].ver} \cup (IF objs[vh.o].at = "gone" THEN {nver + 1} ELSE {})]
    /\ UNCHANGED <<up, fst, orig, row, rver, rs, cnt, rp>>
VClose ==
    /\ vh.o # 0 /\ vh' = [vh EXCEPT !.o = 0]
    /\ UNCHANGED <<objs, up, fst, orig, row, rver, rs, nver, cnt, ghost, rp>>

-----------------------------------------------------------------------------
(* atoll conflicts resolve <path> --keep <disk> (trash.Session.Resolve). Refused, with nothing done, unless the file
   is in conflict, holds none while corrupt, and has a row on that disk. Its look at each copy as it is now; the kept
   copy gone, or on a disk that cannot be reached: nothing moves. *)
RStart(k) ==
    /\ rs.ph = "idle" /\ fst = "conflict" /\ row[k] # 0
    /\ LET seen == [d \in Disks |-> IF up[d] THEN AtPath(d) ELSE -1]
           sv == [d \in Disks |-> IF seen[d] > 0 THEN objs[seen[d]].ver ELSE 0]
       IN IF seen[k] <= 0
          THEN UNCHANGED <<rs, row, rver, ghost>>
          \* its event before anything moves, and the kept copy's row brought to what its disk holds
          ELSE /\ rs' = [ph |-> "other", keep |-> k, seen |-> seen, sv |-> sv, i |-> 1, stayed |-> FALSE]
               /\ row' = [row EXCEPT ![k] = seen[k]] /\ rver' = [rver EXCEPT ![k] = sv[k]]
               /\ ghost' = [ghost EXCEPT !.said = @ \cup {sv[d] : d \in {d \in Disks \ {k} : seen[d] > 0}}]
    /\ UNCHANGED <<objs, up, fst, orig, vh, nver, cnt, rp>>

(* Each other copy in turn: on a disk that could not be reached it stays; gone, its row goes; otherwise its row is
   brought to what was seen, then the move to the trash, checked against that row right before it: a copy that is no
   longer the object and version the row says is changed, and stays. With resolve-view-free, proposed, the move also
   waits while the view holds that copy open to write, as the SSD cache's transfer does (TrashStart.ViewFree). *)
ROther ==
    /\ rs.ph = "other" /\ rs.i <= NDisks
    /\ LET d == rs.i
           s == rs.seen[d]
           next == [rs EXCEPT !.i = @ + 1]
       IN IF d = rs.keep THEN /\ rs' = next /\ UNCHANGED <<objs, row, rver, ghost>>
          ELSE IF s = -1 \/ ~up[d] THEN /\ rs' = [next EXCEPT !.stayed = TRUE] /\ UNCHANGED <<objs, row, rver, ghost>>
          ELSE IF s = 0 THEN /\ rs' = next /\ row' = [row EXCEPT ![d] = 0] /\ rver' = [rver EXCEPT ![d] = 0]
                             /\ UNCHANGED <<objs, ghost>>
          ELSE IF AtPath(d) # s \/ objs[s].ver # rs.sv[d] \/ ("resolve-view-free" \in Fixes /\ vh.o = s)
               THEN /\ rs' = [next EXCEPT !.stayed = TRUE]
                    /\ row' = [row EXCEPT ![d] = s] /\ rver' = [rver EXCEPT ![d] = rs.sv[d]]
                    /\ UNCHANGED <<objs, ghost>>
               ELSE /\ objs' = [objs EXCEPT ![s].at = "trash"]
                    /\ row' = [row EXCEPT ![d] = 0] /\ rver' = [rver EXCEPT ![d] = 0]
                    /\ rs' = next
                    /\ ghost' = [ghost EXCEPT !.sent = @ \cup {objs[s].ver}]
    /\ UNCHANGED <<up, fst, orig, vh, nver, cnt, rp>>

(* Once every copy was taken: with none left but the kept one, the file pending from it, its original. *)
REnd ==
    /\ rs.ph = "other" /\ rs.i > NDisks
    /\ IF ~rs.stayed /\ \A d \in Disks \ {rs.keep} : row[d] = 0
       THEN /\ fst' = "pending" /\ orig' = rs.keep
       ELSE UNCHANGED <<fst, orig>>
    /\ rs' = RIdle
    /\ UNCHANGED <<objs, up, row, rver, vh, nver, cnt, ghost, rp>>

(* The next pass's copy of a pending file from its original to the other disk, where nothing is at the path, as the
   object it reads holds the version its row says; the file ok. *)
Replicate ==
    /\ fst = "pending" /\ rs.ph = "idle"
    /\ LET k == orig
           e == CHOOSE e \in Disks : e # k
       IN /\ up[k] /\ up[e] /\ AtPath(k) # 0 /\ AtPath(k) = row[k] /\ objs[row[k]].ver = rver[k] /\ AtPath(e) = 0
          /\ objs' = Append(objs, [ver |-> rver[k], disk |-> e, at |-> "path"])
          /\ row' = [row EXCEPT ![e] = Len(objs) + 1] /\ rver' = [rver EXCEPT ![e] = rver[k]]
          /\ fst' = "ok"
    /\ UNCHANGED <<up, orig, rs, vh, nver, cnt, ghost, rp>>

(* The repair of disk 1's corrupt copy, by chunk (§16.1): a copy whose name holds another object, or another version,
   than its row says, was touched since it was found corrupt, and is settled before anything is written: holding the
   file's content it is ok; holding another, the file becomes a conflict, every version kept, the copy's row taking
   what the look found, nothing written. Unchanged, the bad chunks are written from the healthy copy, the copy
   compared with its row right before the first write (step 4): one changed since stops the repair, nothing written. *)
RLook ==
    /\ Corrupt /\ fst = "corrupt" /\ rp = "idle" /\ up[1] /\ up[2] /\ AtPath(1) # 0
    /\ LET o == AtPath(1) IN
       IF o = row[1] /\ objs[o].ver = 0
       THEN /\ rp' = "write" /\ UNCHANGED <<fst, row, rver>>
       ELSE /\ fst' = IF objs[o].ver = rver[1] THEN "ok" ELSE "conflict"
            /\ row' = [row EXCEPT ![1] = o] /\ rver' = [rver EXCEPT ![1] = objs[o].ver]
            /\ UNCHANGED rp
    /\ UNCHANGED <<objs, up, orig, rs, vh, nver, cnt, ghost>>
RWrite ==
    /\ rp = "write" /\ rp' = "idle"
    /\ IF up[1] /\ AtPath(1) = row[1] /\ objs[row[1]].ver = 0
       THEN /\ objs' = [objs EXCEPT ![row[1]].ver = rver[1]]
            /\ fst' = "ok"
       ELSE UNCHANGED <<objs, fst>>
    /\ UNCHANGED <<up, orig, row, rver, rs, vh, nver, cnt, ghost>>

Next ==
    \/ \E d \in Disks : UWrite(d) \/ USave(d) \/ UDelete(d) \/ Drop(d) \/ Return(d)
    \/ VOpen \/ VWrite \/ VClose
    \/ \E k \in Disks : RStart(k)
    \/ ROther \/ REnd \/ Replicate \/ RLook \/ RWrite

Spec == Init /\ [][Next]_vars

-----------------------------------------------------------------------------
TypeOK == fst \in {"conflict", "pending", "ok", "corrupt"} /\ rs.ph \in {"idle", "other"} /\ rp \in {"idle", "write"}

(* A version is held at a path on a disk, or in a trash. *)
AtAPath(v) == \E o \in DOMAIN objs : objs[o].ver = v /\ objs[o].at = "path"
InTrash(v) == \E o \in DOMAIN objs : objs[o].ver = v /\ objs[o].at = "trash"

(* §15.7, every version kept: no version a user wrote is lost, but what the user's own write, save or deletion
   destroyed, until a resolve the user asked for, and then only one that resolve said it sends away, to the trash. *)
NoVersionLost == \A v \in ghost.written \ ghost.udest : AtAPath(v) \/ (InTrash(v) /\ v \in ghost.said)

(* The resolve removes only what it says, and only through the trash: what is in a trash is a version it said. *)
OnlyWhatItSays == \A v \in ghost.written : InTrash(v) => v \in ghost.said

(* The view never presents a version a resolve sent away: the copy it presents is none in a trash, and no program
   opens through it a version the resolve sent to the trash. *)
ViewNeverSentAway ==
    /\ Presented # 0 => objs[row[Presented]].at # "trash"
    /\ "presents-sent" \notin ghost.flags
=============================================================================
