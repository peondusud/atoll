-------------------------------- MODULE Scrub --------------------------------
(* Scrub and repair against modifications (specs.md §4, §14, §15.4, §16, §16.1, §17.2; invariants 1, 4, 7 and 9): a
   model of its own. One file of two chunks, a copy at the same path on each disk, in sync at the start and verified
   but on the disks of Unverified. A chunk holds the file's version (0), rot (-1), or a user's edit (its number); a
   sector may not be read at all, and a read through the page cache may come back wrong once (-2).

   What it keeps, for each disk: its device up or lost; whether the copy is at its path, its chunks, its modification
   time and its change time as the filesystem holds them, the change time in whole ticks on a disk of Coarse (an ext4
   with 128-byte inodes holds whole seconds, specs.md §10), where a change in the tick of the last one moves nothing;
   its size, which an edit in place keeps, is fixed; and what its trash holds of the file. The catalog: the file's
   chunk hashes (its version) and state (ok, corrupt, conflict), each copy's row (ok, corrupt, or none once it went to
   the trash), its times and whether it was ever verified, the repair left planned on it, and the scrub's cursor. And
   Atoll: one process, as a scrub, a repair and a pass each hold the pool's lock (§15.13):
   - a scrub of a copy due (§16, jobs.scrubCopy): opened after a look that its name holds what its row says; each chunk
     read and its hash compared; a chunk that does not match, or cannot be read on a disk that then confirms, the
     copy's times looked at, then the chunk read again past the page cache; a mismatch then makes the copy corrupt, or
     its file a conflict for a copy never verified, and on a disk of Coarse, whose times cannot tell an edit from rot
     (coarse-conflict); a disk that does not confirm is lost, and nothing is concluded; the
     copy verified once every chunk matched and its times are still those it was opened with; the budget may stop it
     between two chunks, and the next scrub begins at the cursor while the row has not changed;
   - a corrupt copy healed (§16, §16.1): a healthy copy, ok on a disk that can be read, read whole, its chunks the
     file's and its times its row's; by chunk (trash.Session.RepairChunks): the copy read whole, its bad chunks those
     that are not the file's or cannot be read, a conflict on a disk of Coarse; the operation and its trash row
     journaled; the bad chunks saved to the trash; each bad chunk read from the healthy copy and checked, the copy looked
     at against what the repair's own last change left and the chunk read again (repair-recheck), written, read back;
     looked at again, the modification time set back to the row's; looked at again, the commit, the row taking the change
     time the copy has then; a look that finds someone else's write stops the repair, the copy left as it is; a repair
     that stops otherwise resolved as §16.1 says, the run knowing whether it wrote; by file: the
     copy sent to the trash, its copy made again by the next pass; without auto_repair (Mode "off"), quarantined: sent
     to the trash; with no healthy copy, left as it is; a corrupt copy left in the root taken again by the next scrub;
   - the reprise of a repair left planned (§16.1, Reprise);
   - a pass (§15.4), which takes a copy changed against its row in as a modification, its content the file's version,
     the other copies sent to the trash and made again from it, or waits, as a guard holds it; it decides nothing of a
     file corrupt or in conflict, and makes again a copy that went to the trash.
   A user edits a chunk of a copy in place, directly or through the view, keeping its modification time (touch -r) or
   not; the world rots a chunk, makes a sector unreadable, makes a read through the cache come back wrong once, loses a
   disk and brings it back, and the clock ticks; Atoll's process may be killed between any two steps.

   Flaws 31 and 32, fixed (README.md): on a disk whose change time holds whole seconds, an edit in place that keeps
   the size and the modification time, in the second of the copy's last change, moved none of the times a look
   compares, so that the scrub called it corrupt and repaired it away; and a user's write that landed after the
   repair's look right before its first write, while it read the healthy chunk, was written over unsaved, or kept in
   the copy while the commit recorded the change time it left. coarse-conflict and repair-recheck, the maintainer's
   remedies of 2026-11-26, are what every instance does; racy-conflict, the other remedy proposed for flaw 31, is noted
   for format version 2, and gone from the model.

   Flaws 33 and 34, fixed (README.md): a crash may tear a chunk's write, part old bytes and part new, and the reprise
   took a bad chunk that holds neither the file's content nor what was saved of it for that torn write: a user's write
   into it before the reprise was taken so, the time set back over it, and the next scrub repaired it away. And a chunk
   that could not be read at the reprise, but a bad one saved as an empty file, left the operation planned for as long as
   it could not be read, the copy corrupt in the root for good. torn-foreign and unread-found, the maintainer's remedies of
   2026-11-26, are what every instance does; torn-index, checked and holding not, is gone from the model.

   What it leaves out: a corrupt copy that changed, settled into a conflict (Conflicts.tla's: here it waits), the
   pipeline of a copy (Replication.tla's: one step here), the trash's purge (Trash.tla's), renames and inodes (an
   atomic save is a new inode, which every look sees), the scrub's time windows, scrub_every and the order of disks,
   files of another size: see README.md. *)
EXTENDS Integers, FiniteSets, TLC

CONSTANTS
    NDisks,      \* the disks, each holding a copy of the file at the start
    Coarse,      \* the disks whose change time holds whole ticks
    Unverified,  \* the disks whose copy was never verified at the start
    Mode,        \* "chunk", "file" (integrity.repair_mode), or "off" (auto_repair false: quarantine)
    UserActs,    \* some of "edit" (the modification time moves, as through the view), "keep" (touch -r: it does not)
    UserDisks,   \* the disks a user writes to
    MaxUser,
    WorldActs,   \* some of "rot", "sector", "flip", "lose"
    WorldDisks,  \* the disks the world acts on
    MaxWorld,
    AtollActs,   \* some of "scrub", "pass", "budget", "kill"
    MaxKill,
    Fixes        \* the remedies proposed for the flaws found here and still open, by name (README.md); {} is specs.md as it is

Disks == 1..NDisks
Chunks == {1, 2}
\* flaws 31 to 34 are fixed, with coarse-conflict, repair-recheck, torn-foreign and unread-found, what every instance does
\* (README.md): no remedy
FixNames == {}

ASSUME /\ NDisks \in 2..3 /\ Coarse \subseteq Disks /\ Unverified \subseteq Disks /\ Mode \in {"chunk", "file", "off"}
       /\ UserActs \subseteq {"edit", "keep"} /\ UserDisks \subseteq Disks /\ MaxUser \in Nat
       /\ WorldActs \subseteq {"rot", "sector", "flip", "lose"} /\ WorldDisks \subseteq Disks /\ MaxWorld \in Nat
       /\ AtollActs \subseteq {"scrub", "pass", "budget", "kill"} /\ MaxKill \in Nat
       /\ Fixes \subseteq FixNames

VARIABLES
    w,      \* the world: [up, at |-> the copy at its path, ch |-> its chunks, bad |-> the sectors that cannot be read,
            \*   flip |-> a read through the cache wrong once, mt, ct |-> its times as held, fresh |-> whether the clock
            \*   ticked since its last change, tr |-> what each trash holds: <<chunk, value>>, and <<0, value>> of each
            \*   chunk of a whole copy, users, acts, kills]
    cat,    \* the catalog: [want |-> the file's chunks, fst, cs |-> each copy's state, rmt, rct |-> each row's times,
            \*   ver |-> verified once, op |-> the repair left planned on each disk, cursor]
    pc,     \* Atoll's process: [ph, d |-> the copy, e |-> the healthy copy, i |-> the chunk, st |-> the times it
            \*   opened with, or its own last change left, bad |-> its bad chunks, seen, sec |-> each chunk, and the
            \*   sectors that could not be read, as the repair found them bad]
    ghost   \* [flags]

