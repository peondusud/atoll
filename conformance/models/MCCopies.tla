------------------------------ MODULE MCCopies ------------------------------
(* The instances of Copies.tla: what a .cfg file cannot hold. Disks 1 and 2 are disks with a spindle, one copy at
   once, each full past min_free with the file it holds; disk 3 is an SSD, two copies at once, with room for two
   copies past min_free. File 1 is on disk 1, file 2 on disk 2, each of one unit, each wanting one replica, which only
   disk 3 can take: two copies at once to one disk, from two disks. *)
EXTENDS Copies

SizeOne == <<1, 1>>
CapThree == <<2, 2, 3>>
CapTwo == <<2, 2, 2>>             \* the SSD with room for one copy past min_free: the other file waits
MinFreeOne == <<1, 1, 1>>
LimitSSD == <<1, 1, 2>>
StartApart == <<{1}, {2}>>

\* One file at three copies on three SSDs, each two copies at once, with room for its refresh beside its entry in the
\* trash: a user's write into one copy makes the two others stale, sent to the trash, then refreshed from it, two
\* copies of one file to two disks, from one; and one file on one SSD wanting two replicas, two copies of it at once
\* to two disks, from the same source.
SizeUnit == <<1>>
CapRoom == <<3, 3, 3>>
LimitFlash == <<2, 2, 2>>
StartAll == <<{1, 2, 3}>>
StartFirst == <<{1}>>
=============================================================================
