------------------------------ MODULE ViewMove ------------------------------
(* Atoll's own view: the move of a new file whose write ran out of space (specs.md §21.2, "A write that runs out of
   space", view.move_on_enospc; internal/view/writer, move.go and reprise.go), a model of its own beside
   Replication.tla's view: a file the view made on a full disk, still open through the handle that made it, whose write
   that disk answers with ENOSPC, copied to another disk by the copy pipeline's steps, then the copy left on the full
   disk sent to its trash and purged from there at once, or removed at its path when that trash has no room for it,
   each journaled; with atoll mount stopped short (kill -9), the power cut, a disk lost and mounted again, and a user
   writing directly to either disk, between any two steps.

   What it keeps: the two disks, the full one (1) and the target (2); on each the file's logical path, the move's part
   in <workdir>/tmp/ and its entry in <workdir>/trash/, as the kernel shows them and as a power cut leaves them; the
   objects (an inode with its creation time) with their content (a version), what size and modification time say of it
   (sm) and what moves the change time alone (cg); the program's handle, the view's row of the move (kind = move, its
   look at the full disk's copy when it began, and the operation of ops it runs now) and the journal ops (create of the
   part, trash of the copy left, unlink of its purge or of its removal at its path). The view: the move's steps, the
   program's writes held back meanwhile, then the write made on the target; and its reprise at its next start, and its
   check every 30 s, of a move left planned.

   What it leaves out: the daemon's passes (they wait at the path of a move planned, and record the file once it is
   taken in: the catalog holds no file at its path all through, as the move asks); the choice of the target and its
   room (the target always has room); parts written in parallel (dropped first, Replication.tla's); a hard link (the
   move refuses one); folders; the look right before a removal and the removal, which are one step, as the daemon's
   moves are in Replication.tla. *)
EXTENDS Naturals, FiniteSets, Sequences, TLC

CONSTANTS
    MaxUser,    \* how many times a user acts directly on a disk
    UserActs,   \* some of "write" (a new content, its size or date moved), "rewrite" (keeping both), "delete"
    MaxWrite,   \* how many times the program writes through the view
    MaxStop,    \* how many times atoll mount is stopped short
    MaxCut,     \* how many power cuts
    MaxDrop,    \* how many times a disk is lost
    NoRoom,     \* TRUE: the full disk's trash may have no room for the copy left
    MaxOps,     \* how many operations the view may journal
    Fixes       \* the remedies proposed for the flaws found here and still open, by name (README.md)

Full == 1
Target == 2
Disks == {Full, Target}
Names == {"m", "p", "x"}      \* the logical path, the part in tmp/, the entry in trash/
OpIds == 1..MaxOps

ASSUME /\ UserActs \subseteq {"write", "rewrite", "delete"} /\ NoRoom \in BOOLEAN /\ Fixes \subseteq {"move-held", "move-look"}

NoOp == [k |-> "none", d |-> 0, key |-> 0, st |-> "none"]
NoMove == [st |-> "none", op |-> 0, obj |-> 0, sm |-> 0, cg |-> 0, part |-> 0]
Idle == [ph |-> "idle"]
Down == [ph |-> "down"]

VARIABLES
    objs,    \* [d, ver, dver (after a power cut), sm, dsm, cg, dcg]
    vol,     \* [Disks -> [Names -> object or 0]], as the kernel shows them
    dur,     \* the same, as a power cut leaves them
    dstat,   \* [Disks -> {"up", "gone"}]
    mgen,    \* the mount id of each disk
    vsess,   \* the mount each disk had when the view opened its handle there, plus 1; 0 for none
    vf,      \* the program's handle: [open, d, o]
    mv,      \* the view's row of the move: [st, op, and its look at the full disk's copy when it began: obj, sm, cg]
    ops,     \* the journal
    nextOp,
    vpc,     \* where the view is: down, idle, or a step of the move
    cnt,     \* how many of each event so far
    nver,    \* the last version written
    ghost    \* for the invariants

vars == <<objs, vol, dur, dstat, mgen, vsess, vf, mv, ops, nextOp, vpc, cnt, nver, ghost>>
disk == <<objs, vol, dur, dstat, mgen>>
cat == <<mv, ops, nextOp>>

-----------------------------------------------------------------------------
(* Helpers *)

