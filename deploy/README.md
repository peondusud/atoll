# Deployment files

| Path | Content |
|---|---|
| `atoll.toml` | A sample configuration: the example of `specs.md` §24.1, byte for byte. A test compares the two, and loads it. |
| `atoll.service` | The systemd unit of the daemon that runs for good (`specs.md` §15.13): `Type=notify`, as the dedicated user `atoll`. |

## The account

The maintainer chose a dedicated user over root. The unit gives it the group `disk`, to read a device's head when a disk is confirmed (`specs.md` §10). It also gives it these capabilities, and no others:

| Capability | What needs it |
|---|---|
| `CAP_SYS_ADMIN` | fanotify's filesystem marks (§15.1), `umount` and `mount` after a USB reset, and mergerfs's runtime branches |
| `CAP_DAC_READ_SEARCH`, `CAP_DAC_OVERRIDE` | reading every file of the library, and writing copies and trash where the library's owners allow nothing |
| `CAP_CHOWN`, `CAP_FOWNER` | giving a copy its source's owner, mode and times |

Measured in a Debian 13 container, as such a user with those capabilities and nothing else:
- it reads and writes mergerfs's runtime branches through `.mergerfs`;
- it runs `mount` and `umount` through util-linux.

`NoNewPrivileges`, `ProtectHome=read-only` and `PrivateTmp` harden it no further than the disks allow: the library's mount points stay writable.

The catalog's folder (`/var/lib/atoll`) and the configuration's (`/etc/atoll`) belong to `atoll`: `StateDirectory` and `ConfigurationDirectory` see to it. Commands run by hand as root work on the same catalog; they wait while a pass holds it (§25).

Each disk's work folder, `Atoll/` at its top, must belong to root or to `atoll` (`specs.md` §10): `atoll disk add` refuses one that another account owns, on ext4, XFS and btrfs, and says the `chown` that gives it back, which a disk needs when it comes from a machine where `atoll` has another number. Keep the top of each disk closed to the accounts that must not read every file of the library: whoever owns it can make that folder again.

The catalog's folder is closed to other accounts (`StateDirectoryMode=0750`). The catalog and its backups list every path of the library, its trash and its history, which the local API gives only through its socket, of mode 0660, or with its token (§25.1): a folder anyone could read would give them to every account of the machine. A member of the group `atoll` reads them, and may ask the socket.