vars == <<w, cat, pc, ghost>>

Idle == [ph |-> "idle", d |-> 0, e |-> 0, i |-> 0, st |-> <<0, 0>>, bad |-> {}, seen |-> <<0, 0>>, sec |-> {}]
NoOp == [on |-> FALSE, bad |-> {}, saved |-> {}, wrote |-> FALSE, sv |-> FALSE]
NoCursor == [d |-> 0, i |-> 0, t |-> <<0, 0>>]

Init ==
    /\ w = [up |-> [d \in Disks |-> TRUE], at |-> [d \in Disks |-> TRUE], ch |-> [d \in Disks |-> [i \in Chunks |-> 0]],
            bad |-> [d \in Disks |-> {}], flip |-> [d \in Disks |-> {}],
            mt |-> [d \in Disks |-> 1], ct |-> [d \in Disks |-> 1], fresh |-> [d \in Disks |-> FALSE],
            tr |-> [d \in Disks |-> {}], users |-> 0, acts |-> 0, kills |-> 0]
    /\ cat = [want |-> [i \in Chunks |-> 0], fst |-> "ok", cs |-> [d \in Disks |-> "ok"],
              rmt |-> [d \in Disks |-> 1], rct |-> [d \in Disks |-> 1], ver |-> [d \in Disks |-> d \notin Unverified],
              op |-> [d \in Disks |-> NoOp], cursor |-> NoCursor]
    /\ pc = Idle
    /\ ghost = [flags |-> {}]

-----------------------------------------------------------------------------
(* A change of a copy, as its filesystem holds its change time: on a coarse disk, one in the tick of the last moves
   nothing. *)
Bump(d) == IF d \in Coarse /\ ~w.fresh[d] THEN w.ct[d] ELSE w.ct[d] + 1
Times(d) == <<w.mt[d], w.ct[d]>>
Row(d) == <<cat.rmt[d], cat.rct[d]>>
(* a read of chunk i through the cache, wrong once with flip *)
Cached(d, i) == IF i \in w.flip[d] THEN -2 ELSE w.ch[d][i]
Holds(d) == w.at[d] /\ \A i \in Chunks : w.ch[d][i] = cat.want[i] /\ i \notin w.bad[d]
(* a chunk holding a user's edit that is not the file's version, which a pass may have made it *)
Edited(d) == w.at[d] /\ \E i \in Chunks : w.ch[d][i] > 0 /\ w.ch[d][i] # cat.want[i]
(* §16: a healthy copy: ok on a disk that can be read, read whole, every chunk the file's, its times its row's *)
Healthy(e) == cat.cs[e] = "ok" /\ w.up[e] /\ Holds(e) /\ Times(e) = Row(e)
HealthyOf(d) == {e \in Disks \ {d} : Healthy(e)}
First(S) == CHOOSE x \in S : \A y \in S : x <= y
Flag(f) == ghost' = [ghost EXCEPT !.flags = @ \cup f]
(* the file's state once a copy's state is cs: corrupt while a copy is, then ok, but a conflict *)
Settle(cs) == IF \E x \in Disks : cs[x] = "corrupt" THEN "corrupt" ELSE IF cat.fst = "corrupt" THEN "ok" ELSE cat.fst

