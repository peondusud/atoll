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
=============================================================================
