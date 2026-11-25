----------------------------- MODULE MCNested -----------------------------
(* What Nested.tla's instances name and a .cfg file cannot hold: the folders of each disk at the start, the files and
   where each is, and the names each disk refuses. Names: D is 1, E 2; a path is a sequence of them. *)
EXTENDS Nested

\* D and E on both disks, D's file on both.
DirsDE == [d \in {1, 2} |-> {<<1>>, <<2>>}]
\* D alone on both disks.
DirsD == [d \in {1, 2} |-> {<<1>>}]
\* D with D/E below it on both disks, each with its file.
DirsNested == [d \in {1, 2} |-> {<<1>>, <<1, 2>>}]

\* One file, in D, on both disks.
FileD == [f \in {1, 2} |-> IF f = 1 THEN <<1>> ELSE <<>>]
OnD == [f \in {1, 2} |-> IF f = 1 THEN {1, 2} ELSE {}]
\* A file in D and one in D/E, on both disks.
FilesNested == [f \in {1, 2} |-> IF f = 1 THEN <<1>> ELSE <<1, 2>>]
OnNested == [f \in {1, 2} |-> {1, 2}]

\* No disk refuses a name; disk 2 refuses E, as NTFS refuses a name with ":".
RefuseNone == [d \in {1, 2} |-> {}]
RefuseE2 == [d \in {1, 2} |-> IF d = 2 THEN {2} ELSE {}]
=============================================================================