(* coarse-conflict, the maintainer's remedy of flaw 31: on a disk whose change time is coarse, an edit in the tick of
   the copy's last change moves no time a look compares, so a mismatch there is never rot: a conflict, every version
   kept, nothing written (specs.md §16). *)
Unvouched(d) == d \in Coarse

(* a user's edit a repair would hide, recording its copy verified with the change time it has: one a later scrub may
   still call corrupt, on a disk whose times vouch for a copy *)
Hidden(d) == Edited(d) /\ ~Unvouched(d)

(* the copy written in place, its chunk i given value v: a change of its times *)
Written(d, i, v) == [w EXCEPT !.ch[d][i] = v, !.bad[d] = @ \ {i}, !.mt[d] = @ + 1, !.ct[d] = Bump(d), !.fresh[d] = FALSE]

-----------------------------------------------------------------------------
(* The scrub of one copy due (§16, jobs.scrubCopy): of a file neither corrupt nor in conflict, ok, nothing planned on it;
   opened after a look that its name holds what its row says, from the cursor while its row has not changed. A disk lost
   is read by nothing, and a copy changed waits for the next pass. *)
ScrubStart(d) ==
    /\ "scrub" \in AtollActs /\ pc.ph = "idle"
    /\ cat.cs[d] = "ok" /\ cat.fst = "ok" /\ ~cat.op[d].on
    /\ w.up[d] /\ w.at[d] /\ Times(d) = Row(d)
    /\ pc' = [Idle EXCEPT !.ph = "read", !.d = d, !.st = Times(d),
                          !.i = IF cat.cursor.d = d /\ cat.cursor.t = Row(d) THEN cat.cursor.i ELSE 1]
    /\ cat' = [cat EXCEPT !.cursor = NoCursor]
    /\ UNCHANGED <<w, ghost>>

NextChunk == IF pc.i = 2 THEN [pc EXCEPT !.ph = "end"] ELSE [pc EXCEPT !.i = 2]

ScrubRead ==
    /\ pc.ph = "read"
    /\ LET d == pc.d  i == pc.i IN
       IF ~w.up[d]
       THEN \* the disk does not confirm: lost, and nothing concluded (invariant 4)
            /\ pc' = Idle /\ UNCHANGED <<w, cat, ghost>>
       ELSE IF i \notin w.bad[d] /\ Cached(d, i) = cat.want[i]
       THEN /\ pc' = NextChunk /\ UNCHANGED <<w, cat, ghost>>
       ELSE IF Times(d) # pc.st
       THEN \* a chunk that does not match, the copy's times looked at first: changed, for the next pass
            /\ pc' = Idle /\ UNCHANGED <<w, cat, ghost>>
       ELSE \* read again past the page cache
            /\ w' = [w EXCEPT !.flip[d] = @ \ {i}]
            /\ IF i \notin w.bad[d] /\ w.ch[d][i] = cat.want[i]
               THEN /\ pc' = NextChunk /\ UNCHANGED <<cat, ghost>>   \* a bad read: the read goes on
               ELSE IF ~cat.ver[d] \/ Unvouched(d)
               THEN \* never verified: another version of the file, a conflict, nothing moved (§16); and the same on a
                    \* disk whose times cannot tell an edit from rot (coarse-conflict)
                    /\ cat' = [cat EXCEPT !.fst = "conflict"]
                    /\ pc' = Idle
                    /\ UNCHANGED ghost
               ELSE /\ cat' = [cat EXCEPT !.cs[d] = "corrupt", !.fst = "corrupt"]
                    /\ Flag((IF Edited(d) THEN {"edit-called-corrupt"} ELSE {}) \cup (IF Holds(d) THEN {"sound-corrupt"} ELSE {}))
                    /\ pc' = [Idle EXCEPT !.ph = "heal", !.d = d]

