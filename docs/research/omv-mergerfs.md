# The openmediavault mergerfs plugin

This report tries the openmediavault mergerfs plugin with what `atoll mergerfs-config` prints (`specs.md` §21.1), for the settings of the plugin that §21.1 leaves for later. It is research, not a contract: what is decided goes to `specs.md` §21.1.

## Why it matters

- openmediavault is the NAS system the maintainer runs, and its users make their mergerfs pool through the plugin's form, not through `/etc/fstab`: the plugin writes the mount itself, and a line appended to `/etc/fstab` by hand sits beside what the system manages.
- So the fstab line is not enough there: a user needs what to type in each field of the form, and to know what the form cannot hold.

## Method

- On 2026-11-19, in a Debian 13 container running systemd (privileged, with `/dev/fuse`), on Docker Desktop's WSL 2 kernel.
- openmediavault 8.6.1 (`synchrony`) from its own APT repository; `openmediavault-omvextrasorg` 8.0.4, then `openmediavault-mergerfs` 8.0.5 and `mergerfs` 2.42.0 from omv-extras, as their documentation installs them.
- Three folders of the container's own filesystem as branches, `/srv/d1/media`, `/srv/d2/media` and `/srv/d3/media`, each with a file below `Movies/` on the first two.
- First, Atoll's own line, in `/etc/fstab`, mounted by `mount(8)`: the branches `=RW,4G`, `=NC,2G` and `=RO`, and the options of §21.1 in their order, `category.create=mfs`.
- Then the same pool through the plugin's RPC, as its form sends it: `omv-rpc -u admin MergerFS set` with `name`, `paths`, `createpolicy`, `minfreespace`, `minfreespaceunit` and `options`, then `omv-salt deploy run mergerfs`, as the form's Apply does. The plugin's source was read where it decides: `engined/rpc/mergerfs.inc` (`set`), the data model `conf.service.mergerfs.json`, and the Salt state and template that write the mount, `srv/salt/omv/deploy/mergerfs/`.

## Summary

- **mergerfs 2.42.0 takes Atoll's line as it is.** Each branch kept its mode and its own `minfreespace`, each option read back from `.mergerfs` with the value set, and a change of `user.mergerfs.branches` at runtime, as Atoll makes for a missing disk, took effect. §21.1 was measured on 2.40.2; the plugin needs 2.42 or later.
- **The plugin takes the branches' modes and Atoll's options.** A pool made with the paths `/srv/d1/media=RW`, `/srv/d2/media=NC` and `/srv/d3/media=RO`, one a line, and Atoll's options, mounted and listed both files. Its unit is `/etc/systemd/system/srv-mergerfs-<name>.mount`, whose `Options` are `branches=` and the paths joined by `:`, then `category.create=` from `createpolicy`, `minfreespace=` from its two fields, `fsname=<name>:<uuid>`, then `options` as given.
- **One `minfreespace` for the pool, never one a branch.**
  - The RPC splits `paths` on `,`, `:` and line breaks, drops what is 2 characters or fewer, and refuses what does not start with `/`. So `/srv/d1/media=RW,4G` is saved as `/srv/d1/media=RW` with no error, and `=RW,500G` is refused ("Invalid path").
  - Written straight into the database, a branch's size would not pass either: the unit gives the branches inside the mount options, `branches=…`, where a `,` ends the option.
  - Its unit is K, M or G: no T.
- **The mount point is not chosen**: it is always `<OMV_MOUNT_DIR>/mergerfs/<name>`, `/srv/mergerfs/<name>` by default. `view.mount` must be that path.
- **Atoll's options pass**, but `category.create`: the RPC removes it from `options` and the form's `createpolicy` sets it, from the same values (`mfs`, `ff`, `pfrd`). It also removes `nonempty`, `use_ino` and `allow_other`; `findmnt` shows `allow_other` all the same, as mergerfs run as root sets it.
- **Paths are narrower than mergerfs's**: besides `:` and `,`, which §21.1 already refuses, a path may not hold `"`, `'`, `` ` ``, `$`, `\`, `;`, `|`, `&`, `<`, `>` or a line break.
- **Atoll's changes at runtime still work**: they go through the mount's `.mergerfs`, whoever mounted it. A deployment of the plugin restarts the unit only when the pool is not mounted.

## Decisions for the maintainer

1. **The form of the output.**
   - **A.** `atoll mergerfs-config --omv` prints each field of the plugin's form, a line each, as the form names them: `Paths` (one path a line, with its mode), `Create policy`, `Minimum free space` with its unit, and `Options`; plus the mount point the pool will have. A new flag (§25, §29.3).
   - **B.** An `omv` object in `--json` (`api/schemas/mergerfs-config.schema.json`, an added property), which a script could send to `omv-rpc`.
   - **C.** No new output: a page of documentation that maps the fstab line to the form by hand.
2. **Which `minfreespace` stands for the branches' own.**
   - **a.** The largest: no disk takes a new file past its `min_free`, and a disk whose own is smaller stops taking new files earlier than Atoll would place its copies there.
   - **b.** The smallest: a disk whose own is larger can take new files past it, which Atoll then works around, or fails to.
   - **c.** Refuse, exit 4, when the branches' values differ, so the user sets one `min_free` for every disk.
3. **`view.mount`.** Warn when it is not `/srv/mergerfs/<name>`, or leave it to the documentation.

The author's suggestion, for what it is worth: A, with a, and a warning on `view.mount`: an output of its own changes no existing output, and the largest value never lets mergerfs fill a disk past its `min_free`.

## Not measured

- The form itself in a browser: the RPC was called as the form calls it, with the same parameters.
- Real disks, and branches on separate filesystems, whose mounts the unit waits for (`RequiresMountsFor`, from `omv-findmnt`): the branches here were folders of one filesystem.
- openmediavault 7 (`sandworm`, Debian 12), whose plugin version differs.
