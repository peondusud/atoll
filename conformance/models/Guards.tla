-------------------------------- MODULE Guards --------------------------------
(* The safety guards (specs.md §15.6): a model of its own. Two disks: D, where a user deletes, puts back and rewrites
   files directly, whose guards are modelled, and E, which holds the other copy of each file, where a user may edit
   one. Each file has a copy on each, in sync at the start. Time is a pass's: each walk is one tick, and an answer is
   given between two passes, as it takes the pool's lock (flock) that a pass holds.

   What it keeps: each disk's copy of each file, its version and change time; whether D is mounted; the catalog: D's
   row of each file (ok, gone since a time, stale, or none), E's (ok, stale or none), the file's version and whether it
   is in conflict; the trash jobs, of a deletion (E's copy for D's gone, src_disk D), of a refresh of E from D
   (`overwritten`, src_disk D) and of D from E; the events: copy-gone (with its gone_ns), guard, guard-clear, the
   answers of the mass-deletion guard (with up_to), modified (answered or not), modify-guard (its window or its day),
   their answers (up_to, the window); and a pass: its walk, which records what it finds of D (gone, or back) and keeps
   what it saw, then its guards (catalog.ReadGuard, jobs.readGuards: the mass-deletion guard worked out once, from rows
   and events, a guard event written when it trips with none standing, a guard-clear when it no longer trips with one
   standing; jobs.readModifyGuards: the mass-modification guard stands while a modify-guard event stands since its last
   answer), then its decisions file by file (a gone copy decided but where the guard holds it, a deletion when E's copy
   is unchanged since in sync, a kept file otherwise; a changed copy on D asked of its guard, jobs.guardModification: a
   change made by the up_to of any reject is a conflict, catalog.ModifyGuardRead.Rejected; an answer that covers its
   change time and has not lapsed decides it, a reject as a conflict; otherwise the count, over
   the window then over the day, the decision that would trip either held, with its event), then its trash jobs one at
   a time (jobs.runTrashJob: held while the mass-deletion guard of its src_disk is tripped; a deletion dropped when the
   walk found D's copy back; the move checked against the copy's row, trash.Session.Send). atoll guard confirm and
   reject, by the CLI or the local API, the same code (catalog.ConfirmGuard, RejectGuard, AnswerModifyGuard). A stop of
   the daemon at any step: the pass ends, and the next one starts again from what the catalog holds. A user deletes,
   puts back, rewrites a file on D, edits one on E, or unmounts D and mounts it again, at any moment.

   What it leaves out: the percent of each guard (its files threshold alone), refused renames (§15.3), what a copy
   moved hides from the guard, the guards of E, the link guard, the view, the SSD cache, copies at once, the slices of
   reads, and the copies a plan makes again (Replication.tla's, Copies.tla's): see README.md.

   Flaw 27, fixed (README.md): a reject of the mass-modification guard decided what it covered only while it was the
   last answer and had not lapsed; a change it covered and no pass decided by then was decided as any other, and
   overwrote the other copies. The maintainer's remedy, reject-kept (2026-11-25), is what every instance does. *)
EXTENDS Integers, FiniteSets, TLC

CONSTANTS
    NFiles,     \* files, each with a copy on D and on E
    DelFiles,   \* deletes.mass_guard.files: it trips past that many
    DelWin,     \* deletes.mass_guard.window, in passes
    ModFiles,   \* modifies.mass_guard.files
    ModWin,     \* modifies.mass_guard.window, in passes: an answer lapses past it too
    DayFiles,   \* modifies.day_guard.files
    DayWin,     \* modifies.day_guard.window, in passes
    UserActs,   \* some of "del", "back", "mod", "edit", "drop": what a user does
    MaxUser,    \* how many times
    MaxTime,    \* how many passes
    AnswerActs, \* some of "dconfirm", "dreject", "mconfirm", "mreject": how a user answers
    Fixes       \* the remedies proposed for the flaws found here and still open, by name (README.md); {} is specs.md as it is

Files == 1..NFiles
Times == 0..MaxTime
\* flaw 27 is fixed, with reject-kept, what every instance does (README.md): no remedy is proposed
FixNames == {}

ASSUME /\ NFiles \in 1..3 /\ DelFiles \in Nat /\ ModFiles \in Nat /\ DayFiles \in Nat
       /\ DelWin \in Nat /\ ModWin \in Nat /\ DayWin \in Nat /\ MaxTime \in Nat /\ MaxUser \in Nat
       /\ UserActs \subseteq {"del", "back", "mod", "edit", "drop"}
       /\ AnswerActs \subseteq {"dconfirm", "dreject", "mconfirm", "mreject"}
       /\ Fixes \subseteq FixNames

VARIABLES
    dHas, dVer, dCt,  \* D's copy of each file: there or not, its version, its change time
    eHas, eVer,       \* E's copy of each file
    upD,              \* D mounted
    rD,               \* [Files -> [st |-> "ok", "gone", "stale" or "none", ver, ct, gone]]: D's row
    rE,               \* [Files -> [st |-> "ok", "stale" or "none", ver]]: E's row
    fv,               \* [Files -> the file's version, 0 when the catalog has no file]
    conflict,         \* [Files -> BOOLEAN]
    jobs,             \* set of [f, k |-> "del", "owE" or "owD", ver |-> the copy's version the job expects, at |-> time]
    cg,               \* set of <<ts, gone>>: copy-gone events of D
    gEv,              \* the time of the guard event of D that stands since its last answer or clear, 0 for none
    dAns,             \* [k |-> "", "confirm" or "reject", up |-> up_to]: the last answer of the mass-deletion guard
    mEv,              \* set of <<ts, f, answered>>: modified events from D
    mgEv,             \* [t |-> time of the modify-guard event that stands, 0 for none, w |-> "w" or "day"]
    mAns,             \* [k |-> "", "confirm" or "reject", up, t |-> when given, w]: its last answer
    mDayT,            \* when the last answer to a hold the day tripped was given, 0 for none
    pc,               \* [ph |-> "idle", "walked", "decide", "work", i]: the pass
    snap,             \* [Files -> [dh, dv, dct, eh, ev]]: what its walk saw
    walkedD,          \* its walk reached D
    trip,             \* the mass-deletion guard of D tripped in this pass
    mc, md,           \* the mass-modification guard's counts in this pass, over its window and its day
    now, users, nver,
    dSt,              \* [Files \X Times -> "", "asked", "allowed", "refused"]: what became of a deletion, by its gone time
    mSt,              \* [Files \X Times -> ...]: of a change, by its change time
    trash,            \* set of <<disk, f, ver, reason>>: what went to a trash
    flags             \* the invariants' witnesses

vars == <<dHas, dVer, dCt, eHas, eVer, upD, rD, rE, fv, conflict, jobs, cg, gEv, dAns, mEv, mgEv, mAns, mDayT, pc,
          snap, walkedD, trip, mc, md, now, users, nver, dSt, mSt, trash, flags>>

Idle == [ph |-> "idle", i |-> 0]
NoSnap == [f \in Files |-> [dh |-> TRUE, dv |-> 1, dct |-> 0, eh |-> TRUE, ev |-> 1]]

Init ==
    /\ dHas = [f \in Files |-> TRUE] /\ dVer = [f \in Files |-> 1] /\ dCt = [f \in Files |-> 0]
    /\ eHas = [f \in Files |-> TRUE] /\ eVer = [f \in Files |-> 1]
    /\ upD = TRUE
    /\ rD = [f \in Files |-> [st |-> "ok", ver |-> 1, ct |-> 0, gone |-> 0]]
    /\ rE = [f \in Files |-> [st |-> "ok", ver |-> 1]]
    /\ fv = [f \in Files |-> 1] /\ conflict = [f \in Files |-> FALSE]
    /\ jobs = {} /\ cg = {} /\ gEv = 0 /\ dAns = [k |-> "", up |-> 0]
    /\ mEv = {} /\ mgEv = [t |-> 0, w |-> "w"] /\ mAns = [k |-> "", up |-> 0, t |-> 0, w |-> "w", rej |-> -1] /\ mDayT = 0
    /\ pc = Idle /\ snap = NoSnap /\ walkedD = FALSE /\ trip = FALSE /\ mc = 0 /\ md = 0
    /\ now = 0 /\ users = 0 /\ nver = 1
    /\ dSt = [k \in Files \X Times |-> ""] /\ mSt = [k \in Files \X Times |-> ""]
    /\ trash = {} /\ flags = {}

Flag(name, cond) == IF cond THEN flags \cup {name} ELSE flags

-----------------------------------------------------------------------------
(* The world: a user, directly on a disk, at any moment. *)
UserStep == users < MaxUser
UDel(f) ==
    /\ "del" \in UserActs /\ UserStep /\ upD /\ dHas[f]
    /\ dHas' = [dHas EXCEPT ![f] = FALSE] /\ users' = users + 1
    /\ UNCHANGED <<dVer, dCt, eHas, eVer, upD, rD, rE, fv, conflict, jobs, cg, gEv, dAns, mEv, mgEv, mAns, mDayT, pc,
                   snap, walkedD, trip, mc, md, now, nver, dSt, mSt, trash, flags>>
(* puts the copy back as it was, as an undone move does: the same file *)
UBack(f) ==
    /\ "back" \in UserActs /\ UserStep /\ upD /\ ~dHas[f] /\ rD[f].st \in {"ok", "gone"}
    /\ dHas' = [dHas EXCEPT ![f] = TRUE] /\ users' = users + 1
    /\ UNCHANGED <<dVer, dCt, eHas, eVer, upD, rD, rE, fv, conflict, jobs, cg, gEv, dAns, mEv, mgEv, mAns, mDayT, pc,
                   snap, walkedD, trip, mc, md, now, nver, dSt, mSt, trash, flags>>
(* rewrites the copy on D in place, or replaces it by an atomic save: a modification, never a deletion (invariant 10) *)
UMod(f) ==
    /\ "mod" \in UserActs /\ UserStep /\ upD /\ dHas[f]
    /\ dVer' = [dVer EXCEPT ![f] = nver + 1] /\ dCt' = [dCt EXCEPT ![f] = now] /\ nver' = nver + 1
    /\ users' = users + 1
    /\ UNCHANGED <<dHas, eHas, eVer, upD, rD, rE, fv, conflict, jobs, cg, gEv, dAns, mEv, mgEv, mAns, mDayT, pc,
                   snap, walkedD, trip, mc, md, now, dSt, mSt, trash, flags>>
UEdit(f) ==
    /\ "edit" \in UserActs /\ UserStep /\ eHas[f]
    /\ eVer' = [eVer EXCEPT ![f] = nver + 1] /\ nver' = nver + 1 /\ users' = users + 1
    /\ UNCHANGED <<dHas, dVer, dCt, eHas, upD, rD, rE, fv, conflict, jobs, cg, gEv, dAns, mEv, mgEv, mAns, mDayT, pc,
                   snap, walkedD, trip, mc, md, now, dSt, mSt, trash, flags>>
(* D unmounted, its mount point empty, and mounted again: a missing disk, never a mass deletion (invariant 4) *)
UDrop ==
    /\ "drop" \in UserActs /\ UserStep /\ upD
    /\ upD' = FALSE /\ users' = users + 1
    /\ UNCHANGED <<dHas, dVer, dCt, eHas, eVer, rD, rE, fv, conflict, jobs, cg, gEv, dAns, mEv, mgEv, mAns, mDayT, pc,
                   snap, walkedD, trip, mc, md, now, nver, dSt, mSt, trash, flags>>
UMount ==
    /\ ~upD /\ upD' = TRUE
    /\ UNCHANGED <<dHas, dVer, dCt, eHas, eVer, rD, rE, fv, conflict, jobs, cg, gEv, dAns, mEv, mgEv, mAns, mDayT, pc,
                   snap, walkedD, trip, mc, md, now, users, nver, dSt, mSt, trash, flags>>

-----------------------------------------------------------------------------
(* The mass-deletion guard of D (catalog.ReadGuard, solver.GuardTrips): what it counts, with the up_to of its last
   answer; it is worked out once, at the start of the decisions. *)
UpTo == IF dAns.k = "" THEN -1 ELSE dAns.up
GoneHeld == {f \in Files : rD[f].st = "gone" /\ rD[f].gone > UpTo}
DelCount == Cardinality(GoneHeld) + Cardinality({e \in cg : e[1] > now - DelWin /\ e[2] > UpTo})

(* The mass-modification guard: its last answer decides a change made by its up_to, but once lapsed (catalog.
   ModifyGuardRead.Answers); its counts, of the modified events since that answer, over its window and over its day. *)
Lapsed == mAns.k # "" /\ now - mAns.t > ModWin
AnswersCt(ct) == mAns.k # "" /\ ~Lapsed /\ ct <= mAns.up
(* reject-kept, the maintainer's remedy of flaw 27 (2026-11-25): a change made by the up_to of any reject is a
   conflict, whatever answer came after it, however long ago (catalog.ModifyGuardRead.Rejected): what a reject covered
   is never decided otherwise. *)
RejectedCt(ct) == ct <= mAns.rej
ModCount == Cardinality({e \in mEv : e[1] > now - ModWin /\ e[1] > mAns.t /\ ~e[3]})
DayCount == Cardinality({e \in mEv : e[1] > now - DayWin /\ e[1] > mDayT /\ ~e[3]})

(* A pass. Its walk: D's copies found gone, or back as they were; what it sees of both disks, kept for its decisions. A
   disk not mounted is not walked: nothing of it is found gone (invariant 4). *)
PWalk ==
    /\ pc = Idle
    \* time stops at MaxTime, the passes going on
    /\ now' = IF now < MaxTime THEN now + 1 ELSE now
    /\ rD' = [f \in Files |->
                IF upD /\ rD[f].st = "ok" /\ ~dHas[f] THEN [rD[f] EXCEPT !.st = "gone", !.gone = now']
                ELSE IF upD /\ rD[f].st = "gone" /\ dHas[f] THEN [rD[f] EXCEPT !.st = "ok", !.gone = 0]
                ELSE rD[f]]
    /\ snap' = [f \in Files |-> [dh |-> dHas[f], dv |-> dVer[f], dct |-> dCt[f], eh |-> eHas[f], ev |-> eVer[f]]]
    /\ walkedD' = upD
    /\ pc' = [ph |-> "walked", i |-> 0]
    /\ UNCHANGED <<dHas, dVer, dCt, eHas, eVer, upD, rE, fv, conflict, jobs, cg, gEv, dAns, mEv, mgEv, mAns, mDayT,
                   trip, mc, md, users, nver, dSt, mSt, trash, flags>>

(* Its guards: the mass-deletion guard worked out once (jobs.readGuards), its event or its clear written; the
   mass-modification guard read from events (jobs.readModifyGuards). *)
PGuard ==
    /\ pc.ph = "walked"
    /\ LET t == DelCount > DelFiles IN
       /\ trip' = t
       /\ gEv' = IF t /\ gEv = 0 THEN now ELSE IF ~t THEN 0 ELSE gEv
       /\ dSt' = [k \in Files \X Times |->
                    IF t /\ k[1] \in GoneHeld /\ k[2] = rD[k[1]].gone THEN "asked"
                    ELSE IF ~t /\ gEv # 0 /\ dSt[k] = "asked" THEN "allowed"
                    ELSE dSt[k]]
    /\ mc' = ModCount /\ md' = DayCount
    /\ pc' = [ph |-> "decide", i |-> 1]
    /\ UNCHANGED <<dHas, dVer, dCt, eHas, eVer, upD, rD, rE, fv, conflict, jobs, cg, dAns, mEv, mgEv, mAns, mDayT,
                   snap, walkedD, now, users, nver, mSt, trash, flags>>

Unchanged4Decide == UNCHANGED <<dHas, dVer, dCt, eHas, eVer, upD, snap, walkedD, trip, now, users, nver, trash, flags,
                                dAns, mAns, mDayT>>

(* A gone copy (propagate.go, §15.5): held while the guard is tripped and its gone time is past the last answer's
   up_to; a deletion when E's copy is what its row says, the gone row dropped, a copy-gone event, a trash job of E's
   copy; kept otherwise, E's copy changed meanwhile being a modification that survives. *)
DecideGone(f) ==
    LET s == snap[f] IN
    IF trip /\ rD[f].gone > UpTo THEN UNCHANGED <<rD, rE, fv, cg, jobs>>
    ELSE IF s.eh /\ rE[f].st # "none" /\ s.ev = rE[f].ver
         THEN /\ rD' = [rD EXCEPT ![f] = [st |-> "none", ver |-> 0, ct |-> 0, gone |-> 0]]
              /\ cg' = cg \cup {<<now, rD[f].gone>>}
              /\ jobs' = jobs \cup {[f |-> f, k |-> "del", ver |-> rE[f].ver, at |-> rD[f].gone]}
              /\ UNCHANGED <<rE, fv>>
         ELSE /\ rD' = [rD EXCEPT ![f] = [st |-> "none", ver |-> 0, ct |-> 0, gone |-> 0]]
              /\ fv' = [fv EXCEPT ![f] = IF s.eh THEN s.ev ELSE 0]
              /\ rE' = [rE EXCEPT ![f] = IF s.eh THEN [st |-> "ok", ver |-> s.ev] ELSE [st |-> "none", ver |-> 0]]
              /\ UNCHANGED <<cg, jobs>>

(* A copy changed on D, E's unchanged (§15.4): asked of D's mass-modification guard (jobs.guardModification). *)
OwJob(f, ct) == [f |-> f, k |-> "owE", ver |-> rE[f].ver, at |-> ct]
DecideMod(f) ==
    LET s == snap[f]
        ct == s.dct
        apply(answered) ==
            /\ rD' = [rD EXCEPT ![f].ver = s.dv, ![f].ct = ct]
            /\ fv' = [fv EXCEPT ![f] = s.dv]
            /\ rE' = [rE EXCEPT ![f].st = "stale"]
            /\ jobs' = jobs \cup {OwJob(f, ct)}
            /\ mEv' = mEv \cup {<<now, f, answered>>}
    IN
    CASE mgEv.t # 0 /\ ~AnswersCt(ct) /\ ~RejectedCt(ct) ->
           \* a guard that stands holds it, nothing read
           /\ mSt' = [mSt EXCEPT ![<<f, ct>>] = "asked"]
           /\ UNCHANGED <<rD, rE, fv, jobs, mEv, mgEv, conflict, mc, md>>
      [] (AnswersCt(ct) /\ mAns.k = "reject") \/ RejectedCt(ct) ->
           \* a conflict: every version stays
           /\ conflict' = [conflict EXCEPT ![f] = TRUE]
           /\ rD' = [rD EXCEPT ![f].ver = s.dv, ![f].ct = ct]
           /\ UNCHANGED <<rE, fv, jobs, mEv, mgEv, mSt, mc, md>>
      [] AnswersCt(ct) ->
           /\ apply(TRUE)
           /\ UNCHANGED <<mgEv, conflict, mSt, mc, md>>
      [] mc + 1 > ModFiles \/ md + 1 > DayFiles ->
           \* the decision that would trip it is not applied, and its event written: the window's first
           /\ mgEv' = [t |-> now, w |-> IF mc + 1 > ModFiles THEN "w" ELSE "day"]
           /\ mSt' = [mSt EXCEPT ![<<f, ct>>] = "asked"]
           /\ UNCHANGED <<rD, rE, fv, jobs, mEv, conflict, mc, md>>
      [] OTHER ->
           /\ apply(FALSE)
           /\ mc' = mc + 1 /\ md' = md + 1
           /\ UNCHANGED <<mgEv, conflict, mSt>>

(* A copy changed on E alone: a modification from E, whose guards are not modelled; D's copy refreshed from it. *)
DecideEdit(f) ==
    LET s == snap[f] IN
    /\ fv' = [fv EXCEPT ![f] = s.ev]
    /\ rE' = [rE EXCEPT ![f].ver = s.ev]
    /\ rD' = IF rD[f].st = "ok" THEN [rD EXCEPT ![f].st = "stale"] ELSE rD
    /\ jobs' = IF rD[f].st = "ok" THEN jobs \cup {[f |-> f, k |-> "owD", ver |-> rD[f].ver, at |-> 0]} ELSE jobs

(* The decisions, file by file, each committed on its own. *)
PDecide ==
    /\ pc.ph = "decide" /\ pc.i <= NFiles
    /\ pc' = [pc EXCEPT !.i = @ + 1]
    /\ LET f == pc.i
           s == snap[f]
           busy == \E j \in jobs : j.f = f
       IN
       CASE rD[f].st = "gone" /\ walkedD ->
              DecideGone(f) /\ UNCHANGED <<conflict, mEv, mgEv, mSt, mc, md, dSt>>
         [] busy \/ conflict[f] ->
              UNCHANGED <<rD, rE, fv, cg, jobs, conflict, mEv, mgEv, mSt, mc, md, dSt>>
         [] walkedD /\ rD[f].st = "ok" /\ s.dh /\ s.dv # rD[f].ver /\ s.eh /\ rE[f].st # "none" /\ s.ev # rE[f].ver ->
              \* both changed: a conflict, every version kept
              /\ conflict' = [conflict EXCEPT ![f] = TRUE]
              /\ rD' = [rD EXCEPT ![f].ver = s.dv, ![f].ct = s.dct] /\ rE' = [rE EXCEPT ![f].ver = s.ev]
              /\ UNCHANGED <<fv, cg, jobs, mEv, mgEv, mSt, mc, md, dSt>>
         [] walkedD /\ rD[f].st = "ok" /\ s.dh /\ s.dv # rD[f].ver /\ s.eh /\ rE[f].st # "none" ->
              DecideMod(f) /\ UNCHANGED <<cg, dSt>>
         [] s.eh /\ rE[f].st # "none" /\ s.ev # rE[f].ver /\ (rD[f].st = "none" \/ (walkedD /\ rD[f].st = "ok" /\ s.dv = rD[f].ver)) ->
              DecideEdit(f) /\ UNCHANGED <<cg, conflict, mEv, mgEv, mSt, mc, md, dSt>>
         [] OTHER ->
              UNCHANGED <<rD, rE, fv, cg, jobs, conflict, mEv, mgEv, mSt, mc, md, dSt>>
    /\ Unchanged4Decide /\ UNCHANGED gEv

(* Its trash jobs, one at a time (jobs.runTrashJob), once every file is decided. *)
PWorkStart ==
    /\ pc.ph = "decide" /\ pc.i > NFiles
    /\ pc' = [ph |-> "work", i |-> 0]
    /\ UNCHANGED <<dHas, dVer, dCt, eHas, eVer, upD, rD, rE, fv, conflict, jobs, cg, gEv, dAns, mEv, mgEv, mAns, mDayT,
                   snap, walkedD, trip, mc, md, now, users, nver, dSt, mSt, trash, flags>>

Gone(f) == [st |-> "none", ver |-> 0, ct |-> 0, gone |-> 0]
RunDel(j) ==
    LET f == j.f IN
    \* the walk found D's copy back: a copy the job does not cover, and the deletion no longer stands
    IF walkedD /\ snap[f].dh THEN /\ jobs' = jobs \ {j} /\ UNCHANGED <<eHas, rE, fv, trash, flags>>
    \* the move, checked against the copy's row right before it: one changed since stays
    ELSE IF eHas[f] /\ eVer[f] = j.ver
         THEN /\ eHas' = [eHas EXCEPT ![f] = FALSE]
              /\ rE' = [rE EXCEPT ![f] = [st |-> "none", ver |-> 0]]
              /\ fv' = [fv EXCEPT ![f] = IF rD[f].st = "none" THEN 0 ELSE fv[f]]
              /\ jobs' = jobs \ {j}
              /\ trash' = trash \cup {<<"E", f, eVer[f], "deleted">>}
              /\ flags' = Flag("trashed-held", dSt[<<f, j.at>>] = "asked")
                              \cup (IF dSt[<<f, j.at>>] = "refused" THEN {"trashed-refused"} ELSE {})
         ELSE /\ jobs' = jobs \ {j} /\ UNCHANGED <<eHas, rE, fv, trash, flags>>

RunOwE(j) ==
    LET f == j.f IN
    IF conflict[f] \/ (eHas[f] /\ eVer[f] # j.ver) THEN /\ jobs' = jobs \ {j} /\ UNCHANGED <<eHas, eVer, rE, trash, flags>>
    \* the source, D's copy, must still be the version decided: on a disk away, the job waits; changed or gone since, it
    \* goes, and the next pass decides what D holds then
    ELSE IF ~upD THEN UNCHANGED <<jobs, eHas, eVer, rE, trash, flags>>
    ELSE IF ~(dHas[f] /\ dVer[f] = fv[f]) THEN /\ jobs' = jobs \ {j} /\ UNCHANGED <<eHas, eVer, rE, trash, flags>>
    ELSE /\ trash' = IF eHas[f] THEN trash \cup {<<"E", f, eVer[f], "overwritten">>} ELSE trash
         /\ eHas' = [eHas EXCEPT ![f] = TRUE] /\ eVer' = [eVer EXCEPT ![f] = fv[f]]
         /\ rE' = [rE EXCEPT ![f] = [st |-> "ok", ver |-> fv[f]]]
         /\ jobs' = jobs \ {j}
         /\ flags' = Flag("overwrote-held", mSt[<<f, j.at>>] = "asked")
                         \cup (IF mSt[<<f, j.at>>] = "refused" THEN {"overwrote-refused"} ELSE {})

RunOwD(j) ==
    LET f == j.f IN
    IF ~upD \/ ~dHas[f] \/ dVer[f] # j.ver \/ ~eHas[f] \/ eVer[f] # fv[f]
    THEN /\ jobs' = jobs \ {j} /\ UNCHANGED <<dVer, dCt, rD, trash>>
    ELSE /\ trash' = trash \cup {<<"D", f, dVer[f], "overwritten">>}
         /\ dVer' = [dVer EXCEPT ![f] = fv[f]] /\ dCt' = [dCt EXCEPT ![f] = now]
         /\ rD' = [rD EXCEPT ![f] = [st |-> "ok", ver |-> fv[f], ct |-> now, gone |-> 0]]
         /\ jobs' = jobs \ {j}

PWork(j) ==
    /\ pc.ph = "work" /\ j \in jobs
    \* held while the mass-deletion guard of its src_disk, D, is tripped in this pass; a refresh of D is E's
    /\ ~(trip /\ j.k \in {"del", "owE"})
    /\ CASE j.k = "del" -> RunDel(j) /\ UNCHANGED <<dVer, dCt, eVer, rD>>
         [] j.k = "owE" -> RunOwE(j) /\ UNCHANGED <<dVer, dCt, rD, fv>>
         [] OTHER -> RunOwD(j) /\ UNCHANGED <<eHas, eVer, rE, fv, flags>>
    /\ UNCHANGED <<dHas, upD, conflict, cg, gEv, dAns, mEv, mgEv, mAns, mDayT, pc, snap, walkedD, trip, mc, md, now,
                   users, nver, dSt, mSt>>

(* The end of a pass, or a stop of the daemon at any step: what the pass held in memory goes with it. *)
Ended ==
    /\ pc' = Idle /\ trip' = FALSE /\ walkedD' = FALSE /\ snap' = NoSnap
    /\ UNCHANGED <<dHas, dVer, dCt, eHas, eVer, upD, rD, rE, fv, conflict, jobs, cg, gEv, dAns, mEv, mgEv, mAns, mDayT,
                   mc, md, now, nver, dSt, mSt, trash, flags>>
(* A job that runs no more in this pass: held by the guard, or a refresh whose source is on a disk away. *)
Waits(j) == (trip /\ j.k \in {"del", "owE"}) \/ (j.k = "owE" /\ ~upD)
PEnd == pc.ph = "work" /\ (\A j \in jobs : Waits(j)) /\ Ended /\ UNCHANGED users
(* The daemon stopped at any step, which counts among what the world does. *)
PStop == pc.ph # "idle" /\ UserStep /\ Ended /\ users' = users + 1

-----------------------------------------------------------------------------
(* atoll guard confirm and reject, by the CLI or the local API, between two passes, under the pool's lock. What the
   guard held past its up_to is not answered: counted anew, it is asked about again when it trips the guard again. *)
Answered(st, up, to) == [k \in Files \X Times |-> IF st[k] = "asked" THEN (IF k[2] <= up THEN to ELSE "") ELSE st[k]]
DConfirm ==
    /\ "dconfirm" \in AnswerActs /\ pc = Idle /\ gEv # 0
    /\ dAns' = [k |-> "confirm", up |-> gEv] /\ gEv' = 0
    /\ dSt' = Answered(dSt, gEv, "allowed")
    /\ UNCHANGED <<dHas, dVer, dCt, eHas, eVer, upD, rD, rE, fv, conflict, jobs, cg, mEv, mgEv, mAns, mDayT, pc, snap,
                   walkedD, trip, mc, md, now, users, nver, mSt, trash, flags>>
(* the gone rows up to it go, a file with no copy left with them; every queued trash job of a deletion from D goes *)
DReject ==
    /\ "dreject" \in AnswerActs /\ pc = Idle /\ gEv # 0
    /\ dAns' = [k |-> "reject", up |-> gEv] /\ gEv' = 0
    /\ rD' = [f \in Files |-> IF rD[f].st = "gone" /\ rD[f].gone <= gEv THEN Gone(f) ELSE rD[f]]
    /\ fv' = [f \in Files |-> IF rD[f].st = "gone" /\ rD[f].gone <= gEv /\ rE[f].st = "none" THEN 0 ELSE fv[f]]
    /\ jobs' = {j \in jobs : j.k # "del"}
    /\ dSt' = Answered(dSt, gEv, "refused")
    /\ UNCHANGED <<dHas, dVer, dCt, eHas, eVer, upD, rE, conflict, cg, mEv, mgEv, mAns, mDayT, pc, snap, walkedD, trip,
                   mc, md, now, users, nver, mSt, trash, flags>>
MAnswer(kind) ==
    /\ pc = Idle /\ mgEv.t # 0
    \* rej: the latest up_to a reject gave, which RejectedCt reads
    /\ mAns' = [k |-> kind, up |-> mgEv.t, t |-> now, w |-> mgEv.w,
                rej |-> IF kind = "reject" /\ mgEv.t > mAns.rej THEN mgEv.t ELSE mAns.rej]
    /\ mDayT' = IF mgEv.w = "day" THEN now ELSE mDayT
    /\ mgEv' = [t |-> 0, w |-> "w"]
    /\ mSt' = Answered(mSt, mgEv.t, IF kind = "confirm" THEN "allowed" ELSE "refused")
    /\ UNCHANGED <<dHas, dVer, dCt, eHas, eVer, upD, rD, rE, fv, conflict, jobs, cg, gEv, dAns, mEv, pc, snap, walkedD,
                   trip, mc, md, now, users, nver, dSt, trash, flags>>
MConfirm == "mconfirm" \in AnswerActs /\ MAnswer("confirm")
MReject == "mreject" \in AnswerActs /\ MAnswer("reject")

Next ==
    \/ \E f \in Files : UDel(f) \/ UBack(f) \/ UMod(f) \/ UEdit(f)
    \/ UDrop \/ UMount
    \/ PWalk \/ PGuard \/ PDecide \/ PWorkStart \/ PEnd \/ PStop
    \/ \E j \in jobs : PWork(j)
    \/ DConfirm \/ DReject \/ MConfirm \/ MReject

Spec == Init /\ [][Next]_vars

(* The passes go on, each to its end; D comes back; and the user answers each guard that asks. *)
LiveSpec ==
    /\ Spec
    /\ WF_vars(PWalk) /\ WF_vars(PGuard) /\ WF_vars(PDecide) /\ WF_vars(PWorkStart) /\ WF_vars(PEnd)
    /\ WF_vars(\E j \in jobs : PWork(j) /\ jobs' # jobs)
    /\ WF_vars(UMount)
    \* an answer waits for the pool's lock, which each pass takes: it gets it between two
    /\ SF_vars(DConfirm \/ DReject) /\ SF_vars(MConfirm \/ MReject)

-----------------------------------------------------------------------------
TypeOK ==
    /\ pc.ph \in {"idle", "walked", "decide", "work"}
    /\ \A f \in Files : rD[f].st \in {"ok", "gone", "stale", "none"} /\ rE[f].st \in {"ok", "stale", "none"}
    /\ flags \subseteq {"trashed-held", "trashed-refused", "overwrote-held", "overwrote-refused", "gone-not-deleted"}

(* §15.6: no copy goes to the trash of another disk for a deletion while the mass-deletion guard holds it, nor ever
   for a deletion a reject answered; none is overwritten for a change the mass-modification guard holds, nor for one a
   reject answered. *)
(* §15.6: what goes unasked stays within the thresholds: the deletions decided within the window since the last answer,
   the modifications decided within the window, and within the day, since the last answer to a hold of each. *)
DelBounded == Cardinality({e \in cg : e[1] > now - DelWin /\ e[2] > UpTo}) <= DelFiles
ModBounded == ModCount <= ModFiles
DayBounded == DayCount <= DayFiles

NoTrashHeld == "trashed-held" \notin flags
NoTrashRefused == "trashed-refused" \notin flags
NoOverwriteHeld == "overwrote-held" \notin flags
NoOverwriteRefused == "overwrote-refused" \notin flags

(* Liveness: once the user stops acting and answers each guard that asks, the passes settle: nothing gone left
   undecided, no job left, no guard standing, every change on D decided. A pass ends of itself once its jobs are run;
   it may stop short MaxStops times, as the daemon stopped; D, unmounted, is mounted again. *)
Settled ==
    /\ jobs = {} /\ gEv = 0 /\ mgEv.t = 0
    /\ \A f \in Files : rD[f].st # "gone" /\ (conflict[f] \/ ~dHas[f] \/ rD[f].st # "ok" \/ dVer[f] = rD[f].ver)
EventuallySettled == <>[]Settled
=============================================================================