(* the budget runs out between two chunks: the cursor records the copy, the next chunk and its row's times *)
ScrubBudget ==
    /\ "budget" \in AtollActs /\ pc.ph = "read" /\ pc.i = 2
    /\ cat' = [cat EXCEPT !.cursor = [d |-> pc.d, i |-> 2, t |-> Row(pc.d)]]
    /\ pc' = Idle
    /\ UNCHANGED <<w, ghost>>

ScrubEnd ==
    /\ pc.ph = "end"
    /\ LET d == pc.d IN
       IF w.up[d] /\ Times(d) = pc.st
       THEN cat' = [cat EXCEPT !.ver[d] = TRUE]
       ELSE UNCHANGED cat
    /\ pc' = Idle
    /\ UNCHANGED <<w, ghost>>

-----------------------------------------------------------------------------
(* A corrupt copy healed (§16, §16.1): from the first healthy copy, by Mode; with none, it stays. A corrupt copy left in
   the root is taken again by the next scrub of its disk, unless it changed since it was found corrupt, which settles
   into a conflict (Conflicts.tla's) and waits here. *)
TakeAgain(d) ==
    /\ "scrub" \in AtollActs /\ pc.ph = "idle"
    /\ cat.cs[d] = "corrupt" /\ ~cat.op[d].on /\ w.up[d] /\ w.at[d] /\ Times(d) = Row(d)
    /\ HealthyOf(d) # {}
    /\ pc' = [Idle EXCEPT !.ph = "heal", !.d = d]
    /\ UNCHANGED <<w, cat, ghost>>

Heal ==
    /\ pc.ph = "heal"
    /\ LET d == pc.d  H == HealthyOf(d) IN
       /\ IF H = {} \/ ~w.up[d]
          THEN pc' = Idle   \* no healthy copy, or its disk lost: it stays, corrupt
          ELSE pc' = [pc EXCEPT !.ph = IF Mode = "chunk" THEN "bad" ELSE "move", !.e = First(H)]
       /\ UNCHANGED <<w, cat, ghost>>

(* By file, or quarantined: the copy, looked at against its row right before, sent to the trash whole; the next pass
   makes it again from the healthy copy (§16, §16.1, §17.1). *)
Move ==
    /\ pc.ph = "move"
    /\ LET d == pc.d IN
       IF w.up[d] /\ w.at[d] /\ Times(d) = Row(d) /\ cat.cs[d] = "corrupt"
       THEN /\ w' = [w EXCEPT !.at[d] = FALSE, !.tr[d] = @ \cup {<<0, w.ch[d][i]>> : i \in Chunks}]
            /\ cat' = [cat EXCEPT !.cs[d] = "none", !.fst = Settle([cat.cs EXCEPT ![d] = "none"])]
            /\ Flag((IF Edited(d) THEN {"edit-repaired"} ELSE {})
                    \cup (IF \A x \in Disks \ {d} : ~w.at[x] THEN {"last-moved"} ELSE {}))
       ELSE UNCHANGED <<w, cat, ghost>>
    /\ pc' = Idle

(* By chunk, step 1: the copy read whole, its bad chunks. One that holds the file's content after all is verified. *)
RBad ==
    /\ pc.ph = "bad"
    /\ LET d == pc.d  B == {i \in Chunks : i \in w.bad[d] \/ w.ch[d][i] # cat.want[i]} IN
       IF ~w.up[d] \/ ~w.at[d] \/ Times(d) # Row(d)
       THEN /\ pc' = Idle /\ UNCHANGED <<w, cat, ghost>>   \* lost, or changed: it stays, for a later run
       ELSE IF B = {}
       THEN /\ cat' = [cat EXCEPT !.cs[d] = "ok", !.ver[d] = TRUE, !.fst = Settle([cat.cs EXCEPT ![d] = "ok"])]
            /\ pc' = Idle /\ UNCHANGED <<w, ghost>>
       ELSE IF Unvouched(d)
       THEN \* coarse-conflict: a corrupt copy on a disk whose times cannot tell an edit, as one found before Atoll
            \* knew the disk so, is read as it is (jobs.changedCopy): its bad chunks may be an edit, a conflict, nothing
            \* written
            /\ cat' = [cat EXCEPT !.fst = "conflict"]
            /\ pc' = Idle /\ UNCHANGED <<w, ghost>>
       ELSE /\ pc' = [pc EXCEPT !.ph = "journal", !.bad = B, !.st = Times(d), !.seen = w.ch[d], !.sec = w.bad[d]]
            /\ UNCHANGED <<w, cat, ghost>>

(* Step 2: the operation and its trash row, in one transaction. *)
RJournal ==
    /\ pc.ph = "journal"
    /\ cat' = [cat EXCEPT !.op[pc.d] = [on |-> TRUE, bad |-> pc.bad, saved |-> {}, wrote |-> FALSE, sv |-> FALSE]]
    /\ pc' = [pc EXCEPT !.ph = "save"]
    /\ UNCHANGED <<w, ghost>>

(* Step 3: every bad chunk that can be read saved to the trash before any is written. *)
RSave ==
    /\ pc.ph = "save"
    /\ LET d == pc.d  S == {i \in pc.bad : i \notin w.bad[d]} IN
       IF ~w.up[d]
       THEN /\ pc' = Idle /\ UNCHANGED <<w, cat, ghost>>   \* lost: the operation stays planned, for the reprise
       ELSE /\ w' = [w EXCEPT !.tr[d] = @ \cup {<<i, w.ch[d][i]>> : i \in S}]
            /\ cat' = [cat EXCEPT !.op[d].saved = S, !.op[d].sv = TRUE]
            /\ pc' = [pc EXCEPT !.ph = "look"]
            /\ UNCHANGED ghost

(* A repair that stops, by the run that knows whether it wrote (§16.1): a write began, the modification time is set back
   to the row's and the row takes the change time the copy has then; the operation done or undone; the copy corrupt. *)
Stop(d, wrote) ==
    /\ IF wrote
       THEN /\ w' = [w EXCEPT !.mt[d] = cat.rmt[d], !.ct[d] = Bump(d), !.fresh[d] = FALSE]
            /\ cat' = [cat EXCEPT !.op[d] = NoOp, !.rct[d] = Bump(d)]
       ELSE /\ UNCHANGED w
            /\ cat' = [cat EXCEPT !.op[d] = NoOp]
    /\ pc' = Idle

(* Step 4: right before the first write, the copy compared with its row: an edit in place is never overwritten. *)
RLook ==
    /\ pc.ph = "look"
    /\ LET d == pc.d IN
       IF ~w.up[d]
       THEN /\ pc' = Idle /\ UNCHANGED <<w, cat>>
       ELSE IF Times(d) # pc.st
       THEN Stop(d, FALSE)
       ELSE /\ pc' = [pc EXCEPT !.ph = "write", !.i = First(pc.bad)]
            /\ UNCHANGED <<w, cat>>
    /\ UNCHANGED ghost

(* repair-recheck, the maintainer's remedy of flaw 32 (trash.Session.RepairChunks): the repair keeps the times its own
   last change gave the copy, and right before each later step, the copy's times are looked at; right before each
   write, the chunk it writes is read again too (ops.Disk.ChunkStill), and must be as the repair found it bad, as a
   write in the tick of the repair's own last change moves no time on a coarse disk: moved, someone else wrote, and the
   repair stops with changed (ops.Disk.LeaveRepair), the modification time not set back, the row left as it was, so
   that the change stays a change. *)
Moved(d) == Times(d) # pc.st
Reread(d, i) == w.ch[d][i] # pc.seen[i] \/ (i \in w.bad[d]) # (i \in pc.sec)
Left(d) == /\ cat' = [cat EXCEPT !.op[d] = NoOp]
           /\ pc' = Idle
           /\ UNCHANGED w

(* Step 5: each bad chunk read from the healthy copy and checked, written, then read back. *)
RWrite ==
    /\ pc.ph = "write"
    /\ LET d == pc.d  e == pc.e  i == pc.i  v == w.ch[d][i]  wrote == cat.op[d].wrote IN
       IF w.up[d] /\ (Moved(d) \/ Reread(d, i))
       THEN /\ Left(d) /\ UNCHANGED ghost
       ELSE IF ~w.up[e] \/ w.ch[e][i] # cat.want[i] \/ i \in w.bad[e]
       THEN \* the healthy copy lost, or no longer holding its chunk: the repair stops (io-error, changed)
            /\ IF w.up[d] THEN Stop(d, wrote) ELSE /\ pc' = Idle /\ UNCHANGED <<w, cat>>
            /\ UNCHANGED ghost
       ELSE IF ~w.up[d]
       THEN /\ pc' = Idle /\ UNCHANGED <<w, cat, ghost>>   \* lost: planned, for the reprise
       ELSE /\ w' = Written(d, i, w.ch[e][i])
            /\ cat' = [cat EXCEPT !.op[d].wrote = TRUE]
            /\ Flag((IF v > 0 /\ v # cat.want[i] THEN {"edit-repaired"} ELSE {})
                    \cup (IF <<i, v>> \notin w.tr[d] /\ i \notin w.bad[d] THEN {"unsaved"} ELSE {})
                    \cup (IF w.ch[e][i] # cat.want[i] THEN {"bad-source"} ELSE {}))
            /\ pc' = IF \E j \in pc.bad : j > i THEN [pc EXCEPT !.i = First({j \in pc.bad : j > i}), !.st = <<w.mt[d] + 1, Bump(d)>>]
                     ELSE [pc EXCEPT !.ph = "times", !.st = <<w.mt[d] + 1, Bump(d)>>]

(* A crash in the middle of a chunk's write, once its looks passed: the chunk torn, part its old bytes and part the new
   (-3), what the write moved of the copy's times moved; the operation planned, a write begun. *)
RTear ==
    /\ "kill" \in AtollActs /\ w.kills < MaxKill /\ pc.ph = "write"
    /\ LET d == pc.d  e == pc.e  i == pc.i IN
       /\ w.up[d] /\ ~Moved(d) /\ ~Reread(d, i)
       /\ w.up[e] /\ w.ch[e][i] = cat.want[i] /\ i \notin w.bad[e]
       /\ w' = [Written(d, i, -3) EXCEPT !.kills = @ + 1]
       /\ cat' = [cat EXCEPT !.op[d].wrote = TRUE]
       /\ pc' = Idle
    /\ UNCHANGED ghost

(* Steps 6 and 7: the modification time set back to the row's, then the commit, the copy ok and verified, the row taking
   the change time the copy has then. *)
RTimes ==
    /\ pc.ph = "times"
    /\ LET d == pc.d IN
       IF ~w.up[d]
       THEN /\ pc' = Idle /\ UNCHANGED <<w, cat>>
       ELSE IF Moved(d)
       THEN Left(d)
       ELSE /\ w' = [w EXCEPT !.mt[d] = cat.rmt[d], !.ct[d] = Bump(d), !.fresh[d] = FALSE]
            /\ pc' = [pc EXCEPT !.ph = "commit", !.st = <<cat.rmt[d], Bump(d)>>]
            /\ UNCHANGED cat
    /\ UNCHANGED ghost
RCommit ==
    /\ pc.ph = "commit"
    /\ LET d == pc.d  cs == [cat.cs EXCEPT ![d] = "ok"] IN
       IF Moved(d)
       THEN /\ Left(d) /\ UNCHANGED ghost
       ELSE
       /\ cat' = [cat EXCEPT !.cs[d] = "ok", !.ver[d] = TRUE, !.rct[d] = w.ct[d], !.op[d] = NoOp, !.fst = Settle(cs)]
       /\ Flag(IF Hidden(d) THEN {"verified-wrong"} ELSE {})
       /\ pc' = Idle
       /\ UNCHANGED w

(* §16.1, Reprise (integrity.RepairReprise, ops.Disk.resolveRepair): a repair left planned, the copy read whole. Each
   chunk holds the file's content (good), what the repair saved of it (saved: its bytes in the trash, or, for a chunk it
   could not read, a read that still fails), or neither (other). A chunk the repair did not find bad that is not good is
   someone else's change; so is a bad chunk
   good while saving was not over. The bad chunks, by index, must hold what is good, then what was saved: every one good,
   the writes were all done, steps 6 and 7; every one saved, no write began, nothing written, or someone else's change
   once the change time moved; otherwise a write began, the modification time set back and the row taking the change
   time, the copy still corrupt. A bad chunk that holds neither, a write the crash tore or a user's, is someone else's
   change (torn-foreign, the maintainer's remedy of flaw 33). A chunk that cannot be read, on a disk that confirms, is
   the chunk's fault (unread-found, his remedy of flaw 34): a bad one the repair could not read, nothing saved of it yet,
   holds what the repair found, and any other holds neither its hash nor what was saved of it. *)
(* a bad chunk the repair could not read that still cannot be read: what the repair found, an empty file saved for it
   or not yet, as no write begins before every bad chunk is saved (ops.chunksNow, ops.Disk.ResolveRepairConfirmed) *)
UnreadFound(d, i) == i \in cat.op[d].bad \ cat.op[d].saved /\ i \in w.bad[d]
HoldsR(d, i) == IF w.ch[d][i] = cat.want[i] /\ i \notin w.bad[d] THEN "good"
                ELSE IF cat.op[d].sv /\ i \in cat.op[d].saved /\ i \notin w.bad[d] /\ <<i, w.ch[d][i]>> \in w.tr[d] THEN "saved"
                ELSE IF UnreadFound(d, i) THEN "saved"
                ELSE "other"
(* the bad chunks' holds in the order of their indexes, Chunks being {1, 2} *)
BadSeq(d) == IF cat.op[d].bad = {1, 2} THEN <<HoldsR(d, 1), HoldsR(d, 2)>> ELSE <<HoldsR(d, First(cat.op[d].bad))>>
(* what is good, then what was saved *)
InOrder(s) == IF DOMAIN s = {1} THEN s[1] \in {"good", "saved"}
              ELSE <<s[1], s[2]>> \in {<<"good", "good">>, <<"good", "saved">>, <<"saved", "saved">>}
Reprise(d) ==
    /\ pc.ph = "idle" /\ cat.op[d].on /\ w.up[d] /\ w.at[d]
    /\ LET op == cat.op[d]
           s == BadSeq(d)
           n == Cardinality(op.bad)
           ctimeSame == w.ct[d] = cat.rct[d]
           others == {i \in op.bad : HoldsR(d, i) = "other"}
           foreign == \/ \E i \in Chunks \ op.bad : HoldsR(d, i) # "good"
                      \/ ~op.sv /\ \E i \in op.bad : HoldsR(d, i) = "good"
                      \/ ~op.sv /\ ~ctimeSame
                      \/ op.sv /\ ~InOrder(s)
                      \/ op.sv /\ \A j \in DOMAIN s : s[j] = "saved" /\ ~ctimeSame
           finish == op.sv /\ \A j \in DOMAIN s : s[j] = "good"
           untouched == \A j \in DOMAIN s : s[j] = "saved" \/ ~op.sv
       IN IF ~foreign /\ finish
          THEN \* every write done: steps 6 and 7
               /\ w' = [w EXCEPT !.mt[d] = cat.rmt[d], !.ct[d] = Bump(d), !.fresh[d] = FALSE]
               /\ cat' = [cat EXCEPT !.cs[d] = "ok", !.ver[d] = TRUE, !.rct[d] = Bump(d), !.op[d] = NoOp,
                                     !.fst = Settle([cat.cs EXCEPT ![d] = "ok"])]
               /\ Flag(IF Hidden(d) THEN {"verified-wrong"} ELSE {})
          ELSE IF ~foreign /\ ~untouched
          THEN \* a write began: the modification time set back, the row takes the change time, the repair stops; a chunk
               \* that holds neither, were it ever taken here for the repair's own torn write, flagged (flaw 33)
               /\ w' = [w EXCEPT !.mt[d] = cat.rmt[d], !.ct[d] = Bump(d), !.fresh[d] = FALSE]
               /\ cat' = [cat EXCEPT !.rct[d] = Bump(d), !.op[d] = NoOp]
               /\ Flag(IF \E i \in others : w.ch[d][i] > 0 /\ w.ch[d][i] # cat.want[i] THEN {"taken-torn"} ELSE {})
          ELSE \* no write began, or someone else's change: nothing written, the repair stops
               /\ cat' = [cat EXCEPT !.op[d] = NoOp]
               /\ UNCHANGED <<w, ghost>>
    /\ UNCHANGED pc

-----------------------------------------------------------------------------
(* A pass (§15.4): a copy changed against its row taken in as a modification, its content the file's version; each
   other copy at its path sent to the trash, overwritten, and made again from it; a copy that went made again. It decides
   nothing of a file corrupt or in conflict. A guard may hold the change: the pass then waits, which is its not running. *)
PassModify(d) ==
    /\ "pass" \in AtollActs /\ pc.ph = "idle" /\ cat.fst = "ok"
    /\ cat.cs[d] = "ok" /\ w.up[d] /\ w.at[d] /\ Times(d) # Row(d) /\ d \notin w.bad[d]
    /\ \A x \in Disks : ~cat.op[x].on
    /\ \A x \in Disks \ {d} : w.up[x]
    /\ LET v == w.ch[d]  others == Disks \ {d} IN
       /\ w' = [w EXCEPT !.ch = [x \in Disks |-> IF x = d THEN v ELSE v],
                         !.at = [x \in Disks |-> TRUE],
                         !.bad = [x \in Disks |-> IF x = d THEN w.bad[d] ELSE {}],
                         !.tr = [x \in Disks |-> IF x \in others /\ w.at[x] THEN w.tr[x] \cup {<<0, w.ch[x][i]>> : i \in Chunks}
                                                ELSE w.tr[x]],
                         !.mt = [x \in Disks |-> IF x = d THEN w.mt[d] ELSE w.mt[x] + 1],
                         !.ct = [x \in Disks |-> IF x = d THEN w.ct[d] ELSE w.ct[x] + 1],
                         !.fresh = [x \in Disks |-> IF x = d THEN w.fresh[d] ELSE FALSE]]
       /\ cat' = [cat EXCEPT !.want = v, !.cs = [x \in Disks |-> "ok"], !.ver = [x \in Disks |-> TRUE],
                             !.rmt = [x \in Disks |-> IF x = d THEN w.mt[d] ELSE w.mt[x] + 1],
                             !.rct = [x \in Disks |-> IF x = d THEN w.ct[d] ELSE w.ct[x] + 1]]
    /\ UNCHANGED <<pc, ghost>>
PassMake(d) ==
    /\ "pass" \in AtollActs /\ pc.ph = "idle"
    /\ cat.cs[d] = "none" /\ w.up[d] /\ ~w.at[d] /\ HealthyOf(d) # {}
    /\ w' = [w EXCEPT !.at[d] = TRUE, !.ch[d] = cat.want, !.bad[d] = {}, !.mt[d] = @ + 1, !.ct[d] = @ + 1, !.fresh[d] = FALSE]
    /\ cat' = [cat EXCEPT !.cs[d] = "ok", !.ver[d] = TRUE, !.rmt[d] = w.mt[d] + 1, !.rct[d] = w.ct[d] + 1]
    /\ UNCHANGED <<pc, ghost>>

Kill ==
    /\ "kill" \in AtollActs /\ pc.ph # "idle" /\ w.kills < MaxKill
    /\ pc' = Idle
    /\ w' = [w EXCEPT !.kills = @ + 1]
    /\ UNCHANGED <<cat, ghost>>

-----------------------------------------------------------------------------
(* A user, directly on a disk or through the view: a chunk of the copy written in place, keeping its modification
   time or not; the change time moves as its filesystem holds it. *)
Edit(d, i, keep) ==
    /\ w.users < MaxUser /\ d \in UserDisks /\ w.up[d] /\ w.at[d]
    /\ (IF keep THEN "keep" ELSE "edit") \in UserActs
    /\ w' = [w EXCEPT !.ch[d][i] = w.users + 1, !.bad[d] = @ \ {i}, !.users = @ + 1,
                      !.mt[d] = IF keep THEN @ ELSE @ + 1, !.ct[d] = Bump(d), !.fresh[d] = FALSE]
    /\ UNCHANGED <<cat, pc, ghost>>

Away(a) == a \in WorldActs /\ w.acts < MaxWorld
Rot(d, i) ==
    /\ Away("rot") /\ d \in WorldDisks /\ w.at[d] /\ w.ch[d][i] # -1
    /\ w' = [w EXCEPT !.ch[d][i] = -1, !.acts = @ + 1]
    /\ UNCHANGED <<cat, pc, ghost>>
Sector(d, i) ==
    /\ Away("sector") /\ d \in WorldDisks /\ w.at[d] /\ i \notin w.bad[d]
    /\ w' = [w EXCEPT !.bad[d] = @ \cup {i}, !.acts = @ + 1]
    /\ UNCHANGED <<cat, pc, ghost>>
Flip(d, i) ==
    /\ Away("flip") /\ d \in WorldDisks /\ i \notin w.flip[d]
    /\ w' = [w EXCEPT !.flip[d] = @ \cup {i}, !.acts = @ + 1]
    /\ UNCHANGED <<cat, pc, ghost>>
Lose(d) ==
    /\ Away("lose") /\ d \in WorldDisks /\ w.up[d]
    /\ w' = [w EXCEPT !.up[d] = FALSE, !.acts = @ + 1]
    /\ UNCHANGED <<cat, pc, ghost>>
Back(d) ==
    /\ ~w.up[d]
    /\ w' = [w EXCEPT !.up[d] = TRUE]
    /\ UNCHANGED <<cat, pc, ghost>>
Tick ==
    /\ \E d \in Coarse : ~w.fresh[d]
    /\ w' = [w EXCEPT !.fresh = [d \in Disks |-> TRUE]]
    /\ UNCHANGED <<cat, pc, ghost>>

-----------------------------------------------------------------------------
Steps == ScrubRead \/ ScrubBudget \/ ScrubEnd \/ Heal \/ Move \/ RBad \/ RJournal \/ RSave \/ RLook \/ RWrite
         \/ RTimes \/ RCommit
Atoll == \/ Steps
         \/ \E d \in Disks : ScrubStart(d) \/ TakeAgain(d) \/ Reprise(d) \/ PassModify(d) \/ PassMake(d)
World == \/ Kill \/ RTear \/ Tick
         \/ \E d \in Disks, i \in Chunks, keep \in BOOLEAN : Edit(d, i, keep)
         \/ \E d \in Disks, i \in Chunks : Rot(d, i) \/ Sector(d, i) \/ Flip(d, i)
         \/ \E d \in Disks : Lose(d) \/ Back(d)

Next == Atoll \/ World
Spec == Init /\ [][Next]_vars

(* Liveness: the scrub's steps go on, a scrub takes a corrupt copy again and a pass runs once the lock lets them, a
   disk comes back. *)
LiveSpec == /\ Spec /\ WF_vars(Steps)
            /\ \A d \in Disks : SF_vars(TakeAgain(d)) /\ SF_vars(Reprise(d)) /\ SF_vars(PassMake(d)) /\ WF_vars(Back(d))

-----------------------------------------------------------------------------
TypeOK ==
    /\ \A d \in Disks : /\ cat.cs[d] \in {"ok", "corrupt", "none"}
                        /\ w.bad[d] \subseteq Chunks /\ w.flip[d] \subseteq Chunks
    /\ cat.fst \in {"ok", "corrupt", "conflict"}
    /\ pc.ph \in {"idle", "read", "end", "heal", "move", "bad", "journal", "save", "look", "write", "times", "commit"}
    /\ ghost.flags \subseteq {"edit-called-corrupt", "edit-repaired", "unsaved", "bad-source", "verified-wrong", "last-moved",
                              "sound-corrupt", "taken-torn"}

(* Invariant 9, §16: no user's edit is called corrupt, nor repaired away, by a write over it or a move of its copy to
   the trash. *)
NoEditCalledCorrupt == "edit-called-corrupt" \notin ghost.flags
NoEditRepairedAway == "edit-repaired" \notin ghost.flags
(* §16: a copy that holds the file's content is never called corrupt, a read through the cache wrong once included. *)
SoundNeverCorrupt == "sound-corrupt" \notin ghost.flags
(* Invariant 1, §16.1: whatever a repair overwrites is in the trash first, but a sector that could not be read. *)
TrashFirst == "unsaved" \notin ghost.flags
(* §16.1: what a repair writes is the healthy copy's chunk, checked against the file's hash. *)
SourceVerified == "bad-source" \notin ghost.flags
(* §16.1: a repair, or its reprise, never records a copy ok and verified, its change time taken anew, while it holds a
   user's edit: the row would hide that edit, and the next scrub call it corrupt. A scrub that verifies records none of
   the copy's times, so an edit stays what it was to a look. *)
NoWrongVerify == "verified-wrong" \notin ghost.flags
(* §16.1, Reprise: the reprise of a repair stopped by a crash never takes a user's write for its own torn chunk: it would
   set the time back over it and give the row the change time it left, and the next scrub repair it away. *)
NoUserTakenForTorn == "taken-torn" \notin ghost.flags
(* Invariant 1, §16: a corrupt copy never leaves its path as the file's last copy at its path ("nothing is lost to a
   scrub"); the healthy copy found may be a user's newer version by then, which the next pass takes in. *)
LastKept == "last-moved" \notin ghost.flags

(* Liveness: once the world is done, a corrupt copy with a healthy copy elsewhere is no longer corrupt; one without
   stays at its path, reported. *)
Healed == \A d \in Disks : cat.cs[d] = "corrupt" => (HealthyOf(d) = {} \/ ~w.up[d] \/ Times(d) # Row(d))
EventuallyHealed == <>[]Healed
=============================================================================
