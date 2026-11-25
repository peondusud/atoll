----------------------------- MODULE MCViewNames -----------------------------
(* What ViewNames.tla's instances name and a .cfg file cannot hold: the files at the start, where each is, and the
   folders of each disk. Paths are <<folder, name>>: D is 1, E 2; x is 1, y 2. *)
EXTENDS ViewNames

\* D/x on both disks, and the folder D on both.
OnePath == [f \in {1} |-> <<1, 1>>]
OneOn == [f \in {1} |-> {1, 2}]
DirsD == [d \in {1, 2} |-> {1}]

\* D/x and D/y on both disks: a replacement of one onto the other.
TwoPath == [f \in {1, 2} |-> <<1, f>>]
TwoOn == [f \in {1, 2} |-> {1, 2}]

\* D/x on both disks, and D/y on disk 2 alone: a folder renamed, disk 2 holding a file the other lacks.
AlonePath == [f \in {1, 2} |-> <<1, f>>]
AloneOn == [f \in {1, 2} |-> IF f = 1 THEN {1, 2} ELSE {2}]
=============================================================================
