# NFC and case folding for rule matching

Research report, 2026-11-05. It answers two questions that block the rule matcher (`specs.md` §11.3) and that also bear on `lpath_key` (§15.8). The decisions belong to the maintainer: `specs.md` is unchanged, and nothing here is a contract until the spec says so.

- **Q1.** NFC needs a normalization library. Should Atoll depend on `golang.org/x/text/unicode/norm`?
- **Q2.** Which case folding should `ignore_case` use: Unicode simple case folding (what `(?i)` does in Go and Rust regexes), or the NTFS upcase table that §15.8 already names for `lpath_key`?

## Contents

1. [Summary](#1-summary)
2. [Method](#2-method)
3. [Q1: the NFC dependency](#3-q1-the-nfc-dependency)
4. [Q2: case folding for `ignore_case`](#4-q2-case-folding-for-ignore_case)
5. [How NFC and case folding interact](#5-how-nfc-and-case-folding-interact)
6. [Paths that are not valid UTF-8](#6-paths-that-are-not-valid-utf-8)
7. [Unicode versions across Go and Rust](#7-unicode-versions-across-go-and-rust)
8. [`lpath_key` (§15.8)](#8-lpath_key-158)
9. [APIs to avoid](#9-apis-to-avoid)
10. [Open points for the spec](#10-open-points-for-the-spec)
11. [Claims the verification corrected](#11-claims-the-verification-corrected)
12. [What was not verified](#12-what-was-not-verified)
13. [Sources](#13-sources)

## 1. Summary

**Q1: add `golang.org/x/text/unicode/norm`, at v0.43.0 (v0.42.0 at the very least), behind a small Atoll wrapper.**

- The licence is BSD-3-Clause, byte-identical to the Go toolchain's own LICENSE and PATENTS files, and GPL-3-compatible.
- It is pure Go. Only two packages get compiled, and the binary grows by about 110–135 KB.
- The standard library has no usable NFC: its vendored copy cannot be imported.
- It passes the full Unicode 17.0.0 NormalizationTest.
- The same package provides NFD, which `lpath_key` needs (§8).
- Every release before v0.42.0 produces wrong compositions that the official test file does not catch.
- An in-house NFC is feasible, but its only real advantage, pinning the Unicode version for Go/Rust identity, does not survive contact with the regex half of the matcher (§7).

**Q2: Unicode simple case folding (CaseFolding.txt statuses C and S) for `path`, `filename` and `ext`; keep the NTFS upcase table for `lpath_key` only.**

- §29.5 already decides the regex side: "`ignore_case` adds `(?i)`", and `(?i)` is exactly simple case folding in both Go and Rust.
- NTFS semantics cannot be obtained through `(?i)`. Upper-casing the pattern text silently corrupts it.
- With NTFS for globs and `ext`, the same `ignore_case = true` would mean two different things. Realistic paths show it: Greek final sigma, capital ẞ, Georgian Mtavruli titles, µ against μ.
- NTFS upcase equivalence is strictly contained in simple folding. Two characters that any Windows table or Samba treats as one are always treated alike by an `ignore_case` rule.
- "The NTFS upcase table" is not one table. Three exist, chosen per volume, and they cover only the BMP.
- The reason §15.8 gives ("so `ß` and `ss` stay distinct") does not tell NTFS from simple folding. Only full folding merges `ß` and `ss`.

Whatever the answers, the spec has to fix a few points the current wording leaves open (§10):

- how rules match names that are not valid UTF-8;
- class semantics under `ignore_case`;
- the order of NFC and folding;
- a Unicode-version clause, because Go and Rust already disagree today on 56 code points under `(?i)`.

## 2. Method

The research was split into five areas: the Go NFC library, folding in Go, the Rust side, the NTFS `$UpCase` table, and what the spec already implies. In each area, one agent ran experiments and a second agent tried to refute every claim by re-running it with its own, separately written programs. In total there were 79 claims: 72 were confirmed, 4 refuted and 3 left uncertain. Section 11 lists the corrections. All the work was done with Claude Code research agents. The throwaway programs are not kept in the repository; the sources and versions in §13 are enough to reproduce the results.

Versions used:

| Component | Version |
|---|---|
| Go toolchain | 1.27.0 (`unicode.Version` 17.0.0); 1.26.0 for comparison |
| `golang.org/x/text` | v0.43.0 (latest, 2026-10-09); older tags for regressions |
| Rust | rustc 1.97.0 |
| `regex` / `regex-syntax` | 1.13.1 / 0.8.11 (latest) |
| `unicode-normalization` | 0.1.26 (latest, 2026-10-16) and 0.1.25 |
| `icu_normalizer` | 2.4.0 (latest, 2026-10-22) and 2.3.0 |
| ntfs-3g | `tuxera/ntfs-3g` edge, commit 7f0f841f |
| Linux ntfs3 | master, 7.4-rc1 |
| Samba | master 9374273c (4.26.0pre1), plus 4.13, 4.19 and 4.22 tables |

Unicode data came from ICU release tags, because www.unicode.org was unreachable from the research environment (§13).

## 3. Q1: the NFC dependency

### 3.1 Why a library is needed

- §11.3 matches "the NFC form of the path and of the pattern", and §15.8 computes `lpath_key` from NFC. NFC is required by two sections.
- The Go standard library has no public normalization package. Go 1.27 vendors `golang.org/x/text` v0.37.0, including `unicode/norm`, for `net/http` (through `x/net/idna`). Importing it from user code fails with "use of vendored package not allowed".
- Rust's standard library has none either, so the Rust version needs a crate or in-house tables in any case.

### 3.2 Cost and provenance of `x/text`

- **Licence.** BSD-3-Clause ("Copyright 2009 The Go Authors"), with a PATENTS grant. Both files are byte-identical to the Go 1.27.0 toolchain's own (LICENSE sha256 `911f8f5782931320f5b8d1160a76365b83aea6447ee6c04fa6d5591467db9dad`). Every Go binary already carries this notice, so no new obligation is added. The FSF lists the Modified BSD licence (SPDX BSD-3-Clause) as GPL-3-compatible. This was read from a mirror of the FSF licence data, because gnu.org was unreachable.
- **Module graph.** v0.43.0's `go.mod` says `go 1.26.0`. It requires `golang.org/x/tools` v0.51.0 (`tagx:ignore`), `x/mod` and `x/sync`, none of which is compiled.
  - In a fresh module, `go mod tidy` adds 2 `go.sum` lines.
  - With the dependencies §29.7 plans (`modernc.org/sqlite`, whose `modernc.org/libc` already requires `x/tools`, `x/mod` and `x/sync`), adding `x/text` adds exactly one module to `go list -m all`.
- **Build.** Only `golang.org/x/text/transform` and `golang.org/x/text/unicode/norm` are compiled. `unicode/norm` imports nothing but the standard library and `transform`. The only `import "C"` in the module is behind a test-only `icu` build tag.
- **Size.** A hello-world grows by 135,007 bytes, or 110,592 with `-s -w`. A program that also links `net/http` grows by 128,679 bytes, or 106,496 stripped. Since §29.7 plans `net/http` for the local API, the toolchain's vendored copy (about 75 KB of symbols) will be linked anyway; the module copy is a second one. Cold build time shows no measurable difference.
- **Maintenance.** The Go team tags it about monthly: 14 tags from v0.30.0 (2025-10-08) to v0.43.0 (2026-10-09). Eleven commits touched `unicode/norm` in 2026.

### 3.3 Correctness

- v0.43.0 passes all 100,170 checks of NormalizationTest-17.0.0, plus 1,094,978 checks that unlisted code points are left unchanged.
- **Every release before v0.42.0 (2026-09-08) has a composition bug.** It truncates the recomposition key and produces results that are not canonically equivalent to the input:
  - NFC(U+05D2 U+0307) = U+105C9;
  - NFC(U+0300 U+11F41) = U+1F43.

  The upstream commit (f53c316) counts 15,379 affected two-character inputs under Unicode 17 and 15,056 under the Unicode 15 tables, so v0.20.0 and v0.33.0 are affected as well. On a 4.7-million-pair test set, v0.41.0 differs from v0.43.0 on 1,036 pairs.
- v0.42.0 also fixes:
  - Hangul syllables followed by the new Unicode 17 starter–starter compositions (golang/go#81021): NFC(U+AC00 U+113C2 U+113B8) must give U+AC00 U+113C7;
  - a starter blocking issue (golang/go#81001);
  - three more `norm` defects.
- `norm.Iter` loops forever on the invalid input `\xf3\xcc\x80` up to v0.38.0 (fixed in v0.39.0, golang/go#80142). `NFC.String` returns normally on that input.
- Every release from v0.34.0 to v0.43.0 passes NormalizationTest.txt, so that file catches none of these bugs.

So the minimum is v0.42.0, and v0.43.0 is the version to require.

### 3.4 Behaviour that Go and Rust must agree on

**Unicode version.** `x/text` picks its tables with a Go build tag: `tables17.0.0.go` is `//go:build go1.27` and `tables15.0.0.go` is `//go:build !go1.27`. Under Go 1.27, `norm.Version` is 17.0.0 like `unicode.Version`; under Go 1.26 both are 15.0.0. The tag `go1.27` is also satisfied by later toolchains. So if a future Go moves the standard library to Unicode 18, `x/text` v0.43.0 will stay at 17.0.0 until `x/text` itself is updated. This was inferred from build-tag semantics, not tested. The two versions match today but do not track each other automatically.

On the Rust side:

- `unicode-normalization` 0.1.26 is already at Unicode 18.0.0, while 0.1.25 is at 17.0.0.
- With 0.1.25, Go and Rust give identical NFC, NFD and canonical combining class for every scalar value.
- With 0.1.26, they differ on the 34 code points that Unicode 18 assigns with a non-zero canonical combining class. Examples:
  - `e` U+1AEE U+0301 stays unchanged in Go but becomes U+00E9 U+1AEE in Rust;
  - U+05D0 U+05C9 U+05B0 is reordered by Rust only.

**Stability.** UAX #15 §3 says that, from Unicode 4.1 on, "the results of normalizing a string on one version will always be the same as normalizing it on any other version, as long as the string contains only assigned characters according to both versions". It was quoted from a mirror of UAX #15, because unicode.org was unreachable. The experiments agree:

- Go under Unicode 15 fails 278 checks of the 17.0 test file, and every failure involves a code point unassigned in 15.
- Go under Unicode 17 fails 66 of the 100,855 checks of the 18.0 test file, and every failure involves one of the 34 code points new in 18.
- Between 17 and 18 the canonical decompositions are unchanged (2,081). The only change is 34 more non-zero combining classes (968 to 1,002).

Version drift therefore only affects names that contain newly assigned characters. It still matters for conformance vectors and for stored `lpath_key` values.

**Invalid UTF-8.** `x/text` copies invalid bytes unchanged and treats each as a hard boundary: `e \xff U+0301` stays as it is, with no composition across the byte. This is implemented but not documented. The intended rule is "NFC on each maximal valid UTF-8 run, other bytes copied", and `x/text` almost implements it.

- An exhaustive scan of 854,065,152 inputs found 168 exceptions.
- When the input ends with a 4-byte lead byte (F0–F7) followed by exactly one of U+0340, U+0341, U+0343, U+0344, U+0374, U+037E or U+0387, that code point is left unnormalized. For example, `\xf3` U+0344 stays as it is instead of becoming `\xf3` U+0308 U+0301.
- With any following byte, the output is correct.
- The cause: the trie lookup treats a lead byte with too few remaining bytes as "incomplete" without checking for continuation bytes, and the tail is then copied verbatim (`normalize.go`, "Append incomplete UTF-8 encoding").

The Go side must therefore split the input into maximal valid UTF-8 runs itself and call `norm.NFC` on each run. On the Rust side, `<[u8]>::utf8_chunks()` plus `unicode-normalization` on each valid run gives the same result. It matched Go byte for byte on 200,000 random byte strings (about 60 % invalid UTF-8), on hand-made WTF-8 surrogate cases, and on 30,000 more random inputs; none of these test sets contained the F0–F7 case above. `icu_normalizer` does not fit:

- 2.4.0 `normalize_utf8` replaces invalid input with U+FFFD;
- 2.2.0 and 2.3.0 (its only Unicode 17 versions) are unsound on invalid UTF-8: they return a `str` that still holds invalid bytes.

**Stream-Safe Text Format.** `norm.NFC` is not pure NFC on long runs of non-starters. After 30 of them (`maxNonStarters = 30`) it inserts U+034F COMBINING GRAPHEME JOINER, following the UAX #15 Stream-Safe Text Format, with no option to turn it off.

- `e` + 31 × U+0301 gives one CGJ, while 30 give none.
- A precomposed `é` followed by 30 × U+0301 also gets one, because the count runs on the decomposition.
- `IsNormalString` of 35 × U+0316 is false in Go, while Rust's `is_nfc` says true.

Rust's `.nfc()` never inserts a CGJ. `.stream_safe().nfc()` matches Go only when the run contains neither Hangul jamo nor any of about 51 other ccc = 0 code points that Go counts as non-starters. These are mostly vowel signs and length marks that combine backwards, such as U+0B3E and U+0CD5. Reproducing Go exactly would mean copying its counting rule. Real file names never contain 30 consecutive combining marks, so the simplest course is to keep such runs out of the vectors, or to define the behaviour explicitly.

**Parity.** On valid text made of characters assigned in both Unicode versions, with runs of at most 30 non-starters, Go `x/text` v0.43.0 and Rust `unicode-normalization` agree exactly:

- on 4,705,535 two-code-point strings (every pair from a set of 4,435 normalization-relevant code points × 1,061 combining or "maybe" code points);
- on 1,000,000 random strings;
- on all 14,139 NFD sequences.

### 3.5 The in-house alternative

An in-house NFC was prototyped in about 120 lines of Go:

- recursive canonical decomposition;
- Hangul arithmetic;
- stable reordering by combining class;
- composition with blocking;
- per-run handling of invalid bytes, with no stream-safe insertion.

It passes all of NormalizationTest-17.0.0 and the invariant checks, and matches `x/text` on the 200,000-case corpus and the 4.7 million pairs. On raw bytes it would differ from `x/text` in the F0–F7 case above, where it gives the intended per-run result. It is about 2.3 times slower than `x/text` (3.6 µs against 1.6 µs for three typical paths), which does not matter here.

The data it needs, for Unicode 17:

- 2,081 canonical decompositions;
- 1,120 full composition exclusions;
- 961 primary composites;
- 968 code points with a non-zero combining class.

The prototype loads these from the UCD at run time. A production version needs a table generator and generated tables, several thousand lines of data, in both Go and Rust. For scale, `x/text/unicode/norm` is 2,264 lines of code plus 8,104 lines of generated tables.

Its one real advantage is pinning the Unicode version in the repository, independently of the toolchain. That advantage does not carry through the matcher. Regex `(?i)` and `\p{…}` use the engines' own tables, which already differ between Go and Rust (§7), so the vectors must avoid recently assigned characters anyway. Within that set, the stability policy guarantees identical NFC. And the `x/text` bugs of §3.3 show the cost of in-house code: subtle errors that the official test file does not catch.

### 3.6 Recommendation for Q1

Require `golang.org/x/text` v0.43.0 and wrap it in an Atoll function that:

- splits input into maximal valid UTF-8 runs and normalizes each run, copying other bytes unchanged;
- is the only place that calls `norm`.

Import `unicode/norm` only, never `x/text/cases` (§9). The spec and vectors then need:

- a vector for the per-run rule, for example `\xf3` U+0344 → `\xf3` U+0308 U+0301;
- runs of more than 30 non-starters kept out of the vectors, or the CGJ behaviour defined;
- vectors limited to characters assigned in Unicode 16.0, the oldest version among the libraries in use (§7);
- on the Rust side, `unicode-normalization` on `utf8_chunks` pieces, with the Unicode version recorded.

## 4. Q2: case folding for `ignore_case`

### 4.1 What `(?i)` does in Go and Rust

**Go `regexp`.** A literal under `(?i)` matches every member of its `unicode.SimpleFold` orbit:

- the parser stores the smallest member;
- the compiler drops the fold flag for one-member orbits;
- the matcher walks the orbit.

Classes are folded range by range (each code point gets its whole orbit), and only then negated. POSIX, Perl and `\p{…}` classes are folded too. For example:

| Pattern | Parsed as |
|---|---|
| `(?i)[a-z]` | `[A-Za-zſK]`, with U+017F and U+212A |
| `(?i)[^k]` | `[^KkK]`, which does not match U+212A |
| `(?i)\w` | `[0-9A-Z_a-zſK]` |

A case-sensitive `[Kk]` does not match U+212A.

**Go's orbits are exactly Unicode simple case folding.** Over U+0000..U+10FFFF, the `unicode.SimpleFold` orbits of Go 1.27 equal the C+S equivalence classes of CaseFolding-17.0.0 with 0 discrepancies. `strings.EqualFold` and single-rune `(?i)` matching also show 0 discrepancies. CaseFolding-17.0.0 has 1,481 C, 31 S, 104 F and 2 T entries; its C+S relation forms 1,482 classes with more than one member.

**Rust `regex`.** It documents simple case folding (UTS #18 RL1.5) and works the same way:

- `regex-syntax` folds every code point of a range ("we must apply case folding before negation!");
- its orbits over all code points equal the C+S classes of CaseFolding-16.0.0 exactly.

About 400 probes gave identical results in Go and Rust, apart from the Unicode-version differences of §7. The probes were literals, classes and negated classes over Kelvin, ſ, İ/ı, ß/ẞ, Ohm, Angstrom, ς, ǅ, U+0345, Deseret, Cherokee and Georgian, plus structural cases such as `[^\P{Lu}]`, `[[:^lower:]]` and `(?-i:k)`. Some of the shared results:

- U+212A matches `k`;
- U+017F matches `s`;
- U+0130 and U+0131 match nothing but themselves;
- ß matches ẞ but never `ss`;
- U+0345, ι, Ι and U+1FBE all match each other;
- Cherokee U+13A0 matches U+AB70, and Georgian U+10D0 matches U+1C90.

Neither engine applies full (F) or Turkic (T) mappings.

### 4.2 The spec already decides the regex side

§29.5 says "`ignore_case` adds `(?i)`", which is simple case folding in both engines. Neither engine has an NTFS mode, and none can be added by rewriting the pattern text.

Upper-casing the text:

| Original | Rewritten | Effect |
|---|---|---|
| `\p{Ll}` | `\P{LL}` | accepted by both engines, and means the opposite |
| `\p{Greek}` | `\P{GREEK}` | accepted by Go, inverted |
| `\x{e9}`, `\t`, `(?-i:Foo)` | `\X{E9}`, `\T`, `(?-I:FOO)` | errors |
| `[à-ÿ]` | `[À-Ÿ]` | covers U+00C0..U+0178, so it matches `Ā` |

Lower-casing the text:

| Original | Rewritten | Effect |
|---|---|---|
| `\PL` | `\pl` | inverted |
| `\P{Lu}` | `\p{lu}` | inverted |
| `\S` | `\s` | inverted |
| `[Z-a]` | `[z-a]` | invalid range |
| `(?-i:Foo)` | `(?-i:foo)` | no longer matches `Foo` |

Folding the subject instead, while keeping `(?i)`, breaks the user's `(?-i:…)` groups.

NTFS semantics in regexes would therefore mean rewriting the parsed regex (literals and classes expanded to NTFS equivalence classes) in both `regexp/syntax` and `regex-syntax`, with byte-identical expansion code in both languages.

### 4.3 The NTFS `$UpCase` table

**Format.** `$UpCase` is MFT record 10. Its unnamed `$DATA` is exactly 131,072 bytes: 65,536 little-endian 16-bit entries indexed by UTF-16 code unit, used to compare and sort file names. Since Windows 8, volumes also carry a 32-byte `$UpCase:$Info` stream (length, CRC64, OS and build version).

**Comparison.** The open drivers compare names one UTF-16 code unit at a time through the volume's table. Every known table maps U+D800–U+DFFF to itself and never maps a non-ASCII unit to ASCII. Supplementary characters and unpaired surrogates are therefore compared exactly, with no case folding. Windows' own driver is closed source; that it compares the same way is inferred from the on-disk format, since the directory index is sorted with the table.

**Three default tables exist.** They were rebuilt from ntfs-3g's data arrays, and the MD5s match ntfs-3g's own comment:

| Table | Non-identity entries | MD5 | Origin |
|---|---|---|---|
| XP / Server 2003 | 636 | `6fa3db2468275286210751e869d36373` | closest to the Unicode 3.2/4.0 round-trip (68/71 differences) |
| Vista | 874 | `2f03b5a69d486ff3864cecbd07f24440` | Unicode 5.0 round-trip with 9 differences; equals Microsoft's published exFAT recommended up-case table (checksum E619D30D) |
| "6.1": Windows 7, 8, 10 | 973 | `7ff498a44e45e77374cc7c962b1b92f2` | Unicode 5.1.0 round-trip, exactly |

Volumes formatted by real systems confirm two of them:

- Server 2003 wrote the XP table and no `$Info`.
- Windows 10 builds 17134, 18894 and 18898 wrote the 6.1 table byte for byte, with `$Info` CRC64 `dadc7e776b1b690c`. ntfs-3g's `mkntfs` writes the same table and CRC, with the version fields set to zero.

No Windows 11 or Windows 7/8 volume was examined (§12). Going from XP to 6.1 changes 339 entries, and from Vista to 6.1 changes 109. One visible change: XP and Vista map ς to Σ, while 6.1 maps ς to itself.

**The 6.1 table can be regenerated exactly from public data.** Take Unicode 5.1.0 simple uppercase (UnicodeData field 12) on the BMP, and keep c → U only when the simple lowercase of U is c. This round-trip rule gives 0 differences from the real table (SHA-256 of the 973-entry table `41c26bc7a12bdaeb26025c93118697c7e3ef81ee048b00fe5cce2a472e0e0742`). Without the round-trip filter, 18 entries differ (U+00B5, U+0131, U+017F, U+01C5, U+0345, U+03C2, U+1FBE…). "BMP simple uppercase of current Unicode" is a poor approximation: Unicode 18 differs in 229 entries.

**Linux drivers.**

- `ntfs3` has no built-in table. It reads each volume's `$UpCase`, folds case only with the `nocase` mount option, and otherwise uses the table only for index collation.
- ntfs-3g also uses the volume's table. Case-insensitive access is the `ignore_case` option, available only in `lowntfs-3g`.

Both drivers' sources acknowledge that the table differs per volume.

**Licensing.** Microsoft publishes no NTFS table, only the exFAT one. ntfs-3g's table code is GPL-2.0-or-later, which a GPL-3.0-or-later project may use. Copying it is unnecessary, though: the 6.1 table derives from Unicode 5.1 data (Unicode License v3) plus the round-trip rule.

### 4.4 Samba

Windows clients reach the phase 1 pool through Samba. With the default `case sensitive = auto`, Samba is case-insensitive for them. Names are compared with `fname_equal` → `strequal` → `strcasecmp_m`:

- two code points are equal if their `toupper_m` values match or their `tolower_m` values match;
- both functions use built-in 65,536-entry tables, and code points at U+10000 and above are left unchanged;
- invalid UTF-8 falls back to `strncasecmp`, which folds ASCII only;
- there is no normalization;
- the exact name is tried first, and the case-insensitive scan returns the first match in directory order only after that fails.

Samba's upcase table is byte-identical to the XP NTFS table, and its lowcase table is the exact inverse. The Samba relation is therefore exactly the XP relation: 637 BMP pairs, including ς~σ~Σ. The tables are identical in 4.13, 4.19, 4.22 and master.

The 6.1 table with ς → Σ added (975 pairs) covers the XP, Vista, 6.1 and Samba relations, and is itself contained in simple folding.

### 4.5 NTFS upcase is strictly contained in simple folding

Every pair of characters that an NTFS table unifies is also unified by simple folding, never the other way round:

| Comparison | Pairs equal under folding only | Pairs equal under NTFS only |
|---|---|---|
| 6.1 table against Go 1.27 folding (Unicode 17), all code points | 572 (265 in the BMP, 307 involving a supplementary code point) | 0 |
| 6.1 table against Unicode 17 folding, NFC-stable code points only | 561 (254 in the BMP) | 0 |
| 6.1 table against Unicode 18 folding, BMP only | 269 (of 1,242 folding pairs; NTFS has 973) | 0 |
| XP table against Go 1.27 folding | 908 | 0 |
| Vista table against Go 1.27 folding | 670 | 0 |

The containment holds for CaseFolding 5.1, 16, 17 and 18, and for the XP, Vista and 6.1 tables.

Consequence: with simple folding for `ignore_case`, two characters that any Windows table or Samba unifies always get the same rule verdict. For whole names this holds as long as both names are already in NFC; §5.1 shows the exception.

The 269 BMP pairs unified only by folding (against Unicode 18) fall into three groups:

- **A.** Characters that were case-related in Unicode 5.1 but deliberately not unified by Windows: 20 classes, 45 pairs. They include:
  - K/k/K (U+212A), Ω/ω/Ω (U+2126), Å/å/Å (U+212B);
  - S/s/ſ (U+017F), µ (U+00B5)/μ, ẞ/ß;
  - the four titlecase digraphs like ǅ, U+0345/ι and U+1FBE;
  - Greek symbol variants (ϐ ϑ ϴ ϕ ϖ ϰ ϱ ϵ), ς/σ/Σ and ẛ.
- **B.** Characters assigned after Unicode 5.1: 210 classes, 221 pairs. They include Cherokee lowercase, 46 Georgian Mtavruli, Latin Extended-D additions, Cyrillic extensions, Coptic, Glagolitic and Greek Yot.
- **C.** Characters that existed in 5.1 but whose folding came later: U+0390/U+1FD3, U+03B0/U+1FE3, U+FB05/U+FB06.

Above U+FFFF, Unicode 18 simple folding has 324 mappings that NTFS cannot express at all: Deseret, Osage, Old Hungarian, Adlam, Beria Erfe and others.

### 4.6 Which differences survive NFC

§11.3 applies NFC before folding. Several characters have canonical singleton decompositions, so NFC removes them before the choice of table matters:

| Character | NFC |
|---|---|
| U+212A KELVIN SIGN | K |
| U+2126 OHM SIGN | Ω (U+03A9) |
| U+212B ANGSTROM SIGN | Å (U+00C5) |
| U+1FBE | ι (U+03B9) |
| U+1FD3, U+1FE3 | U+0390, U+03B0 |

The Kelvin, Ohm and Angstrom signs therefore do not discriminate between the two options. Likewise, the only non-ASCII character that simple folding, but not NTFS, can match to an ASCII letter after NFC is ſ (U+017F) ~ `s`. NFC itself makes U+037E equal `;` and U+1FEF equal `` ` `` under either option.

What remains different after NFC:

- ſ/s, ẞ/ß, ς/σ/Σ, µ/μ (U+00B5 has only a compatibility decomposition);
- the titlecase digraphs;
- the Greek symbol variants;
- letters assigned after Unicode 5.1;
- every supplementary script.

For a media library the two options behave identically on ASCII and on Latin-1, except for µ and ß/ẞ. Q2 is mainly a question of coherence and determinism, not of everyday behaviour.

### 4.7 Globs and regexes under one `ignore_case`

The in-house glob matcher was copied to a scratch module and run in three folding variants: NTFS 6.1 upcase, the pattern text pre-folded with C+S, and orbit closure. The results were compared with a regex using `(?i)`:

| Glob | Path | NTFS | C+S pre-folded | Orbit closure | Regex `(?i)` |
|---|---|---|---|---|---|
| `*ΖΟΡΜΠΆΣ*` | `Ζορμπάς ο Έλληνας (1964).mkv` | no | yes | yes | yes |
| `*straße*` | `STRAẞE (2026).mkv` | no | yes | yes | yes |
| `*საქართველო*` | `ᲡᲐᲥᲐᲠᲗᲕᲔᲚᲝ.mkv` (Mtavruli) | no | yes | yes | yes |
| `*μ*` | `10µF.pdf` | no | yes | yes | yes |
| `*ꮳꮃꭹ*` | `ᏣᎳᎩ.txt` (Cherokee) | no | yes | yes | yes |
| `*𐐨*` | `𐐀.txt` (Deseret) | no | yes | yes | yes |
| `[A-z]*` | `_draft.mkv` | no | no | yes | yes |
| `[À-Þ]*` | `×2.mkv` | yes | no | yes | yes |
| `[À-Þ]*` | `÷2.mkv` | no | yes | no | no |
| `[à-ÿ]*` | `ā.mkv` | yes | no | no | no |
| `[Z-a]*` | `z.mkv` | error | error | yes | yes |

Orbit closure agrees with `(?i)` on every row. A random differential test of 290,403 valid patterns built from tricky characters (ſ ß ẞ σ ς µ İ ı U+0345 ǅ × ÷ …) gave 0 disagreements between the orbit-closure glob and Go `(?i)`. Neither NTFS folding nor pre-folding the pattern text can match regex behaviour on classes. Pre-folding range endpoints also creates invalid or wrong ranges:

- `[Ā-ſ]` folds to `[ā-s]`, an inverted range;
- under NTFS, `[à-ÿ]` becomes `[À-Ÿ]`.

### 4.8 Arguments for the NTFS table

The arguments for the NTFS table, stated fairly:

- **Frozen.** It is fully specified and frozen, and can be regenerated bit-exactly from Unicode 5.1. Go and Rust would embed the same table, so glob and `ext` vectors could never drift between the two implementations. Simple folding drifts with each runtime's Unicode version.
- **One folding function.** The codebase would have a single folding function, shared with `lpath_key`, and `ignore_case` would mean "as Windows would".
- **Fewer cross-script matches.** It never matches a non-ASCII name character to an ASCII pattern letter. After NFC, though, this only concerns ſ (§4.6).

Against it:

- It cannot be used for regexes without rewriting them (§4.2), so it would give `ignore_case` two meanings.
- It is deliberately incomplete and frozen at Unicode 5.1. It splits ς from σ, ẞ from ß, µ from μ, newer letters, and everything outside the BMP.
- It is not one table (§4.3).
- Its purpose is different. `lpath_key` predicts what a client will merge; `ignore_case` expresses what the user wants a rule to select. CLAUDE.md invariant 15 already scopes it: "NFC and NTFS-style case folding only detect collisions".

Two things are the same for both options:

- **`ß` and `ss`.** Simple folding and NTFS upcase both keep them distinct; only full folding (`x/text/cases.Fold`) merges them. So §15.8's example does not discriminate. Better examples for §15.8 would be ẞ/ß, ς/σ or ſ/s, not the Kelvin sign, which NFC already turns into `K`.
- **Exact Go/Rust parity for regexes.** If it is required, the regex-rewrite cost is paid with either folding, because neither can rely on the engines' `(?i)`, which differs between Go and Rust (§7).

### 4.9 Recommendation for Q2

Use Unicode simple case folding for `path`, `filename` and `ext`, with the same semantics as `(?i)`:

- **Literals.** A pattern character matches a path character when both are in the same simple-fold orbit. Any fixed orbit representative, or a direct orbit-membership test, gives the same results. The representative only matters if a folded string is stored or normalized again. Never use the orbit minimum for such keys: the minimum of the ι orbit is U+0345, a combining mark.
- **Classes.** Close each class over orbits first, then negate. Never fold the class text or its range endpoints.
- **Strings.** Compare code point by code point on NFC strings, with no normalization after folding, exactly as `(?i)` does (§5).
- **Regexes.** Only add `(?i)`. Never fold the regex subject or the pattern text.
- **`ext`.** Compare code points, not byte offsets: 52 orbit pairs differ in UTF-8 length (K/K, s/ſ, ß/ẞ, å/Å…).

Where the folding data comes from is a separate choice:

| Option | Effect |
|---|---|
| (a) Each implementation's regex engine data: `unicode.SimpleFold` in Go, `regex_syntax::hir::ClassUnicode::case_fold_simple` in Rust | Globs and regexes agree by construction within each implementation, and no third Unicode version has to be kept in sync. Go and Rust differ on recently changed characters, exactly as their regexes already do. |
| (b) A table generated from one pinned CaseFolding.txt (about 1,500 C+S entries) and embedded in both versions | Globs and `ext` are identical in Go and Rust, but can disagree with regexes on characters whose folding changed between Unicode versions. |

Option (a) is the simpler one, and the vectors must avoid recently changed characters in any case (§7). Either is defensible; the spec should state which.

Keep the NTFS table for `lpath_key` only (§8).

## 5. How NFC and case folding interact

### 5.1 Folding does not preserve NFC

CaseFolding.txt itself warns: "case folding does not preserve normalization formats!". Single code points are safe: for all 1,112,064 scalar values, folding an NFC character gives an NFC result, and folding commutes with NFC. Sequences are not safe. An exhaustive search over two-code-point NFC strings found exactly 41 whose C+S fold is not NFC. In each, an uppercase base plus a mark has no precomposed form, but the lowercase base plus the same mark does:

| NFC string | Fold | NFC of the fold |
|---|---|---|
| `J` U+030C | `j` U+030C | U+01F0 ǰ |
| `H` U+0331 | `h` U+0331 | U+1E96 ẖ |
| `T` U+0308 | `t` U+0308 | U+1E97 ẗ |
| U+017F U+0301 | `s` U+0301 | U+015B ś |
| U+03AA U+0301 | U+03CA U+0301 | U+0390 |

The NTFS 6.1 table has exactly one such case: `i` U+0307 upcases to `I` U+0307, whose NFC is U+0130 İ.

Unicode's canonical caseless match (definition D145) avoids this by normalizing on both sides of full case folding: NFD(toCasefold(NFD(X))). The W3C character model calls dropping the final normalization a "willful violation" of D145 that Unicode confirmed acceptable for comparison. Go and Rust `(?i)` implement neither form. They compare NFC strings code point by code point, by orbit:

- `(?i)^ǰ$` does not match `J` U+030C;
- `(?i)^ẖ$` does not match `H` U+0331;
- `(?i)^ᾳ$` does not match `αι`.

There are 63 Greek precomposed characters whose NFD contains U+0345.

So a glob that folds and then normalizes again would disagree with regexes. To keep globs and regexes consistent, globs must fold per character with no NFC pass after folding, and these cases simply do not match. The limit should be pinned in vectors.

The same per-character design means that two names Windows treats as one can get different rule verdicts. `j` U+030C and `J` U+030C are one name on Windows, which upcases per code unit without normalizing. But NFC turns the first into U+01F0, which has no simple fold to `J` U+030C. NTFS folding for rules would have exactly the same defect, so this is a limit to document, not an argument for either option.

### 5.2 NFC applied to the pattern text

§11.3 applies NFC to "the pattern", which for a regex means its source text. This can change what a class or quantifier means, identically in Go and Rust:

- `^[e` U+0301 `]$` becomes `^[é]$` and no longer matches `e`;
- `^e` U+0301 `?$` becomes `^é?$`: the quantifier now applies to the whole character, so it no longer matches `e`;
- `^[` U+2126 `-` U+2127 `]$` becomes `^[` U+03A9 `-` U+2127 `]$`, a much larger range that now matches Cyrillic `Ж`;
- escaped marks such as `\x{301}` are not normalized, so `e\x{301}` can never match an NFC subject.

With the path also in NFC, `[é]` matching an NFD `é` is the intended effect; the other cases are hazards. They are rare in real patterns, but the spec should state the rule and the vectors should pin it. Using NFD instead would fix the 41 cases of §5.1, but would break classes and ranges that hold precomposed letters, such as the existing `[à-ÿ]` glob vectors, so NFC remains the right form.

### 5.3 Separators, extensions and glob syntax are safe

- No code point's decomposition contains `.` or `/`, and nothing composes or reorders across them.
- No code point folds to `.`, `/` or a glob metacharacter, under simple folding, orbit-minimum folding or NTFS upcase.
- No metacharacter composes with a following mark.

NFC and folding therefore give the same result whether they are applied before or after splitting a path into components or extracting an extension, and they leave glob syntax intact. Inside a class, though, NFC can merge a base and a mark: `[aé]` written with U+0301 becomes `[aé]`.

For `ext`, after NFC, ſ (U+017F) is the only non-ASCII code point in the orbit of an ASCII letter. The spec does not define what an extension is (§10).

## 6. Paths that are not valid UTF-8

§11.3 is silent on names that are not valid UTF-8; Linux paths are raw bytes. The glob vectors already pin per-byte semantics for the glob matcher: each invalid byte is one character, matched only by `?`, `*` and negated classes. Regexes diverge between Go and Rust:

| Pattern | Subject | Go `regexp` | Rust `regex::bytes` (Unicode mode) |
|---|---|---|---|
| `^.$` | `\xff` | true | false |
| `^[^a]$` | `\xff` | true | false |
| `^\x{FFFD}$` | `\xff` | true | false |
| `^a.b$` | `a\xffb` | true | false |
| `^a...b$` | `a\xed\xa0\x80b` | true | false |
| `(?i)^Films/.*\.mkv$` | `Films/\xe9t\xe9.MKV` | true | false |

Go reads each invalid byte as one U+FFFD (`regexp.go`). Rust's `regex::Regex` only accepts `&str`, and `String::from_utf8_lossy` counts differently from Go: it turns `\xe2\x82` and `\xf0\x9f\x98` into a single U+FFFD each, where Go and the glob vectors count 2 and 3 characters. To match Go, the Rust version would have to decode one U+FFFD per invalid byte and use the `str` API. The other option is that `re:` conditions never match a name that is not valid UTF-8. The spec has to choose (§10).

For NFC, the per-run rule of §3.4 covers these names. `lpath_key` is NULL for them (§15.8).

## 7. Unicode versions across Go and Rust

| Component | Unicode version |
|---|---|
| Go 1.26: `unicode`, `regexp`, `x/text` v0.43.0 `norm` | 15.0.0 |
| Go 1.27: `unicode`, `regexp` `(?i)` and `\p{…}`, `x/text` v0.43.0 `norm` and `cases` | 17.0.0 |
| rustc 1.97 `char::UNICODE_VERSION` | 17.0.0 |
| Rust `regex` 1.13.1 / `regex-syntax` 0.8.11 (16.0.0 since 0.8.5, 2024-09-29) | 16.0.0 |
| `unicode-normalization` 0.1.25 | 17.0.0 |
| `unicode-normalization` 0.1.26 | 18.0.0 |
| `icu_normalizer_data` 2.2.0, 2.3.0 | 17.0.0 (ICU 78.1rc) |
| `icu_normalizer_data` 2.4.0 | 18.0.0 |
| NTFS 6.1 `$UpCase` | 5.1.0, frozen |

Measured consequences:

- **Case folding.** Go 1.27 and Rust `regex` differ on 56 code points under `(?i)`, exactly the 28 pairs added by CaseFolding 17.0.0:
  - U+A7CE/A7CF, U+A7D2/A7D3 and U+A7D4/A7D5, which are BMP Latin letters;
  - Beria Erfe U+16EA0–16EB8 against U+16EBB–16ED3.

  For example, `(?i)\x{A7CE}` matches U+A7CF in Go but not in Rust.
- **Older Go.** Go 1.26 (Unicode 15) and Rust differ on 60 entries, including characters assigned long ago: U+0390/U+1FD3, U+03B0/U+1FE3, U+FB05/U+FB06. CaseFolding 16 added these S mappings. NFC hides the first two, but not FB05/FB06.
- **Properties.** `\p{L}`, which §29.5 recommends, matches 4,644 more code points in Go 1.27 than in Rust. `(?i)\p{Ll}` differs on 54 code points that only Go matches, and on U+0295, which only Rust matches, because U+0295 moved from Ll in Unicode 16 to Lo in Unicode 17.
- **Unicode 18.** CaseFolding 18.0.0 differs from Go 1.27 on 43 code points:
  - 20 new pairs (U+A7DD/0277, U+A7E2/027C, U+AB6C/AB4B, U+AB6D/AB4C and 16 pairs in U+1DF40–1DF7F);
  - U+1DF95 joining the ß/ẞ orbit through an S mapping.
- **NFC.** Go and Rust NFC differ only when the Rust crate is at a different Unicode version (§3.4).

Go cannot choose its Unicode version: `regexp` and `unicode.SimpleFold` use the toolchain's tables. The Rust version could patch `regex-syntax` tables (not tested), but no released `regex-syntax` uses Unicode 17 today. Exact Go/Rust identity is therefore impossible for regexes with stock libraries. A conformance vector on a character that changed between the versions in use passes in Go and fails in Rust, so it cannot "pin the version boundary" for both.

The honest spec wording is: "Unicode version of the implementation; characters whose case folding, properties or normalization changed after Unicode 16.0 are implementation-defined and not covered by the vectors". The practical rule is that the Unicode version is the Go toolchain's `unicode.Version`, and the Rust version follows it as closely as its crates allow.

## 8. `lpath_key` (§15.8)

These findings concern `lpath_key`, whatever Q2 decides:

- **Which table.** "The NTFS upcase table" is ambiguous (§4.3). The spec should name one. Two candidates:
  - the 6.1 table, written by Windows 7, 8 and 10 and by `mkntfs`, defined as the Unicode 5.1 round-trip simple uppercase on the BMP, with its SHA-256 pinned;
  - the 6.1 table plus ς → Σ (975 pairs), which also covers XP, Vista and Samba.

  The table should be generated from Unicode data, not copied.
- **Supplementary characters** stay unchanged, since the table applies per UTF-16 code unit, as in Windows, ntfs-3g, ntfs3 and Samba.
- **Order of operations.** §15.7 must report names that differ only by normalization and case. A test of 399 pairs, each a precomposed lowercase letter against its uppercase decomposed form, gave:

  | Key | Collisions missed |
  |---|---|
  | NFC → upcase | 30 |
  | NFC → upcase → NFC | 30 |
  | NFD → upcase → NFC | 0 |

  Missed examples: ǰ against `J` U+030C, ΐ against Ϊ́, ẖ against `H` U+0331, ὐ against `Υ` U+0313, ᾲ against `Ὰ` U+0345. NFD is in the same `x/text/unicode/norm` package, which strengthens Q1.
- **The example in §15.8.** `ß` and `ss` should be replaced by an example that actually distinguishes the NTFS table, such as ẞ/ß, ς/σ or ſ/s.

## 9. APIs to avoid

| API | Problem |
|---|---|
| `strings.EqualFold`, `bytes.EqualFold` | Any two invalid bytes compare equal, an invalid byte equals U+FFFD, and two different WTF-8 lone surrogates (`\xed\xa0\x80`, `\xed\xb0\x80`) compare equal. |
| `strings.ToLower`, `strings.ToUpper` | Invalid bytes become U+FFFD. Results differ from Rust: Go lower-cases `İ` to `i`, Rust to `i` U+0307; Go upper-cases `ß` to `ß`, Rust to `SS`; Rust lower-cases a final `Σ` to `ς` by context, Go does not. |
| `golang.org/x/text/cases.Fold` | Full case folding only: `ß` and `ẞ` → `ss`, `ﬃ` → `ffi`, `İ` → `i` U+0307. The `Compact` option is documented as unimplemented and ignored. It also maps the 86 Cherokee capitals U+13A0–U+13F5 to the small letters while mapping the small letters back to capitals, so `Fold("Ꭰ") != Fold("ꭰ")`. This was reported as golang/go#46101; whether that issue is still open was not checked. |
| Rust `str::to_lowercase`, `to_uppercase` | Full and context-sensitive mappings; not simple folding. |
| `String::from_utf8_lossy` | Merges a truncated sequence into one U+FFFD (§6). |
| `icu_normalizer` 2.4.0 `normalize_utf8` | Replaces invalid input with U+FFFD. 2.2.0 and 2.3.0 are unsound on invalid UTF-8. |
| Pattern-text rewriting (upper- or lower-casing a regex) | Silently inverts `\p` classes and breaks escapes (§4.2). |

## 10. Open points for the spec

These are spec changes or ambiguities; per CLAUDE.md they need the maintainer's decision.

1. **Q1 and Q2 themselves** (§3.6, §4.9), including where the folding data comes from: the engines' tables or an embedded table.
2. **Class semantics under `ignore_case`** (§4.9): orbit closure, then negation. §11.3's "both are also case-folded" is ambiguous for classes and ranges.
3. **Order of NFC and folding in rule matching** (§5.1): per-character folding on NFC strings with no normalization afterwards, and its known limits (`J` U+030C against ǰ, `ᾳ` against `αι`).
4. **NFC of regex source text** (§5.2): accept and document, or restrict it.
5. **Rule matching on names that are not valid UTF-8** (§6): `re:` never matches, or the Rust version decodes like Go.
6. **A Unicode-version clause** (§7) qualifying §11.3's promise that "the Go and Rust versions match exactly the same paths".
7. **Stream-safe CGJ insertion** (§3.4): keep runs of more than 30 non-starters out of the vectors, or define the behaviour.
8. **`lpath_key`** (§8): the exact table, the order NFD → upcase → NFC, and a better example than `ß`/`ss`.
9. **What an extension is for `ext`**: the last dot only, multi-dot entries such as `tar.gz`, names starting with a dot such as `.nfo`, or a trailing dot. §11.1 only says "extensions, without the dot".
10. **More rejected regex escapes.** §29.5 rejects `\d`, `\w`, `\s` and `\b` but not `\D`, `\W`, `\S` and `\B`, which diverge the same way:

    | Pattern | Subject | Go | Rust |
    |---|---|---|---|
    | `^\S$` | U+00A0 | true | false |
    | `^\D$` | `٣` | true | false |
    | `^\W$` | `é` | true | false |
    | `^x\B` | `xé` | false | true |

11. **An upstream report.** The `x/text` F0–F7 defect of §3.4 could be reported to the Go project. It has not been reported.

## 11. Claims the verification corrected

| Original claim | Verdict | Correction |
|---|---|---|
| `x/text` NFC on invalid UTF-8 always equals "NFC of each valid run, other bytes copied". | Refuted | 168 of 854,065,152 inputs differ: a final F0–F7 byte followed by one of 7 two-byte code points (§3.4). The original random corpus contained no bare F0–F7 byte. |
| Rust `unicode-normalization` with `utf8_chunks` is byte-identical to Go `x/text` on raw bytes. | Uncertain | True on valid text (re-checked on 1,000,000 strings). False on raw bytes unless Go splits the input into valid runs itself. |
| The `x/text` composition bug affects v0.37.0 to v0.41.0. | Confirmed, scope widened | Every release before v0.42.0 is affected, including the Unicode 15 tables (v0.20.0, v0.33.0). |
| The in-house NFC prototype is about 119 lines and matches `x/text`. | Uncertain | The 119 lines exclude the table generator and data. Its match with `x/text` held only because the corpus could not reach the F0–F7 case; on that case the prototype gives the intended per-run result. |
| Rust `.stream_safe().nfc()` matches Go except with Hangul. | Refuted | About 51 other ccc = 0 code points also count as non-starters in Go but not in Rust (§3.4). |
| `icu_normalizer` =2.3.0 is a Unicode 17 option for Rust. | Dropped | 2.2.0 and 2.3.0 are unsound on invalid UTF-8. Use `unicode-normalization` =0.1.25 on `utf8_chunks` pieces. |
| Simple folding lets ASCII patterns match non-ASCII names such as the Kelvin sign, and NTFS does not. | Refuted in context | NFC turns U+212A into `K` first, under either option. Only ſ is specific to simple folding (§4.6). |
| Names that collide on Windows always get the same rule verdict. | Refuted | `j` U+030C and `J` U+030C collide on Windows but get different rule keys (§5.1). The containment holds per character, and for names already in NFC. |
| The canonical folding representative must be the C+S target. | Refuted | For matching, any fixed representative or an orbit-membership test gives the same results; only stored or re-normalized keys care, and must avoid the orbit minimum (U+0345). |
| Orbit-minimum folding breaks NFC in 1,933 two-code-point strings. | Uncertain | An independent exhaustive run counted 2,059; the counts for C+S (41) and NTFS (1) were confirmed. The figure does not affect the decision. |
| A conformance vector can pin the Unicode version boundary. | Refuted | Such a vector passes in Go and fails in Rust; vectors have to avoid characters that changed across the versions in use (§7). |
| Using Go's `x/text` keeps NFC and folding coherent automatically. | Too strong | They match under Go 1.27, but the `go1.27` build tag keeps `x/text` v0.43.0 at Unicode 17 on later toolchains until `x/text` is updated (§3.4). |

## 12. What was not verified

- **The Unicode standard text itself.** The stability policy, UAX #15 and definition D145 were quoted from mirrors and secondary sources (a UAX #15 mirror, `rust-caseless`, W3C charmod-norm), because www.unicode.org was unreachable.
- **The FSF licence list** was read from a mirror of its data, because gnu.org was unreachable.
- **Windows tables.** No Windows 11, Server 2022, Server 2025, Windows 7 or Windows 8 volume was examined. That Windows 7 and 8 write the 6.1 table rests on ntfs-3g's comment and builder. Whether Windows or `chkdsk` ever rewrites an old table on an existing volume was not checked.
- **The Windows NTFS driver** is closed source: comparison per UTF-16 code unit through the volume table is inferred from the on-disk format and the open drivers.
- **Copyright.** Whether a 973-entry mapping table is copyrightable is a legal question that was not settled.
- **Upstream timing.** When `regex-syntax` will move to Unicode 17 or 18, and when Go and `x/text` will move to Unicode 18, is unknown.
- **The open state of golang/go#46101** could not be checked.
- **macOS clients** (SMB from a Mac, APFS/HFS+ normalization) were not examined.
- **NFC/folding order counts** on larger sets vary with the test set. Reported counts include 45 and 1,099 on the 4.7-million-pair set, and 80 on about 4.4 million single-code-point-plus-suffix strings. They were not reproduced; the exhaustive two-code-point count of 41 was.
- **The upstream count of 15,379 affected inputs** for the `x/text` composition bug was not reproduced; the narrower test set found 1,036 differences between v0.41.0 and v0.43.0.

## 13. Sources

Unicode data, from ICU release tags, since unicode.org was unreachable:

| File | Source | SHA-256 |
|---|---|---|
| CaseFolding-16.0.0.txt | `unicode-org/icu` tag `release-77-1` | — |
| CaseFolding-17.0.0.txt (2025-07-30) | `unicode-org/icu` tags `release-78.1`, `release-78.3` | `ff8d8fefbf123574205085d6714c36149eb946d717a0c585c27f0f4ef58c4183` |
| CaseFolding-18.0.0.txt (2026-02-03) | `unicode-org/icu` `main` and tag `release-79.1`; also `unicode-org/unicodetools` `data/ucd/18.0.0` (identical) | `a004797658a457bec4dc11683e39f69249ea3b595b752dbea6721c4c9f587b0d` |
| NormalizationTest-17.0.0.txt | `unicode-org/icu` tag `release-78.3` | — |
| NormalizationTest-18.0.0.txt | `unicode-org/icu` tag `release-79.1` | — |
| UnicodeData 5.1.0 | `unicode-org/unicodetools` `data/ucd/5.1.0-Update` | — |

Source code read:

- Go 1.27.0 toolchain sources: `regexp/syntax/parse.go` (`literal`, `minFoldRune`, `appendFoldedRange`, class negation), `regexp/syntax/compile.go`, `regexp/syntax/prog.go`, `unicode/letter.go`, `unicode/tables.go`, `strings/strings.go`, `src/vendor/modules.txt`.
- `golang.org/x/text` v0.43.0 and its git history (`unicode/norm`, `cases`); commits f53c316, 37867f6, 0dd525f and 5ae8e57.
- `regex` 1.13.1 (`src/lib.rs`, `UNICODE.md`) and `regex-syntax` 0.8.11 (`src/unicode_tables/case_folding_simple.rs`, `src/hir/mod.rs`, `src/hir/translate.rs`).
- `unicode-normalization` 0.1.25 and 0.1.26 (`src/tables.rs`); `icu_normalizer` 2.3.0 and 2.4.0.
- ntfs-3g (`libntfs-3g/unistr.c`, `include/ntfs-3g/param.h`, `ntfsprogs/mkntfs.c`, `src/ntfs-3g.8.in`).
- Linux `fs/ntfs3` (`super.c`, `upcase.c`, `namei.c`).
- Samba (`source3/smbd/filename.c`, `source3/smbd/files.c`, `lib/util/util_str.c`, `lib/util/charset/util_str.c`, `lib/util/charset/codepoints.c`, `docs-xml/manpages/smb.conf.5.xml`).
- Microsoft documentation: exFAT specification (recommended up-case table), WSL case sensitivity.

NTFS images: `dfir_ntfs` test images (`vss_2003.raw`, `ntfs_index.raw`, `vss_10.raw`), Velocidex `go-ntfs` `test.ntfs.dd`, ColinFinck `ntfs` test images made with `mkntfs`.

Spec references: `specs.md` §11.1, §11.3, §15.7, §15.8, §29.5, §29.7; CLAUDE.md invariant 15.
