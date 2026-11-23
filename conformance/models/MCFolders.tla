------------------------------ MODULE MCFolders ------------------------------
(* The instances of Folders.tla: what a .cfg file cannot hold. A path is <<folder, name>>: the folders D, E and F are
   1, 2 and 3, the names x and y 1 and 2; <<0, 0>> is no file. *)
EXTENDS Folders

\* D/x on both disks, D/y on disk 2 alone, which follows the folder; a third file id for a user's new file.
JobPath == <<<<1, 1>>, <<1, 2>>, <<0, 0>>>>
JobOn == <<{1, 2}, {2}, {}>>
\* The folder D on both disks.
DirsD == <<{1}, {1}>>

\* D/x on both disks alone.
OnePath == <<<<1, 1>>>>
OneOn == <<{1, 2}>>

\* D/x and D/y on both disks: a chain or a cycle of two, by the names they swap or pass on.
TwoPath == <<<<1, 1>>, <<1, 2>>>>
TwoOn == <<{1, 2}, {1, 2}>>

\* D/x and E/x on both disks: two folders whose names a user swaps, their files then each at the other's name.
CrossPath == <<<<1, 1>>, <<2, 1>>>>
CrossOn == <<{1, 2}, {1, 2}>>
DirsDE == <<{1, 2}, {1, 2}>>
=============================================================================
