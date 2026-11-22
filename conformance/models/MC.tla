--------------------------------- MODULE MC ---------------------------------
(* The instances TLC checks: the .cfg files of this folder give the constants of Replication.tla, and take the
   functions below by name, as a .cfg file holds no tuple. *)
EXTENDS Replication

PhysTwo == <<1, 2>>               \* two disks, each on a physical disk of its own
PhysShared == <<1, 2, 2>>         \* disks 2 and 3 share a physical disk (R-06): a file has one copy there at most

StartOne == <<{1}>>               \* one file, its one copy on disk 1: to be replicated
StartTwo == <<{1, 2}>>            \* one file at its level of two copies, on disks 1 and 2
StartTwoFiles == <<{1}, {1, 2}>>  \* a file to replicate, and one at its level
StartNone == <<{}>>               \* a path with no file yet: Atoll's own view makes one there
=============================================================================
