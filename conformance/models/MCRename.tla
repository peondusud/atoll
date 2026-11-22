------------------------------ MODULE MCRename ------------------------------
(* The instances of Rename.tla: what a .cfg file cannot hold. *)
EXTENDS Rename

BNone == <<"none", "none">>            \* no disk holds the folder B at the start
StartBoth == {1, 2}                    \* the file at A/x on both disks
=============================================================================
