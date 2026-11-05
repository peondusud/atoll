# Name corpus

Single names, each a path component, with the reasons every filesystem refuses them for (`specs.md` §15.8). Both versions load `names.json` in their unit tests. The loop-device tests (`sudo make test-loopdev`) create the compatible names on real ext4, XFS, btrfs and NTFS filesystems.

## Format

`names.json` follows the envelope and the rules of [`../../vectors/README.md`](../../vectors/README.md): `format_version` 1, `spec`, `description`, one case per line, an optional `note`, and unknown fields are errors. Each case has these fields:

| Field | Type | Meaning |
|---|---|---|
| `name`, `name_b64` | string | the name; `name_b64` holds its raw bytes in standard base64 when it is not valid UTF-8. A case has exactly one of the two. |
| `ext4`, `xfs`, `btrfs`, `ntfs` | array of strings | the reasons that filesystem refuses the name for, in the order of §15.8; an empty array means the name can be written there. Every case has all four. |

A name is never empty, `.` or `..`, and holds neither `/` nor NUL: those are malformed paths, which `vectors/name_compat.json` covers.

## Reasons

A closed list (§29.3), always in this order: `too-long`, `invalid-utf8`, `control-character`, `forbidden-character`, `trailing-dot-or-space`, `reserved-name`. On ext4, XFS and btrfs, only `too-long` applies.

## Unicode versions

The verdicts use no Unicode data: they read bytes, UTF-8 validity and UTF-16 lengths. So, unlike the vectors, the corpus may hold characters assigned after Unicode 16.0, noncharacters and private-use characters. It must not pin a result that depends on Unicode data, such as an NFC form or an `lpath_key`.
