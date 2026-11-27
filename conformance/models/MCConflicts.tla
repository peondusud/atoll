---------------------------- MODULE MCConflicts ----------------------------
(* What Conflicts.tla's instances name and a .cfg file cannot hold: the version each disk's copy holds at the start. *)
EXTENDS Conflicts

\* Version 1 on disk 1, version 2 on disk 2.
VerTwo == [d \in {1, 2} |-> d]
=============================================================================