Up(d) == dstat[d] = "up"
Ok(d) == Up(d) /\ vsess[d] = mgen[d] + 1
Flag(g, name, cond) == IF cond THEN [g EXCEPT !.flags = @ \cup {name}] ELSE g
Sync(os, S) == [o \in DOMAIN os |-> IF o \in S THEN [os[o] EXCEPT !.dver = os[o].ver, !.dsm = os[o].sm, !.dcg = os[o].cg] ELSE os[o]]
Durable(d, n) == dur' = [dur EXCEPT ![d][n] = vol[d][n]]
(* A name holds the object, as the kernel shows it: a file a user deleted under the program's handle has none, and what
   the program writes there then is the user's to lose. *)
Named(o) == \E d \in Disks, n \in Names : vol[d][n] = o
Holds(os, ds, v) == \E d \in Disks, n \in {"m", "x"} : ds[d][n] # 0 /\ os[ds[d][n]].dver = v
(* The full disk's copy as the move's look found it: its inode, size, times and change time (writer.sameAs). *)
AsLooked(o) == o # 0 /\ o = mv.obj /\ objs[o].sm = mv.sm /\ objs[o].cg = mv.cg
(* What the view presents at the path (conformance/vectors/view_resolve.json): what the full disk holds while the
   operation the move names is its create, or with no move, or one undone; what the target holds from its trash on. *)
Presented == IF mv.st = "done" \/ (mv.st = "planned" /\ ops[mv.op].k # "create") THEN vol[Target]["m"] ELSE vol[Full]["m"]

-----------------------------------------------------------------------------
(* The start: a new file the view made on the full disk, written once, still open through the handle that made it. *)

Init ==
    /\ objs = <<[d |-> Full, ver |-> 1, dver |-> 1, sm |-> 1, dsm |-> 1, cg |-> 0, dcg |-> 0]>>
    /\ vol = [d \in Disks |-> [n \in Names |-> IF d = Full /\ n = "m" THEN 1 ELSE 0]]
    /\ dur = vol
    /\ dstat = [d \in Disks |-> "up"]
    /\ mgen = [d \in Disks |-> 0]
    /\ vsess = [d \in Disks |-> 1]
    /\ vf = [open |-> TRUE, d |-> Full, o |-> 1]
    /\ mv = NoMove
    /\ ops = [p \in OpIds |-> NoOp]
    /\ nextOp = 1
    /\ vpc = Idle
    /\ cnt = [user |-> 0, write |-> 0, stop |-> 0, cut |-> 0, drop |-> 0]
    /\ nver = 1
    /\ ghost = [written |-> {1}, udest |-> {}, flags |-> {}, vlatest |-> 1, direct |-> FALSE]

-----------------------------------------------------------------------------
(* The world: a user directly on a disk, durably, with what came before on that disk, as in the other models. A write in
   place destroys the version it overwrites unless a durable name elsewhere holds it. *)

Gone(os, ds, v) == ~Holds(os, ds, v)

UserOn(d, os1, vol1, old, newv) ==
    LET os2 == Sync(os1, {o \in DOMAIN os1 : os1[o].d = d})
        dur2 == [dur EXCEPT ![d] = vol1[d]]
    IN /\ objs' = os2 /\ vol' = vol1 /\ dur' = dur2
       /\ nver' = IF newv = 0 THEN nver ELSE newv
       /\ cnt' = [cnt EXCEPT !.user = @ + 1]
       /\ ghost' = [ghost EXCEPT !.written = IF newv = 0 THEN @ ELSE @ \cup {newv},
                                 !.udest = IF old # 0 /\ Gone(os2, dur2, old) THEN @ \cup {old} ELSE @,
                                 !.direct = TRUE]
       /\ UNCHANGED <<dstat, mgen, vsess, vf, cat, vpc>>

UserWrite(d) ==
    /\ "write" \in UserActs /\ cnt.user < MaxUser /\ Up(d) /\ vol[d]["m"] # 0
    /\ LET o == vol[d]["m"] IN
       UserOn(d, [objs EXCEPT ![o].ver = nver + 1, ![o].sm = nver + 1], vol, objs[o].ver, nver + 1)

(* A write that keeps the size and the dates, or sets them back: only the change time moves. *)
UserRewrite(d) ==
    /\ "rewrite" \in UserActs /\ cnt.user < MaxUser /\ Up(d) /\ vol[d]["m"] # 0
    /\ LET o == vol[d]["m"] IN
       UserOn(d, [objs EXCEPT ![o].ver = nver + 1, ![o].cg = @ + 1], vol, objs[o].ver, nver + 1)

UserDelete(d) ==
    /\ "delete" \in UserActs /\ cnt.user < MaxUser /\ Up(d) /\ vol[d]["m"] # 0
    /\ UserOn(d, objs, [vol EXCEPT ![d]["m"] = 0], objs[vol[d]["m"]].ver, 0)

Drop(d) ==
    /\ cnt.drop < MaxDrop /\ Up(d)
    /\ dstat' = [dstat EXCEPT ![d] = "gone"]
    /\ cnt' = [cnt EXCEPT !.drop = @ + 1]
    /\ UNCHANGED <<objs, vol, dur, mgen, vsess, vf, cat, vpc, nver, ghost>>

Return(d) ==
    /\ dstat[d] = "gone"
    /\ dstat' = [dstat EXCEPT ![d] = "up"]
    /\ mgen' = [mgen EXCEPT ![d] = @ + 1]
    /\ vol' = [vol EXCEPT ![d] = dur[d]]
    /\ objs' = [o \in DOMAIN objs |-> IF objs[o].d = d THEN [objs[o] EXCEPT !.ver = objs[o].dver, !.sm = objs[o].dsm, !.cg = objs[o].dcg] ELSE objs[o]]
    /\ UNCHANGED <<dur, vsess, vf, cat, vpc, cnt, nver, ghost>>

(* atoll mount stopped short: the program's handle goes with it, and what the move was doing stops where it is. *)
VStop ==
    /\ cnt.stop < MaxStop /\ vpc # Down
    /\ vpc' = Down /\ vf' = [vf EXCEPT !.open = FALSE] /\ vsess' = [d \in Disks |-> 0]
    /\ cnt' = [cnt EXCEPT !.stop = @ + 1]
    /\ UNCHANGED <<disk, cat, nver, ghost>>

PowerCut ==
    /\ cnt.cut < MaxCut
    /\ vol' = dur
    /\ objs' = [o \in DOMAIN objs |-> [objs[o] EXCEPT !.ver = objs[o].dver, !.sm = objs[o].dsm, !.cg = objs[o].dcg]]
    /\ vpc' = Down /\ vf' = [vf EXCEPT !.open = FALSE] /\ vsess' = [d \in Disks |-> 0]
    /\ cnt' = [cnt EXCEPT !.cut = @ + 1]
    /\ UNCHANGED <<dur, dstat, mgen, cat, nver, ghost>>

World ==
    \/ \E d \in Disks : UserWrite(d) \/ UserRewrite(d) \/ UserDelete(d) \/ Drop(d) \/ Return(d)
    \/ VStop \/ PowerCut

-----------------------------------------------------------------------------
(* The program, through the view: a write to the file, which reaches its object, durably as a user's does here; or, on
   the full disk, one that the disk answers with ENOSPC, which starts the move (move.go, Writer.move) when the file may
   move: the view's own new file, open through that one handle, never moved before, still at its path there and as it
   is (mayMove); otherwise ENOSPC, nothing journaled. *)

(* The handle writes its object wherever its names went, as an open file does (Replication.tla's VData): one a move
   sent to the trash under it takes the write there. *)
PWrite ==
    /\ vpc = Idle /\ vf.open /\ cnt.write < MaxWrite /\ Ok(vf.d)
    /\ LET o == vf.o
           os1 == [objs EXCEPT ![o].ver = nver + 1, ![o].dver = nver + 1, ![o].sm = nver + 1, ![o].dsm = nver + 1]
       IN /\ objs' = os1
          /\ ghost' = [ghost EXCEPT !.written = @ \cup {nver + 1}, !.vlatest = nver + 1,
                                    !.direct = IF Named(o) THEN FALSE ELSE @,
                                    !.udest = (IF Gone(os1, dur, objs[o].ver) THEN @ \cup {objs[o].ver} ELSE @)
                                              \cup (IF Named(o) THEN {} ELSE {nver + 1})]
    /\ nver' = nver + 1
    /\ cnt' = [cnt EXCEPT !.write = @ + 1]
    /\ UNCHANGED <<vol, dur, dstat, mgen, vsess, vf, cat, vpc>>

(* Step 1: the move and its create journaled, under the writer's lock, with the look at the full disk's copy. *)
PWriteFull ==
    /\ vpc = Idle /\ vf.open /\ cnt.write < MaxWrite /\ vf.d = Full /\ Ok(Full)
    /\ mv.st = "none" /\ vol[Full]["m"] = vf.o /\ nextOp <= MaxOps
    /\ mv' = [st |-> "planned", op |-> nextOp, obj |-> vf.o, sm |-> objs[vf.o].sm, cg |-> objs[vf.o].cg, part |-> 0]
    /\ ops' = [ops EXCEPT ![nextOp] = [k |-> "create", d |-> Target, key |-> 0, st |-> "planned"]]
    /\ nextOp' = nextOp + 1
    /\ vpc' = [ph |-> "move", s |-> "journaled"]
    /\ cnt' = [cnt EXCEPT !.write = @ + 1]
    /\ UNCHANGED <<disk, vsess, vf, nver, ghost>>

PClose ==
    /\ vpc = Idle /\ vf.open
    /\ vf' = [vf EXCEPT !.open = FALSE]
    /\ UNCHANGED <<disk, vsess, cat, vpc, cnt, nver, ghost>>

-----------------------------------------------------------------------------
(* The move, step by step (§21.2, The steps). Each step on a disk that cannot be relied on stops the move where it is,
   planned, for the view's reprise, and the file never moves again; before the trash is journaled the program gets
   ENOSPC and the file stays open on the full disk, after it the write is made on the target. *)

S(s) == vpc.ph = "move" /\ vpc.s = s
To(s) == vpc' = [ph |-> "move", s |-> s]
COp == mv.op                       \* the move's create: its part on the target
Part == ops[COp].key

(* dropMove: the part removed, its create and the move undone; the program gets ENOSPC, the file open where it was. *)
DropMove ==
    /\ vol' = [vol EXCEPT ![Target]["p"] = 0]
    /\ dur' = [dur EXCEPT ![Target]["p"] = 0]
    /\ ops' = [ops EXCEPT ![COp].st = "undone"]
    /\ mv' = [mv EXCEPT !.st = "undone"]
    /\ vpc' = Idle

(* The part made on the target, mode 0600, synced, its inode recorded (MakePart, SetCreateKey). *)
MMake ==
    /\ S("journaled") /\ Ok(Target)
    /\ objs' = Append(objs, [d |-> Target, ver |-> 0, dver |-> 0, sm |-> 0, dsm |-> 0, cg |-> 0, dcg |-> 0])
    /\ vol' = [vol EXCEPT ![Target]["p"] = Len(objs) + 1]
    /\ dur' = [dur EXCEPT ![Target]["p"] = Len(objs) + 1]
    /\ ops' = [ops EXCEPT ![COp].key = Len(objs) + 1]
    /\ mv' = [mv EXCEPT !.part = Len(objs) + 1]
    /\ To("made")
    /\ UNCHANGED <<dstat, mgen, vsess, vf, nextOp, cnt, nver, ghost>>

(* Steps 2 to 4: the full disk's copy read into the part, each chunk hashed, then looked at again: moved, the move
   stops; the part given the copy's times and synced, read again without its cache. One step. *)
MCopy ==
    /\ S("made") /\ Ok(Full) /\ Ok(Target)
    /\ IF ~AsLooked(vol[Full]["m"])
       THEN /\ DropMove /\ UNCHANGED objs
       ELSE /\ objs' = [objs EXCEPT ![Part].ver = objs[mv.obj].ver, ![Part].dver = objs[mv.obj].ver,
                                    ![Part].sm = objs[mv.obj].sm, ![Part].dsm = objs[mv.obj].sm]
            /\ To("copied")
            /\ UNCHANGED <<vol, dur, ops, mv>>
    /\ UNCHANGED <<dstat, mgen, vsess, vf, nextOp, cnt, nver, ghost>>

(* Step 5, under the writer's lock: the checks of the start made again, the full disk's copy found as it was read; the
   part renamed without replacing to the path on the target. *)
MPlace ==
    /\ S("copied") /\ Ok(Full) /\ Ok(Target)
    /\ IF ~AsLooked(vol[Full]["m"]) \/ vol[Target]["m"] # 0
       THEN DropMove
       ELSE /\ vol' = [vol EXCEPT ![Target]["m"] = Part, ![Target]["p"] = 0]
            /\ To("renamed")
            /\ UNCHANGED <<dur, ops, mv>>
    /\ UNCHANGED <<objs, dstat, mgen, vsess, vf, nextOp, cnt, nver, ghost>>

(* The file synced at its name on the target, then its folder, and tmp/. *)
MPlaceSync ==
    /\ S("renamed") /\ Ok(Target)
    /\ dur' = [dur EXCEPT ![Target]["m"] = vol[Target]["m"], ![Target]["p"] = vol[Target]["p"]]
    /\ objs' = Sync(objs, {vol[Target]["m"]} \ {0})
    /\ To("placed")
    /\ UNCHANGED <<vol, dstat, mgen, vsess, vf, cat, cnt, nver, ghost>>

(* Step 6: the trash of the copy left journaled, surplus, the create done, in one transaction (ViewMoveTrash). *)
MTrashJournal ==
    /\ S("placed") /\ nextOp <= MaxOps
    /\ ops' = [ops EXCEPT ![COp].st = "done", ![nextOp] = [k |-> "trash", d |-> Full, key |-> mv.obj, st |-> "planned"]]
    /\ mv' = [mv EXCEPT !.op = nextOp]
    /\ nextOp' = nextOp + 1
    /\ To("trashj")
    /\ UNCHANGED <<disk, vsess, vf, cnt, nver, ghost>>

(* Step 7: the part's handle becomes the file's. *)
MSwap ==
    /\ S("trashj")
    /\ vf' = [vf EXCEPT !.d = Target, !.o = mv.part]
    /\ To("swapped")
    /\ UNCHANGED <<disk, vsess, cat, cnt, nver, ghost>>

(* Step 8: the copy left sent to its trash as §17.1 sends one, compared with what was copied right before the move (one
   step with it): changed, it stays at its path beside the target's, the trash undone and the move done; or the trash
   with no room for it, nothing moved, its removal at its path journaled instead, the trash undone (ViewMoveRemoveHere). *)
MTrash ==
    /\ S("swapped") /\ Ok(Full)
    /\ LET o == vol[Full]["m"] IN
       IF ~AsLooked(o)
       THEN /\ ops' = [ops EXCEPT ![mv.op].st = "undone"]
            /\ mv' = [mv EXCEPT !.st = "done"]
            /\ To("write")
            /\ UNCHANGED <<objs, vol, dur, nextOp, ghost>>
       ELSE /\ vol' = [vol EXCEPT ![Full]["m"] = 0, ![Full]["x"] = o]
            /\ To("tmoved")
            /\ UNCHANGED <<objs, dur, ops, mv, nextOp, ghost>>
    /\ UNCHANGED <<dstat, mgen, vsess, vf, cnt, nver>>

MNoRoom ==
    /\ S("swapped") /\ Ok(Full) /\ NoRoom /\ nextOp <= MaxOps /\ AsLooked(vol[Full]["m"])
    /\ ops' = [ops EXCEPT ![mv.op].st = "undone", ![nextOp] = [k |-> "remove", d |-> Full, key |-> mv.obj, st |-> "planned"]]
    /\ mv' = [mv EXCEPT !.op = nextOp]
    /\ nextOp' = nextOp + 1
    /\ To("rmj")
    /\ UNCHANGED <<disk, vsess, vf, cnt, nver, ghost>>

(* §17.1 step 5: the entry synced, then the source's folder; step 6: the trash done. Then the purge journaled. *)
MTrashSync ==
    /\ S("tmoved") /\ Ok(Full)
    /\ dur' = [dur EXCEPT ![Full]["x"] = vol[Full]["x"], ![Full]["m"] = vol[Full]["m"]]
    /\ objs' = Sync(objs, {vol[Full]["x"]} \ {0})
    /\ To("tsynced")
    /\ UNCHANGED <<vol, dstat, mgen, vsess, vf, cat, cnt, nver, ghost>>

MPurgeJournal ==
    /\ S("tsynced") /\ nextOp <= MaxOps
    /\ ops' = [ops EXCEPT ![mv.op].st = "done", ![nextOp] = [k |-> "unlink", d |-> Full, key |-> mv.obj, st |-> "planned"]]
    /\ mv' = [mv EXCEPT !.op = nextOp]
    /\ nextOp' = nextOp + 1
    /\ To("pj")
    /\ UNCHANGED <<disk, vsess, vf, cnt, nver, ghost>>

(* ops.PurgeEntry: the file in the trash looked at by its inode and unlinked, its folder synced; the purge done and the
   move done. A file there that is not that one stays, the purge undone. *)
(* With move-look, the remedy proposed for flaw 17 (README.md), as flaw 8's xfer-look for the cache's transfer: right
   before the purge, or the removal at its path, the target's path looked at, on a disk the view can read: one that no
   longer holds the copy the move placed as it placed it, its inode, size, times and change time, deleted, replaced or
   written into directly since, keeps the copy left, in the trash unpurged, or at its path. *)
TargetHolds == /\ Ok(Target) /\ mv.part # 0 /\ vol[Target]["m"] = mv.part
               /\ objs[mv.part].sm = mv.sm /\ objs[mv.part].cg = 0
Looks == "move-look" \in Fixes

MPurge ==
    /\ S("pj") /\ Ok(Full)
    /\ IF vol[Full]["x"] # ops[mv.op].key \/ (Looks /\ ~TargetHolds)
       THEN /\ ops' = [ops EXCEPT ![mv.op].st = "undone"]
            /\ mv' = [mv EXCEPT !.st = "done"]
            /\ To("write")
            /\ UNCHANGED <<vol, dur, ghost>>
       ELSE /\ vol' = [vol EXCEPT ![Full]["x"] = 0]
            /\ dur' = [dur EXCEPT ![Full]["x"] = 0]
            /\ ghost' = Flag(ghost, "moved-first", ~(mv.part # 0 /\ dur[Target]["m"] = mv.part))
            /\ To("purged")
            /\ UNCHANGED <<ops, mv>>
    /\ UNCHANGED <<objs, dstat, mgen, vsess, vf, nextOp, cnt, nver>>

MPurgeCommit ==
    /\ S("purged")
    /\ ops' = [ops EXCEPT ![mv.op].st = "done"]
    /\ mv' = [mv EXCEPT !.st = "done"]
    /\ To("write")
    /\ UNCHANGED <<disk, vsess, vf, nextOp, cnt, nver, ghost>>

(* ops.Disk.RemoveLeft: after the checks the trash makes, the copy at its path compared with what was copied, by its
   inode, size, times and change time, right before (one step with the removal); the name removed, the folder synced;
   the unlink done and the move done. Changed since, or gone: what is there stays, the unlink undone, the move done. *)
MRemoveHere ==
    /\ S("rmj") /\ Ok(Full)
    /\ LET o == vol[Full]["m"] IN
       IF ~AsLooked(o) \/ (Looks /\ ~TargetHolds)
       THEN /\ ops' = [ops EXCEPT ![mv.op].st = "undone"]
            /\ mv' = [mv EXCEPT !.st = "done"]
            /\ To("write")
            /\ UNCHANGED <<vol, dur, ghost>>
       ELSE /\ vol' = [vol EXCEPT ![Full]["m"] = 0]
            /\ dur' = [dur EXCEPT ![Full]["m"] = 0]
            /\ ghost' = Flag(ghost, "moved-first", ~(mv.part # 0 /\ dur[Target]["m"] = mv.part))
            /\ To("removed")
            /\ UNCHANGED <<ops, mv>>
    /\ UNCHANGED <<objs, dstat, mgen, vsess, vf, nextOp, cnt, nver>>

MRemoveCommit ==
    /\ S("removed")
    /\ ops' = [ops EXCEPT ![mv.op].st = "done"]
    /\ mv' = [mv EXCEPT !.st = "done"]
    /\ To("write")
    /\ UNCHANGED <<disk, vsess, vf, nextOp, cnt, nver, ghost>>

(* Step 10: the write that ran out of space made on the target, through the handle that is the part's now. *)
MWrite ==
    /\ S("write")
    /\ IF Ok(vf.d) /\ vf.open
       THEN LET o == vf.o
                os1 == [objs EXCEPT ![o].ver = nver + 1, ![o].dver = nver + 1, ![o].sm = nver + 1, ![o].dsm = nver + 1]
            IN /\ objs' = os1
               /\ nver' = nver + 1
               /\ ghost' = [ghost EXCEPT !.written = @ \cup {nver + 1}, !.vlatest = nver + 1,
                                         !.direct = IF Named(o) THEN FALSE ELSE @,
                                         !.udest = (IF Gone(os1, dur, objs[o].ver) THEN @ \cup {objs[o].ver} ELSE @)
                                                   \cup (IF Named(o) THEN {} ELSE {nver + 1})]
       ELSE UNCHANGED <<objs, nver, ghost>>
    /\ vpc' = Idle
    /\ UNCHANGED <<vol, dur, dstat, mgen, vsess, vf, cat, cnt>>

(* A disk the move cannot rely on: the move stays planned where it is, for the view's reprise or its check; before
   the trash is journaled, the program gets ENOSPC, the file open on the full disk. *)
MLost ==
    /\ vpc.ph = "move" /\ vpc.s \notin {"write", "purged", "removed", "trashj"}
    /\ ~Ok(Full) \/ ~Ok(Target)
    /\ vpc' = IF vpc.s \in {"journaled", "made", "copied", "renamed", "placed"} THEN Idle ELSE [ph |-> "move", s |-> "write"]
    /\ UNCHANGED <<disk, vsess, vf, cat, cnt, nver, ghost>>

-----------------------------------------------------------------------------
(* §21.2, A stop of atoll mount, or a crash: the view's next start opens its handles, then resolves a move left planned
   by the operation it names (reprise.go, repriseMove); its check every 30 s does the same for a move a disk lost left
   planned, once that disk is back (Recheck), while no step of it runs. One step: the names, then the journal. *)

(* Its create planned: the target's path holding the part, by inode, says that the rename went through; the part's name
   in tmp/ goes either way; then the copy on the full disk: none of it, the move done; another size or time than the
   target's, both stay, the create done, the move undone; the same, it goes to the trash, unpurged, the move done, or
   stays planned when the trash has no room. Its trash planned: the copy still at its path, by inode, goes to the trash
   all the same, whatever it holds, unpurged, then the table of §17.2. Its unlink planned, a purge: nothing in the trash
   by inode, done; anything else, undone. Its removal at its path planned: nothing there, done; anything else, undone,
   beside the moved copy. *)
Resolve(room) ==
    LET op == ops[mv.op]
        here == vol[Full]["m"]
        placed == op.k = "create" /\ Part # 0 /\ vol[Target]["m"] = Part
    IN CASE op.k = "create" /\ ~placed ->
              /\ vol' = [vol EXCEPT ![Target]["p"] = 0]
              /\ dur' = [dur EXCEPT ![Target]["p"] = 0]
              /\ ops' = [ops EXCEPT ![mv.op].st = "undone"]
              /\ mv' = [mv EXCEPT !.st = "undone"]
              /\ UNCHANGED <<objs, nextOp, ghost>>
         [] op.k = "create" /\ (here = 0 \/ here # mv.obj) ->
              /\ vol' = [vol EXCEPT ![Target]["p"] = 0]
              /\ dur' = [dur EXCEPT ![Target]["p"] = 0, ![Target]["m"] = vol[Target]["m"]]
              /\ objs' = Sync(objs, {Part})
              /\ ops' = [ops EXCEPT ![mv.op].st = "done"]
              /\ mv' = [mv EXCEPT !.st = "done"]
              /\ UNCHANGED <<nextOp, ghost>>
         [] op.k = "create" /\ objs[here].sm # objs[Part].sm ->
              /\ vol' = [vol EXCEPT ![Target]["p"] = 0]
              /\ dur' = [dur EXCEPT ![Target]["p"] = 0, ![Target]["m"] = vol[Target]["m"]]
              /\ objs' = Sync(objs, {Part})
              /\ ops' = [ops EXCEPT ![mv.op].st = "done"]
              /\ mv' = [mv EXCEPT !.st = "undone"]
              /\ UNCHANGED <<nextOp, ghost>>
         [] op.k = "create" ->
              IF ~room \/ nextOp > MaxOps
              THEN \* no room in that trash: the copy stays at its path, the move planned, for the next start
                   /\ vol' = [vol EXCEPT ![Target]["p"] = 0]
                   /\ dur' = [dur EXCEPT ![Target]["p"] = 0, ![Target]["m"] = vol[Target]["m"]]
                   /\ objs' = Sync(objs, {Part})
                   /\ UNCHANGED <<ops, mv, nextOp, ghost>>
              ELSE /\ vol' = [vol EXCEPT ![Target]["p"] = 0, ![Full]["m"] = 0, ![Full]["x"] = here]
                   /\ dur' = [dur EXCEPT ![Target]["p"] = 0, ![Target]["m"] = vol[Target]["m"], ![Full]["m"] = 0, ![Full]["x"] = here]
                   /\ objs' = Sync(objs, {Part, here})
                   /\ ops' = [ops EXCEPT ![mv.op].st = "done", ![nextOp] = [k |-> "trash", d |-> Full, key |-> here, st |-> "done"]]
                   /\ nextOp' = nextOp + 1
                   /\ mv' = [mv EXCEPT !.st = "done", !.op = nextOp]
                   /\ UNCHANGED ghost
         [] op.k = "trash" ->
              IF vol[Full]["x"] = op.key
              THEN \* in the trash already: the source's name synced, done
                   /\ dur' = [dur EXCEPT ![Full]["x"] = op.key, ![Full]["m"] = vol[Full]["m"]]
                   /\ objs' = Sync(objs, {op.key})
                   /\ ops' = [ops EXCEPT ![mv.op].st = "done"]
                   /\ mv' = [mv EXCEPT !.st = "done"]
                   /\ UNCHANGED <<vol, nextOp, ghost>>
              ELSE IF here = op.key /\ room
              THEN \* still at its path: to the trash all the same, unpurged
                   /\ vol' = [vol EXCEPT ![Full]["m"] = 0, ![Full]["x"] = here]
                   /\ dur' = [dur EXCEPT ![Full]["m"] = 0, ![Full]["x"] = here]
                   /\ objs' = Sync(objs, {here})
                   /\ ops' = [ops EXCEPT ![mv.op].st = "done"]
                   /\ mv' = [mv EXCEPT !.st = "done"]
                   /\ UNCHANGED <<nextOp, ghost>>
              ELSE IF here = op.key
              THEN UNCHANGED <<objs, vol, dur, ops, mv, nextOp, ghost>>      \* no room: planned, for the next start
              ELSE \* absent, or another file at its path: undone, what is there stays
                   /\ ops' = [ops EXCEPT ![mv.op].st = "undone"]
                   /\ mv' = [mv EXCEPT !.st = "done"]
                   /\ UNCHANGED <<objs, vol, dur, nextOp, ghost>>
         [] op.k = "unlink" ->
              /\ dur' = [dur EXCEPT ![Full]["x"] = vol[Full]["x"]]
              /\ ops' = [ops EXCEPT ![mv.op].st = IF vol[Full]["x"] = 0 THEN "done" ELSE "undone"]
              /\ mv' = [mv EXCEPT !.st = "done"]
              /\ UNCHANGED <<objs, vol, nextOp, ghost>>
         [] OTHER ->     \* remove, at its path
              /\ dur' = [dur EXCEPT ![Full]["m"] = vol[Full]["m"]]
              /\ ops' = [ops EXCEPT ![mv.op].st = IF here = 0 THEN "done" ELSE "undone"]
              /\ mv' = [mv EXCEPT !.st = "done"]
              /\ UNCHANGED <<objs, vol, nextOp, ghost>>

(* The view's start: its handles opened on the disks there, then the reprise of a move left planned, on disks both
   there; a disk away leaves it planned, for the next start or check. *)
VStart ==
    /\ vpc = Down
    /\ vsess' = [d \in Disks |-> IF Up(d) THEN mgen[d] + 1 ELSE 0]
    /\ vpc' = Idle
    /\ IF mv.st = "planned" /\ Up(Full) /\ Up(Target)
       THEN \E room \in IF NoRoom THEN BOOLEAN ELSE {TRUE} : Resolve(room)
       ELSE UNCHANGED <<objs, vol, dur, cat, ghost>>
    /\ UNCHANGED <<dstat, mgen, vf, cnt, nver>>

(* Its check every 30 s: a disk found back on a mount of its own gets a new handle; a move left planned, no step of it
   running, is then resolved as at a start (writer.Recheck, which holds back only a move during its copy). With
   move-held, the remedy proposed for flaw 15 (README.md): a move whose file the program still holds open through the
   handle that made it is that handle's, and waits for its close or the next start. *)
VCheck ==
    /\ vpc = Idle
    /\ \/ \E d \in Disks : Up(d) /\ ~Ok(d)
       \/ mv.st = "planned" /\ Ok(Full) /\ Ok(Target)
    /\ vsess' =[d \in Disks |-> IF Up(d) THEN mgen[d] + 1 ELSE vsess[d]]
    /\ IF mv.st = "planned" /\ Up(Full) /\ Up(Target) /\ ~("move-held" \in Fixes /\ vf.open)
       THEN \E room \in IF NoRoom THEN BOOLEAN ELSE {TRUE} : Resolve(room)
       ELSE UNCHANGED <<objs, vol, dur, cat, ghost>>
    /\ UNCHANGED <<dstat, mgen, vf, vpc, cnt, nver>>

View ==
    \/ PWrite \/ PWriteFull \/ PClose
    \/ MMake \/ MCopy \/ MPlace \/ MPlaceSync \/ MTrashJournal \/ MSwap \/ MTrash \/ MNoRoom \/ MTrashSync
    \/ MPurgeJournal \/ MPurge \/ MPurgeCommit \/ MRemoveHere \/ MRemoveCommit \/ MWrite \/ MLost
    \/ VStart \/ VCheck

Next == View \/ World

Spec == Init /\ [][Next]_vars

FairSpec == Spec /\ WF_vars(View) /\ \A d \in Disks : WF_vars(Return(d))

-----------------------------------------------------------------------------
(* Invariants. *)

(* No version a user or the program wrote is lost: each is durably at the path or in the trash of some disk, missing
   disks included, unless a write in place or a deletion of a user's took its last copy. *)
NoUserDataLost == \A v \in ghost.written \ ghost.udest : Holds(objs, dur, v)

(* §21.2, invariant 2: the copy on the full disk leaves its path, or the trash, only once the target holds the copy the
   move placed durably at its path, its content the copied one, or what a user wrote into it since. *)
Inv2_MovedFirst == "moved-first" \notin ghost.flags

(* 14 and §21.2, What it presents: while the view runs and both disks are there, the path presents the content the
   program last wrote through the view, as long as no user wrote to a disk directly since; never nothing between the
   two disks. *)
ViewPresentsLatest ==
    (vpc # Down /\ Up(Full) /\ Up(Target) /\ ~ghost.direct)
        => (Presented # 0 /\ objs[Presented].ver = ghost.vlatest)

(* 12: every part and every entry of the trash, visible or durable, is named by an operation of that disk journaled. *)
Inv12_JournalFirst ==
    /\ (vol[Target]["p"] # 0 \/ dur[Target]["p"] # 0) => \E p \in OpIds : ops[p].k = "create" /\ ops[p].d = Target
    /\ (vol[Full]["x"] # 0 \/ dur[Full]["x"] # 0) => \E p \in OpIds : ops[p].k = "trash" /\ ops[p].d = Full

TypeOK ==
    /\ \A p \in OpIds : ops[p].st \in {"none", "planned", "done", "undone"}
    /\ mv.st \in {"none", "planned", "done", "undone"}

(* Liveness: no move stays planned. *)
EventuallyResolved == <>[](mv.st # "planned")

=============================================================================
