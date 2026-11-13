# Done

Finished work, newest first. Each item names its commits; unless it says otherwise, they are on `claude/funny-allen-jyq7pa` (pull request #2). Open work is in [TODO.md](TODO.md), lessons in [LESSONS.md](LESSONS.md).

## Two decisions of the review, made by the maintainer (specs.md §10, §15.10, §23, §25)

Commit "Take a name that refuses Atoll for a name, and a work folder of another account for what it is", on `beta`. The review left both as decisions; the maintainer made them on 2026-11-13, each from options set out with what it costs in security, data, performance, function and product.

- **A name that refuses the account that asks is a name the walk could not reach.** `EACCES` from the `lstat`, the `openat`, the `readlink` or the listing of a name below the root stopped the walk as a disk that fails does: the mount of another user below `media/`, which answers so whatever the rights of who asks, took its whole disk out of the view at every pass, and whoever can write under `media/` can make one. Such a name is now reported in `unreadable`, as `ENOENT` and `ELOOP` are: nothing is concluded of it or of what is below it, and the rest of the disk is walked. The listing of `<root>` itself refusing is still the disk's. A test pinned the old answer.
- **And an alert tells of those names**, `no-access:<id>`, with their count and the first ten: what is below them is no longer copied nor verified, and a rights problem must not stay silent. Only the names that refuse the account count, not every unreadable one: one that went during the walk is there again at the next, and would be an alert for nothing.
- **`disk add` refuses a work folder that another account owns**, on ext4, XFS and btrfs: neither root's, the command's nor the catalog's owner's. Whoever makes `<workdir>` first makes its `tmp/`, `links/` and `trash/`, reads what Atoll copies through them and removes what the trash holds. It says the `chown` that gives the folder back, as for a disk that comes from a machine where Atoll's account has another number (`conformance/scenarios/disk-add-foreign-workdir.json`).
- **After that the daemon tells of it and goes on**: `workdir-owner:<id>`, an alert once a pass finds a mounted disk's work folder to be another account's. Nothing becomes read-only by itself: the option that did was set aside, as a disk moved between two machines would stop taking copies with nothing but an alert to say so. NTFS is never asked, as its names have the owner their mount gives.
- Two new kinds of alert, one more refusal of `disk add`, and no change to any `--json` output: a pass's line of a disk holds the count and the first names for the alert only.

## What a review of the code found (specs.md §9, §15.1, §15.13, §21.5, §23, §25.1)

Commits "Find an entry of the trash through an index", "Give the remount its time, and a check its time before the next", "Give up an answer of the local API nobody waits for", "Follow no redirect with a token", "Keep what a process is for 10 s, not for good", "Close the catalog's folder to other accounts" and "Start each measured peak from what the process keeps, and hold a long soak's window", on `beta`, after a review of the code for performance, security and conformance to the spec. Each fix came after a test that fails without it.

- **The trash read whole, again and again.** No index held its entries but by expiry. Each purge deleted its row by disk and place, each undone operation and each refresh of a stale copy looked for theirs by operation, and each read every entry to find its own: a purge of every entry of a large trash read the whole of it once for each. The list sorted the whole trash for each page of 1,000 entries. And after every pass the daemon loaded every entry, paths included, to ask whether a purge was due. `0010_trash_by_path.sql` indexes the entries by path, then by operation; each statement that finds one names its path, which its operation holds; the list reads each page in the index's order; and `PurgeDue` reads what the choice needs of each entry, with no path and no sort. `TestATrashQueryReadsOnlyItsEntries` pins the seven plans.
- **A remount with no limit of time.** After a USB reset the daemon ran `umount -l`, then `mount`, with the daemon's own context. On a disk that died halfway a `mount` can wait for good, and the daemon's only loop with it. Each of the two now has 90 s, then is killed with the helpers it started (`TestRemountGivesUp`).
- **A check due again as soon as it ended.** The time of the next mount check was counted from the start of a check. One that took longer than the 30 s between two, as when a remount waited for its disk, was due again at once, and a check comes before every pass: no pass ran. It is counted from the check's end (`TestACheckLeavesItsTimeBeforeTheNext`).
- **Answers of the local API nobody waited for.** `/v1/status`, `/v1/trash` and `/v1/guards` read the catalog with a context of their own, so a client that was gone, or whose 60 s were over, stopped nothing; and any number were made at once, in the daemon's own process, where `/v1/status` plans the whole pool when a strict setting exists. Each answer now takes its request's context, with its 60 s, and those that read the catalog are made one at a time. A connection that asks nothing more is closed after 60 s: one left open for good, which a client with no token could do, held a descriptor the daemon needs for its disks.
- **Redirects followed with a token.** The requests to ntfy, Gotify, Plex, Emby and Jellyfin went through Go's default client, which follows redirects and, to another host, drops only `Authorization` and `Cookie`: `X-Plex-Token`, `X-Emby-Token` and `X-Gotify-Key` went to whatever host an answer named. A `POST` redirected with 301 or 302 also becomes a `GET`: an alert sent that way reached nobody, was answered 200 and counted as sent. No redirect is followed now, and one is an answer that is not 2xx.
- **What a process is, kept for good.** The watcher kept what the process behind an event was, by its number, until it knew a thousand. The kernel gives the number of a process that ended to another: after a command of Atoll that wrote to a disk, a program that took its number had its changes counted as Atoll's own, which bring no pass. The answer is kept 10 s (`TestOriginIsAskedAgain`).
- **The catalog readable by every account.** The unit left `/var/lib/atoll` at 0755, and the catalog and its backups are made 0644: every account could read every path of the library, its trash and its history, which the local API gives only through its socket or with its token. `StateDirectoryMode=0750` closes the folder.
- **Two faults of the measures.** `TestScale` started a step's peak from what the step before had left resident, so a light step logged the peak of the one before it; and the soak's maintenance window, written from its two ends as times of day, was 3 hours a day for a run of 72 hours, where the run of 4 hours had it all along.

- **Eight more reads of the code, by area, and what is fixed of what they found.** Each fix came after a test that fails without it; what is still open is in TODO.md.
  - **A new folder's mode set by its name** ("Set a new folder's mode and owner on the folder itself"). `Adopt` used `fchmodat`, which follows a symbolic link. Whoever owned the parent could trade the new name for a link between the making and that call: the mode, taken from a folder that same user made, setuid bits included, went to whatever the link named. With `CAP_FOWNER`, a user of the library could make any file of the machine setuid. The folder is now opened without following a link, must hold nothing, and takes its owner, then its mode, through that descriptor. Three of the eight reads found it.
  - **Two bytes of an NTFS boot sector stopped the daemon** ("Refuse two geometries of an NTFS boot sector that stopped the daemon"): 2^63 sectors a cluster read as a negative count, then a division by zero; a record of 2^-128 bytes, then a shift by a negative count. Two vectors in `ntfs_dirty.json`.
  - **A FIFO or a socket at a copy's name lost its disk** ("Sync only a file or a folder by its name"): the sync after a move failed on it as on a failing disk, at each reprise. `TestSyncEntry` pinned the old answer for a FIFO.
  - **A disk's marker read by its path** ("Read a disk's marker as a marker and nothing else"): a link was followed, a FIFO held `disk add` with the catalog's lock, and a file of any size was read whole, also by the daemon for `/v1/disks/{id}`.
  - **The configuration written through `<file>.new`** ("Write the configuration through a file made for it alone"): a draft or a link at that name was written through, then renamed over the configuration.
  - **A scrub that wrote over a copy never verified** ("Call a copy never verified that does not match a conflict"). The scrub called any copy that did not hold its file's content `corrupt`: with `auto_repair`, on by default, it wrote the file's recorded version over it, and without it sent it to the trash. A copy never verified, as one that came with a disk added, was put back from the trash, or was renamed by someone else, may hold another version of the file: §15.12 says of such a copy that a later mismatch is "a `conflict`, never `corrupt`". `TestScrubCallsACopyNeverVerifiedAConflict` showed the second version rewritten from the first. Such a mismatch now makes the file a `conflict`, nothing written, and the copies of a file in conflict are not read again while it lasts.
  - **A disk that may only be read was written to** ("Write nothing to a disk that may only be read when a copy is corrupt"). The scrub, and `atoll repair`, repaired a corrupt copy in place on a `readonly` disk, or moved it to that disk's trash, making its work folder on the way: nothing asked whether the disk may be written, as every other writer does. `Send` and `RepairChunks` now answer `disk-unavailable` for such a disk, whoever calls them, and the scrub says of its corrupt copy that it is kept, with or without `--yes`.
  - **A scrub without `--yes` resolved what a crash had left** ("Resolve nothing in a scrub that was not told --yes"). Its session was opened to write, so that its disks were confirmed, and so ran the reprise and cleaned `tmp/`: a move to the trash left planned was finished or undone on the disk by a command that had not been told it may write, against §17.2. A session can now be opened to read through confirmed disks and write to none.
  - **A source that failed late lost the target** ("Lose the disk that failed, not the one a copy goes to"). Once a copy is read and verified, the folder it goes in is made like the source's, which is looked at then. An error there was taken for the target's: the healthy disk a copy went to was set `missing` for the run, every other job for it ending `disk-unavailable`, with the operation left planned, and the failing source stayed in service. The same when a file is put back from a trash to another disk. Both now lose the disk that failed, and undo the operation.
  - **A repair that failed left its corrupt copy in `media/`** ("Quarantine the copy a repair could not mend"). The scrub sent a corrupt copy to the trash after a repair that found no room, and after any other failure listed it as kept: it stayed in `media/`, where mergerfs serves it when its branch comes first, and no scrub reads a corrupt copy again. §16 sends it to the trash "when the repair fails". It now goes after `io-error` too, its row read again first (`TestARepairThatFailsQuarantines`). Three more things came out of the test:
    - a write that the disk drops, the copy holding what it held with new times, was taken for someone else's change by the run that had just written it: the row kept its old times, so the move to the trash, and every later repair, found a changed copy and left it (`TestARepairWhoseWriteIsDroppedBeginsAgain`, `TestARepairWhoseWriteIsDroppedQuarantines`);
    - the healthy copy's disk failing when the repair opened it lost the disk being repaired, whose operation stayed planned, and left the failing disk in service (`TestAHealthyDiskThatFailsIsTheOneLost`), as a source that failed late did in a copy;
    - and the general case, a corrupt copy that anyone touches, is a gap between §15.4 and §16.1, written in TODO.md as a decision.
  - **`balance` sent a copy to the trash with none made in its place** ("Send an old copy to the trash only once the one that replaces it is there"). With strict domains the plan keeps a copy still to make for the domain it is to cover, and marks an old copy `surplus` for it. `balance` sent any misplaced Atoll copy marked so to the trash, on that mark alone: when the copy's job found no room, the file was left with two copies of three, and the third in the trash (`TestBalanceRetiresNothingForACopyStillToMake`). Nor did it ask that the file is not in `conflict`, or that the copies that stay are verified. The old copy now goes only as `atoll prune` would send it (§17.3), and the next run sends it once the copy is made.
  - **A stale copy that lost its trash job stayed stale for good** ("Give a stale copy its trash job again"). Only a modification queues the job that sends a stale copy to the trash and has it made again, and three decisions drop every trash job of a file, that one included: a rename seen after a deletion, a guard rejected, a deletion that no longer stands. A film edited while a disk was away, then renamed: when the disk came back its copy followed the rename and kept the version that came before, in `media/`, where mergerfs serves it when its branch comes first, and no replica of the current version was ever made (`TestAStaleCopyRenamedWhileAwayIsMadeAgain`, `TestAGuardRejectedLeavesAnOverwrite`). Each pass now gives such a copy its job again before it runs anything, when the job would stand, and never to a copy its look found changed, which would be tried at every pass (`TestAStaleCopyThatChangedGetsNoJobAgain`).
  - **The copy that replaces a stale one waited for the window** ("Make the copy that replaces a stale one whatever the window"). A file in `scheduled` mode edited in the morning had its other copy sent to the trash at once, and the copy made in its place waited for `replication.window`, the next night: nothing told that job from any other copy of the file. §12.3: "Refreshing stale copies ignores the mode and runs as soon as possible". The job now says what it is (`conformance/scenarios/daemon-scheduled-refresh.json`, `TestARefreshOfAScheduledFileWaitsForNoWindow`).
  - **A mail server could fill the daemon's memory, and a mail it took was sent again** ("Take only so much from a mail server, and a mail as sent once it has it"). A server's line is read whole, however long: one that never ends, from the server or from anyone on the way before `STARTTLS`, was held as fast as the link carried it for the 10 s a conversation has. A server may now send 256 KiB in a conversation (`TestSMTPTakesOnlySoMuch`). A mail the server had taken counted as not sent when its `QUIT` failed, as with a server that hangs up there, and was sent again at each try, against §23 (`TestSMTPAMailTakenIsSent`). And what a server says of a refusal was kept whole, for the log: its first 512 bytes are, as of an HTTP answer.
  - **File names written as they are to the terminal** ("Write a name to a terminal as a name, whatever it holds"). The human output of `scan`, `status --path`, `guard list`, `names check`, `daemon --until-idle`, `scrub`, `repair`, `dupes`, `conflicts`, `prune`, `trash`, `replicate`, `balance` and the disk commands wrote a path as it is when it is UTF-8: a file named with a line feed wrote a line of its own, as "moved to the trash: every film", and one with an escape sequence cleared the screen or recoloured what follows. Only the alerts wrote such a name in base64. Every path now goes through one rule, the alerts' (`TestANameWritesNoLineOfItsOwn`), and what a server answered to `atoll notify test` has its control characters replaced.
  - **What went during a walk took its disk with it** ("Take what goes during a walk for what it is"). A folder removed once the walk had opened it answers `ENOENT` to its listing, and a symbolic link replaced by a file once it was looked at answers `EINVAL` to `readlink`: each stopped the walk as a disk that fails does. The disk went `missing`, its branch left mergerfs, the next check brought it back, and whoever can write under `media/` could do it again at every pass. Both are now names the walk could not reach, reported in `unreadable`, as §15.10 says of what is removed or replaced during a walk (`TestWhatGoesDuringAWalkSaysNothingOfTheDisk`, which really removes them at that moment). `EACCES` from a mount below the root is still a disk that fails, which a test pins: a decision, in TODO.md.
  - **A private folder deleted whole came back open** ("Keep a folder that went private in the trash"). With the link guard, the data of a file deleted through the view goes to the trash from its link, under folders that stand for those of its path. A folder that had gone with its files took the mode and owner of the last folder on the way that was still there: `media/alice`, 0700, deleted whole, stood in the trash as 0755 like `media/`, its files readable by every account for as long as the trash kept them, and `trash restore` made it again 0755. A folder that is gone, and every one below it, is now made 0700 with the owner of the file (`TestAFolderThatWentIsPrivateInTheTrash`).
  - **A links folder found open was used as it was** ("Close a links folder found open"). Each link reaches a file whatever the folders above it in the root forbid, which is why Atoll makes `links/` 0700. One that was there already, open, kept its mode: every guarded file whose own mode lets others read it was then open to all, private folder or not. It is closed before a link is made in it, as the folder of the parts is (`TestTheGuardClosesALinksFolderFoundOpen`).
  - **A disk's copy of the catalog removed before the new one was in place** ("Put a disk's copy of the catalog in place in one step"). `PutCatalog` removed `catalog.db`, then renamed the new one without replacing anything: a failure in between left the disk with no copy, and on ntfs-3g and lowntfs-3g, which refuse such a rename whatever the names, every backup did, leaving only `catalog.db.new`. It is one rename that replaces, as §9 says (`TestPutCatalogReplacesInOneStep`). For the same reason `disk add` never wrote a disk's marker on those drivers: it now moves it as every move is made, by a link where the rename is refused (`TestWriteMarkerOnNTFS3G`). And a backup that stopped, for want of room or by a crash, left its `.part` on the system disk, which only a backup of the very same second removed: each backup removes its own on failing, and those left before it (`TestABackupLeavesNoPart`).
  - **A folder left half made came back the daemon's own** ("Give a folder left half made the owner it was made for"). A folder made below the root, for a restore, a copy or a rename, is made, then given the mode and owner of the folder it stands for. A disk that failed in between, or a crash, left its operation planned, and the reprise finished whatever folder it found: nothing journaled what it was to take. A private folder of someone's came back owned by the daemon, closed to them on that disk. The operation's event now holds the mode and owner, and the reprise gives them to a folder that has not got them yet (`TestAFolderHalfMadeTakesItsOwnerAtTheReprise`). The folder of the trash is looked at before the operation is journaled, where it was after: a look that fails journals nothing.
  - **A reminder waited for a pass that nothing brought** ("Come back for an alert when it is due"). Alerts are worked out after a pass, and nothing brought a pass for them: on a pool where nothing changes, a disk gone missing was told of once, and its reminder of every 24 h waited for the next rescan, a week later with `rescan_every = "7d"`. An NTFS volume found dirty at a mount check brought none either, so its alert waited for a pass that had nothing to do with it. What a pass leaves now says when the next reminder is due, and the daemon passes then; a volume found dirty brings a pass at once, as a reading of SMART that tells of trouble does (`TestPrepareSaysWhenToRemind`, `TestWhatBringsAPassForAnAlert`).
  - **A window slept through on the night the clock springs forward** ("Open a window at the instant the clock jumps to"). A window's start that such a night skips is, by §29.5, the first instant after it. The code left that to `time.Date`, which for a wall-clock time that does not exist gives one on either side of the jump: the hour before it west of UTC, which is outside the window, so the daemon waited for the next night; and, east of UTC, as long after the jump as the start was into the skipped hour, 03:30 for a start at 02:30. With `timezone = "America/New_York"` and the sample window, no maintenance ran on that night. The start is now the instant the clock jumps to. `window.json` has eleven cases in named zones, with a new field `zone`: its instants had a fixed offset, where no clock ever jumps.
  - **Two faults of the plan** ("Take a path a copy still sits at for taken, and a copy canceled for none"). A copy that has not followed a rename still sits at its old path on its disk. The plan took that path for free there: the replica of another file that came at that path was sent to that disk, ended `conflict`, and was sent there again at each plan, with room on a third disk, for as long as the rename stayed stuck. The path is taken, and so is any below it. And a file's count of the copies it reaches took in a pending copy the plan had just canceled: beside a copy on a disk away, a file with one copy that can be read was not `degraded` until the next plan. Three cases in `plan.json`.
  - **Five small faults of the commands**, each with its test.
    - `atoll disk bogus --help` wrote the help of `disk` and exited with 0, where §25 makes an unknown subcommand a usage error ("Refuse the help of a subcommand its command does not have").
    - A marker that said the same as the one `disk add` would write, with other line ends or its keys in another order, was left as §10 says, then written over all the same, which is refused: the disk was added with a warning and exit 4, for a marker that was right. And a marker took the grammar of a time zone for its `pool_id`, so that it read and wrote ids no configuration can hold ("Leave a marker that says the same, and take a pool's id for an identifier").
    - A disk command did not find a daemon whose executable had been replaced on disk since it started, as an upgrade leaves it: it said that none ran, exited with 0, and the daemon kept its configuration. And a `SIGHUP` that came while the daemon read its configuration for the first time ended it, as it listened only after ("Tell the daemon after an upgrade, and let it hear a hangup as it starts").
  - **Replayed on loop devices, in qemu, on Linux 6.12 and 7.3.** The whole of `test/loopdev` passes with every fix above but the one of the reload, and the daemon's and the watcher's tests pass with that one too. `TestDaemonHearsADiskMountedAgain`, new, fails there without the fix it is for. Nothing had run those tests since the review's fixes began, as the full check has no loop device: the first run found the two tests above, and nothing else.
  - **A configuration read again kept the debounce and the view of the one before** ("Take the debounce and the view of a configuration read again"). On `SIGHUP` the daemon took the new configuration for everything but what it had made from the old one as it started: the grouping kept its `deletes.debounce`, and the watcher its `view.mount`, by which it tells the view's own changes from a user's. Both follow the configuration now, as §15.13 says of one that loads.
  - **A file still being written read whole at each slice of the hashes** ("Compare a copy with its row before reading it"). The reading of a file's hash looked at the copy, read it from end to end, then compared what it had looked at with the copy's row, and left the copy aside when they differed. A file still being written, a download of tens of gigabytes, was so read whole at each slice of the hashes, every 30 s under the catalog's lock, which a slice is there to hold for 30 s, until a pass recorded it again. The comparison comes first: such a copy is not read. A scan's `hashed` counts the copies read, so no longer that one.
  - **Every chunk a repair saved held in memory at once** ("Look at the chunks a repair saved one at a time"). A repair that ends, or the reprise of one a crash left, compares each chunk of the copy with what was saved of it. Every saved chunk was read whole first, and kept: 4 MiB each, so gigabytes for a copy with many bad chunks, in a daemon that is to stay under 200 MB. Each is now read in its turn through one buffer, and only its size and its hash are kept, which is all the comparison needs. A file of that folder larger than a chunk is not read at all.
  - **Every copy of a missing disk read for each folder to tell a media server of** ("Read the files of a folder, not the copies of a disk away"). While a disk is missing, the daemon holds back the scans of the folders that have a file on it, and asks the catalog for each folder at each pass. SQLite, with no statistics, led that question by the disk: every copy of the missing disk was read for each folder that holds none of them, 200,000 lookups a folder on such a disk. The files of the folder are read first, by the range of their paths, as the other questions below a folder are (`TestQueriesBelowAFolderReadOnlyWhatIsBelowIt`).
  - **A scrub woke every disk, each time** ("Leave asleep a disk the scrub has nothing to read on"). A scrub confirmed each disk before it asked the catalog what was due on it, and a confirmation reads the disk's head past the cache, which wakes it. The daemon scrubs by slices of 30 s, each from the first disk on: every disk before the one being read was read so twice a minute all night, and a night with nothing due woke them all once. The catalog is now asked first, and a disk with nothing due is not opened.
  - **Two filesystems on one disk, one of them on a partition of an array, were both taken** ("Follow a partition down to the disks of what it cuts"). The disks a filesystem lives on are followed down through device-mapper and md devices (R-06). A partition gave the name of the device it cuts and stopped there: a filesystem on `md0p1`, where `md0` is made of `sda1` and `sdb1`, lived on "md0", and one on `sda3` on "sda", so `disk add` took both in one pool, two copies of a file then on one physical disk. A partition is now followed down through its device like any other.
  - **Two tests on loop devices failed since the scrub tells rot from another version** (commit "Call a copy never verified that does not match a conflict", of earlier that day). `TestScrubFindsRot` and `TestRepairOnEachDriver` made their copies due by forgetting when they were verified, which now makes a copy that differs a conflict: they make them verified long ago instead. Nothing ran them in between, as they need qemu: the full check has no loop device.
  - **The configuration open to all, and a URL in its errors** ("Close a new configuration to other accounts, and keep a URL out of its errors"). `atoll init` made the configuration 0644 whatever the mask, where the spec's own example keeps the topic of a ntfy server in it, a secret on a public server, and where the catalog beside it is closed to other accounts: it is made 0640. And a value refused by the schema was quoted in the error, on standard error and so in the journal: a URL mistyped, with its topic, or one typed with a user and a password. A value the schema marks as one that may hold a secret is no longer shown; the key is, and any other value still is.
  - **A run read the clock once** ("Read the time as a run goes"). A pass or a command took the time as it began and judged everything by it. A pass begun a minute before `replication.window` closed made every scheduled copy in the queue, for hours past the window that is there to keep the disks quiet by day. A copy dropped as `changed` hours into a run was to be tried again `settle` after the run began, a time already past, so the next pass read the file again at once. And a copy made hours into a run was recorded as verified when the run began. The window is now judged as each copy is about to start, and the two times are read when they are written. `atoll balance` already read the clock as it went.
  - **Two faults of the disk commands on the configuration** ("Edit the configuration a disk command finds under the lock"). `disk add`, `disk remove`, `disk fail` and `disk replace` read the configuration, then waited for the catalog's lock, then edited the text they had read: the second of two run at once edited the text of before the first, and failed with "the file changed since it was read", having changed nothing. Each now reads the file again once it holds the lock. And a failure to sync the folder after the new file had its name was reported as "the configuration is unchanged", of a configuration just changed, the command stopping there with the catalog and the daemon not told: it is now a warning, `config-not-synced`, and the command goes on, to exit with 4.
  - **A folder that appeared was opened by its path** ("Open a folder that arrives from the root"). The watcher marks a folder made or moved in, and lists it, some time after the event that told of it. It opened it by its path below the root: with a folder above it traded for a symbolic link meanwhile, by whoever can write in the library, the mark went on a folder elsewhere on the machine, and what that folder held was told of as new files of the library, which a media server is then asked to scan. No decision of a pass rests on an event, so nothing moved for it. The folder is now opened from the root, one name at a time, following no link, and is taken only on the root's mount. Tested with inotify; the fanotify marks folder by folder share the code, and are tested on ntfs-3g in qemu (`TestWatcherOnEachDriver`).
  - **`atoll capacity` counted no room for the copies waiting in the queue** ("Take the copies yet to make off the room a pool has left"). The capacity reachable is worked out from the free space the plan leaves, and only the copies that plan would queue came off it. A copy already queued and not yet made, which the plan keeps, takes its room as well (§13.4), and counted for none: with 100 GB of copies waiting for their window, the pool was said to hold up to 100 GB more than it will.
  - **A folder renamed waited for a copy that was never there** ("Hold a folder renamed for no copy yet to make"). A folder renamed on a disk is acted on once no copy of that disk still sits below its old name. A copy queued for that disk and not yet made, of a file another disk holds there, counted as one: the rename reached no other disk for as long as the copy waited, for its window, for room or after an error, and the copy, once made, brought the old folder back beside the new one. Such a copy is nowhere on the disk, and no longer counts: the rename goes on, the file follows it on the disk that holds it, and the copy is made at its new path.
  - **A walk of another folder passed for the disk** ("Take a walk only for the disk it read"). A walk opens `<root>` by its path, some time after the mount check that found its disk, minutes on a large pool. A disk unmounted in between leaves its mount point, and a folder at the root's path there, as a download client makes when its target is away, was walked for the disk: every file absent. With the disk mounted again by the time those absences were confirmed, the confirmation passed, the absences were recorded, and the other copies went to the trash as deletions, under the guard's counts. Once the walk is over, the mount check must now hold again, and the mount path and the root's path must lead to the mount and the folder the walk held open; otherwise the walk says nothing, as one an error stopped.
  - **A disk that stopped being heard was never watched again** ("Watch again a disk whose watch ended"). The daemon kept a disk for watched from the moment its watch started until the disk went `missing`. A watch that ended by an error said so once, with an overflow, and that was all. And a disk unmounted and mounted again between two mount checks, which find it mounted both times, lost its watch without a word: the kernel drops the marks with the filesystem that goes. Either way nothing more was heard of that disk until the daemon started again, so that a file put on it waited for a pass brought by something else, a week at most (`maintenance.rescan_every`). The daemon now asks at each check whether each watch still hears its disk, watches again one that does not, and has the disk looked at whole. Not replayed on a device that fails under its mount: the test in qemu unmounts and mounts a disk, on XFS for the filesystem mark and on ntfs-3g for the marks folder by folder.
  - **What a repair left in `tmp/` stayed for good, and its names counted for nothing** ("Take the names of a repair for an operation's"). A chunk a repair saves goes through `tmp/` as `<op-id>.<index>.part`, and what cleans `tmp/` knew only `<op-id>.part`: a repair that stopped while it saved a chunk, for want of room or at a power cut, left up to 4 MiB there that nothing ever removed, once more each time a scrub tried again. Such a part now goes when a command that writes next opens the disk, once its repair is `done` or `undone`; and `tmp/` is cleaned again after the reprise, as §14.2 says, where it was cleaned only before it. A new catalog started its operations past the ids of `trash/<date>/<op-id>/` and `tmp/<op-id>.part` alone: after `catalog rebuild`, a repair could take the id of one whose `<op-id>.chunks` the trash still held, and find its chunk's place taken or count the other's chunks as its own. Those names count now, each read only as Atoll writes it, where a folder named `050` counted for the id 50; and a date of the trash that cannot be read is the warning of §25, where it was passed over without a word.
  - **The folder of the parts open to all** ("Close the folder of the parts, and give a part its owner before its mode"): a part carries its source's mode and owner while it is read again, in a folder made 0755. And the mode was set before the owner, which took the setuid and setgid bits off the copy of a file that has them.
### Decided without the maintainer, to confirm

- **A new folder is adopted only when it holds nothing**: one that does was put there in its place, and keeps its mode and owner; the caller logs it and goes on, as when a driver refuses.
- **What is neither a file nor a folder is not synced after a move** (§17.1): it is not what was moved there. A failed sync of a file or a folder is still a disk lost.
- **`<workdir>/tmp/` is closed to other accounts** (§14.2), one found open included; **a part takes its owner before its mode** (§14), and loses its setuid and setgid bits when the target refuses the owner.
- **What the scrub does with a copy a repair could not mend** (§16.1): it goes to the trash after `io-error` as after `no-room`, but only while its row still says `corrupt`, its disk can be written to, the healthy copy's disk can be read, and no operation on it is left `planned`, which is for the reprise. After `changed`, `name-refused` and `disk-unavailable` it stays where it is. **The run that stops a repair knows whether it wrote** (§16.1): a copy that holds what the repair found, with a change time that moved, is then "a write began", the time set back and the row taking the change time, where a reprise reads it as someone else's change; `conformance/vectors/repair_reprise.json` is unchanged. **The healthy copy's disk failing when a repair opens it** is the one lost, with `io-error`; a healthy copy that cannot be opened for another reason is one that changed.
- **`atoll balance` sends an old copy to the trash only as `atoll prune` would** (§13.3, §17.3): a move's `outcome` can now be one of §17.3's when the old copy stays, `below-level`, `unverified`, `conflict`, `corrupt`, `changed` or `disk-unavailable`, which `api/schemas/balance.schema.json` already allows, as any text; such a move counts among those that stayed, and the command exits with 4. Without `--yes`, a copy that would stay so is no longer said `to-move`.
- **A stale copy with no trash job gets one again at each pass** (§15.4), from the first copy `ok` or `surplus` by disk position, with the priority of its file's policy, under the conditions a job stands by (§15.12). **`schema/migrations/0011_copies_stale.sql`**, a new migration: `CREATE INDEX copies_stale ON copies(disk_id, file_id) WHERE state = 'stale'`, so that the question reads the stale copies and not every copy of the pool, at each pass.
- **The payload of a `replicate` job has a fourth key, `refresh`** (§9), `true` for the copy that takes the place of a stale one, and left out otherwise. A job queued by an earlier version has none, and waits for its window as before.
- **A mail server may send 256 KiB in a conversation**, its TLS handshake included, and **512 bytes are kept of what it says of a refusal** (§23).
- **A path with a control character is written in base64 in the human output** (§25), a tab included, as in an alert; the `--json` outputs are unchanged.
- **Two more answers make a name `unreadable` in a walk** (§15.10): `EINVAL` from `readlink`, for a name that is no longer a symbolic link, and `ENOENT` from the listing of a folder below the root, removed once it was opened. The root's own listing failing is still the disk's.
- **A folder that went with what it held is private in the trash** (§17.1): 0700, with the owner of the file the link guard sends, and so a restore makes it again private, also one that was open. Nothing records how open it was.
- **`atoll init` makes the configuration 0640** (§25), for its owner and its group, where it was 0644: an account that is neither, and has no capability to read past that, as the daemon's has in `deploy/atoll.service`, no longer reads it, as it no longer reads the catalog. A configuration that exists keeps its mode. **`schema/config.schema.json` uses one more keyword, `writeOnly`**, on its URLs (`$defs/http_url`): an annotation, which changes nothing of what loads, and says that no message shows the value.
- **A new warning of the disk commands, `config-not-synced`** (§10, §25), in `api/schemas/disk-add.schema.json`, which `disk replace` shares, and `disk-change.schema.json`: the configuration is written, and its folder could not be synced after. The command goes on and exits with 4, where it stopped with 1 and said the configuration unchanged. **A disk command gives up when the configuration names another catalog once it holds the lock** than when it started, with exit 1 and nothing changed: the lock it holds is the other catalog's.
- **A scan's `hashed` counts the copies it read** (§15.10): one that is no longer what its row says is not read, and no longer counted. **A disk on which a scrub has nothing due is not opened**, and is said `scrubbed` all the same, as before (§16): only a disk the mount check finds `missing` is one the scrub could not read.
- **A walk that read another folder than the disk's root is a walk stopped** (§15.10): its disk is `missing` with the reason `io-error` until the next mount check, also when the disk is mounted again by then; and each walk ends with one more mount check, answered from the caches, which wakes nothing.
- **How a watch is found to have ended** (§15.1, §15.13): at each mount check, 30 s apart, never sooner, so that up to 30 s of changes on a disk mounted again are found by the look at the whole disk rather than heard; a fanotify watch by the marks the kernel lists for its group in `/proc/self/fdinfo`, and taken for alive where that cannot be read; a watch folder by folder also by the device and inode of `<root>`. The look at the whole disk is an `overflow` grouped by the daemon itself, so its pass comes under the reason `events`: no new reason, and `conformance/vectors/daemon_wake.json` is unchanged.
- **The names of the work folder are read as Atoll writes them** (§9): in decimal, with no sign and no leading zero, in a new file of vectors, `conformance/vectors/work_names.json`. A folder `trash/<date>/007` or a part `+7.part` counted for the id 7 when a new catalog chose where its operations start, and counts for nothing now: Atoll writes no such name, so none of its own can meet one. **A chunk's part is named by its repair alone, whatever its index** (§14.2): `tmp/<op-id>.<index>.part` goes once the repair `<op-id>` of that disk is `done` or `undone`, with no look at the indexes its event holds. **A date of the trash that cannot be read leaves its disk's highest id unknown** (§9, §25), which `init` and `catalog rebuild` say as they do for a disk they cannot read at all.
- **A new kind of event, `mkdir`** (§9), written with the operation of a folder that stands for another, with the `mode`, `uid` and `gid` it is to take, which its reprise reads (§17.2). An operation journaled by an earlier version has none, and is finished as before.
- **A pass a day while an alert lasts** (§15.13, §23): the reminder of every 24 h brings its own pass, under the reason `due`, where it waited for one. The function that says when the daemon wakes, and its vector, are unchanged.
- **`conformance/vectors/window.json` has a new field, `zone`**, a name of the time zone database, for the cases about a change of offset: the Rust version needs that database to run them.
- **`atoll scrub --json` has a new list for each disk, `conflict`** (`api/schemas/scrub.schema.json`): the copies never verified that hold another version of their file. One found makes the command exit with 4, as a corrupt copy with no healthy one does. **The copies of a file in `conflict` are no longer due for the scrub** (§16).
- **`schema/migrations/0010_trash_by_path.sql`**, a new migration: `CREATE INDEX trash_by_path ON trash(lpath, op_id)`. One index rather than one by operation and one by place: with an index by operation alone, SQLite leads the join that sums each trash's sizes by the journal, which keeps every operation for good, and reads all of it (measured on its plan).
- **The next mount check is due 30 s after the one before ended** (§15.13), no longer 30 s after it started. The two differ only when a check is slow.
- **90 s for `umount`, then for `mount`**, at a remount (§15.13): what systemd gives a mount.
- **The local API** (§25.1): the answers that read the catalog one at a time, another waiting within its own 60 s; an answer given up with its request; 60 s for a connection that asks nothing more.
- **No redirect is followed** (§21.5, §23). A server reached through one, as behind a proxy that sends `http` to `https`, must be written by the address the redirect leads to: until then its alerts and scans fail, and say why.
- **What a process is stays known 10 s** (§15.1).
- **`StateDirectoryMode=0750`** in `deploy/atoll.service`: the catalog's folder is for `atoll`, its group and root. The process's mask is left as it is, as it would also reach the folders Atoll makes on the disks with no folder to make them like.

### Left as the spec says, for the maintainer to decide

- **Reading hashes stops its disk at an error that is not absence**, which §15.10 asks for: TODO.md says what that costs when one file cannot be opened.
- **One transaction per file hashed**, which §15.10 asks for too: TODO.md says what it costs on many small files.

## What the soak on 200,000 files found (specs.md §15.13, §26)

Commits "Hear the watcher between the daemon's steps, and give the others a turn after a pass", "Give each step of TestScale its own peak", "Look the copies of a disk up by inode through an index", "Measure the daemon's own steps at scale, each with its own peak" and "Record what TestScale measures on 200,000 files a disk", on `beta`, from the first runs of `TestDaemonSoak` and `TestScale` on 200,000 files a disk, in qemu without KVM.

- **No pass while hashes were read.** The daemon's loop heard the watcher and `SIGHUP` only while it slept. After the first pass on 200,000 files a disk, the 400,000 copies wait for their hash, read by slices of 30 s, each due as soon as the one before ends: the loop never slept. For 18 minutes, the soak's writer, a change every second, brought no pass at all, its events piling up in the watcher's queue. Each turn of the loop now begins with what the watcher and `SIGHUP` said since the last, taken without waiting. `TestRunListensBetweenSteps` fails without it: a `SIGHUP` waits for as long as checks come one after the other.
- **Nothing but passes, once they are heard.** On that pool, a pass walks 400,000 copies: about 7 minutes in qemu without KVM, 5 s on a machine (§26). That is longer than the writer takes to change something again, so every pass ends with a ripe group, and the next pass follows at once. The hashes, the catalog backup, the scrub, balancing, SMART and the media servers all come after the passes: they would never run for as long as the changes went on. Right after a pass, another step due now runs before a pass whose every reason is `events` or `due`, which waits one slice at most. A pass for any other reason does not wait. 12 vectors in `daemon_wake.json`, 7 of which failed before the change.
- **The link guard read every copy of a disk for each link.** What the names of a file record when one of them changes, and what those names are, are looked up by inode (`writeLinks`, `LinkedPaths`), and no index held `copies.file_key`: SQLite walked `copies_by_disk` and read each row to compare its inode. Only an operation on a file with more than one name asks, which without the guard is rare; with it every copy has two names, so every trash, purge, restore and rename does, and so does each link the guard makes or removes. The first pass with the guard on, which makes a link for each copy, had not ended after 8 hours on 200,000 files a disk, one batch of 1,000 links taking over 13 minutes to record. `0009_copies_by_inode.sql` adds a partial index on `copies(disk_id, file_key)`, and `TestAQueryByInodeReadsOnlyThatInode` pins both plans; it fails without the index.
- **What `TestScale` measures there**, with the index, on 200,000 files a disk on two disks, in qemu without KVM:

  | Step | Time | The most resident |
  |---|---|---|
  | The first pass, which finds every copy of a catalog that knows none | 5 min 50 s | 190.0 MiB |
  | A scan with every copy known | 1 min 8 s | 190.1 MiB |
  | Reading the hash of every copy | 31 min 17 s | under 189.3 MiB |
  | A pass with nothing to do | 1 min 14 s | 190.6 MiB |
  | `atoll status` | 35 s | under 101.8 MiB |
  | `atoll status` with a strict rule, its plan of the pool included | 1 min 13 s | 192.5 MiB |
  | The first pass with the link guard, which makes the 400,000 links | 19 min 52 s | 196.5 MiB |
  | A pass with the link guard | 1 min 36 s | 205.3 MiB |
  | The pass that removes the links, the guard off | 17 min 5 s | 197.2 MiB |

  A step's peak starts from what the process holds when the step begins, which is what the step before left: the hashes and `atoll status` held less than that, and how much less is not measured. The catalog is 125 MiB after the first pass, 140 MiB with the hashes, and 197 MiB after the guard's 800,000 operations, which the journal keeps. The first soak's first tenth, after 7 h 13 min: 218 passes, 3.6 MiB of heap in use, 7 goroutines, 14 open files, a catalog of 98.5 MiB, 171.2 MiB resident at the end of a pass, and 222.8 MiB at most since its start.
- **What the soak measures at that scale.** `TestDaemonSoak` and `TestScale` now set the binary's soft memory limit, 170 MiB unless `GOMEMLIMIT` says otherwise, which the test's process lacked: the peaks they log are now the daemon's. The soak samples each tenth's memory at the end of the pass that follows it, in the daemon's own goroutine. A pass over 400,000 copies holds over a hundred MiB while it runs, and a sample taken in its middle would say how far the pass had gone rather than what the daemon keeps. The pass count the soak requires shrinks with the pool: a pass a minute on 2,000 files a disk, a hundred times fewer on a hundred times as many, and at least 10. It compares the passes of the second tenth with the last only when the second holds at least 4.

### Decided without the maintainer, to confirm

- **`schema/migrations/0009_copies_by_inode.sql`**, a new migration: `CREATE INDEX copies_by_inode ON copies(disk_id, file_key) WHERE file_key IS NOT NULL`. Partial, as a pending copy has no inode, and so that a query that names no inode can never take it for an index of the disk, where `copies_by_disk` already serves. It costs every copy an entry: the size it adds to the catalog is in §26.
- **The turn after a pass.** Another step due runs before a pass for `events` or `due` alone. It does not run before a pass for `start`, `reload`, `gone`, `back` or `rescan`. The first two reasons are routine: they already wait for their group to ripen or their time to come. The others say that the pool itself changed, or that the window asked for a rescan. A check does not count as a turn.
- **How the first long soak ended.** The soak meant to last 72 hours, on the code of commit 25ec786, was stopped by hand from Docker Desktop's interface on 2026-11-13 at 16:36:51 UTC: its log ends there, Docker's own log holds the request to stop its container, and the kernel log of Docker's machine holds no out-of-memory kill. It had run the daemon for 13 h 10 min and 394 passes, none held, with no error and one warning, expected: no `smartctl` in the guest. Its passes took 116, 111, 118 and 121 s on average over the four quarters of that time, so nothing slowed. Its only memory sample is the first tenth's, above. The 72 hours remain to be run, on the code as it is now.
- **Past the 200 MB of §26, left open.** For 190.7 MiB allowed, `atoll status` with a strict rule peaks at 192.5 MiB, the passes with the link guard at 196.5 to 205.3 MiB, and the soak's process reached 222.8 MiB. The soft limit of 170 MiB bounds the heap alone: what SQLite and the binary hold comes on top, about 20 MiB in a pass, which leaves the passes themselves at 190.0 to 190.6 MiB. Nothing is changed yet: a lower limit costs time near it, and what the link guard and a plan of the pool hold is to look at first (TODO.md).
- **What stays open.** Under a change every second on 200,000 files, passes still follow one another, each walking every copy. That is about one core's worth on a machine, far from the 5 % of §26. Only passes that walk what changed, rather than every disk, would fix that; phase 2b changes passes the same way to spare the disks' sleep. It is left to the maintainer, in TODO.md.

## What the local CI found on the beta (specs.md §15.13, §16)

Commit "Keep what ripe events ask for until a pass", on `beta`, after `test/ci/run.sh` on `8c3facf` failed two tests of `test/loopdev`, on both kernels.

- **A pass lost to a check.** A group of the watcher leaves the grouping once ripe. When the same turn of the daemon's loop also had a mount check due, the check ran first and the turn ended: what the group asked for was gone, and no pass followed. A file closed then waited for its `settle` rather than being copied at once, and its folder was told to no media server. `TestNotifyMediaServer` failed so 4 runs in 5 on `b5cc156`, the commit that brought it, and 2 in 3 on `8c3facf`, as each pass grew a little longer. The loop now keeps the groups' origins until a pass takes them (`TestRipeGroupsWaitForAPass`, which fails without the change); in qemu the test then passed 5 runs in 5.
- **`TestScrubFindsRot`** expected a quarantine, which the scrub no longer does by default since `integrity.auto_repair` came with the repair: it now runs without it, as `TestRepairOnEachDriver` covers the repair.
- **The link guard off** opens a disk only when its journal says the guard wrote to it, through a partial index added to `0008_link_guard.sql` (`ops_by_link`): a pool that never used the guard opens no disk for it at each pass.

## `balance --move-originals`, and the bytes correctly placed in `atoll status` (specs.md §13.3, §15.11)

Commit "Move user copies with balance --move-originals, and count the bytes correctly placed", on `beta`.

- `atoll balance --move-originals` moves the user copies that break a strict setting too. A file's misplaced user copies come first; each copy made in place of one carries that copy's disk in its job (`replaces`). The transaction that records the new copy makes it the user copy, and the old one an Atoll copy, which then goes to the trash as `rebalance` as Atoll's do. When the file's well-placed copies already reach its level, no copy is made: the user copy hands its part to the first of them, in one transaction, then goes the same way. The daemon never moves a user copy.
- `atoll status` gives `bytes_placed`, the bytes of the files with no misplaced copy, from the same plan as `balance.trigger`, and its share in the text output. With no strict setting in the pool or a rule, no plan is made.
- Verified:
  - 5 new plan vectors (`move_originals`);
  - 5 new scenarios: the moves themselves, a user copy on a disk after the Atoll copy's, a hand-over with no copy, a crash once the new copy is in place, before it is recorded, and the placed bytes before and after a balance, within `--path` too;
  - the 13 status scenarios, which now give `bytes_placed`, and `balance-tags`, whose dry run with `--move-originals` shows a user copy with no other usb disk to go to.
  - unit tests of the hand-over, of the measure `balance.trigger` takes, which leaves user copies out, and of the daemon's balancing, which never moves one (`BalanceAtoll`);
  - 19 mutants of the new code, all caught.
- **Decided without the maintainer, to confirm:**
  - **The user copy's part moves with it.** The copy made in its place becomes the user copy, so that invariant 2 (never remove the last user copy) holds through the move. Otherwise a file whose only user copy moved would be left with Atoll copies alone.
  - **The hand-over with no copy**, when the file's well-placed copies already reach its level. Without it, such a user copy would stay misplaced forever, as `no-target`.
  - **The contracts:** the `replaces` field of a replicate job's payload; `bytes_placed`, required, in `api/schemas/status.schema.json`.
  - **`atoll balance` without the flag says nothing yet** of the user copies that `--move-originals` would move (TODO.md).

## The link guard (specs.md §17)

Commit "Keep what a deletion through mergerfs removes, with the link guard", on `beta`.

- With `trash.link_guard = true`, each pass gives each copy a hard link at `Atoll/links/<file id>`, in a folder of mode 0700, an operation `kind = link`, journaled a thousand at a time. A link moves its file's change time: the rows of its names take it, so that no copy reads as changed.
- Once everything else of a pass ran, a pure function decides each link: it stays; it goes, `kind = unlink`, when its data is another copy found there or an entry of the trash; it goes to the trash from the link, `kind = trash` with the new column `ops.link`, when its data lost every name Atoll knows in the root, `overwritten` while its file has a copy on that disk and `deleted` otherwise, at its file's path or at the path of the last `copy-gone` event, which now holds `file`; it stays, with a warning, when nothing names it or Atoll did not make it.
- No count of a file's names includes the guard's link: the walk and the looks of the operations take it out of `nlink`.
- A purge removes the links of an entry first. Turned off, the guard removes its links at the next pass, sending to the trash first what they alone hold, and the folder. A new catalog, from `atoll init` or a rebuild, starts its file ids past the highest the links name.
- Verified:
  - the 26 vectors of `conformance/vectors/link_guard.json`;
  - 17 scenarios: a deletion through mergerfs and its restore, a deletion on one disk, an atomic save, a rename and the link counts it records, the guard off, a purge before the next pass, a rebuild, what the guard cannot name, a dry run, and kill -9 at `linked`, `unlinked`, `trash-folders` and `moved`, and between the two names of a move on a disk with no `RENAME_NOREPLACE`;
  - unit tests with EIO, EPERM and ENOSPC on each call, a link that is the last name of its data, and a trash with no room;
  - 31 mutants of the new code, all caught but one that changes nothing: a trash from a link that looks for a copy row to remove, which none can match;
  - `TestTheLinkGuardThroughMergerfs` in qemu on Linux 6.12 and 7.3, on ext4, XFS, btrfs, ntfs3, ntfs-3g and lowntfs-3g: a rename then a deletion through a real mergerfs, the data in each disk's trash, and a restore through the view, all of it through a cold cache.
- **Decided without the maintainer, to confirm:**
  - **Invariant 11, as written.** A copy whose data only its link holds was removed by someone else: a deletion like any other, propagated, whose data goes to the trash from the link. Looking in `links/` as in `trash/` and `tmp/`, for an operation of Atoll's, would have kept every deletion through mergerfs from propagating.
  - **The contracts:**
    - the column `ops.link` (migration 0008), the file id whose link an operation is about;
    - the field `file` of `copy-gone`;
    - `link` on the `trash` event of the guard.
  - **The reason** follows the copy's row: `overwritten` while the file has a copy on that disk, `deleted` once its row went. An atomic save through mergerfs gives `overwritten` on every disk, as the pass copies the new version back in the same pass.
  - **The folder is private (0700).** A link reaches its file whatever the folders above it in the root forbid.
  - **A link nothing names stays**, with a warning in the log, rather than going to the trash at a path made up for it.
  - **The guard runs in a pass only**, at its end. A command that sends a copy to the trash leaves its link to the next pass, and a purge before it removes the link first.
  - **No new field in `catalog rebuild --json`** for the file ids it starts past.

## An NTFS volume marked dirty (specs.md §22, §23)

Commit "Alert on an NTFS volume marked dirty", on `beta`.

- When an NTFS disk is missing, the daemon reads its volume once, as soon as its device is plugged in, through the name `/dev/disk/by-uuid/` gives its serial. It reads the boot sector and `$Volume`, the fourth record of the MFT, update sequence applied, for the flag `VOLUME_IS_DIRTY`.
- A volume found dirty raises `ntfs-dirty:<id>`, saying to repair it with `chkdsk /f` on Windows, and is never remounted after a USB reset, as Linux mounts it only by force.
- Verified: the 14 vectors of `conformance/vectors/ntfs_dirty.json`, on images built from the on-disk layout (several cluster, sector and record sizes, a torn record, no volume information); `TestProbeNTFSDirty`, `TestProbeDirty` and the alert's cases in `TestHoldings` and `TestDescribe`; `TestNTFSDirtyOnARealVolume` in qemu on Linux 6.12 and 7.3, on a volume `mkntfs` made. It reads clean once made, and still clean while ntfs3 has it mounted for writing, as ntfs3 does not mark it. Marked dirty as Windows marks it, it reads dirty and ntfs3 refuses to mount it. Once `ntfsfix -d` clears it, it reads clean and mounts again. A first run of that test expected ntfs3 to mark the volume while mounted: it does not, so the spec says what was measured. 14 mutants, each caught.
- **Decided without the maintainer, to confirm:**
  - **Read once per absence**, once the device is there, so an unplugged disk is never woken.
  - **Found through udev's `/dev/disk/by-uuid/`.** Without udev, as in a container, a dirty volume is not seen (TODO.md).
  - **Priority 4**, beside the disk's own `disk-missing`.
  - **Only while missing.** A volume mounted dirty, as ntfs-3g allows, raises nothing: it is in service.

## The daemon's balancing (specs.md §13.3, §15.13)

Commit "Balance from the daemon, as balance.schedule says", on `beta`.

- The daemon that runs for good now checks `balance.trigger`, once a stretch of `balance.schedule`, or at most once an hour with `immediate`. While the trigger holds, it moves the misplaced copies as `atoll balance --yes` does, 5 minutes at a time under the catalog's lock, within `balance.budget`.
- The trigger holds when the share of bytes correctly placed falls below `ratio_below`, compared exactly by the new `Percent.Below`, or when more than `or_more_than` bytes are to move. `balance.schedule` no longer warns of a later phase.
- A run stopped at its budget, or by a crash after a copy was made, is taken on by the next run: its plan makes no second copy, and the old copy, marked surplus by then, goes to the trash.
- Verified: 8 `percent.json` vectors of `below`; 3 `daemon_wake.json` vectors; `TestBalanceDue`, `TestBalanceHolds`, `TestBalanceChecksAnHour`, `TestBalanceStopsAtItsTime`; the scenario `balance-resume`, with a crash between the copy and the trash; `rules-check-preset-warnings` now on the preset `energy`, as `safety` no longer asks for a later phase; 15 mutants, each caught but two equivalent.
- **Decided without the maintainer, to confirm:**
  - **When to check:** at most once an hour with `immediate`, per the maintainer's answer. With a window, once per stretch.
  - **The bytes to move:** a file's size once for each misplaced copy.
  - **The order:** after notifications and SMART, before reading hashes.
  - **Slices of 5 minutes** under the lock.
  - **Not in shadow mode:** a daemon in shadow mode does not balance.
  - The share of bytes correctly placed is not shown by `atoll status` yet (TODO.md).

## `atoll balance` (specs.md §13.3, §13.4)

Commit "Move the copies a strict setting no longer allows", on `beta`.

- `atoll balance` moves the copies Atoll made that break a strict setting of their file, as a change of rule or of a disk's tags or domain leaves them.
- How: the plan of the pool, made with such a copy counting for nothing, places a replacement where the setting wants it. The copy pipeline makes it. The plan, made again, marks the old copy surplus, and only then does it go to the trash, as `rebalance`. A file never has fewer copies than it had during a move, and a user copy never moves.
- Verified: 12 `plan.json` vectors of the plan's `balance` mode; the scenarios `balance-tags`, `balance-no-target` and `balance-spread`; `TestRetireKeepsWhatThePlanKeeps` and `TestBalanceMoveOriginals`; 17 mutants, each caught but one equivalent (a plan of balance writes nothing of its results, which `DryRun` also ensures).
- **Decided without the maintainer, to confirm**, the safest reading of a short paragraph of the spec:
  - **What is misplaced:** only what breaks a strict setting. That is a disk the lists no longer allow, where only listed disks are candidates, or a failure domain doubled up while another is uncovered, with `spread_mode = "strict"`. A copy that overflowed in `prefer` mode stays.
  - **By hand only.** The daemon's balancing inside `balance.schedule`, its hysteresis and the share of bytes correctly placed come later (TODO.md), and `balance.schedule` still warns of a later phase.
  - **`--move-originals` comes later:** for now it is refused, with exit 2.
  - **Only the files with a misplaced copy** get a job, so a run of `atoll balance` copies nothing else.
  - **New contracts:** `api/schemas/balance.schema.json` and its outcomes (`to-move`, `moved`, `no-target`, `copied`); `plan.json`'s `balance` and `misplaced`; and a scenario step `config`, which rewrites the configuration midway, as a change of rule needs.

## The copy the lists want, kept (specs.md §13.4)

Commit "Keep the copy the rule's lists want", on `beta`: the first piece of `atoll balance`.

- When more copies count than a file's level, the plan keeps some and marks the others `surplus`. Where only listed disks are candidates, `tags_mode = "strict"` or `overflow = "deny"`, a copy on a listed disk now outranks one of the same maker on a disk the lists no longer allow. So once a copy is made where a changed rule wants it, the one left behind is the surplus, rather than the younger copy that is in the right place. A user copy still stays before any Atoll copy.
- Verified: 4 `plan.json` vectors, two of them failing before the change; the solver's property tests.

## The capacity of the pool (specs.md §13.1, §13.3)

Commit "Say what the pool can hold at 1, 2 and 3 copies", on `beta`.

- `atoll capacity` gives, for 1, 2 and 3 copies of every file, the formula of §13.1 over the disks the solver can write to. Beside it comes what the plan reaches without moving a copy: the files that reach that many copies, and the formula over the free space the plan leaves. With `spread_mode = "strict"`, both use the failure domains. It reads the catalog alone and wakes no disk.
- Verified: the scenarios `capacity`, `capacity-short`, `capacity-strict` and `capacity-planned`, each value worked out by hand; 7 mutants, each caught.
- **Decided without the maintainer, to confirm:**
  - **What counts as reaching k copies:** a file the plan does not leave `degraded`, `unavailable`, `name-incompatible` or `name-collision`. Files in `manual` mode are planned as any other.
  - **`api/schemas/capacity.schema.json`**, a new contract: per level, `theoretical`, `reachable`, `stored` and `short`, with `spread_mode`.
  - **The free space and states are the last scan's**, so the command wakes no disk.

## Tags, domains and strict placement (specs.md §12.4, §13.4)

Commit "Place copies by the rules' tags and domains", on `beta`.

- The plan already knew a rule's tags and domains, `tags_mode` and `spread_mode` in strict mode, and `overflow = "deny"`. The scan now gives them to it, rather than `prefer` and `allow` with no list, and these settings no longer warn of a later phase. A copy goes only to a listed disk when the rule says so, and a file that must cover another failure domain waits for it rather than doubling up on its own. Either way the file is `degraded` when that cannot be met, or with `overflow = "allow"` its copy goes to another disk.
- Verified: the scenarios `placement-tags-strict`, `placement-tags-full` and `placement-spread-strict`, which each fail without the change; `scan-later-phase` became `scan-strict-unmet`, whose file is now `degraded`; the warning scenarios updated; 5 mutants, each caught.
- **Decided without the maintainer, to confirm:**
  - Copies already placed stay where they are when a rule's lists change: only new copies follow them, until `atoll balance` moves the others (§13.3).

## Conflicts list and resolve (specs.md §15.7)

Commit "List the files in conflict, and keep one version of one", on `beta`.

- `atoll conflicts list` gives each file in conflict with what each disk holds of it.
- `atoll conflicts resolve <path> --keep <disk>` keeps that disk's copy and sends the others to the trash, `--dry-run` and `--yes` as for every command that moves things. The file then takes the kept copy's size and time, its hash is read again, and the next pass makes its replicas from it.
- A walk leaves the copies of a file in conflict as it found them: a scan never records an edit to one. So the command looks at each copy again rather than trusting its row; otherwise the version a user just edited, the one most likely to be kept, could never be.
- Verified: the scenarios `conflicts-list`, `conflicts-resolve`, `conflicts-resolve-refused`, `conflicts-resolve-disk-away`, `conflicts-resolve-as-it-is`, `conflicts-resolve-gone-only` and `conflicts-resolve-kept-gone`; `TestConflictRows`, `TestConflictsResolveUnknownDisk`; 20 mutants, each caught.
- **Decided without the maintainer, to confirm:**
  - **Reason `overwritten`** for the versions that go, as the trash table's reasons are a contract (§29.3) and none other fits: a version another replaced.
  - **Every other copy goes**, even one that holds the same bytes as the kept copy: telling them apart would mean reading both whole, and the next pass makes the replica again.
  - **A copy changed since the last walk** goes as it is now; the kept copy is kept as it is now. A copy no longer on its disk loses its row. Only a disk that cannot be reached keeps the file in conflict.
  - **The new contracts:** the two commands and their exit codes; `api/schemas/conflicts-list.schema.json` and `conflicts-resolve.schema.json`; the event `conflict-resolve`, with the kept disk and the path, written before anything moves.
  - **Queued jobs of the file go** once it is resolved, as they were planned from another version.

## Notifications to Plex, Emby and Jellyfin (specs.md §21.5)

Commit "Tell Plex, Jellyfin and Emby of the folders that changed", on `beta`.

- The daemon that runs for good tells each media server of `[notify]` which folders of the view changed beside it. That is where the watcher saw a file change straight on a disk, and where a pass renamed or trashed a copy. Plex gets a partial scan of each library that holds the folder. Jellyfin and Emby get `/Library/Media/Updated`. A folder is told at most once in 30 s. A server that fails is tried again 5 minutes later. While a disk is missing, a folder with a file that has a copy on it is held until a pass finds the disk back.
- `atoll notify test` also reaches each media server set, with a request that changes nothing, to check its URL and its token or key.
- The `notify.*.url` keys no longer warn of a later phase.
- Verified: the 10 vectors of `conformance/vectors/path_map.json`; 3 new `daemon_wake.json` vectors; `TestQueue`, `TestPlexScan`, `TestPlexRefuses`, `TestMediaBrowserScan`, `TestServers`, `TestMediaServers`, `TestFoldersWithCopiesOn`, `TestChangedFolders`, `TestOperationFolders`, `TestTell`, `TestTellShadow` and `TestNotifyTestMediaServers`; `TestNotifyMediaServer` in qemu on Linux 6.12 and 7.3, against a fake Plex, with 24 mutants, each caught. The qemu guest now brings its loopback up, for the tests that answer as a local server would.
- **Decided without the maintainer, to confirm:**
  - **What is told.** A folder whose change someone other than the view and Atoll made, or with inotify, whoever made it, and that changed more than attributes. And the folders of a pass's renames and of its copies moved to the trash. A copy Atoll makes changes no name, so the initial replication tells nothing.
  - **The window.** A folder is told at once, then not again for 30 s; what changes in between is told when they end.
  - **Held folders.** While a disk is missing, a folder is held if any file below it has a copy on that disk, in any state, even when the view still shows it from another disk. That is broader than needed, and safe.
  - **Plex.** The libraries are read again before each batch. A folder above a library's folder asks for a scan of that folder. With no `token_file`, no token is sent.
  - **Jellyfin and Emby.** Jellyfin gets `Authorization: MediaBrowser Token="…"`, as Jellyfin 12.0 refuses `X-Emby-Token` by default; Emby gets `X-Emby-Token`. `UpdateType` is always `Modified`, a hundred folders a request, and a path that is not UTF-8 is left out.
  - **Failures.** A server that fails waits 5 minutes, without an alert, with a warning in the log.
  - **`atoll notify test`** checks the media servers too: Plex's `GET /library/sections`, Jellyfin's and Emby's `GET /System/Info`. This changes `api/schemas/notify-test.schema.json`, a contract (§29.3).
  - **Not told:** a new file that a walk finds after the daemon was stopped (TODO.md).

## SMART (specs.md §23)

Commit "Alert on a disk that SMART says is failing", on `beta`.

- The daemon that runs for good reads SMART once an hour with `smartctl -j -n standby`, which leaves a sleeping disk asleep, on each whole disk below each mount, again with `-d sat` behind a USB bridge it does not know. It keeps the reading of a disk awake in `disks.smart_json`. Two conditions come from it: `smart-failing:<id>` and `smart-sectors:<id>`. A reading that tells of more trouble than the one before brings a pass, so that the alert goes at once rather than at the next pass.
- Verified: the 13 vectors of `conformance/vectors/smart.json`; 4 new `daemon_wake.json` vectors; `TestReadSmart` (one device, a USB bridge, two devices, asleep, no `smartctl`), `TestReadSmartRecords`, `TestReadSmartShadowAndSilent`, `TestSetSmart`, and the SMART cases of `TestHoldings`, `TestGather` and `TestDescribe`; 17 mutants, each caught. Not verified on a real disk: no disk in qemu answers SMART.
- **Decided without the maintainer, to confirm:**
  - **Two conditions** rather than one "degrading SMART". A failed health check is count 1 at priority 4. Bad sectors count reallocated, pending and uncorrectable sectors together, so the alert comes again when the count grows. Their priority is 4 when some are pending or uncorrectable, which cannot be read now, and 3 for reallocated ones alone.
  - **A disk with a few reallocated sectors that never grow is reminded every 24 h**, as any condition that lasts. A baseline that alerts only on growth would need a key or a command to accept the count.
  - **Thresholds.** The temperature is recorded, with no alert, as no key sets a threshold. Once an hour, fixed, with no key.
  - **What `disks.smart_json` holds**, a contract of the catalog (§29.3): `read_ns`, `failing`, `reallocated`, `pending`, `uncorrectable` and `temperature`.
  - **Which disks.** A failed disk is not read and raises nothing. A missing one keeps its last reading, and its alerts.
  - **Without `smartctl`**, one warning, and no reading until the configuration is read again.
  - **`deploy/atoll.service` gives the daemon `CAP_SYS_RAWIO`**, which `smartctl` needs for ATA pass-through, beside the `disk` group it already had.
  - **No reading sets the state `standby`**, which §4 says SMART gives: a reading an hour old says nothing of now (TODO.md).

## The immediate mode (specs.md §12.3, §14.1)

Commit "Copy a file in immediate mode once it is closed", on `develop`.

- With it, the beta's replication modes are complete. The daemon that runs for good hands each pass the paths its watcher saw closed after a write, by anyone but Atoll, then left alone for `deletes.debounce`. A file in `immediate` mode at one of them is copied at once, without waiting for its `settle`. A file no such group told of, as one a walk finds, and every file of `daemon --until-idle`, which watches nothing, waits for its `settle`, as in `deferred`. No mode warns of a later phase any more.
- Verified: `TestImmediateCopiesOnceClosed`; `TestClosedPaths`; the scenario `daemon-immediate-without-watcher.json`; `TestImmediateCopiesOnClose` in qemu on Linux 6.12 and 7.3, where a file written through mergerfs is copied in under 3 s with a `settle` of an hour, while the file in `deferred` mode beside it waits; four mutants, each caught.
- **Decided without the maintainer, to confirm:**
  - What the watcher saw closed serves the next pass only: a copy that pass could not queue waits for its `settle` then.
  - A close by Atoll's own process never counts; one by mergerfs does, as that is how a user writes in phase 1.

## Alerts by mail (specs.md §23)

Commit "Send alerts by mail", on `develop`.

- The beta's second new channel, beside ntfy and Gotify. `alerts.smtp` is `smtp://[user@]host[:port]?from=<address>&to=<address>`, one `from` and one `to` or more, or `smtps://` for TLS from the first byte; the port is 587 or 465 when left out. `smtp` takes `STARTTLS` when the server offers it, and must when a user is given: the password, read at each send from `alerts.smtp_password_file`, never goes in clear, nor in the address. The subject is the alert's title, RFC 2047 when it is not ASCII; the text is in UTF-8, quoted-printable; `X-Priority: 1` marks what ntfy sends at 4.
- Only Go's standard library: `net/smtp`, `crypto/tls`, `mime`.
- Verified: against a fake mail server with a certificate of its own, over `STARTTLS` with `AUTH PLAIN` to two recipients, over TLS from the first byte, a password kept out of clear text, a recipient refused; `TestSMTPAddress`, twelve forms of the address; two vectors of `config.json`; the scenario `notify-test-smtp-unreachable.json`.
- **Decided without the maintainer, to confirm:**
  - The form of `alerts.smtp`, with `from` and `to` in its query, and a new key, `alerts.smtp_password_file`.
  - `STARTTLS` is required only when a user is given: a relay that takes mail without a password, on the same network, may be reached in clear.
  - A server's refusal, such as `550` for a recipient, is its answer, as an HTTP refusal is for ntfy and Gotify.

## Alerts through Gotify (specs.md §23)

Commit "Send alerts through Gotify", on `develop`.

- The first of the beta's two new channels, beside ntfy, as the maintainer asked. An alert is an HTTP `POST` to `<alerts.gotify>/message` of a JSON object, `title`, `message` and `priority`: 8 where ntfy's priority is 4, which Gotify's clients sound, and 5 otherwise. The application token is read at each send from the file `alerts.gotify_token_file` names, and goes in `X-Gotify-Key`, so that it stays out of the configuration.
- `atoll notify test` covers it, and `alerts.gotify` no longer warns that it does nothing.
- Verified: `TestGotifySends` and `TestGotifyFails` against a fake server; two vectors of `config.json`; the scenario `notify-test-gotify-unreachable.json`.
- **Decided without the maintainer, to confirm:**
  - A new configuration key, `alerts.gotify_token_file`, as ntfy has `alerts.ntfy_token_file`; `alerts.gotify` is the server's address, with no path.
  - Gotify's priorities 8 and 5.
  - A channel with no token file never goes, and says why, rather than refusing the configuration.

## The scheduled mode (specs.md §12.3, §14.1)

Commit "Run scheduled copies inside replication.window", on `develop`.

- The beta's third piece. A file in `scheduled` mode is queued as before, once its `settle` has passed, and its copy runs only inside `[replication].window`, in the time zone of `timezone`. Outside it, the job waits in the queue, counts in `waiting`, and `next_due` says when the window opens, which wakes the daemon then. `atoll replicate --now` runs it whatever the time, and a dry run lists only what it would copy now.
- `scheduled` no longer warns that it works as `deferred`; `immediate` still does. Two scenarios of `rules check` keep their later-phase rows with `immediate`.
- Verified: `TestScheduledCopiesWaitForTheirWindow`; the scenario `daemon-scheduled.json`; two mutants, each caught by both.
- **Decided without the maintainer, to confirm:**
  - A pass judges every copy at its own time: a copy that starts inside the window runs to its end, even past it.
  - A file in `scheduled` mode waits for the window whatever its size; a rule can give a file another mode.

## The report of duplicates (specs.md §16)

Commit "Report the files that hold the same content", on `develop`.

- R-15, the beta's second piece. `atoll dupes [--path <path>] [--min-size <size>]` groups the files that hold the same content under different logical paths, from the catalog alone: it reads no disk and takes no lock. A group is two files or more of one size and one BLAKE3. It never holds an empty file, nor one not hashed yet, which `unhashed` counts. Groups go by the bytes they hold more than once, the most first.
- Verified: the scenario `dupes.json`, with a quick scan then a full one, each filter and the order; eight mutants, each caught.
- **Decided without the maintainer, to confirm:**
  - A group is listed when one of its files is at or below `--path`, with all its files, so that a folder's duplicates elsewhere show.
  - An empty file is never a duplicate; two hard links of one file are, as each logical path is a file.
  - `--min-size` takes a size with its unit, `10B`, as the configuration writes it; 1 byte by default.
  - The order: by the bytes a group holds more than once, then by hash.
  - New contracts: `atoll dupes` and `api/schemas/dupes.schema.json`.

## The repair of a corrupt copy (specs.md §16.1)

Commit "Repair a corrupt copy from a healthy one", on `develop`.

- The beta's first piece (R-14), as the maintainer ordered it on 2026-11-12. With `integrity.auto_repair`, `true` by default, the scrub repairs a corrupt copy it finds from a healthy one, which it only reads; `atoll repair <path>` repairs those already found, with `--disk`, `--dry-run` and `--yes`; `repair_mode`, or `--mode`, says how.
- By chunk, the default: one operation `repair`. Every bad chunk reaches the trash first, at `Atoll/trash/<date>/<op>.chunks/<path>/<index>`, then each is written in place from the healthy copy, checked against its hash before and read back past the page cache after, then the time is set back. The copy keeps its inode and its creation time. A copy with more than one link is always repaired so.
- Whole, with `file`: the corrupt copy goes to the trash, `repaired`, and replication makes it again once the run is over.
- A repair that stops leaves the copy `corrupt`: an edit found right before the first write, a healthy copy that no longer reads as its hashes say, no room, a crash. Its reprise reads the copy whole, and `integrity.RepairReprise`, a pure function with 25 vectors, decides: finish; set the time back and record the change time; write nothing; or leave someone else's change alone.
- An entry of chunks is listed, never restored (`not-a-copy`), and purged whole.
- Without `auto_repair`, the scrub quarantines as before, and also when a repair found no room, as a move to the trash takes only folders.
- Verified: 14 unit tests. They cover a repair in place, by the scrub and by the command; an edit never overwritten; a crash at each of 6 points, and its reprise; no room; a whole file; a copy that reads right after all; no healthy copy; the trash entry; a hard-linked copy; a healthy copy that changes; and a disk that reads it wrong. Also the reprise's vectors and a property over every combination of four chunks; 13 mutants, each caught by a test; the scenarios `repair-usage.json` and `repair-nothing.json`; and `TestRepairOnEachDriver` in qemu on Linux 6.12 and 7.3, rot written on the device below ext4, XFS, ntfs3, ntfs-3g and lowntfs-3g, with a name of 255 bytes: each repaired in place, inode and time kept.
- **Decided without the maintainer, to confirm:**
  - A repair that stops ends its operation `done` when a chunk reached the trash and `undone` when none did. Its event `repair-stopped` says why.
  - In an entry of chunks, the folder named like the file is its owner's alone, mode 0700, and each chunk takes the copy's mode and owner.
  - A copy found corrupt that reads right after all, read whole by `atoll repair`, is recorded `ok` and verified: the outcome `verified`.
  - `file` reports `repaired` once the corrupt copy is in the trash; replication then makes it again.
  - New contracts: the operation `repair` and the events `repair`, `repaired` and `repair-stopped`; `atoll repair` and `api/schemas/repair.schema.json`; `repaired` in `scrub.schema.json`; `not-a-copy` in `trash-restore.schema.json`; `repair_reprise.json` and the chunk cases of `trash_path.json`; the crash points `chunk-saved`, `chunks-saved` and `chunk-written`.
  - btrfs is not in the test on real disks: it answers a read of a block that rotted with `EIO`.

## A file once its corrupt copy is gone (specs.md §16)

Commit "End a file's corrupt state with its corrupt copy", on `develop`.

- Found while designing the repair: a file the scrub found a corrupt copy of stayed `corrupt` for good, even once that copy was in the trash and made again from the healthy one. Only the file's state remembers `corrupt`, and every plan kept it, as it keeps a conflict. So `atoll status` said `corrupt` of a file whose copies were all in sync, an alert said so every 24 hours, and a deletion of it never propagated, as no trash job of a corrupt file stands (§15.12).
- A file is now `corrupt` while a copy of it is: once none is, it takes the state its copies give it. A conflict its copies still hold is found again by the next pass, from their rows, as a conflict's changed copies never take a new look.
- Verified: `TestScrubQuarantines` follows the file to `pending` once its copy went, then to `ok` once it is made again, and fails without the change; `TestScrubKeepsTheLastCopy` keeps it `corrupt` after a pass while its copies are. Two tests of `watch` had made a file `corrupt` with no corrupt copy, which nothing does: they now set it up as the scrub leaves it, and still find it `corrupt` through an outage and beside a copy that differs.

## A file whose deletion waits (specs.md §13.4)

Commit "Copy nothing of a file whose deletion waits", on `develop`.

- Found while writing the scenario of a full disk: a file deleted on one disk, whose other copy could not go to the trash yet, was copied back to the disk the user deleted it from once its `settle` passed, and that copy, having no queued job, then ended the deletion (§15.12). The user's deletion was undone, on every disk. Any wait did it: a full disk, a disk `readonly`, a guard that holds the job, an `io-error`.
- The plan now takes a file with a trash job queued for a deletion as leaving: it gets no job, neither a copy nor a refresh, and its pending copies are canceled. It shows `degraded` while it waits, as it holds fewer copies than its level.
- A plan reads which files are leaving a page of paths at a time, each file's jobs through their index (`TestLeavingReadsItsPage`): a join walked every job.
- Verified: five plan vectors, with `leaving`, a new field of `plan.json`; the property test, which never sees a job for a leaving file nor a pending copy kept for one; the scenario `daemon-trash-full.json`, where nothing comes back to the disk the files were deleted from ten minutes on; four mutants, each caught.
- **Decided without the maintainer, to confirm:**
  - A file leaving shows `degraded`, the state its copies give it, rather than a state of its own.
  - Only a deletion makes a file leaving: a trash job for another reason leaves the plan as it was.

## A full disk's trash (specs.md §17.1)

Commit "Send nothing more to the trash of a full disk", on `develop`.

- Found by a four-hour soak of the daemon: its 64 MiB disk ran out of inodes after 36 minutes, as each copy sent to the trash takes folders there and the writer sent about two a second, which a retention of 30 days never purged and `max_size`, which counts the files' sizes, never saw. Every pass then tried each of its 2,010 queued moves to the trash, each journaled and undone on `ENOSPC`: 194,000 operations in under an hour, passes of 32 to 35 s, and a catalog of 33 MiB. The daemon's memory did not move: 3.2 to 3.9 MiB of heap in use.
- A disk that answers `ENOSPC` to a move to its trash takes no more in that session: the rest wait in the queue, and the next session tries one again.
- The conformance runner gains `full` on an `atoll` step: the disks it names have no room left for that run, `mkdir` and the creation of a file giving `ENOSPC` there (`conformance/scenarios/README.md`).
- The soak's trash turns over every ten minutes, so that it measures a daemon at work rather than a full disk.
- Verified: `TestAFullDiskTakesNoMoreForItsTrash`, three moves to a trash with no room, one of them tried, then all three once room comes back; the scenario `daemon-trash-full.json`; two mutants, each caught by both.
- **Decided without the maintainer, to confirm:**
  - While a disk is full, one move to its trash is tried per session, journaled and undone: what it costs to notice that room came back, with no table that remembers the disk full.
  - `full`, on an `atoll` step and on a disk of the control file, is a new field of the conformance contract.

## NTFS short names (specs.md §15.8)

Commit "Refuse the short name of another entry on NTFS", on `develop`.

- Windows gives a long name a short one, `FILMNA~1.MKV` for `Film Name.mkv`, which no Linux driver lists and each finds at a look. A copy or a rename to such a name on an NTFS disk was looked at with `stat` alone: through the short name of a folder, a copy landed in the folder it names, and its row then read as moved, which would have renamed the user's file on the other disks; at the short name of a file, a conflict at every pass on ntfs3, and on ntfs-3g a new name Windows would read as the same.
- On a disk the configuration says is `ntfs`, the looks before a copy, a rename and the folders they make refuse a name of the short form (`names.ShortForm`, 21 vectors in `short_form.json`) that its folder does not list, when a look at it or at it in capitals finds an entry: `name-refused`, before anything is written. A folder renamed there goes one copy at a time, each refused.
- Measured in qemu on Linux 6.12 and 7.3, with real short names set by ntfs-3g: ntfs3, ntfs-3g and lowntfs-3g refuse both a copy through the folder's short name and one at the file's in small letters. The first run of that test showed the copy in small letters made on ntfs-3g and lowntfs-3g, against the research's table, which led to the look in capitals; the research says so.
- Verified: `TestAShortNameIsNoWayIn`, with ntfs3's and ntfs-3g's answers; `TestShortNamesOnEachNTFSDriver`; six mutants, each failing one of them.
- **Decided without the maintainer, to confirm:**
  - Only names of the form Windows gives are looked for, as reading a folder at each look costs: a short name a tool set otherwise is not.
  - A copy refused so is queued again at each pass and refused again, with nothing written; no table remembers it.
  - A restore through such a name is a look that fails, `io-error`, with nothing written.

## A folder renamed into the old name of one that waits (specs.md §15.12)

Commit "Hold back what lands in the old name of a folder that waits", on `develop`.

- `mv Dx N; mv D Dx` while `Dx` waits, for a copy below it on another disk whose clock runs, for the guard or for a look that cannot get there: the files of `D` took the name `Dx` at once, the next look read `Dx` as a folder made at `N`, and what only other disks held below `Dx` stayed in the folder that took its name. Before the files of a pass are decided, the folders that wait for what no decision of the pass ends are now worked out, and a copy that moved to a name at or below the old name of one of them waits for it; a folder renamed to below that name waits with its copies. Once the wait ends, the folder that waited leaves first, what only other disks hold follows it, and the rest lands.
- Only such a wait holds anything back, so that nothing waits for itself, which a cycle would otherwise.
- Verified: the scenario of that rotation, which now ends with the file only other disks hold at `N`; scenarios of a chain (`mv Dx N; mv D Dx; mv E D`), of a folder renamed below the old name (`mkdir Dx; mv D Dx/D`), and of two folders swapped, which no walk reads as folders renamed, pinned as the limit it is; two mutants, nothing held and no copy held, each failing three of them.
- **Decided without the maintainer, to confirm:**
  - A copy held so waits with no line of its own, as one whose debounce runs does.
  - Two folders that swap their names stay a limit: no walk reads them as renamed, as each name still holds a row, and what only other disks hold below them stays at its path.

## A swap of two names (specs.md §15.12, §17.2)

Commit "Exchange two names that wait for each other", on `develop`.

- A swap of two names on one disk was followed by the rows, and on the other disks each copy waited for a target the other held, `path-taken` at every pass. Once a round of renames moves nothing, the copies left whose targets lead round a cycle back to one of them are now exchanged two at a time, `renameat2` with `RENAME_EXCHANGE`: the first by place reaches its target, the other takes its place, and a cycle of n names ends in n - 1 exchanges. Each side is journaled as a rename before anything moves, so that a reprise finishes or undoes it as any other, and a dry run lists them as `to-rename`.
- The reprise read the row of a copy by its place alone, and while an exchange is half recorded two rows say they sit at one place: a crash right after the exchange undid the second rename against the wrong row. It now takes the row with the operation's inode.
- ext4, XFS and btrfs exchange two names; ntfs3, ntfs-3g and lowntfs-3g give `EINVAL` (measured on Linux 6.12 and 7.3), so a disk declared `ntfs` is not asked, and its copies wait as before.
- Verified: scenarios for two names across a dry run and a pass, three names round, an NTFS disk, and a crash once journaled and once moved; the scenario of a page of decisions, whose swap now ends; `TestFindSwap`; `TestASwapOnEachDriver` in qemu on Linux 6.12 and 7.3, on ext4, XFS, btrfs, ntfs3, ntfs-3g and lowntfs-3g, with the inodes kept through a cold cache.
- **Decided without the maintainer, to confirm:**
  - An exchange is two renames in the journal, not an operation of a kind of its own, so that the reprise needs nothing new. After a crash before the exchange both are undone, with an `op-undone` event each, as each target holds the other copy.
  - A disk is asked for an exchange by its filesystem in the configuration, not by trying: trying on NTFS would journal and undo two renames at every pass.

## An overwriting move (specs.md §15.12)

Commit "Propagate an overwriting move", on `develop`.

- `mv a b` over `b` on one disk was a limit: `a` was reported `path-taken` at every pass, and the other disks kept the old `a` and `b` for good. The copy of `b` the move overwrote is now `gone`, as its file is nowhere: the walk tells it from a replacement, as the name holds the target of another row's copy `moved` there. So `b` is decided as any deletion, after its debounce, its other copies going to the trash as `deleted`, under the guard as any other; once its row has gone, `a` takes the name, its other copies renamed there with nothing copied again, a pass later at the earliest.
- Verified: four scenarios, with copies read (the deletion, then the rename), with copies no scan read (`b` kept, and `a` waits), over a file with no other copy (`gone`, then the rename), and under a tripped guard (both wait for `guard confirm`); a unit test of the walk, which fails without the change. The code that set such a copy aside from the modifications went with it.
- **Decided without the maintainer, to confirm:**
  - An overwriting move is a deletion of what it overwrote, not a modification of it: its other copies go to the trash as `deleted`, kept for their whole retention, and count for the deletion guard.
  - Where the other copies of `b` are no peers, two user copies no scan has read, `b` is kept and `a` waits for its name. `b`, then below its level, is copied again to the disk of the move at each pass once `settle` has gone by, which the look before the copy refuses as `conflict`, with nothing written.

## The daemon's soak (specs.md §26)

Commit "Soak the daemon that runs for good", on `develop`.

- `TestDaemonSoak` (`test/loopdev/soak_test.go`), run in qemu with `SOAK=<duration>` (`test/qemu/README.md`): the daemon that runs for good on two ext4 disks of 2,000 files each, while a writer of its own process edits a file, renames another or puts it back, adds a file and deletes the one it added five seconds before, every second. Every tenth of the run it logs the heap in use after a collection, the resident memory and its peak, the goroutines, the open files, the passes and the catalog's size.
- It fails when the heap grows past the second tenth's by more than a tenth and 8 MiB, the files or goroutines by more than 16, when the daemon makes fewer passes than the run has minutes, when a guard holds a pass, or when the last tenth runs fewer than half the passes of the second.
- What its first runs found is in the four entries above: a copy tried again at every pass, the files a guard holds read with queries prepared anew, and two reads of the whole journal at every pass. Its guards are now out of the writer's way, a minute's window and counts it never reaches, so that it measures a daemon that decides.
- A four-minute run on Linux 6.12: 155 passes, each deciding 3 or 4 files and running 4 to 7 operations, none held; the heap in use 2.8 to 4.1 MiB, the resident memory about 30 MiB and at most 44.2 MiB, 8 to 10 goroutines, 21 or 22 open files.
- A four-hour run on Linux 6.12, on 2026-11-12, after the fixes the second soak called for: 8,341 passes, 780 to 888 a tenth of the run, none held; the heap in use 2.8 to 6.8 MiB from the second tenth on, the resident memory 31.6 to 41.3 MiB and at most 51.0 MiB, 8 or 9 goroutines, 21 to 30 open files; the catalog grew to 22.1 MiB, its events log with it. Nothing grew but the catalog, and the passes kept their pace: it passed.

## The parts of `tmp/` looked for where they are (specs.md §14.2)

Commit "Look up only the parts tmp/ holds", on `develop`.

- Found the same way. Each pass that opens a disk cleans its `tmp/`, and to know which parts it may remove it read every operation `create` the disk ever had into memory, though `tmp/` nearly always holds none: 0.35 s a pass over a million copies, and a map of a million names each time, against a daemon's memory of §26.
- `tmp/` is listed first; only what it holds is looked for, each part from the id its name gives on, as only its operation and those that resume it name it: 66 ms a pass over a million copies. What is removed is unchanged.
- Verified: the unit tests of the cleaning, a test of the lookup's plan (`EXPLAIN QUERY PLAN`), and the trash, jobs and daemon tests under the race detector; the conformance suite.

## The events log read through an index (specs.md §9, §15.6)

Commit "Read the guards' events through an index", on `develop`.

- Looked for once the soak had shown passes slowing down: what else a pass reads grows for good. The guards read the events log at every pass, for each disk, and three of their reads went over the whole log, three more over all of it since the last answer to a guard, which most pools never give. `events` is never pruned: over a million events, a pass with nothing to do took 0.8 s, against 64 ms.
- `schema/migrations/0007_events_by_kind.sql`: an index on `events(kind, ts_ns)`. Each read goes over the events of its kinds, and a count over its window of them: 64 ms over a million events of the kinds a pool writes. It takes a second or two to build over a million events, once.
- The reads are named constants, and a test explains each (`EXPLAIN QUERY PLAN`): none may scan the log. The indexes vector lists the new one.
- **Contract to review:** the migration.

## A pass while the mass-modification guard holds (specs.md §15.6, §26)

Commit "Read the files a pass looks at again through prepared statements", on `develop`.

- Found by the same soak. While the mass-modification guard holds a file, each pass reads it again, its row twice and whether a job of it runs, and each read prepared its query anew: 95 µs a file held, measured natively, most of a pass that held 2,000 (260 ms, against 64 ms with nothing held). Under qemu without KVM, that is what slowed the soak's passes from the start.
- `catalog.FileFinder` prepares those three queries once a pass: 20 µs a file held, 103 ms for 2,000. What a pass decides does not change.
- Verified: a unit test of the finder against `FileByID`; the catalog and jobs tests under the race detector; the conformance suite.

## A copy that is gone is no source (specs.md §13.4, §14.2)

Commit "Give no job to a file whose only source is gone", on `develop`.

- Found by an hour's soak of the daemon in qemu (`test/loopdev/soak_test.go`). Once its writer had tripped the mass-deletion guard, a file it added and deleted before its copy ran kept its row, held by the guard, with its copy `ok`. The plan read that copy as a source, and each pass queued its copy again, journaled an operation `create` with its `replicate` event, made a part in `tmp/` and dropped it as `changed`. The operations of a pass grew by five at each pass, to 1,131 after an hour, each with its rows in `ops` and `events`, which no one ever removes.
- A source is a copy whose clock does not run (§13.4): one its last look found `moved`, `gone`, `uncertain` or `own` still counts toward its file's level, and is no source until its file is decided. A job queued before its source went goes with its pending copy as `changed`, and the next plan queues none.
- A copy that starts from nothing looks at its source before anything is journaled (§14.2): one gone, or no longer what its row says, ends the run as `changed` with no operation, event or part, and waits `settle`. That also covers a source changed while the mass-modification guard holds it, which no row says.
- Verified: a scenario, a file's only copy gone under a tripped guard, two passes an hour apart that queue and journal nothing, then the copy back and replicated; five plan vectors with copies `away`; unit tests for a job whose source went, and for a source changed or gone before the copy.
- **Decided without the maintainer, to confirm:**
  - A copy found `moved` is no source either, until its rename is decided, a debounce at most: a file with no other copy shows `unavailable` meanwhile.
  - A copy that resumes from its part still journals before it reads its source, as dropping that part needs its operation.

## `atoll init` over a catalog that went missing (specs.md §9, §25)

Commit "Start a new catalog's operations past the disks' ids", on `develop`.

- A catalog `init` creates starts its operations past the highest id the disks' trash and `tmp/` name, as `catalog rebuild` does, so that a new operation's folder of the trash or part never meets an old one. A disk whose folders cannot be read is a warning on standard error.
- A scenario: an id in `tmp/` and another in a trash, the counter past both, the folders left as they are, and a second `init` that keeps the catalog.

## `atoll trash restore --to` (specs.md §14.2, §17.2, §17.4)

Commit "Restore an entry of the trash to another disk", on `develop`.

- `--to <disk>` puts each entry on that disk: moved back when it is the entry's own, copied there otherwise through the copy pipeline, from the trash it is in, which keeps it. One operation `create` on the target, journaled with a `restore` event whose `source` is the entry's disk: the part made in `tmp/`, the file of the trash read once with each chunk hashed, looked at again, its times, mode and owner given, synced, read again without its cache against every chunk and the entry's whole-file hash, the missing parents made like the same folders of the trash, then the part takes the path without replacing anything. The copy is recorded as a restored copy, the user's.
- The reprise of a `create` with no job and a `restore` event records the copy as a restored one, never as a replica of what the path holds; it also removes a part made before its inode could be recorded, whose name, of the operation's id, is the operation's alone.
- `api/schemas/trash-restore.schema.json`: `to` on each entry with `--to`, for review.
- Verified: three scenarios, the copy with the entry left in its trash, the move back to its own disk, a disk the configuration does not list, a path taken, a dry run, and kill -9 once copied and once moved; unit tests with EIO on the trash's read, a file of the trash replaced, a part whose sync fails, a target that cannot be written to.
- **Decided without the maintainer, to confirm:**
  - The entry stays in its trash once copied, to expire as any other: a copy never takes away what it copies from.
  - An entry on a `readonly` or `draining` disk may be copied from, as its trash can be read; the target must be `online` or `standby`.
  - The copy is unverified, as a restored copy is, though it was read back.

## `--help`, and the CLI in French (specs.md §25)

Commit "Give the CLI its help and its French", on `develop`.

- `atoll --help`, `-h` and `help` list the commands; `atoll help <command>`, and `--help` or `-h` among a command's arguments before a `--`, give its synopsis and what it does, exit 0.
- What the CLI writes for people is in French when the first of `LC_ALL`, `LC_MESSAGES` and `LANG` that is set starts with `fr`: 175 messages and the help, through `T()`, whose key is the English text. A test reads every `T("…")` of the sources and requires its French, with the same verbs in the same order and the same line breaks, and no French nothing asks for. The JSON outputs, the words they share with the human output, names, paths, keys, the daemon's log and the details of an error from below the CLI stay in English.
- `atoll rules explain` writes a time as the configuration does, `settle = 10m`, never in nanoseconds (`value.FormatDuration`, read back by `ParseDuration`).
- `guard confirm` and `reject` say their answer in a sentence of their own, rather than with the English verb inside it.
- **Decided without the maintainer, to confirm:**
  - The outcomes, reasons, states and decisions the JSON shares stay in English in the human output too, as a search reads them; so do the synopses' placeholders, such as `<path>`.
  - A duration is written in days rather than weeks: `7d`, not `1w`.
  - French typography puts a space before a colon, a plain one, as a terminal shows it.

## The local API (specs.md §25.1, §26, §29.5)

Commit "Answer questions over a local API", on `develop`, as the maintainer chose on 2026-11-12.

- The daemon that runs for good answers `GET /v1/health`, `/v1/status`, `/v1/disks`, `/v1/disks/{id}`, `/v1/trash` and `/v1/guards`, each with what that command's `--json` gives, made by the command's own writer with the configuration the daemon runs with, a reload included. `api/openapi.yaml` describes it.
- On the Unix socket `api.socket` (`/run/atoll/atoll.sock`), made with mode 0660, never wider for a moment, in the folder the unit's `RuntimeDirectory` makes; a socket an earlier run left is replaced, anything else stays. Over TCP on `api.listen`, a loopback address or one of the Tailscale network, every request carries the Bearer token `api.token_file` holds, read again at each request and compared in constant time.
- Configuration: `[api]` with `socket`, `listen` and `token_file`; `api.listen` follows a grammar of its own (§29.5), with 37 vectors in `listen.json`, parsed in-house so that the Rust version can match it; `listen` without `token_file` is refused. 12 cases in `config.json`.
- Verified: each answer equals its command's output byte for byte; 405 for any method but GET, 404 for another path or disk, 503 for a catalog that cannot be used; the token missing, wrong, changed, empty or gone; the socket's mode, a stale socket replaced, a file at its path left alone, the socket gone at the stop; a round trip over TCP.
- **Decided without the maintainer, to confirm:**
  - The alpha's API only answers: moving and deleting stay with the CLI, which takes the catalog's lock.
  - A socket or a TCP address that cannot be opened leaves that side off, with a line of the log, rather than stopping the daemon.
  - The token is read at each request, so that a new one takes effect without a reload; HEAD is refused like any method but GET.
  - Every answer that cannot be made is 503, with its error.

## The owner applied to the other copies (specs.md §9, §15.4, §15.10, §17.2)

Commit "Apply the owner of a metadata change to the other copies", on `develop`, as the maintainer chose on 2026-11-12.

- `schema/migrations/0006_owner.sql`: `copies.uid` and `copies.gid`, NULL until known; the next look that finds a copy unchanged since its row records its owner, as a `chown` moves the change time. `ops` is made again to take the kind `chown`: SQLite changes a CHECK only so. Its rows, the trash's references to them and its counter are kept, tested from a version-5 catalog whose counter is past its last row. Migrations now run with `PRAGMA defer_foreign_keys = ON` (§9, `schema/README.md`).
- `solver.MetadataPlan`: the owner of the changed copy whose owner moved last, both ids, goes to each other copy that holds another or an unknown one, as the mode does. 13 more cases in `metadata.json`, 6 in `catalog.json`.
- An operation `chown`, journaled with a `chown` event that holds `uid` and `gid`, runs before the `chmod` and the `utimens` of the same copy, as a `chown` may clear setuid and setgid. A driver, or a run without `CAP_CHOWN`, that refuses the owner (`EPERM`, `EINVAL`, `EOPNOTSUPP`) leaves the copy its own: the operation is `undone`, the outcome `kept`, and the file is decided all the same. The reprise finishes on the event's owner, undoes quietly on the row's, with an event otherwise.
- `api/schemas/daemon.schema.json`: the operation `chown`, with `kept`.
- The conformance runner: `root`, for a scenario that gives files away, which anyone else skips and says so, never counting it as a success; a step `chown`; `uid` and `gid` in trees. Six `daemon-metadata-chown-*` scenarios, root only, run by the CI's containers. Five unit tests in `jobs`: applied, kept, a disk lost, a reprise, and an owner the look records.
- Measured in qemu on Linux 6.12.116 and 7.3.1: ext4, XFS, btrfs and ntfs3 keep the owner given; ntfs-3g and lowntfs-3g answer `fchown` and keep `0:0`, which the row records, and the next pass applies nothing; the reprise after a crash at `owner-set` is quiet on all six. The mode tests pass again on both.
- **Decided without the maintainer, to confirm:**
  - The owner is an operation `chown` of its own, which needed the journal made again; folding it into `chmod` would have spared that.
  - A refused owner is `kept`, not an error: otherwise each pass would read the whole file again, and fail again. The copy pipeline already leaves a copy another owner so.
  - The owner scenarios run only as root, and the runner says how many it skipped.
  - Migrations defer foreign keys to their commit, a rule the Rust version must follow too.

## Atoll's own empty folders (specs.md §6.1, §17.2, §17.5)

Commit "Remove the empty folders Atoll made", on `develop`.

- At the end of a pass, a prune or a scrub that writes, each folder below the root that Atoll made, and that its own move of that run left empty, a copy sent to the trash or renamed away, goes; then each folder above it that this left empty, the root never. One operation `kind = rmdir` for each, journaled with an `rmdir` event: the name must still hold that folder, by inode; `unlinkat` with `AT_REMOVEDIR`, which only an empty folder passes; the parent's time restored and the parent synced; then the folder's row goes. A folder that holds anything, is gone or is another, stays, and the operation is `undone`.
- Reprise: nothing at the name finishes it; the folder, by inode, undoes it; anything else undoes it with an event. Three more cases in `op_recovery.json`.
- Verified: five scenarios, a folder removed while the one above it keeps a folder the user made there, then stays once the user empties it, and kill -9 after the journal, after the removal, after the sync, and with a file made at the name before the reprise; unit tests with EIO on the removal and on the parent's sync, something new in the folder, another folder at its name, and a dry run.
- Fixed on the way: a disk lost while the scrub moves a copy to the trash now makes the scrub partial.
- **Decided without the maintainer, to confirm:**
  - Only what Atoll's own operations of that run emptied goes: a folder the user emptied stays, as an empty folder they kept may be one they want, and so does one a run left empty before it was stopped; nothing tries those again.
  - These operations are not in a command's output: the journal and the events hold them.

## `trash.max_size`, and the daemon's purge (specs.md §15.13, §17, §17.4, §23)

Commit "Purge the trash for room, and from the daemon", on `develop`.

- What a purge reaches is a pure function, `solver.ChoosePurge`, with 29 vectors in `conformance/vectors/trash_purge.json`: what has reached its time; then, on each disk whose trash holds more than `trash.max_size` of its capacity, surplus and rebalance, then overwritten, repaired and quarantined, the oldest first, until it holds no more. Deleted and renamed entries go only at their time.
- `atoll trash purge` says why each entry goes (`expired`, `max-size`, `older-than`) and which disks' trash stays full (`full`). New fields of `api/schemas/trash-purge.schema.json`, for review; the 17 scenarios that read that output take them.
- The daemon purges after each pass, under the same lock, as `trash purge --yes` does, and works out the alerts after it: `trash-full:<id>` for a disk whose trash is still full.
- Verified: the vectors; a scenario where a disk's trash past 0.5 % loses its surplus copies, then an overwritten one, keeps a deleted one until its time, and stays `full` until then, and where `--older-than` purges nothing for room; unit tests of the alert and of the daemon's purge, with and without `--dry-run`.
- **Decided without the maintainer, to confirm:**
  - A trash is counted as the sizes of its entries in the catalog, not by the disk's free space; an entry of unknown size counts for nothing and never goes for room; a hard-linked one counts with its size.
  - The capacity is the one the catalog last recorded; a disk with none recorded is never purged for room, nor full.
  - "Reached" is read as "more than": at the limit exactly, nothing goes.
  - `--older-than` purges nothing for room, as the user chose what goes.
  - The daemon purges after every pass, not once a day, and only reads the catalog when nothing is due; `--dry-run` purges nothing.
  - `trash-full` covers the disks that hold their files now, whatever their role, with priority 3, and is sent again every 24 h while it holds.

## `atoll catalog rebuild` (specs.md §9, §25)

Commit "Rebuild the catalog from the disks", on `develop`.

- A new catalog is made beside the old one, its operations started past the highest id the disks' `trash/` and `tmp/` name, then filled by a full scan, which reads every copy whole; copies with different hashes at one path are a conflict. The old catalog, its log moved into it or set aside with it, trades names with the new one in one step (`RENAME_EXCHANGE`), and is kept as `<catalog>.old-<UTC time>`. It works when the old catalog cannot be opened.
- Without `--yes` it says what it would do (exit 3); `--dry-run` likewise with 0.
- Verified: unit tests of the swap, the log set aside and the operation ids; a scenario: a conflict found again, ids past the trash and `tmp/`, the disks' folders left as they are, and `verify` finding the new catalog in step.
- **Decided without the maintainer, to confirm:**
  - The old catalog is set aside beside the new one, never removed, under that name.
  - The disks' trash entries are not listed in the new catalog: they stay on the disks, and `trash list` does not show them. Listing them again needs their reasons, which only the old catalog knew.
  - A disk whose trash and `tmp/` cannot be read is a warning: a new operation may then meet an old one there once, which costs it an `io-error`.

## `atoll catalog verify` against the disks (specs.md §9, §25)

Commit "Compare the disks with the catalog in catalog verify", on `develop`.

- After SQLite's checks, `catalog verify` walks each disk not failed and compares it with the catalog as a scan would (`watch.Compare`), each disk confirmed past the cache first, writing nothing. Each disk comes out as `atoll scan --json` gives it, and a difference is exit 4.
- A scenario: a disk in step with the catalog, then a file added, one changed and one deleted, which verify reports and the catalog still does not know.
- **Decided without the maintainer, to confirm:** `verify` takes no lock, as other read-only commands; a catalog SQLite finds damaged is not compared with the disks; a name that cannot be read counts as a difference.

## The daemon's daily catalog backup (specs.md §9, §15.13)

Commit "Back the catalog up every night", on `develop`.

- Once for each stretch of `maintenance.window`, after the new files' hashes and before the scrub, the daemon backs the catalog up under its lock, through `jobs.BackupCatalog`, which `atoll catalog backup` now shares. `Wake` says when, with four more vectors in `daemon_wake.json`.
- `TestRunBacksUp`: a window around now, one backup after the first pass, and none more for a pass in the same stretch.
- **Decided without the maintainer, to confirm:** a backup that fails is not tried again before the next stretch, with a line of the log; no alert says it yet.

## A nested btrfs subvolume is another mount (specs.md §15.10)

Commit "Leave a nested btrfs subvolume out of the walk", on `develop`, as the maintainer chose on 2026-11-12.

- The walk lists a folder on another device under the same mount, as a nested btrfs subvolume is, in `other_mounts`, and records nothing below it, as for another mount: its inode numbers are its own, so `file_key` would not tell its files apart, and nothing could move from it to the trash, as `rename` gives `EXDEV` there.
- `TestNestedSubvolume`, in qemu on Linux 6.12 and 7.3: a subvolume below the root is listed, and only the file beside it is recorded.

## `atoll catalog backup` and `verify` (specs.md §9, §25)

Commit "Back the catalog up, and check it", on `develop`.

- **`catalog backup`**: SQLite's `VACUUM INTO`, from one snapshot, into `<catalog folder>/backups/catalog-<UTC time>.db` through `.part`, checked whole before it is renamed (integrity check, foreign keys, `pool_id`); the seven newest are kept. With `maintenance.catalog_on_disks`, a copy goes to each disk online and not failed, as `Atoll/catalog/catalog.db`, through a confirmed handle.
- **`catalog verify`**: SQLite's integrity and foreign key checks on the catalog, exit 4 when they find anything.
- **Verified**: a unit test of the backups kept and checked, and a scenario of both commands, their usage errors, and a missing catalog.
- **Left for later, in TODO.md**: the daemon's daily backup, the read-only comparison of the catalog with the disks in `verify`, and `catalog rebuild`.
- **Decided without the maintainer, to confirm**: the names and the place of the backups, the copy on a disk replaced whole each time, and `verify` without the disk comparison for now.

## A correction: the qemu runs of this night (test/qemu/run.sh)

Commit "Run the scrub in the daemon", on `develop`.

- **What was claimed.** The commits "Send alerts through ntfy", "Add a disk to the pool" and "Check that mergerfs takes the NC and RO branches at runtime", and their entries below, say that `TestDaemonRunsForGood` with its alerts, `TestTopologyAndImmutable` and `TestBranchModes` passed in qemu. They had not run: `run.sh` booted `bin/qemu/loopdev.test` as the CI of `cec308f` had built it, so a new test reported `PASS` with nothing run, and `TestDaemonRunsForGood` ran its old code.
- **What a rebuilt binary found.** All four failed, each for its test or the guest, not for the code under test: the guest left its loopback interface down, so the daemon's alert, computed and sent, met "network is unreachable" (the daemon logged it, and would have tried again within 5 minutes); a RW branch with no `minfreespace` of its own fell under mergerfs's default of 4G on the guest's memory; the scrub test waited for the 10 minutes of `settle` before the copy could be made again; and without udev, a device-mapper target needs `dmsetup mknodes`. With those fixed, all four pass on Linux 6.12 and 7.3, their `=== RUN` lines read.
- **So it does not happen again.** `run.sh` refuses a test binary older than the code beside it, and LESSONS.md says why.

## The scrub, with quarantine (specs.md §9, §16, §25)

Commit "Scrub the copies, and quarantine the corrupt ones", on `develop`.

- **`jobs.Scrub`** reads the copies due, one disk at a time: never verified first, then the oldest verified. Each chunk is checked against its hash. A copy that matches is verified. A chunk that does not is read again past the page cache, after a look that the copy's name, size and times are unchanged: a copy that changed is a modification, left for the next pass; one that still does not match is `corrupt`, as is its file, with a `corrupt` event first.
- **Quarantine.** A corrupt copy goes to the trash, reason `quarantined`, when another copy, read whole first, is healthy; the next pass makes it again from that one. With none, every copy stays.
- **The budget.** When it runs out in a copy, the new table `scrub_cursor` (`0005_scrub.sql`) records the copy, the next chunk, and its row's size and times; the next scrub resumes there, or from the start of a copy that changed.
- **In the daemon.** With no hash to read, inside `integrity.window`, it scrubs by slices of 30 s under the lock, within `scrub_budget` per stretch of the window, and quarantines, as starting it is the consent; a corrupt copy brings a pass, for the alert and the copy made again. `Wake` says when, with ten more vectors in `daemon_wake.json`, and `value.Window.Opened` tells a stretch from the next, with six more in `window.json`. `TestScrubFindsRot`, in qemu on Linux 6.12 and 7.3, changes a byte of an ext4 copy on the device itself: the scrub finds it, quarantines it, and the next pass makes it again.
- **`atoll scrub [--disk] [--path] [--budget] [--dry-run] [--yes]`**: without `--yes` it records and says what it would move, exit 3; `--dry-run` records nothing. Output `api/schemas/scrub.schema.json`. `fsx.File` gained `DropCache`, for the second read.
- **Verified.** Unit tests rot a copy as a disk would, same size and the catalog none the wiser: quarantined, then made again by the next pass; two copies rotted, both kept and the file `corrupt`; a copy changed left for the pass; a dry scrub writes nothing; a budget run out in the middle of a 8 MiB file resumes at its second chunk and reads only the rest; `--path` by whole components. Two scenarios: copies due after `scrub_every`, `--disk`, `--path`, `--dry-run`, a changed copy never called corrupt, and the usage errors. A conformance build cannot rot a file, so corruption is judged by the unit tests only.
- **Decided without the maintainer, to confirm:**
  - The CLI quarantines only with `--yes`, as it moves copies; without, it records the corruption and exits with 3. The daemon, which takes no `--yes`, will quarantine.
  - A chunk that reads right the second time, past the cache, is a bad read, not corruption: a line of the log.
  - The budget counts from the start of the scrub, across disks; the disks after the one where it ran out are listed, not scrubbed.
  - A quarantined copy's trash entry has no hash, as it holds none of the file's versions.
  - A copy of a file whose chunk hashes are not recorded yet is not the scrub's: reading its hashes is.

## The disk commands, part 2: `remove`, `fail`, `replace`, `show` (specs.md §18, §25)

Commit "Drain, fail, replace and show a disk", on `develop`.

- **What depends on a disk.** A file with a copy there, `ok` or `unavailable`, that has fewer copies `ok` on the disks `online`, in `standby` or `readonly` than its level, or that is in `conflict`, as the disk may hold one of its versions (`catalog.ReadDiskHolds`, with what the disk holds by copy state, the files with no copy elsewhere, and the operations left planned there).
- **`disk remove <id> --evacuate`** marks the disk `draining`, with no `--yes`: it adds copies and removes nothing. **`disk remove <id>`** takes out of the configuration a disk nothing depends on, or a failed one, and refuses one with planned operations. **`disk fail <id>`** marks it `failed`. Both need `--yes`, and without it say what they would do and exit with 3. **`disk replace <id> --with <mount>`** adds the new disk with the old one's domain and tags, and marks the old one draining in the same edit. **`disk show <id>`** gives the disk's configuration, its mount check, what the catalog knows of it and its marker.
- **Verified.** `TestBranchModes`, in qemu on Linux 6.12 and 7.3: mergerfs takes `NC` and `RO` at runtime, a new file lands on neither, and a file only on the `RO` branch is still read. Four scenarios run each cycle through `daemon --until-idle`: a disk refused at `remove`, evacuated, its copies made on the third disk, then taken out and `retired`, what it held still on it; a disk failed, its replica made elsewhere from the other copy, `show` saying so, then taken out; a disk replaced, the new one with the old one's domain and tags; and the usage errors. A unit test pins what depends on a disk, case by case.
- **Decided without the maintainer, to confirm:**
  - What "nothing depends on it" means, above: the catalog's view, refreshed by each scan or pass. Copies on a disk draining, failed or missing do not count, nor a stale or surplus copy on the disk itself.
  - `disk remove` of a disk in service that nothing depends on, such as an empty one, is allowed without `--evacuate`.
  - `disk replace` leaves the old disk `draining`: `disk remove` takes it out once drained, rather than the daemon editing the configuration. `--domain` and `--tags` are refused there, as the new disk takes the old one's.

## The disk commands, part 1: a disk's state, the configuration's edits, `disk add` (specs.md §9, §10, §18, §21.1, §25)

Commits "Keep a disk draining or failed in the configuration", "Edit a disk in the configuration, keeping every other byte" and "Add a disk to the pool", on `develop`.

- **A disk's `state`.** A `[[disk]]` may say `state = "draining"` or `"failed"`. The catalog records a failed disk as failed whatever its mount check says, and a draining one as draining while the check finds it `online`. A draining disk is read, as its copies are the sources that evacuate it, and written, as it stays in step until it leaves; the plan already counted none of its copies and placed nothing on it. A failed disk is never walked, read for hashes, watched or remounted, and brings no pass. In mergerfs a draining disk's branch is `NC`, and a failed one's `RO` and last, so that `ff` reads its copy only when no other disk has one.
- **The configuration's edits.** `config.AppendDisk`, `RemoveDisk` and `SetDiskState` keep every other byte, comments included, follow the file's line ends, and refuse an edit that would change anything else, or disks written as inline tables. 21 vectors in `config_edit.json`, written from the spec before the code ran.
- **`atoll disk add`.** It reads the mount and the head, runs the mount check, and refuses what §10 and §18 refuse: an id or a mount the pool has, a disk missing, an identifier the pool or the catalog knows, the variants that change the rules of names, a physical disk another disk lives on (R-06), a marker of another pool, disk or filesystem. Then it appends the disk to the configuration, written whole through `<file>.new` and a rename, records the disks in the catalog, writes `Atoll/disk.toml` through a confirmed handle, and sends `SIGHUP` to the daemon. `--dry-run` writes nothing. Five scenarios; the conformance build tells a scenario's disks apart by a new `device` field, and says whether a mount point is immutable by `immutable`.
- **Found while writing it.** A session that confirmed a draining disk wrote the mount check's `online` over `draining`, which would have made it a target again; it now keeps `draining`. A test that said a draining disk is never opened predated evacuation, which reads it: changed.
- **Decided without the maintainer, to confirm:**
  - The administrative state lives in the configuration, as the key `state` of a `[[disk]]`, `draining` or `failed`; the catalog can be rebuilt, and a disk declared failed must stay so.
  - mergerfs: `NC` for a draining disk, `RO` and last for a failed one, rather than leaving their branches out, so that no file only they hold leaves the view.
  - A draining disk mounted read-only is `readonly`: its copies count again and its evacuation waits until it is mounted read-write.
  - A failed disk has no line in a scan or a pass, rather than a line saying `missing`, and no alert of its own.
  - `disk add` needs no `--yes`: it adds and removes nothing. It takes a read-only disk, with a warning, and refuses only the variants that change name rules among the read-only reasons.
  - Its id comes from the last component of the mount point, or `--id`; `--tags` takes a comma-separated list.
  - The daemon is told by `SIGHUP` to each process of this executable that runs `daemon` without `--until-idle`, found through `/proc`.
  - Topology: a loop device counts as its own disk, and a disk of the pool that is not mounted cannot be compared, which is a warning, not a refusal.
  - `chattr +i` is read through `open_tree(OPEN_TREE_CLONE)` of the parent folder, a detached, non-recursive clone, rather than a bind mount in a private namespace: the same view of the folder under the mount point, gone when its descriptor closes. It needs `CAP_SYS_ADMIN`, which the service has; without it, a warning.
  - A marker that disagrees refuses the disk; `disk add --id <its id>` takes back a disk the marker names.

## `atoll names check` (specs.md §15.9, §25)

Commit "Report the names to fix", on `develop`.

- The command reads every logical path of `files` and `entries`, each once (`catalog.AllPaths`), and the disks of the configuration, and gives the report of `names.Check`, which its 56 vectors already pinned. It reads no disk and takes no lock. `--path` keeps a folder and what is below it; `--json` follows `api/schemas/names-check.schema.json`, a name that is not UTF-8 in `path_b64`. Without `--json`, a finding a line, then how many names were checked.
- Three scenarios: a pool with a name of each kind, the same report with the disk unmounted, `--path` on a folder, on a name whose collision is still found in the whole pool, and on a path the pool does not hold; the usage errors; and no catalog. The report the scenario expects was written from §15.9 before the command ran, and it agreed.

## ntfy alerts (specs.md §23, §15.13, §25)

Commit "Send alerts through ntfy", on `develop`.

- **What holds.** After each pass, under the catalog's lock, the daemon works out which conditions hold, each with a key and a count: a disk `missing` or `readonly`, the files in each of `unavailable`, `corrupt`, `conflict`, `name-incompatible` and `name-collision`, a guard tripped on a disk, the files in `manual` mode left unprotected past `defaults.manual_alert_after`, the pool more than 90 % full, and the names a driver refused in the pass. One read of `files` counts them all; the paths an alert names are read only for the alerts that go.
- **When.** `alerts.PlanAlerts`, pure, with 14 vectors in `alerts_plan.json`, says what to send: a condition that begins, one whose count grew, and every 24 h one that lasts; one that ended is forgotten. The new table `alerts` (`0004_alerts.sql`) keeps what was sent, so a restart sends again only what is due.
- **Sending.** The daemon lets go of the lock to send, then takes it again to record what went. ntfy gets an HTTP `POST`, with the title, the priority and the tag `warning`, and the token of `alerts.ntfy_token_file`, a new key, read at each send.
- **`atoll notify test`** sends a test alert through each channel set: exit 0 when each went, 4 when one did not, 1 with none set, 2 on a usage error (`api/schemas/notify-test.schema.json`, three scenarios).
- **The pass's memory.** The daemon now counts a pass's decisions and operations through a sink instead of keeping each one, so a pass over a whole pool no longer holds them all.
- **Decided without the maintainer, to confirm:**
  - With no channel set, the daemon works out no alert and leaves the table as it is, so that a channel set later hears of what holds then.
  - A daemon in shadow mode (`--dry-run`) logs the alerts due, and sends and records none.
  - An alert that could not go brings a pass within 5 minutes, instead of waiting for whatever pass comes next, which could be hours away. A channel that cannot be reached is not tried again for the other alerts of that pass, each of which would wait its 10 s; one whose server answered with a refusal is, as the refusal may be that message's own.
  - The text of an alert stays within 4,000 bytes, as ntfy takes a longer one only as an attachment: the paths stop at the first that would not fit, and the alert says how many more there are. A path that is not UTF-8, or holds a control character, is shown in base64.
  - A guard tripped counts 1, so it is sent once when it trips, and not again each time it holds more.
  - `pool-full` counts the data disks `online`, in `standby`, `readonly` or `draining`, not those `failed` or `retired`.
  - The token file must hold the token on one line; anything else is a send that fails. A send's error never names the URL, whose topic is the secret on a public ntfy server.
  - The alerts are written in English; their French comes with the CLI's translation.
- **Verified.**
  - Unit tests for the catalog's queries, the conditions and what they read, the texts, ntfy against a test server, and what is recorded after a send.
  - `TestRunSendsAlerts` runs the daemon on a disk not mounted: the alert goes, a refused one goes again at the next pass, and one already sent does not. `TestFailedAlertBringsAPass`, `TestDryRunSendsNoAlert` and `TestPassSink` cover the retry, shadow mode and the names refused.
  - In `test/loopdev`, three times on Linux 6.12 and on 7.3, `TestDaemonRunsForGood` now has a ntfy server: no alert while every disk is there, one when a disk is unmounted, and the table empty once it is back.
  - Judged by mutations: the 37 of the alerts' code are caught, once a count of one was tested and `Gather`, shadow mode and the sink had tests of their own.
- **Found while testing it.** `TestDaemonRunsForGood` could unmount a disk while the daemon read a new file's hash on it, after a pass: `umount` then says the target is busy, as it would to a user. Once on Linux 6.12 it did. The test now waits for the disk to be free, for 30 s at most.

## The daemon that runs for good (specs.md §15.13, §25)

Commits "Record the maintainer's decisions for the alpha" and "Run the daemon for good", on `develop`.

- **What it does.** `atoll daemon`, with or without `--foreground`, runs until `SIGTERM` or `SIGINT`, then exits with 0; it exits with 1 when its configuration or catalog cannot be used. It tells systemd it is ready, and that it is stopping. It checks every disk's mount every 30 s, and watches every disk that is not `missing`.
- **When it runs a pass.** At start; when a disk goes or comes back; when a group of the watcher ripens that someone other than Atoll made; when the last pass's `next_due` comes; and every `rescan_every`, inside `maintenance.window`. When that is, a pure function (`daemon.Wake`) works out, with 24 vectors in `daemon_wake.json`. Rescans needed the first time inside a window at or after an instant: `value.Window.Next`, with six vectors more in `window.json`.
- **Each pass.** It takes the catalog's lock, waiting while a command holds it, opens the catalog, runs the pass, puts mergerfs's runtime branches right, closes the catalog, then lets go of the lock.
- **The branches.** They are those of `mergerfs-config`, without a disk `missing` or not walked since it came back, written whole and only when they differ.
- **A USB reset.** A disk that went `missing` for `device-gone`, `device-unreadable` or `io-error` is remounted through its fstab entry, `umount -l` then `mount`, at each check until it is back.
- **New files' hashes.** Inside `maintenance.window`, after a pass, the daemon reads the copies of the files that have no hash yet (`watch.HashNew`), by slices of 30 s under the lock. Each copy is checked against its row first, as `scan --full` does. `Wake` says when, with four more vectors. `TestDaemonRunsForGood` checks that a file with no copy to make gets its hash, and its copy is verified.
- **Reload.** `SIGHUP` reads the configuration again, as the maintainer chose: a pass for `reload` follows. A configuration that does not load is left aside (`TestRunReloads`).
- **A failing device.** A device that fails still answers the mount check from the cache. The loop therefore takes a disk's state from each pass's outcome too.
- **Commands wait.** A command that finds the catalog's lock taken waits for it (`fsx.WaitLockFile`), saying so once, instead of exiting with 1 (§25). Every command that writes does: `scan`, `prune`, `replicate`, `trash restore` and `purge`, `daemon --until-idle`, and `guard confirm` and `reject`.
- **The service.** `deploy/atoll.service` runs it as the dedicated user `atoll`, with the capabilities the maintainer chose, and `deploy/README.md` says why each is there.
- **Found while testing it.**
  - **Unmounting a watched disk.** The watcher's descriptor on the root of a disk watched by a filesystem mark made `umount` say "target is busy": no disk could be unmounted while Atoll watched it. The watcher now opens the root only to resolve a handle, and `TestWatcherOnEachDriver` unmounts a watched disk on every driver.
  - **`O_PATH`.** It will not do for that descriptor: `open_by_handle_at` refuses it.
- **Verified.**
  - `TestRunStartsAndStops`, `TestRunNeedsItsCatalog` and `TestSdNotify` run under the race detector, as does the test of the lock: a command that waits, then runs.
  - In `test/loopdev`, on Linux 6.12 and 7.3, `TestDaemonRunsForGood` covers XFS and ntfs-3g, watched by a filesystem mark and folder by folder. It checks a pass at start, a pass for a user's file and its copy on the other disk, a pass when a disk is unmounted with its branch out of mergerfs, a pass when it is back with its branch back at its place, and a stop. `TestDaemonRemountsAfterAReset` takes a device away from under its mount, and brings it back under another name: the daemon remounts it through fstab and walks it, and its branch comes back.
  - The scenarios `daemon-usage` and `daemon-needs-init` now hold the daemon that runs for good: it exits with 1 at once without a usable configuration or catalog, and `--json` goes only with `--until-idle`.

## The watcher (specs.md §15.1)

Commit "Watch each disk and group its events per path", on `watcher`.

- §15.1 now says what the watcher gives: an event below a disk's root only, at a logical path, `""` for the root or a folder that can no longer be told; what it says (`created`, `written`, `attrib`, `deleted`, `moved-from`, `moved-to`, `overflow`) and whether it names a folder; who made it (`view`, `atoll`, `user`, `unknown`); and how events are grouped per disk and path until `deletes.debounce` has gone by since the last. The watcher only says where to look.
- `watch.Grouping`, pure, with 20 vectors in `event_grouping.json`.
- `watch.Watcher` watches each disk the first way that works:
  - a fanotify mark on the whole filesystem;
  - else a fanotify mark per folder, for ntfs-3g and lowntfs-3g, with `FAN_UNLIMITED_MARKS`;
  - else an inotify watch per folder.

  A rename becomes `moved-from` and `moved-to`, and a folder that appears is marked and then listed, as it may have been filled first. Who made an event comes from `/proc`: mergerfs with the view's mount point among its arguments, or Atoll's own executable. A read error or a malformed record counts as an overflow. `fsx.Descriptor` and `fsx.Readlink` let it mark a folder through its handle and read `/proc` without leaving `fsx`.
- `TestWatcherOnEachDriver` in `test/loopdev` runs each driver by its own way and by inotify, three times on Linux 6.12 and on 7.3. It covers what a user's process, the mergerfs process and Atoll itself do: a folder made with a file at once, a folder moved in with a folder below it and then written in, a folder renamed and then written below, a chmod, a deletion, a create, rename and delete through mergerfs, and nothing from the workdir or elsewhere. `TestWatcherByInotify` runs the watcher's life under the race detector, with no privilege.
- Judged by mutations: the 22 of the grouping, the decoding and the tree are caught, three of them only once three vectors were added; and the four of the system parts, in qemu: no listing of a folder that appears, Atoll's own process read as a user's, the view's too, and no rename asked of a filesystem mark.
- The loop that uses it is the next change (TODO.md).

## The Linux watcher measured (specs.md §15.1, §30)

Commit "Measure fanotify and inotify on each driver", on `watcher-research`.

- Why: the daemon that runs for good needs a watcher, as a pass finds changes by walking every disk, which wakes them; and §30 left open whether fanotify works on ntfs-3g and ntfs3.
- Measured in qemu on Linux 6.12.116 and 7.3.1, on the six drivers, with a throwaway test: report in [docs/research/watcher.md](docs/research/watcher.md). ext4, XFS, btrfs and ntfs3 take a filesystem-wide fanotify mark with every handle flag, and report a rename as one event with both names; ntfs-3g and lowntfs-3g refuse it (`ENODEV`, fsid 0) and take an inode mark per folder; every operation through mergerfs carries mergerfs's pid, on all six; inotify works on all six, with no process; a mark reads nothing from the device; the default queue overflows at 16,384 events; an event waiting costs 328 to 413 bytes of kernel memory, a mark per folder 171 to 194, an inotify watch 155 to 178.
- The maintainer chose an inode mark per folder on ntfs-3g and lowntfs-3g rather than inotify, so that the process is known on every driver, and the bounded queue with a rescan on overflow rather than an unlimited one. §15.1 says so; the question of §30 is closed.
- Also chosen for the daemon, recorded in TODO.md for its own change: the catalog's lock taken per pass, a command waiting for it rather than exiting with 1; `atoll daemon` without `--foreground` the same as with it, under systemd.

## `atoll mergerfs-config` and the break-glass test (specs.md §21.1, §25, §27)

Commit "Print the mergerfs configuration and test break glass", on `mergerfs-config`.

- The maintainer chose: the runtime branches come with the daemon that runs for good, which brings the 30 s mount check they need; an fstab line and `--json` now, the openmediavault settings later; each branch's `minfreespace` from the capacity in the catalog, so that the command reads no disk; a `minfreespace` not larger than the largest file a warning only.
- `atoll mergerfs-config` prints one fstab line, type `mergerfs` (the `mount.mergerfs` helper mergerfs ships): the root folders of the data disks in configuration order, each `<path>=RW,<minfreespace>`, then the options of §21.1 in its order, `category.create=mfs` as every `new_files` works as `most-free` until phase 2b. `--json` gives the branches, the options, the largest file and the line (`api/schemas/mergerfs-config.schema.json`). A disk no scan has found takes the smallest size of `min_free`; with percentages only, its branch has none of its own and the command exits with 4. Sizes in the largest unit of 1024 that holds them exactly. fstab's escapes for a space, a tab, a line feed and a backslash; a branch path with `:` or `,` refused with exit 1, as is a pool with no data disk.
- Measured on mergerfs 2.40.2 (Debian 13) before writing it: the line, mounted through `mount -T`, gives exactly those settings; sizes are powers of 1024; `\040` and `\134` work in branches and mount point; `,` breaks the branches and `:` ends one; `=` works as long as the mode follows; a branch whose folder does not exist stays listed and serves nothing; writing the whole list to `user.mergerfs.branches` puts a branch back at its place.
- `TestBreakGlass` in `test/loopdev`, on every driver: a library on two disks, replicated; mergerfs mounted from the line through `mount -T`, as a boot would; a rename, a deletion, an atomic save and a new file through it; Atoll catches up to no stale copy and no conflict; then its catalog and configuration go, and plain mergerfs mounted again from the line shows the same files, contents and folders, which are the library as the user left it, each disk holds every file in its `media/` and nothing else there, and no disk holds a hidden name. The qemu guest installs mergerfs and sets `ATOLL_QEMU=1`, so that the test fails there rather than skips without it. 25 s for the six drivers on Linux 6.12.
- Found on the way, not a bug: the copy back from an atomic save waits for the `settle` of the file's rule, ten minutes by default, as the replaced copy is decided only then, and its first attempt ends `changed` (LESSONS.md). The test sets `settle = "0s"`.
- Judged by mutations: the 29 of the command, its view and the catalog are caught, one of them by a test of the plain output, which the scenarios cannot read; and two of the break-glass test in qemu, branches at the disk's top and a hidden `Atoll/.tmp`, are caught too. The local CI passes, `test/loopdev` on Linux 6.12.116 and 7.3.1.
- `config.MinFree` replaces the scan's own reading of `placement.min_free`; `value.MinFree.SmallestSize`, with four vectors of an unknown capacity in `min_free.json`; `catalog.Disk.HasCapacity` and `catalog.LargestFile`. Nine scenarios `mergerfs-config-*`.

## Milestones on the roadmap (specs.md §28)

Commit "Name the milestones of the roadmap", on `roadmap-milestones`.

- The maintainer chose them: the alpha at the end of phase 1a, the beta at 1b, and v0.1, the first stable version, at 2a. Each counts every phase before it. No code, contract or vector change.

## The `ntfs` driver of Linux 7.1 measured (specs.md §10, §15.8, §22)

Commit "Measure the ntfs driver of Linux 7.x", on `claude/modest-chatterjee-ab55eb`.

- Why: Debian sid's 7.3.3 kernel, the future Debian 14's, builds no ntfs3, only the new read-write `ntfs` driver, which Atoll makes `readonly` as never measured.
- Measured in qemu on Ubuntu's mainline 7.3.1, beside ntfs3 and ntfs-3g, with throwaway tests and a scratch copy of Atoll that let it write there. Report: [docs/research/ntfs-driver.md](docs/research/ntfs-driver.md).
- Names, times, birth time, identity and modes of its own match ntfs3's, and Atoll's `test/loopdev` suite passes on it, but where ntfs3 is exempt already and for `windows_names`. It differs on hibernated and dirty volumes (read-write under its default `errors=continue`), on `windows_names` (`link` ignores it, `unlink` obeys it), on `link` (which moves the modification time), on files with no mode of their own (`umask=00`), and on names with `" * < > ?` (`ERANGE`).
- No code, spec or vector change: the decisions it needs are at the end of the report, and in TODO.md.

## A scan confirms each disk it walks (specs.md §10, §15.10)

Commit "Confirm each disk a scan walks before recording it", on `scan-confirm`.

- Asked by the maintainer after the failed-mount test: against a mount check that took the folder below a failed mount for the disk, `atoll scan` recorded that folder as the disk, adopting a service's file as a copy and starting the clock of a copy it did not find, where only a pass's confirmation kept the deletions from being decided. §15.6 already said an absence counts only once the disk's identity is confirmed in the same scan.
- What it does: once a walk is over, and before anything it found is written, a scan confirms the disk, as a session does before it writes: a mount check, then the head read past the page cache, whose identifier must be the disk's. Every disk it walks, whatever the walk found, where a pass confirms only one it would conclude something from: a scan records all it finds, a copy it seems to gain as one it seems to lose. One that fails is `missing`, `io-error`, recorded so, and nothing of its walk is written. It costs one read of 68 KiB a disk walked: on 200,000 files on two disks, `scan --full` takes 234.5 s at 177 MiB, as before (225 s to 331 s, 178 MiB to 181 MiB, in earlier runs), and a scan of copies all known 4.6 s at 152 MiB.
- Verified: a unit test of a disk whose mount the table still shows and whose device the read past the cache cannot read, failing before the change; `TestConfirmationOfAFailingDevice` runs a scan too, on ext4, XFS, ntfs3 and ntfs-3g over dm-error; three tests that count the reads past the cache of a session count from the session on, as the scan that sets their bench up reads too. The local CI on this commit, `test/loopdev` in qemu on Linux 6.12.116; kernel.ubuntu.com did not answer for 7.3.1.

## The failed-mount test, on every driver (CLAUDE.md, invariant 4)

Commit "Test a failed mount end to end on every driver", on `failed-mount`.

- `TestAFailedMountIsAMissingDisk`, in `test/loopdev`, so in the qemu job of `test/ci/run.sh`: on ext4, XFS, btrfs, ntfs3, ntfs-3g and lowntfs-3g, a disk holding two copies is unmounted, which leaves the folder of its mount path, where a service that did not notice, as a media server, makes `media/` and writes a file. A scan and two passes, an hour and two days later, past the debounce of a deletion and with the mass-deletion guard set aside, find the disk `missing`, `not-mounted`: nothing decided, nothing run, the copies' rows as they were, no `copy-gone`, trash, job or guard, and the service's folder as it left it. Mounted again, the disk holds what it held, and a pass has nothing to do.
- How it was judged: against a mount check that takes a path holding its parent's mount for the disk, it fails on every driver. Without the service's `media/`, the root folder alone, absent, kept that disk `missing`, so the first version caught the broken check only by its reason. With it, the scan, which writes the catalog and relies on the mount check alone, adopted the service's file as a copy and found one of the disk's two copies gone; only the confirmation of a pass, which reads the head past the cache before any write, kept the deletions from being decided.
- Verified: in qemu on Linux 6.12.116, on the six drivers, and in the local CI; kernel.ubuntu.com did not answer for 7.3.1.

## The mass-modification guard (specs.md §15.6)

Commit "Hold a modification in mass until someone answers", on `modify-guard`.

- The maintainer's decisions: only content modifications count, the ones that make other copies stale, not a `touch` or a `chmod`; a circuit breaker, which lets through what its thresholds allow and holds the rest, rather than reading everything before deciding; a reject makes each file held a `conflict`, as when in doubt both versions stay; keys and a kind of their own, `[modifies.mass_guard]` and `atoll guard confirm|reject --kind modifications`, `deletions` being the default.
- What it does: per disk, the `modified` events within `window`, later than the last answer and without `answered`, with the decisions of the pass, the disk being that of the first changed copy. Before each decision that would make copies stale, the guard counts it, with the thresholds of the mass-deletion guard (`files`, 100, and `percent`, 5%, of the copies the disk had when the pass began); the one that would trip it is not applied, a `modify-guard` event at level `warning` is written, and the pass is held, with exit code 3. While that event stands, a file whose first changed copy is on that disk, and changed after the last answer, waits with nothing read. A confirm answers what changed by the trip, by its change time: those are decided as they would have been, their events `answered` and counted for nothing; a reject decides them as conflicts. A copy that changed since is asked about again.
- Decided in the work, and confirmed by the maintainer: the guard holds until someone answers, with no `clear`, as what it holds stays changed and a hold that ended with the window would let it through; the answer covers a change by its change time, compared with the time of the trip, as nothing records when a pass first saw it; `guards`, `guard list` and the answers say their `kind`, `guard list` giving each disk's two guards, the files counted standing for the files held, which only a look can tell; and `modified` is written only when a copy goes stale, as §9 already said, so that a file with no other copy, which loses nothing, counts for nothing.
- What it meant for the scenarios: 42 gave their guard outputs a `kind`, and `guard list` the mass-modification guard of each disk; the 27 daemon-modify and daemon-metadata scenarios, of one file, set this guard's `percent` to 100%, as they did the other's; `daemon-modify-atomic-save` has no `modified` event any more, as nothing went stale. 4 new: a confirm, a reject, a mass `touch` that trips nothing, and a dry run.
- Found by the mutations: a branch no file could reach, removed, as a file the guard holds is held before it is read; and three gaps closed with tests: both guards of one disk in order, a change on another disk than the first, and a file a confirm answered decided after a new trip in the same pass.
- Memory, on 200,000 empty files on both of two disks, every copy on one of them rewritten (§26): with the guard as it comes, the dry run decides 100 modifications and holds the rest unread, in 21 s at 159 MiB, against 56 s at 174 MiB with the guard out of the way (`files` a million); the pass stopped at the first trash job, 20 s at 172 MiB, against 243 s at 154 MiB. A touch of every copy, which the guard does not count, stays at 178 MiB, in 681 s.
- Verified: the local CI on this commit, race, lint, 496 scenarios (4 new) and `test/loopdev` in qemu on Linux 6.12.116; kernel.ubuntu.com did not answer for 7.3.1, and this changes nothing a driver sees. The CI found the bench of `test/loopdev` held by the guard, as the unit benches and the scenarios first were (LESSONS.md). Unit tests cover the threshold and nothing read past it, the percent, a file with no other copy, a confirm that asks again about what changed since, a crash after a modification, the window, the order of the guards, a change on another disk than the first, and a file answered after a new trip. Of 33 mutations, 32 are caught, and 1 went with the branch no file could reach.

## The mode applied to the other copies (specs.md §9, §15.4, §15.10, §17.2)

Commit "Apply the mode of a metadata change to the other copies", on `permissions`.

- The maintainer's decisions: the mode only, all of its 07777 bits as the copy pipeline gives them, and the owner left for later; the rows the migration finds get their mode from the next look; a copy whose mode is unknown, on a disk the pass cannot write to, goes stale, as for a time.
- What it does: migration `0003_copy_mode.sql` adds `copies.mode`, 0 to `07777`, NULL until known. The scan, the copy pipeline and a decision record it with the copy's times; a look records it for a copy `there`, unchanged since its row, whose row has none, as a `chmod` moves the change time. A `metadata` decision applies the mode of the changed copy whose mode moved last, by change time, to each other copy that holds another or an unknown one, through an operation `chmod` journaled with a `chmod` event that holds the mode, before the `utimens` of the same copy, copy by copy by position. A copy the pass cannot reach goes stale, refreshed from the copy that gives the time, or the mode when no time moved. The reprise finishes on the mode its event holds, undoes with no warning on the mode its row says, and with one otherwise.
- Measured on both kernels: ext4, XFS, btrfs and ntfs3 keep the mode; ntfs-3g and lowntfs-3g, mounted without `permissions`, keep `0777` whatever is asked. The row records what the disk holds, and the difference is then one that was already there: the next pass applies nothing.
- A dry run records no mode, so a copy whose row has none reads as unknown and gets an operation that the pass after it would not run: added to the dry run's limits (§15.12).
- The conformance runner: a step `chmod`, and `mode` in a tree, both in octal digits. 6 new scenarios: a `chmod`, a `chmod` with a `touch`, a disk away, a dry run, and a crash at `journaled` and at `mode-set`.
- Found by the mutations: a condition that only a changed copy has a look's mode made redundant, removed, and two gaps closed with tests: setgid, and the `INSERT` of a copy whose pending row went meanwhile.
- Memory, on 200,000 empty files on both of two disks (§26): the pass that finds every row without a mode, as the migration leaves them, records them all at 152 MiB, in 6.1 s against 5.0 s for a pass with nothing to do; with every copy on one disk `chmod`, the pass that applies 200,000 modes peaks at 176 MiB, in 720 s, and its dry run at 175 MiB; with every copy touched, 174 MiB, in 720 s; rewritten, the dry run at 174 MiB and the pass stopped at the first trash job at 166 MiB.
- Verified: the local CI on this commit, race, lint, 492 scenarios (6 new) and `test/loopdev` in qemu on Linux 6.12.116. On 7.3.1 only the tests of the mode and the time ran, in a targeted run before the last changes, which only added tests and removed a redundant condition: kernel.ubuntu.com did not answer for the full run, which is still to do. On ext4, XFS, btrfs, ntfs3, ntfs-3g and lowntfs-3g: a `chmod` applied by a pass and recorded as the disk holds it, read through a cold cache, with nothing left for the next pass, and a crash at `mode-set` resolved by the reprise with no warning. Unit tests cover a `chmod` that reaches the other copy, a `chown` alone, a mode the look records before the decision and a dry run that cannot, a changed copy with no mode, a mode then a time, setgid, an operation `chmod` left planned, and the order by position. Of 40 mutations, 39 are caught, and 1 went with the redundant condition it changed.
- Left for later, in [TODO.md](TODO.md): the owner, with `copies.uid` and `copies.gid` and operations `chown`.

## The precision of each disk's times, and the comparisons that need it (specs.md §9, §10, §15.4, §17.2)

Commit "Compare times at the precision each filesystem holds them", on `time-precision`.

- The maintainer's decision: the precision comes from the filesystem, read with its identifier, never measured by writing to the disk and never set in the configuration.
- What it does: `disks.time_precision_ns`, in the schema since the start and never written, now holds how finely a disk holds a time: 1 for XFS and btrfs, 100 for NTFS, and for ext4 1, or 10^9 when the superblock says its inodes are 128 bytes (revision 0 at 1100, or the inode size at 1112). The mount check reads it with the identifier, and the disk row records it with the state; a check that reads no head keeps what an earlier one read, and a disk never seen has none, which compares exactly. The conformance build, which has no head, takes it from the disk's `fs`.
- Two times are the same at a precision when each, divided by it and rounded down, toward the past, gives the same number, which is what a filesystem that holds only multiples of it keeps (`solver.SameTime`, `conformance/vectors/same_time.json`, whose cases before the epoch tell rounding down from truncating).
- Where it counts, the two comparisons of a time across filesystems: the metadata plan gives no operation to a copy that holds the time at the coarser precision of its disk and of the disk that gives it; and the reprise of an operation `utimens` finishes when the disk holds the event's time at its own precision. Until now NTFS made that reprise undo the operation with a warning, and the next pass applied the time again. Every other comparison of a time is of a copy with its own row, which one filesystem wrote, and stays exact.
- Memory, on 200,000 files every copy on one disk touched, then rewritten (§26): 176 MiB for the pass that applies every time, in 873 s, against 174 MiB and 862 s before; 171 MiB for its dry run, 175 MiB for the dry run where every copy was rewritten, and 171 MiB for the pass that rewrites them all, stopped at the first trash job, against 160 MiB before, of which nothing new but an integer per copy decided stays: the collector's timing.
- Verified: the local CI (`test/ci/run.sh`) on this commit, which caught a field `gofmt` would align, and in qemu on Linux 6.12.116 and 7.3.1, on each of ext4, ext4 with 128-byte inodes, XFS, btrfs, ntfs3, ntfs-3g and lowntfs-3g: the precision the head gives is what the driver keeps of a time set to the nanosecond, the ext4 bytes of the vectors are what `mkfs.ext4` writes, and a pass killed once the time is set is finished by the reprise, with no warning, on all six drivers. 486 scenarios (1 new: `daemon-metadata-same-tick`). Of 26 mutations of the new code, 26 are caught.

## A local CI: every check of a commit, with only git and Docker

Commit "Run every check of a commit locally, with only git and Docker", on `local-ci`.

- Why local: the repository has no CI, and a hosted runner has no loop device, no device-mapper and no ntfs3, which `test/loopdev` needs. A self-hosted runner on a public repository would run the code of any fork's pull request on this machine.
- `test/ci/run.sh [--report] [--jobs "..."] [REV]` copies the commit, as git holds it, into a Docker volume through `git archive`, never the working tree, and runs five jobs at once, each in a container of its own and a copy of its own: `race` (`make test-race`), `integration` (`make test-integration`, which has no test of its tag yet), `conformance` (`make conformance`), `lint` (`make lint`, then the loopdev build), and `qemu` (`make qemu-build`, then `test/qemu/run.sh` on both kernels, with `/dev/kvm` when the host has it). Each job writes its log to `bin/ci/<commit>/`; the exit code is 0 when every job passed. The module cache is a volume that outlives the run; everything else goes when it ends, Ctrl+C included.
- `--report` posts each job to GitHub as a commit status, `local/<job>`, pending at the start and passed or failed at its end, which a pull request on that commit lists among its checks.
- Runs from Linux, macOS (bash 3.2) and Git Bash on Windows, where `MSYS_NO_PATHCONV` keeps `/src` a path of the container.
- Verified on the commit of pull request #8: the five jobs passed, in 23 minutes, qemu the longest without KVM; race 3 min 38 s, conformance (485 scenarios) 1 min 48 s, lint 59 s.

## Metadata changes: the modification time applied to the other copies (specs.md §15.4, §17.2)

Commit "Apply the time of a metadata change to the other copies", on `metadata`.

- The maintainer's decisions:
  - the modification time only: no row records a copy's mode, so a change of permissions could not be told from a difference that was already there; the permissions come with a column of their own, in [TODO.md](TODO.md);
  - a copy on a disk the pass cannot write to goes stale, and is refreshed once back, its time with it: no new kind of job;
  - of copies touched to different times, the last change, by change time, gives the time, the first by position on a tie.
- What it does: a `metadata` decision whose changed copy's time is no longer what its own row says applies that time to each other copy that is not stale and holds another (`solver.MetadataPlan`, `conformance/vectors/metadata.json`): through an operation `utimens` where the pass can write and its look reached the copy, by making it stale otherwise. The operation is journaled with a `utimens` event that holds the time, looks at the copy as its row says, sets the time on the file open for reading, the access time left alone, syncs it, and records what the disk holds. The decision is recorded only once every time is applied: recorded first, a crash before the time reached a copy would leave a difference that reads as one that was already there, which nothing would ever apply. A time that did not get through leaves the file to the next pass, which reads it again and finds what was applied already. The operations run with their decisions and are listed first in `operations`, as the output gives every decision before the first operation.
- The reprise of a `utimens` reads the time from its event: the copy, synced, with that time finishes; with its row's, nothing happened; anything else changed since, `undone` with an `op-undone` event.
- Also decided in the work, confirmed by the maintainer on 2026-11-12: the outcomes `applied`, `changed`, `io-error` and, in a dry run, `to-apply`; a stale copy's source is the copy that gives the time; a time that NTFS holds less finely than its event reads, in a reprise, as changed, and the next pass applies again what it finds, until `time_precision_ns` exists.
- What it meant for the scenarios: the two of the previous pull request that touched a copy now apply the time; 6 new `daemon-metadata-*` scenarios pin a disk away, the last change, a touch through mergerfs, a dry run, and a crash at each point.
- Found by measuring: four statements found a copy by its place with one OR over `copies` and `files`, `c.old_lpath = ? OR (c.old_lpath IS NULL AND f.lpath = ?)`, which leaves SQLite no index but `copies_by_disk`: each read every copy of the disk. Applying 200,000 times went at 6 a second, at full CPU, on its way to nine hours. The end of a trash (`finishCopy`), of a rename (`renameRow`) and `CopyOf` paid the same, unseen because the measure of the previous pull request stopped at the first trash. They now share one condition, a union that reads one index per half (`copies_old`, then the unique path of `files` and the primary key of `copies`), and `TestAQueryByPlaceReadsOnlyThatPlace` pins the four plans.
- Memory, measured on 200,000 empty files on both of two disks, every copy on one of them touched, then rewritten (§26): the pass that applies the 200,000 times peaks at 174 MiB, in 862 s, three syncs a file (the operation journaled, the file, the operation done); its dry run at 178 MiB, as it lists an operation for each. On 50,000 files the same pass takes 212 s: linear. The dry run where every copy was rewritten peaks at 176 MiB, and the pass that rewrites them all, stopped at the first trash job, at 160 MiB. This machine ran about a third slower than for the previous pull request: the scan of 200,000 files took 331 s, against 234 s.
- Verified: `go test ./...`, the race detector, lint on the default, conformance and loopdev builds, 485 scenarios (6 new), and `test/loopdev` in qemu on Linux 6.12.116 and 7.3.1, with a time applied by a pass on ext4, XFS, btrfs, ntfs3, ntfs-3g and lowntfs-3g, NTFS holding it to 100 ns, and the next pass quiet. Unit tests cover the time that reaches the other copy, a chmod alone, a copy changed before its time, a disk lost during a time, a time that fails, a crash at each of the two points of a time, a reprise on a copy changed since or of another size, the last touch that gives the time, a time not applied twice, a copy on a read-only disk or unreached by the look, the stale copy refreshed from the copy that gives the time, an operation left planned that holds the change, and the access time kept. Of 39 mutations of the new code, 36 are caught, 2 went with the code they changed, two checks that an earlier one already made (a disk the pass can write to, a trash job not yet planned), and 1 changes nothing: the reprise of an operation with no event, which is written in the operation's own transaction.
- Left for later, in [TODO.md](TODO.md): the permissions, with `copies.mode`; `time_precision_ns`, for the reprise.

## Modifications: a copy edited or replaced (specs.md §15.4, §15.12)

Commit "Decide modifications in each pass of the daemon", on `modifications`.

- The maintainer's decisions:
  - a changed copy waits for `settle` counted from its change time, as its walk found it: no new column, and a file still being written is not read;
  - a file never hashed whose copy changed and kept its size is compared by reading a copy that did not change, two reads for a `touch`, rather than re-copied whole;
  - two pull requests: the content first; then metadata-only changes applied to the other copies (`utimens`, `chmod`), until when only the row of the copy that changed follows it;
  - no guard against modifications in mass yet: the copies overwritten stay in the trash for `retention`, and [TODO.md](TODO.md) records the risk.
- What it does: after the decisions of propagation, each pass looks at every file a copy of which is `edited`, or `replaced` by a regular file, by path in byte order, one file to a transaction. A pure function (`solver.Modify`, `conformance/vectors/modification.json`) says what to read, then what it means: another size is `modified` with nothing read; the same size is read, and compared with the file's hash or, with none, with a copy that did not change: the same content is `metadata`, another is `modified`, two contents among the changed copies, or a stale copy changed, a `conflict`. `modified` makes the changed copies the user's and the current version, every other copy `stale`, drops every queued job of the file, and queues a trash job `overwritten` for each stale copy. The work of the same pass sends it to the trash, and its row then becomes a pending copy with a `replicate` job from the copy that changed, which the same pass runs, whatever the mode or `settle`: the room and the name stay held, and no new kind of job is needed. A read runs at the lowest best-effort level, as a copy does.
- Also decided in the work, confirmed by the maintainer on 2026-11-12:
  - a file waits, with no line, while a copy of it is moved, gone, uncertain or own, or its row says moved and this look could not get to it, while a job of it is `running`, and while a changed copy is not due, which counts in `waiting`; a file in conflict counts in nothing;
  - a copy replaced by what is no regular file, or by the file another copy of its disk moved to (an overwriting move), is no modification: the file waits, and the limits of §15.12 stand;
  - the dry run counts every change as due, as it does every absence, reads what a real pass would, and lists the trash jobs that would follow, not the copy that only the next pass lists;
  - a `modified` event, with the disk of the first changed copy; the trash entry of an overwritten copy has no hash, as it holds a version the file's hash no longer describes;
  - a conflict writes nothing to a disk, and its changed copies lose `verified_ns`;
  - an overwrite gives way to a deletion decided later, and goes, `canceled`, when the file has no copy `ok` or `surplus` left: the stale copy is then all that is left of it, and stays;
  - a replicate job that ends with no copy of its file left takes the file with it.
- What it meant for the scenarios written before: 15 of them have a copy edited, which the conformance build's clock, behind the real one, finds not yet due. Each counts one more in `waiting`, and `next_due`, a real time nobody can write down, reads `${ANY}` where it was `null`. Two dry runs, for which everything is due, now decide the modification too, and in one of them that drops the job whose `changed` it was written for: its description says so. 14 new `daemon-modify-*` scenarios run with a clock past the real one, so that what the kernel times is past `settle`; the wait itself is pinned in Go, with the bench's clock set from the real change time.
- Found by testing:
  - an overwrite would have sent to the trash the stale copy of a file whose only current copy was then deleted, which was all that was left of it (a unit test, while writing the tests of a stale copy's job);
  - the test of the read's I/O priority kept the first value it saw unless it was 0, which is what a thread with no priority set reads: it kept the copy's read that followed, and passed with the priority removed (a mutation);
  - the chunks of a file that a metadata change gives a hash were checked nowhere (a mutation); the size a modification wrote was written again by the end of the pass, and went (a mutation).
- Memory, measured on 200,000 empty files on both of two disks, every copy on one of them touched, then rewritten (§26): the pass where every copy was touched peaks at 175 MiB, in 202 s, nearly all of it one transaction a file; its dry run at 153 MiB. The dry run where every copy was rewritten first peaked at 188 MiB, close to the 190.7 MiB of the budget, as it kept a whole operation for each trash job it would list, and a map of the files changed: a place and a flag for each, and a sorted list, bring it to 174 MiB. The pass that rewrites them all, stopped at the first trash job, peaks at 157 MiB, in 250 s.
- Verified: `go test ./...`, the race detector, lint on the default, conformance and loopdev builds, 479 scenarios (14 new), and `test/loopdev` in qemu on Linux 6.12.116 and 7.3.1, with a copy edited on ext4 and the stale copy on each of ext4, XFS, btrfs, ntfs3, ntfs-3g and lowntfs-3g sent to that disk's trash and refreshed, read back through a cold cache. Of 84 mutations of the new code, 80 are caught. The 4 left are second guards: a copy replaced by what is no regular file waits at the gate and again at the look before the read; the look checks the kind of file and its change time; the reprise of an overwrite reads its reason from the trash before it looks for the job of that reason; and an overwrite stands only for a stale copy, which only a stale copy has.
- Left for later, in [TODO.md](TODO.md): metadata-only changes applied to the other copies; a guard against modifications in mass; the overwriting move, the swap of two names and a copy replaced by what is no regular file; `deletes.direct = "restore"`.

## The daemon runs the copies: the `deferred` mode (specs.md §12.3, §14, §14.1, §15.12)

Commit "Run the due copies in each pass of the daemon", on `daemon-replicates`.

- The maintainer's decisions:
  - a copy in the daemon's output is an operation of kind `replicate`, with its target as `disk` and a `source` (`api/schemas/daemon.schema.json`);
  - with no watcher, a copy is queued once `settle` has passed since the file was first seen (`files.first_seen_ns`); `immediate` and `scheduled` fall back to that until they exist;
  - the files whose copy waits for `settle` count in `waiting`, and in `next_due`, so that a timer knows when to run the next pass;
  - a copy runs at the lowest level of the best-effort I/O class, not the idle class, which a load that never stops would starve; `atoll replicate --force` keeps the normal priority.
- What it does: after the work of propagation, each pass plans, queues the copies of every file not in `manual` mode whose `settle` has passed, then runs every job due, highest `priority` first, then in the order queued, one at a time, through the pipeline of `atoll replicate`. `io-error` and `disk-unavailable` make the exit code 4, as a trash job's do. A dry run lists the jobs due and those the plan would queue, as `to-replicate`, and writes nothing of its plan. The files are worked out again only when a copy ran.
- What it meant for the scenarios written before: 109 of them changed, 103 only in `waiting` and `next_due`. In the 6 others a copy really ran, as the spec wants (a file `kept` below its level, the two files of a split conflict, a rejected hold of the guard), and made a later check of the scenario moot. Those 109 test propagation: their configuration now turns replication off, a one-line change each, and 9 new scenarios pin what copies and propagation do together. A deletion waiting for its debounce is not copied back, a hold of the guard copies nothing however long it lasts, and a reject or a `kept` decision gets its copies back.
- Memory, measured on 200,000 empty files all on one of two disks (§26): the first pass, where every copy waits for `settle`, peaked at 196 MiB, past the budget, as it kept every job of the plan and a second list of them. The solver now hands each file's jobs on as it makes them (`solver.PlanEach`), and the queue takes them a thousand at a time: 182 MiB, against 183 MiB for the same pass on `main`. A dry run with every copy due peaks at 180 MiB, a pass that queues them all at 181 MiB (stopped at the first copy, through the conformance build's `crash_at`), and a dry run with every copy queued at 177 MiB. A pass that copies 2,000 empty files takes 21 s, about 10 ms a copy, at 34 MiB.
- Verified: `go test ./...`, the race detector, lint on the default, conformance and loopdev builds, 465 scenarios (9 new), and `test/loopdev` in qemu on Linux 6.12.116 and 7.3.1, with a copy made by a pass on ext4, XFS, btrfs, ntfs3, ntfs-3g and lowntfs-3g. Unit tests cover the settle boundary to the nanosecond, the earliest of what waits, a copy queued for later, each failure and whether it is partial, a pass killed at each of the six points of a copy, the I/O priority read on the copy's own thread, and a sink or a cancel that stops the copies. Of 63 mutations of the new code, 60 are caught. The three left change nothing a test can see: the check of the context between copies, as the next catalog call fails on it before any disk is touched; dropping what the walks found before the plan, which only frees memory sooner; and a job `running` with a future `not_before_ns`, which only a crash of `--now` on a job not yet due leaves.
- Left for later, in [TODO.md](TODO.md): `immediate`, with the watcher, and `scheduled`, inside `[replication].window`; `max_parallel_per_disk`; the `manual_alert_after` alert.

## The copy pipeline: `atoll replicate` (specs.md §9, §14, §14.1, §14.2, §14.3, §17.2)

Commits "Specify the copy pipeline and atoll replicate", "Copy a file with atoll replicate", "Test what each failure of a copy leaves, and re-read replicas with --force", "Close what the mutations of the copy pipeline found" and "Record the copy pipeline in DONE.md and LESSONS.md", on `copy-pipeline`.

- The maintainer's decisions:
  - `atoll replicate <path> [--now | --force] [--dry-run]` first; the daemon and the `deferred` mode come next;
  - a queued copy is a `replicate` job and a `copies` row `pending` on its target, which holds its room and its name for later plans;
  - `api/schemas/replicate.schema.json` has prune's shape and exit codes, and the command takes no `--yes`, as it creates and moves nothing.
- What it does:
  - `atoll replicate` plans as a scan does, with the `manual` files below the path released, and queues their copies. With `--now` it runs every job of those files; `--force` also re-reads first the copies Atoll made of them, as `scan --full` does.
  - Each job is one operation `create`: a look at the target, the part `<op id>.part` made in `<workdir>/tmp/` with mode 0600, the copy with its chunk hashes recorded every 64 MiB, a look at the source again, its times and attributes restored, the re-read without cache against the chunk hashes, the whole hash recorded before anything moves, the parents made like the source's folders, the rename without replacing, each name synced before its folder, and one transaction that makes the copy `ok`, verified, gives the file its hash when it had none and verifies the source.
  - A resume keeps the chunks of its part that still match their hashes and reads the source from the first one that does not. The reprise of a `create` follows a table of its own, and `tmp/` loses what operations done or undone left when a command that writes opens the disk.
  - A pending copy is nothing on its disk: a walk is not told of it, the propagation does not see it, `--full` reads nothing of it, the guard does not count it. It follows its file's rename, goes with its last copy, and a plan cancels it with its job when it no longer keeps it. A file found where one was pending takes its place.
- Found by testing:
  - a resume truncated its part to whole chunks, so the last short chunk grew to 4 MiB and the copy read back wrong (a scenario);
  - `no-room` ended its job instead of putting it back in the queue (a unit test);
  - on ntfs3, a device that failed at `target-synced` left two names to the copy, the reprise rightly concluded nothing, and the run then judged the job anyway: its own copy at the target read as a conflict, and the job and its pending copy went. A job still `running` after the reprise now waits (qemu, Linux 6.12 and 7.3).
- Measured in qemu, Linux 6.12.116 and 7.3.1: a file of three chunks and one below a private folder copied both ways between ext4 and each of ext4, XFS, btrfs, ntfs3, ntfs-3g and lowntfs-3g, read back through a cold cache; and a device failing at each of the six points of a copy on ext4, XFS, ntfs3 and ntfs-3g, then the next run resuming or finishing it.
- Also decided in the work, and confirmed by the maintainer:
  - no `failed` state yet: `conflict`, `path-taken` and `name-refused` end the job with its pending copy, and the next plan decides again; `io-error`, `changed` and `no-room` put it back, `changed` not before `settle`;
  - a file in `conflict` is never queued, and lines are given for jobs only;
  - the outcome `queued`, for a run that only queues;
  - the job's payload `{"part", "blake3"}`, and the event `replicate`, with `source`;
  - a look at the target before anything is copied, so that a large file is not copied only to meet a taken path at the rename;
  - `--force` re-reads the copies Atoll made, before it plans;
  - `no-room` drops the part, as it would hold the room it lacks;
  - `tmp/` is cleaned when a command that writes opens a disk, so that no disk is woken for it;
  - with `replication.enabled = false`, nothing is queued or copied, it says so on standard error, and exits with 0;
  - `--now` ignores `not_before_ns` as well as the window.
- Memory and time, measured on 200,000 empty files all on one of two disks (§26): `atoll replicate` on the folder that holds them peaks at about 175 MiB for a dry run and 178 MiB for queuing every copy, in 8 s; with every copy pending a scan peaks at 179 MiB and a pass at 180 MiB, `status` at 21 MiB; 500 copies with `--now` take 9 s. All under the 190.7 MiB of 200 MB.
- Left for later, in [TODO.md](TODO.md): the daemon, which runs due jobs; `max_parallel_per_disk` and the low I/O priority; refresh jobs; the creation time on Windows.
- Verified: `go test ./...`, the race detector, lint on the default, conformance and loopdev builds, 456 scenarios (13 new), and `test/loopdev` in qemu on both kernels. Of 54 mutations of the pipeline, 53 are caught; the last makes a source that shrinks during the read go on, which the look after the copy then finds changed.

## `test/loopdev` in qemu: `make test-qemu`

Commit "Run test/loopdev in qemu, with only Docker on the host", on `test-in-qemu` (pull request #4).

- The maintainer's decision: the throwaway qemu scripts that found the ntfs3 loss go into the repository, so that `test/loopdev` runs on the maintainer's Windows machine.
- What it does: `make qemu-build` builds `test/loopdev` static, with `go.mod` and `conformance/` beside it, as the tests look for them above their folder. `test/qemu/run.sh`, in a throwaway `debian:13` container, makes an initramfs of that container's files for Debian's kernel and for a mainline one (v7.3.1 from kernel.ubuntu.com), boots each in qemu with no network, and runs the tests there as root. Its exit code is the worst of the guests'. `make test-qemu` does both on Linux, with KVM when there is one; `test/qemu/README.md` gives the two Docker commands for Windows, and what can be chosen: kernels, tests, counts.
- Verified from PowerShell on this machine, exactly as the README says: the whole of `test/loopdev` passes on Linux 6.12.116 and 7.3.1, `TestNameCorpus` included, which the throwaway scripts could not run. Without KVM, a whole run took about 20 minutes. A first try showed the exit code at work: qemu refused its options, and the run ended with 2, each guest reported as having said nothing. Both scripts pass shellcheck, and a run with the variables (`RUN`, `VERBOSE`) after its fixes passed again on both kernels.

## Names made durable on NTFS (specs.md §14, §15.12, §17.1, §17.2)

Commit "Sync what a move names before its folder", on `propagate-renames-deletions` (pull request #3). Found by running `test/loopdev`'s rename tests in qemu, which the propagation work had left for later.

- The maintainer's decisions:
  - what a name leads to is fsynced before its folder on every filesystem, after every move, and the copy pipeline does the same: invariant 7 now says "fsync the file at its new name, then its folder";
  - a folder made is synced too, before its parent, as the trash's showed the same fault;
  - a reprise that finds two names leading to one file that `unlink` cannot separate concludes nothing (below).
- What it does: `fsx.Dir.SyncEntry` opens what a name leads to and fsyncs it. A rename of a file or a folder, a move to or from the trash, the link path, a move back, and a folder made each sync it before the folder that holds it. A reprise syncs what it finds at the target first, the file again once it takes the other name away, and the file at the source before an undo. A failed fsync loses the disk like any other. A name that no longer leads to what moved has nothing to sync. What Atoll may not open keeps the sync of its folder alone, with a warning.
- Why, measured in qemu on ntfs3, Linux 6.12 and 7.3, with a device that fails right after the target folder's fsync: on NTFS a file or a folder keeps its names in its own record too, which ntfs3 does not write when only the folder is synced. A file renamed and a folder renamed were gone under both names, the folder with a file no other disk held. An entry of the trash and a file restored answered `ESTALE` with their operations `done`, and a folder of the trash just made answered `EINVAL`. The tests of a dying device skipped those three, as a name and its record that disagree are a repair's to judge.
- With the fsync, every test of a dying device passes on ext4, XFS, ntfs3 and ntfs-3g, twice on each kernel, with a rename into another folder added, and so does the rest of `test/loopdev` but `TestNameCorpus`, which finds no `go.mod` in the guest. The fsync costs about 1 ms a move, and about 1.5 ms while another file is written. `syncfs` after each folder fsync also fixed the rename, at 35 to 120 ms a move while another file was written.
- One case stays, on ntfs3: a move to or from the trash that the device interrupts between the syncs of its two folders leaves both names leading to the file, with one link, and `unlink` of either answers `ENOENT` and removes nothing (measured). The reprise took that `ENOENT` for a name gone, recorded the move done, and left the other name there, for a later purge to remove the name the file really has. It now concludes nothing: the operation stays `planned` with a warning, every run on that disk exits with 4, and only a repair of the volume can say which name is the file's.
- Verified: `go test ./...`, the race detector, lint on the default, conformance and loopdev builds, 443 scenarios, and `test/loopdev` in qemu on both kernels. The first new tests failed on the code before, in `fsx`, `jobs`, `trash` and `test/loopdev`. Of 25 mutations that take a new sync or check away or change how its errors read, 24 are caught; the last ignores an open of a folder that the next call opens again, and fails on.

## Propagation: `atoll daemon --until-idle` and `atoll guard` (specs.md §15.1, §15.3, §15.6, §15.10, §15.12, §17.2, §25)

Commits "Propagate renames and deletions, with atoll daemon and atoll guard", "Hold a folder back while one renamed inside it waits", "Test what the mutation runs left standing, and fix a dry run", "State a folder rotation while one waits as a limit", "Make the propagation schemas say what their descriptions say", "Read below a folder by its paths, not by its disk", "Bring every pass on 200,000 files under the 200 MB of §26", "Write down what a port from the documents alone could not tell" and "Tell an edit from what was linked by each of size, time and inode alone", on `propagate-renames-deletions` (pull request #3).

- The maintainer's decisions:
  - the command is `daemon --until-idle`, which does all the work that is due once and exits, so a timer can run it until the watcher exists. It takes no `--yes`: starting it is the consent, and the mass-deletion guard is the safety (invariant 17 now says so);
  - no modifications yet: a copy `edited` or `replaced` is only reported until the copy pipeline (§14);
  - a deletion sends to the trash only the copies that hold the same content;
  - the copies below a folder follow it only when its rename is real;
  - a copy that moved is read again before it counts as verified, and the chunks go with the hash;
  - the guard holds the copies of its disk seen before it tripped;
  - a rotation on one disk while the folder that left waits is a stated limit (below).
- What it does: each pass looks at every disk, gives each copy a verdict (`copy_verdict.json`), and decides a file once every copy that moved or went has done so for `deletes.debounce` (`propagation.json`). A rename made on one disk is carried to the others; a folder renamed is one operation on each other disk where it can be, and its copies follow one by one where it cannot. A deletion sends the other copies to the trash. `atoll guard list`, `confirm --disk` and `reject --disk` answer the guard of §15.6 (`guard.json`). Renames and folders are journaled like the trash, with their reprise and crash points.
- Contracts, as agreed: `schema/migrations/0002_propagation.sql` (`gone_ns`, `gone_kind`, `old_lpath` and `old_lpath_key` on copies, `file_key` and `btime_ns` on entries), `api/schemas/daemon`, `guard-list` and `guard-answer`, `moved` and `path_taken` in `scan`, `old_path` in `status`.
- Eleven adversarial reviews of the code, as each round left it, confirmed 62, 44, 32, 32, 23, 22, 13, 18, 16, 7 and 2 findings, each fixed with a failing test first or stated in the spec as a limit. None since the sixth round lost data; the one in the sixth sent a copy to the trash. The eleventh found a folder decided while one renamed inside it waited, which left what only another disk held below the inner one there for good, and a dry run that gave a queued trash job the guard of another source.
- Mutations: 381 of 390 caught on the code before the third commit. With the run before it, 20 mutants were left. 15 now have a test, each seen to fail under its mutant. One was a bug: since a fix of the eleventh round, a dry run hid the trash job of a refused rename once its file was decided deleted, which the real pass keeps and lists. 3 are equivalent: in `cannotLeave` a copy that stays always has its row on its file, in the dry run's guard a gone copy is never something left beside itself, and a folder around one that waits for its own copies counts the same copies. One is left, tied to the limit below.
- A limit found while testing the mutants, stated with the maintainer: `mv Dx N; mv D Dx` on one disk, while Dx waits for a copy below it on another disk, gives D's entries Dx's name, so the next look reads Dx as a folder made at N, and what only other disks hold below Dx stays there, in the folder now named Dx. Nothing is lost (`daemon-folder-rotation-while-waiting`). The fix is in [TODO.md](TODO.md). Without a wait the rotation works, and so does a swap of two folders on one disk.
- Decided without the maintainer, confirmed on 2026-11-12:
  - a copy found moved, then gone, restarts its clock;
  - `guard confirm` restarts the clock of the rows it leaves out, and `guard reject` also takes the rows of copies that have not yet followed a rename;
  - a file's hash goes when no copy left is verified;
  - a disk the configuration no longer lists gets no line and no exit 4 for what waits on it, until `disk remove` exists;
  - a moved copy on a disk out of reach waits without being counted in `waiting`, as counting it would change what that field means;
  - on NTFS, a collision with a name that has no row yet is a stated limit: the next walk reports it;
  - the dry run's differences from a real pass are a class of limits in §15.12;
  - a trash job queued for a deletion stands when its file is decided again, and the job of a refused rename stands beside a deletion's: two clauses in the Deletion row of §15.12;
  - a disk's folder renames are decided in the order of their jobs. Nothing says in what order two disks acted, so across disks a copy decided below a folder's old name is below it like any other, and the next look finds it `moved`.
- Verified: `go test ./...`, the race detector, lint on both builds, 443 scenarios (269 new) and every vector. 2,352 real outputs against their schemas, 809 of them from `daemon` and 28 from `guard`, which refuse 303 broken ones. 18 of those passed at first, each ruled out by a description, §10 or §15.6: a disk's reason as its state says, the format of `next_due` and `up_to`, `up_to` as `done` says, and no paths on a guard not tripped. The schemas now enforce them, and nothing a command prints changes.
- Memory and time, measured again on 200,000 files with two copies each: the worst passes peaked at about 212 and 211 MiB, past the 200 MB of §26 (190.7 MiB), against 205 and 208 MiB before the last seven rounds, and a scan after a rename of a folder holding everything at 195 MiB. The peak was in Go's heap, and up to 25 MiB above what sampling saw: lists of every file grown entry by entry, each for a moment twice its size. Sized once, with no job list for a plan whose jobs nothing runs, one string where the catalog gives a path twice, a compact row for each copy not where its row says, and a scan that lets go of what its looks found before its plan, the highest is now about 187 MiB for that scan and about 181 MiB for a pass. The time had grown, unseen by any test: the passes after every copy of a disk went took about two minutes each, against 7 to 13 s. Four queries that read below a folder were led by the disk, and so read every copy of that disk, once for each of the 800 folders it lost, and a fifth read the old path of every copy of every disk. They now read by the folder's paths, a test pins their plans, and those passes are back at about 8, 7 and 15 s.
- What the first commits left, done since:
  - `test/loopdev`'s rename tests ran in qemu on Linux 6.12 and 7.3, and found ntfs3 losing a renamed file: the section above.
  - The older mutation lists ran again on the code as it is: 1,006 mutations. Six survived their own list's judge; judged by every test, three were caught, and the three left were real: on the link path to the trash, an edit that kept the modification time, an edit of the same size, and a replacement of the same size and time each passed for what was linked. Each now has a case in `trash`, and without the inode the replacement lost its only name. Fourteen mutations no longer compiled; rewritten to compile, each is caught.
  - A port of the three propagation vector files, written in Python from the documents alone, passes their 188 cases. It had to guess in 14 places, each now a sentence of specs.md or of the vectors' README, and a second read by the port found each settled. Three gaps no vector reaches are in [TODO.md](TODO.md), and so is a port judged on the scenarios, which needs a port of the daemon.

## The journal and the trash: `atoll prune` and `atoll trash` (specs.md §6.1, §9, §10, §15.10, §17, §25)

Commit "Send surplus copies to the trash, with atoll prune and atoll trash", on `main`. The first code that writes to a disk.

- The maintainer's decisions:
  - `prune` and `trash` come first, ahead of the copy pipeline, as they need only a move inside one filesystem;
  - a session confirms each disk by reading its head past the page cache, once, before it first reads or writes that disk. The handle comes first, so that what is confirmed is what the handle leads to;
  - a copy goes only if the copies that stay reach the level, unchanged and verified, and the level is the larger of the file's row and what the configuration says now;
  - a verified copy with another hash loses `verified_ns`, or `prune` would count it as good and send the good one to the trash;
  - an exclusive `flock` on the catalog for every command that writes;
  - a reprise tells a file edited in place from an intact one, and starts the retention of what it finishes again;
  - a restore looks first and journals nothing for `path-taken` or `name-collision`. A missing folder is an operation of its own, `kind = mkdir`, and the root is never made;
  - a purge keeps another file at the entry's place, with `changed`, and so does a restore;
  - another root, workdir or id is refused while a disk holds copies, entries, trash or planned operations;
  - a walk reports a name that `lstat`, `openat` or `readlink` cannot reach in `unreadable`, with no row, and the disk stays `online`.
- What it does: one journaled operation per copy, in `internal/catalog/ops.go`, `internal/ops` and `internal/trash`, moved by `renameat2` with `RENAME_NOREPLACE` or by `link`, `fsync` and `unlink`, then synced. A reprise resolves what a crash left from a table of what each name holds. Crash points in the conformance build (`crash_at`) make a `kill -9` at each step a plain scenario.
- Measured on ext4, XFS, btrfs, ntfs3, ntfs-3g and lowntfs-3g, on Linux 6.12 and 7.3, in throwaway probes and now in `test/loopdev`:
  - the move keeps the inode, the modification time and the creation time;
  - ntfs-3g and lowntfs-3g give `EINVAL` for `RENAME_NOREPLACE` whatever the name, so only `link` can tell a refused name;
  - on a device that fails, XFS answers the move with success, and only the first `fsync` after it gives `EIO`: a file can be back at its source after a remount. So nothing is `done` on the strength of the move alone;
  - ntfs3 has no journal of its own: a device that fails during a write can leave a name and its record that disagree, which it answers with `ESTALE` or `EINVAL`. So the test of a dying device skips those cases on ntfs3, where only a repair can say more.
- Four adversarial reviews, on the code as each round left it: 85 findings confirmed, then 48, 33 and 3. What they found, and each with a failing test first:
  - in the third round, on the link path, a source's name gone during the `fsync` made Atoll unlink the file's last name, while a crash one line earlier kept the file;
  - in the third round too, an edit in place during the `fsync`s before a move, which keeps the inode, reached the trash unseen. The copy is now compared with its row right before the move, and a reprise syncs before it looks, so that nothing long comes between a look and what it decides;
  - a move called `undone` with what it left not durable, so that the next move went through a folder that could vanish;
  - a reprise that removed a source's name without looking again after a long `fsync`;
  - `--older-than 0s` purging what its own reprise had just finished, which its dry run had not shown;
  - hard links: every name of a file moves together, so the rows of the others follow, unless they already disagreed, which is a change nobody has looked at.
- A port written from the documents alone, never reading the Go code, passes all 174 scenarios and every vector. Its failures along the way were mostly gaps in the documents, which is how the event levels, the order of a reprise and several exit codes came to be written down.
- Verified: `go test ./...`, the race detector, lint and every scenario. 351 real outputs against their schemas, which refuse 196 broken ones. 301 mutations of this work, and the 432 of the older lists, all caught, and 2 more that did not compile. The older lists ran before the last small fixes, which touch none of what they test. Left out of the lists as equivalent: the two guards that only save a query, and a kind check that only an inode reused by a folder could reach. `test/loopdev` passes on Linux 6.12 and 7.3, on each driver, with ntfs3 skipped where a dying device left its volume in need of a repair. Of 13 mutations judged on real mounts, 10 were caught; the fsync of the target folder removed, in either direction, survives there, as the next fsync catches the same failure on these drivers, and a unit test pins the order; and one mount check is pinned by a unit test.
- Not measured: real disks, and disks asleep.

## `atoll status` (specs.md §7, §15.11, §25)

Commit "Report what the catalog holds, with atoll status", on `main`.

- The maintainer's decisions:
  - it reads the catalog and the configuration and touches no disk, so it says what the last scan saw, and `atoll disk list` is what looks now;
  - `--path` keeps a file or a folder by whole components, in the counts and in `files`, and a path the catalog does not hold is an empty list with exit code 0;
  - a file is at its level by what can be read now, so a copy on a `missing` disk does not count, even before `health.missing_grace`, unlike in the plan. The JSON holds integers only, so no rounding can differ between two versions, and the share of bytes correctly placed waits for `balance`;
  - dates at a fixed width (§29.3), so both versions write the same bytes;
  - with no eligible copy, a file keeps its original, as it keeps its size and date (§7). A scan used to write NULL;
  - `corrupt` and `conflict` now come before `unavailable` in the order of §4.
- A bug in the previous commit, found by writing the scenario for the rule above: `files.state` is all that remembers a conflict, and `unavailable` came first. So when every disk of a file in conflict went away, the file became `unavailable`, then `ok` when they returned, with copies whose content differs. Nothing acted on it yet, and a later phase could have taken those copies for identical.
- Found along the way:
  - holding the files of a large `--path` would have gone far past §26, so they are written as they are read, before the counts, which only the end knows. 200,000 files in JSON peak at about 21 MiB. A catalog that fails half way therefore leaves an unfinished output, and a script goes by the exit code;
  - §25 said a flag given twice is a usage error, and `--json`, `--full` and `--size` accepted it, in every command. Only a scenario was missing;
  - pages are read apart, so a scan running meanwhile can show a copy on a disk not yet in the list, which would have crashed the command;
  - a port written from the documents disagreed with Go on the original of a file with no readable copy, which is how that rule was found.

## The first scan: `atoll init` and `atoll scan` (specs.md §9, §15.10, §25)

Commit "Record what the disks hold, with atoll init and atoll scan", on `main`.

- The maintainer's decisions:
  - a copy that changed or went away is only reported, in `changed` and `absent`, until propagation exists: no state in the catalog would be true for it;
  - `atoll init` is the only one to create a catalog, so a mistyped path cannot start again from nothing in silence;
  - the same size on two disks is `ok`, unverified, until `--full` reads them, and another size is a `conflict` at once;
  - a walk is written whole or not at all, and the rules of `--json`, ids and retired disks are in §15.10 and §9.
- `internal/watch` walks a disk through handles and compares it with the catalog, `internal/catalog` holds the queries, and `internal/fsx` the handles, with `Fault` for injecting errors. Every scan gives each file its original, its policy and what the plan says, so the solver now runs on real data. Its jobs are not kept, as nothing runs them yet.
- The conformance build reads its clock and gives its disks a capacity, and the runner gets `expect_catalog`, so the scenarios compare the catalog itself.
- Found along the way:
  - the first version peaked at 432 MiB on 200,000 files, past §26. Rows kept while the plan ran, a map of known copies, and each string copied before being shared were most of it. A merge in the order of paths, reading by pages and a soft limit bring the first scan to about 180 MiB and the next to about 145 MiB, and it got faster: 12 s against 19 s;
  - a file whose times move while it is read gives a hash of nothing in particular, so `--full` compares them before and after;
  - a port written from the documents showed that what the walk did not reach was reported `absent`, and through another mount `--full` would have read a file of that mount.
- Not done: propagation, jobs, `daemon --until-idle`, the scan in the daemon, hashing inside `maintenance.window`, all in [TODO.md](TODO.md).

## Disk identity and the mount check (specs.md §10, §25)

Commit "Identify disks and check their mounts, with atoll disk list", on `main`.

- The measurements are in [docs/research/disk-identity.md](docs/research/disk-identity.md): both kernels, every driver, each shape of mount, dm-error, and USB disks really unplugged in qemu.
- The maintainer's decisions:
  - the identifier comes from the superblock, with no external tool, which takes root or the `disk` group;
  - what changes name rules (`ads`, `nocase`, `iocharset`, `casefold`, `ascii-ci`, `ignore_case`), the `ntfs` driver of Linux 7.3 and a `ro` mount make a disk `readonly`. No `media/`, a bind of a folder and a btrfs subvolume make it `missing`;
  - `atoll disk list` shows it, with `api/schemas/disk-list.schema.json`. The clock of the conformance build and `daemon --until-idle` wait for `scan`, their first use;
  - a new replica keeps the date of its source (§14): taking the original's would change invariant 6 and §9, and leave the copies that differ as they are.
- `internal/disks` has the mount table, the superblock and the check, pinned by `mountinfo.json`, `superblock.json` and `disk_check.json`, and the observer of each build. The delivered binary has no way to take a folder for a disk, and a test builds it and tries.
- The runner has `unmount` and `mount` steps, for a failed mount.
- Found along the way:
  - an unplugged ext4 keeps answering `statx` and `getdents`, from the cache: only the device node tells;
  - lowntfs-3g `ignore_case` shows nowhere, so a check asks for the root in the other case and compares inodes;
  - Linux 7.3 has a third NTFS driver, reached by `mount -t ntfs` where no helper exists;
  - Docker Desktop's 9p writes `path=C:\` in its options, which the first parser took for a broken table, so one mount that is none of the pool's would have made every disk `missing`. Now a backslash that escapes nothing stays, and a line that cannot be read says nothing about the others. Only the test that runs the delivered binary found it;
  - on XFS `ascii-ci` the root answers in the other case too, which hid the feature behind `ignore-case` until the real test ran;
  - checks that did nothing went: the mount-root attribute, as the entry says all of it, the length of a line, as a short one has no separator, and a test for a relative source, as it leads nowhere anyway.
- Not done: the confirmation before writes, the handle on the verified root, the daemon's check, `Atoll/disk.toml`, the topology check and the remount, all in [TODO.md](TODO.md).

## Conformance scenarios and the first CLI (specs.md §25, §29.4)

Commit "Run conformance scenarios, with rules check and rules explain", on `main`.

- The format is in [conformance/scenarios/README.md](conformance/scenarios/README.md), and `conformance/runner` runs it.
- The maintainer's decisions:
  - a test build (`-tags conformance`) takes folders as disks, so the delivered binary has no way to skip a mount check, and real mounts stay with `test-loopdev`;
  - that build reads the time from a control file, and `daemon --until-idle` does what is due and stops, so nothing waits and Go and Rust reach the same ids. Changes reach scenarios through rescans, and the watcher is tested apart;
  - one JSON file per scenario, with tree and JSON expected exactly. `${ANY}` stands for the wording of a message and a `*` component for an operation's id, and the runner always looks for hidden names and for anything outside `media` and `Atoll`;
  - `atoll rules check` and `atoll rules explain` come first, as they need only the configuration.
- `cmd/atoll` has those two commands and `--config`, with `api/schemas/rules-check.schema.json` and `rules-explain.schema.json`.
- No code reads the control file yet, as nothing needs a disk: that comes with disk identity, and the file steps have only run against a fake binary so far and in two scenarios.
- Found along the way:
  - to Go, `time.LoadLocation("Local")` is the machine's zone, so `timezone = "Local"` would have loaded in Go and failed in Rust. It is now refused;
  - §24.4 had no list of what a later phase brings. It lived only in `phase.go`, so no other version could reproduce those warnings. The table is now in §24.4, and two scenarios pin every row of it, asked for and not;
  - nothing said that `--size` needs its unit, or that a usage error comes before the configuration is read;
  - a field on the wrong step, such as `tree` on `atoll`, would have checked nothing, so the runner refuses it. One `*` entry now takes one file, so it cannot cover two operations.
- Not covered: the default path `/etc/atoll/atoll.toml`, which a scenario must not touch.

## Catalog schema (specs.md §9)

Commit "Define the catalog schema", on `main`.

- `schema/migrations/0001_initial.sql`, and `catalog.Open`, which sets and checks the pragmas of §9 and applies the migrations.
- The maintainer's decisions:
  - chunk hashes in a table of their own: changing the state of a 100 GB file writes 5 KB instead of 780 KB;
  - `STRICT` tables and `CHECK` on the closed lists: through the Go driver, a string written to a `BLOB` column of an ordinary table is stored as `TEXT` with no error;
  - `disks` is a copy of the configuration, rewritten at each start;
  - no `chunk_size`, a `job_chunks` table instead of JSON, and the lists of job states and event levels.
- Found while writing it: `ops.kind` had no `chmod`, though §6.1 propagates permissions; and `files.id` and `ops.id` name things on disks, so they use `AUTOINCREMENT` and never come back.
- Vectors: `catalog.json`, on `catalog_fixture.sql`: plain SQL, which any SQLite must agree on.

## Placement plan (specs.md §12.4, §13.4)

Commit "Plan which copies to create and how to mark the others", on `main`.

- `solver.MakePlan` gives the jobs (`refresh`, then `replicate`), the marks of the copies (`surplus`, back to `ok`, canceled) and the states of each file. It moves and removes nothing.
- The maintainer's decisions:
  - placement only for now: moves, hysteresis and reachable capacity come with `balance` and `capacity` (phase 1b);
  - with `spread_mode = "strict"`, the domains to cover are those of every data disk not `failed`, `retired` or `draining`, so a file waits for a missing bay rather than doubling up on the other;
  - `overflow` concerns the rule's `tags` and `domains` only;
  - the details of §13.4: the order of files and of disks, which copies count, what is kept beyond the level.
- Report: [docs/research/placement-order.md](docs/research/placement-order.md). Where room is short, all orders protect the same bytes; smallest first protects 98% of files at 60% full, against 68% in the library's order and 6.5% largest first.
- Two things the plan found along the way:
  - a refresh writes a copy, so it takes the same name checks as creating, which §17 already required for a name the driver refuses;
  - a plan for 200,000 files first allocated about 270 MB. `names.TargetNames` is now a tree of folders, and a plan holds 76 to 97 MB on top of its input, in about 1 s at worst.
- Vectors: `plan.json`. Property tests in `internal/solver`: no two copies on a disk, `min_free` respected, no copy past the level, marks never away from the last readable or user copy, the order of the input changes nothing, a plan fed back adds nothing, and nothing past the capacity of §13.1.

## Effective policy of a file (specs.md §11.3, §12.4, §24.2)

Commit "Compute the effective policy of a file", on `main`.

- `rules.Effective` takes the first rule that matches and fills what it leaves out from the pool-wide values. `Policy.Sources` says where each value comes from, for `atoll rules explain`. The policy holds what the configuration asks for: the fallbacks of §12.2 are for the code that acts on it.
- The maintainer's decisions:
  - `settle` and `cache` never go with a size condition, as they are read before the size is known;
  - `tags_mode = "strict"` covers `tags` and `domains`, and a disk must satisfy both lists;
  - `strict` from `[placement]` asks nothing of a rule with no list, and `strict` written on such a rule is an error.
- While the size is not known, a rule with a size condition does not match, following §19.
- Vectors: `policy.json`, including the example of §24.1.
- Classifying one file takes 1.8 µs on an i7-12700K with the 10 rules of that example, so 200,000 files take about 0.4 s.

## Configuration schema and loader (specs.md §24)

Commit "Define the configuration schema and load it", on `main`.

- Report: [docs/research/toml.md](docs/research/toml.md). go-toml v2.4.3 and Rust's `toml` 1.2.0 agree on all 746 files of the official corpus; BurntSushi v1.6.0 accepts 16 invalid files they refuse. All three accept TOML 1.1 syntax.
- The maintainer's decisions:
  - `github.com/pelletier/go-toml/v2`, and TOML 1.1 in the contract;
  - `integrity.chunk_size`, `trash.keep_overwritten`, `placement.move_originals` and `read.striping` removed: each contradicted the spec or served nothing;
  - a setting of a later phase is accepted with a warning, and `view.engine = "atoll"` is an error until phase 2a;
  - a `[maintenance]` table for the window, the rescans and the catalog backups, which the other windows follow;
  - rule properties keep their sections of §24, in the order rule, section, preset, default;
  - identifiers in lowercase ASCII, and a required, unique `label` on each rule;
  - `0s` and `0%` refused where they would remove a protection or stop a task.
- `schema/config.schema.json` is the one list of keys, types, allowed values and defaults: `internal/config` reads it at load time. Decoding into a struct cannot tell `Pool_ID` from `pool_id` in either Go library, so the loader checks the generic tree first.
- Vectors: `config.json`. `deploy/atoll.toml` is the example of §24.1, byte for byte.
- A port written in Rust from the spec alone, without the Go code, passes the same vectors. It found the sentences that did not settle a case (a second byte order mark, the case of `\uXXXX`, `catalog` equal to a mount, and others), which were then fixed.

Effective policy, and everything that reads the file from disk, is still open: [TODO.md](TODO.md).

## BLAKE3 library and hashes (specs.md §16, §29.7)

Commit "Hash files and chunks with zeebo/blake3", on `main`.

- Report: [docs/research/blake3.md](docs/research/blake3.md). On one core, zeebo is 36% to 42% faster than lukechampine, with 2 allocations against 10,756 per 64 MiB.
- The maintainer's decisions:
  - `github.com/zeebo/blake3`, main branch, pinned to commit `28a397d`, for its NEON and SVE2 backends;
  - an empty file has no chunks.
- Contract: `conformance/vectors/blake3.json` holds 17 cases, whose hashes come from the official `b3sum`, 10 lengths also checked against the official test vectors.
- Code: `integrity.Hasher`, one read feeding both hashes, with no I/O. Nothing calls it outside its tests yet.
- Not measured: AVX-512, and throughput on real arm64.

## Capacity: `min_free`, strict domains, reachable (specs.md §12.4, §13)

Commit "Define the capacity with min_free and strict domains", on `main`.

- The maintainer's decisions:
  - a disk's size in the formula is its capacity minus `min_free`, for the disks the solver can write to;
  - with fewer copies than domains, each copy goes to a different domain, and a disk with no `domain` forms its own;
  - the reachable capacity is a dry run of the solver, plus the formula on the space left. It waits for the solver.
- Derived: with strict domains, the formula applies to the domains' sizes when k < m; otherwise the capacity is the smaller of the formula and the smallest domain.
- Contract: `conformance/vectors/min_free.json` holds 23 cases, and `capacity_strict.json` 28. `capacity.json` gains the three §13.2 cases with `min_free`.
- Code: `value.MinFree` and `solver.StrictCapacity`. Nothing calls them outside their tests yet.
- Tests:
  - a property test places one-byte files by trying every choice on small pools, and compares with the formula;
  - the cases were recomputed outside Go, in Python with exact integers, from what the numbers mean.

## Copies that differ at `disk add` and `catalog rebuild` (specs.md §4, §9, §15.7, §18)

Commit "Define copies that differ when a disk is added or the catalog rebuilt", on `main`.

- The maintainer's decisions:
  - copies with the same content and different modification times are in sync, each keeps its own time, and nothing is written to them;
  - `files.mtime_ns` holds the original's time;
  - at `catalog rebuild`, different content is a `conflict`, whatever the times.
- Only a later change is applied to the other copies, which makes the times equal again.
- No code and no vector: this lives in the scan and the catalog, which do not exist yet. The scenario is listed in §27, and the scenario format is still to be defined.

## Modification and change times (specs.md §9, §14)

Commit "Define what happens to times a disk or the catalog cannot hold", on `main`.

- Measured on Linux 6.12.115 and 7.3.1:
  - ext4 and XFS clamp a time without an error: a copy to ext4 of an NTFS file dated 1601 holds 1901, and `cp` exits 0;
  - Go's `UnixNano` wraps: 1601 becomes a date in 2185.
- The maintainer's decisions:
  - the copy is made with the time the disk keeps, which is recorded per copy; it stays in sync and the difference is never applied to the other copies. Invariant 6 now says so;
  - a time out of range is stored at the nearest limit, and Atoll sets times from a copy's `stat`, never from the catalog.
- Contract: `conformance/vectors/time_ns.json` holds 26 cases.
- Code: `solver.TimeNS`, with `CreationTimeNS`, in `times.go`. Nothing calls them outside their tests yet.

## Choosing the original (specs.md §7)

Commit "Define the choice of the original", on `main`.

- Measured per driver, on Linux 6.12.115 and 7.3.1: report in [docs/research/file-times.md](docs/research/file-times.md). ext4 reports a crtime of 0 as 1970, and ntfs3 a FILETIME of 0 as 1601, which does not fit in `btime_ns`.
- The maintainer's decisions:
  - creation times are compared in exact nanoseconds;
  - a date of exactly 0, or out of range, counts as absent;
  - every copy that is in sync and can be read is eligible, `surplus` and `draining` included;
  - a file in conflict keeps its previous original while that copy is eligible.
- Contract: `conformance/vectors/original.json` holds 37 cases, and `creation_time.json` 23. `absent` joins the vector flags.
- Code: `solver.SelectOriginal` and `solver.CreationTimeNS`. Disks and the catalog do not exist yet, so nothing calls them outside their tests.
- Left open: modification and change times out of range, in `TODO.md`.

## The name report and the state a file shows (specs.md §4, §15.9)

Commit "Define the name report and the state a file shows", on `main`.

- The maintainer's decisions:
  - one finding per name to fix, the shortest path with the problem, in a flat sorted list;
  - `ext4` and `ntfs` are always checked, whatever the pool holds;
  - exit code 0 whenever the check ran, with or without findings;
  - the name states come before `degraded` in the order of states.
- Also stated:
  - placement and job order read the copies, never the state;
  - the report reads the catalog only, so it wakes no disk;
  - collisions are listed between siblings; a difference in a parent is covered by the finding on that parent;
  - a well-formed logical path is defined in §4.
- Contract: `api/schemas/names-check.schema.json`, the first of the `--json` schemas. `conformance/vectors/names_check.json` holds 56 cases, and `file_state.json` 27.
- Code: `names.Check` and `solver.ShownState`. The CLI and the catalog do not exist yet, so nothing calls them outside their tests, and nothing writes this JSON yet.

## Total path length (specs.md §12.4, §15.8)

Commit "Limit the total path length on mergerfs branches", on `main`.

- Measured:
  - with mergerfs 2.40.2, a copy past 4095 bytes on its branch is listed and `stat` gives `ENOENT`, so it protects nothing, and a deletion or a rename through the view leaves it behind;
  - on Windows 11 with `LongPathsEnabled` = 1, six built-in tools work at 259 UTF-16 units and fail at 260, while `cmd /c type` and PowerShell work at 300.
- The maintainer's decisions:
  - a path past the branch limit is a hard constraint of the solver;
  - a path past the Windows limit is only listed by `atoll names check`.
- Contract: `conformance/vectors/branch_path.json` holds 24 cases, and `windows_path.json` 22.
- Code: `names.FitsBranch` and `names.FitsWindows`. The solver and `atoll names check` do not exist yet, so nothing calls them outside their tests.
- Tests:
  - the cases were generated and checked outside Go, in PowerShell with .NET byte and UTF-16 counts;
  - 24 code mutations each make a vector fail, and 14 mutations of the vector shape each make the test fail.

## ntfs3 `ads` and chunks in the trash (specs.md §10, §15.2, §17)

Commit "Refuse ntfs3 ads and fix the chunk layout in the trash", on `main`.

- Measured in qemu on ntfs3, on Linux 6.12.115 and 7.3.1, re-reading after dropping the caches:
  - on 7.3.1, with `ads` on by default and with or without `windows_names`, a rename through mergerfs onto `m:n` succeeds and replaces the content of the file `m`, even when that was its only copy;
  - `ads=0` stops it, and 7.3.1 then behaves as 6.12, which behaves as ntfs-3g;
  - ntfs3 holds names that differ only by case.
- The maintainer's decisions:
  - ntfs3 with `ads` is refused at `disk add`, and a disk found with it later becomes `readonly`, with an alert;
  - overwritten chunks go to `Atoll/trash/<date>/<op-id>.chunks/<logical path>/<index>`, which holds a file name of 255 bytes.
- Also stated in §17: the index format, and a copy whose name the driver refuses, which cannot reach the trash: it stays, is reported, and is never refreshed or repaired.
- No code: the trash and `disk add` are not written yet. The tests to write are listed in §27.

## Renames to a name a disk refuses (specs.md §15.2, §15.3)

Commit "Decide what happens on a rename a disk refuses", on `main`.

- Measured: when one branch refuses a rename and another accepts it, mergerfs returns success and removes the source from the branch that refused. A file loses that copy, with no trash. A folder that is not empty stays at the old name, and the merged view shows both folders. §15.2 said that renames reach every copy; it now states this.
- The maintainer's decisions:
  - the copy left at the old name goes to the trash, with the new reason `renamed`, and the solver looks for another compatible disk;
  - `windows_names` stays recommended, with its cost stated in §15.8.
- Also stated in §15.3 and §6.1:
  - a copy changed meanwhile stays, as a `conflict`;
  - the renamed copy counts as a user copy, and the user's rename counts as the command that removes a user copy;
  - the mass-deletion guard counts these copies;
  - `renamed` is purged as late as `deleted`.
- No code: the check already exists (`names.CheckPath`, `names.TargetNames`), and propagation is not written yet. The scenario is listed in §27, and the scenario format is still to be defined.

## Name collisions on NTFS targets (specs.md §15.7)

Commit "Refuse name collisions on NTFS targets", on `main`.

- The maintainer's decisions:
  - a colliding copy is blocked: a hard constraint of the solver, not only a report;
  - `entries` gets an `lpath_key` column, so folders are looked up the same way as files.
- Also stated in §15.7:
  - names already side by side on an NTFS disk, created outside Atoll, are only reported;
  - a case-only rename does not collide with its own old name;
  - between two new names, the first in the plan's order gets the disk.
- Contract: `conformance/vectors/ntfs_collision.json` holds 39 cases.
- Code: `names.TargetNames`. The solver and the catalog do not exist yet, so nothing calls it outside its tests.
- Tests:
  - the cases were checked outside Go, in PowerShell with .NET normalization and an upcase built from the rule of §15.8;
  - a property test compares it with a pairwise comparison, on names mixing case, NFC and NFD forms and invalid UTF-8;
  - 11 code mutations each make a vector fail, and 11 mutations of the vector shape each make the test fail.
- Measured: ntfs-3g and, in a later measurement, ntfs3 hold both names, so the drivers do not protect against collisions. Not measured: what Windows does with such names.

## Capacity formula (specs.md §13.1)

Commit "Add the capacity formula and its vectors", on `main`.

- The maintainer's decisions:
  - the result is rounded down to whole bytes;
  - with more copies than disks, the capacity is 0.
- Also stated in §13.1: k is at least 1, no size is negative, and the total fits in a signed 64-bit integer. Any other input is an error.
- Contract: `conformance/vectors/capacity.json` holds 42 cases.
- Code: `solver.Capacity`, with integers only.
- Tests:
  - the values were checked outside Go, in PowerShell with exact integers, against the formula and against the largest x such that k copies of x bytes fit;
  - a property test compares the function with that second definition on 20,000 small pools;
  - of 20 code mutations, 19 make a vector fail. The last, a function that sorts its own input, makes the property test fail.

## One spelling for vector flags (specs.md §29.4)

Commit "Refuse vector flags other than true", on `main`.

- `internal/vectors` refuses `error` and `no_key` with any value but `true`, in every vector file and in the corpus. Only `name_compat.json` refused `"error": false` before.
- `window.json`: an error case can no longer carry `"at": ""`.
- No vector file changed: none used a refused spelling.
- Mutation checks on all nine vector files that have a flag: a flag written as false, an error case with an expected result, and a case with neither. Each test fails on each mutation.

## Name compatibility per filesystem (specs.md §15.8)

Commits `f7b6502`, `10c1c2b`, `b9fbeb3`, and the follow-up after the review.

- Researched:
  - the Windows 10 and 11 rules;
  - ntfs3 on Linux 7.2 and 7.3, measured in qemu;
  - ntfs-3g and lowntfs-3g;
  - ext4, including `casefold`;
  - XFS, including `ascii-ci`, and btrfs.

  Report: [docs/research/name-compatibility.md](docs/research/name-compatibility.md).
- The maintainer's decisions:
  - every NTFS disk follows the Windows rules, as a fixed table in both versions;
  - reserved names as Windows 10 reads them, with `CONIN$` and `CONOUT$`;
  - at most 255 bytes per component on NTFS too;
  - ext4 `casefold`, XFS `ascii-ci` and NTFS `nocase` are refused at `disk add`;
  - every component is checked, folders included;
  - a closed list of six reason codes.
- Contract:
  - `conformance/corpus/names/names.json` holds 101 names with their reasons on ext4, xfs, btrfs and ntfs;
  - `conformance/vectors/name_compat.json` holds 27 whole-path cases;
  - vector files must not hold an unpaired surrogate escape. Keys must be lowercase ASCII and unique, integers plain, and base64 written exactly as the standard encoder writes it, because Go's decoder would otherwise accept what Rust's rejects.
- Code:
  - `internal/names/compat.go`: `ComponentReasons`, `CheckPath`;
  - `internal/vectors` loads the corpus and checks the text rules.
- Tests:
  - an independent Python model agrees on 300,000 random names;
  - mutation checks;
  - fuzzing;
  - `sudo make test-loopdev` creates the corpus on real ext4, XFS and btrfs loop images, and on NTFS with every driver present (ntfs3, ntfs-3g, lowntfs-3g), with and without `windows_names`;
  - an independent review: two spec-only ports agreed with Go on 5.4 million names and paths, and the corpus was replayed on ntfs3 (Linux 7.2 and 7.3), ntfs-3g and lowntfs-3g. The review found lowntfs-3g `ignore_case`, now a refused variant.

## Regex grammar and weight (specs.md §29.5)

Commits `9579074` to `a4e82fa`.

- A positive grammar, the common subset of Go `regexp` and Rust `regex`, which Atoll checks itself in both versions. It is limited to 1000 bytes in NFC form, with Go's nested-repeat product rule.
- A weight limit of 1000:
  - every element that adds to the automaton weighs at least 1 per copy, and a property weighs 100;
  - Rust keeps `size_limit` at 10 MiB and sets `nest_limit` to 1000;
  - the costliest regex found needs 0.41 of the limit.
- A pattern with more than 30 non-starters in a row is rejected. They are counted in the NFKD of each character, exactly where Go's NFC inserts U+034F.
- Class pairs `--`, `&&` and `~~` are rejected anywhere in a class. The grammar applies to the NFC form, and errors quote the typed text.
- Verified by several rounds of independent review:
  - spec-only Rust validators;
  - 4.5 million Go/Rust engine pairs;
  - 5.2 million weight comparisons;
  - adversarial searches for the costliest regexes in Rust.
- Documented and kept as is, by the maintainer's choice:
  - Rust needs an 8 MiB thread to compile the deepest regexes in debug builds;
  - with `ignore_case`, a pathological regex can take about 0.7 s to compile.

## `lpath_key` (specs.md §15.7, §15.8)

Commits `aa7da56`, `5cd9ab7`, `31e7278`, `5fed1ed`, `63e9cf3`.

- The key is NFC of the NTFS upcase table, plus `ς` → `Σ`, applied to NFD.
  - The table is generated from Unicode 5.1 data: 973 entries.
  - It was checked byte for byte against `$UpCase` from Windows 10 images, and from a Windows 11 volume read by the maintainer.
- `meta.lpath_key_version` names the language, the library, its release and its Unicode version: `go golang.org/x/text v0.43.0 17.0.0`. Any difference recomputes every key.

## NFC and case folding in rule matching (specs.md §11.3)

Commits `7db3410` to `34a644d`.

- Research report: [docs/research/nfc-and-case-folding.md](docs/research/nfc-and-case-folding.md).
- NFC comes from `golang.org/x/text/unicode/norm` v0.43.0, applied run by run on invalid UTF-8, only through `internal/names`.
- `ignore_case` uses Unicode simple case folding, exactly as regex `(?i)`.
- Rule conditions are implemented: `path`, `filename` and `ext` (a list), with globs and regexes.
- Vectors: `nfc.json`, `match.json`, `lpath_key.json`.

## Earlier work

Commits `b1cdf1d` to `e384a9c`.

- The specification, CLAUDE.md, the project skeleton, the license, and the `api/`, `schema/` and `deploy/` folders.
- The conformance vector format.
- Value grammars: durations, sizes, percentages, time windows.
- The in-house glob matcher.
