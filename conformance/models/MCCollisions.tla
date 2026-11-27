---------------------------- MODULE MCCollisions ----------------------------
(* What Collisions.tla's instances name and a .cfg file cannot hold: each file's path on S at the start, and the copies
   N holds. Folders: Films is 1, films 2, Other 3; file names: a is 1, A 2, the same key when SameNames. *)
EXTENDS Collisions

\* Films/a on S alone, Other/a on S and N.
PathsTwo == [f \in {1, 2} |-> IF f = 1 THEN <<1, 1>> ELSE <<3, 1>>]
OnOther == {2}
\* The same, and a third file the view may make.
PathsView == [f \in {1, 2, 3} |-> CASE f = 1 -> <<1, 1>> [] f = 2 -> <<3, 1>> [] OTHER -> <<0, 0>>]
\* Films/a on S alone; a second file the view may make.
PathsOne == [f \in {1, 2} |-> IF f = 1 THEN <<1, 1>> ELSE <<0, 0>>]
OnNone == {}
=============================================================================
