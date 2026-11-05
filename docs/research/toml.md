# Choosing a TOML library

This report backs the choice of `github.com/pelletier/go-toml/v2` and the move of the contract to TOML 1.1 (`specs.md` §24, §29.3, §29.7). It is research, not a contract: `conformance/vectors/config.json` is the contract.

## Candidates

| | `github.com/pelletier/go-toml/v2` v2.4.3 | `github.com/BurntSushi/toml` v1.6.0 | Rust `toml` 1.2.0 |
|---|---|---|---|
| Date | July 2026 | December 2025 | |
| License | MIT | MIT | MIT or Apache-2.0 |
| Third-party dependencies at run time | none | none | |
| Added to a binary, `-s -w` | 709 KB | 561 KB | |
| Says it follows | TOML 1.1.0 | TOML 1.1.0 | TOML 1.1.0 (`+spec-1.1.0`) |

## Against the official corpus

`toml-test` v2.2.0, `tests/files-toml-*`. Each parser decoded into its generic tree: maps, lists and scalars.

| Corpus | | go-toml | Rust `toml` | BurntSushi |
|---|---|---|---|---|
| 1.1.0 | valid files accepted | 214/214 | 214/214 | 214/214 |
| 1.1.0 | invalid files refused | 467/467 | 467/467 | 451/467 |
| 1.0.0 | valid files accepted | 205/205 | 205/205 | 205/205 |
| 1.0.0 | invalid files refused | 465/474 | 465/474 | 449/474 |

- On all 746 files of the two lists, go-toml and Rust's `toml` give the same verdict and the same value. Dates and times are compared by kind only, as no key takes one.
- The 16 files that only BurntSushi accepts extend a table already defined, for example `[a]`, `b.c = 1`, then `[a.b]`.
- The 9 files of the 1.0.0 list that go-toml and Rust accept all use syntax that TOML 1.1 allows: no seconds in a time, a line break or trailing comma in an inline table, a `\x` escape. All three also accept `\e`. So none of the three refuses TOML 1.1 syntax, and "TOML 1.0" in the contract was not true of any current version.

## What the corpus does not ask

Each was tried on all three.

| Input | go-toml | Rust `toml` | BurntSushi |
|---|---|---|---|
| A leading byte order mark | refused | accepted | accepted |
| CRLF line endings; no final newline | accepted | accepted | accepted |
| A bare CR | refused | refused | refused |
| Invalid UTF-8, in a value or a comment | refused | refused | refused |
| NUL, a control character or DEL in a comment | refused | refused | refused |
| An integer past the signed 64-bit range | refused | refused | refused |
| The same key twice, also when one is quoted | refused | refused | refused |
| `[[disk]]` after `[[rule]]`, then `[[rule]]` | accepted | accepted | accepted |
| `"é"` in NFC and in NFD, side by side | two keys | two keys | two keys |
| A bare key outside ASCII | refused | refused | refused |

## Decoding into a typed structure

This is the natural way to load a configuration, and it is where the libraries part. Rust used `#[serde(deny_unknown_fields)]`; BurntSushi, `MetaData.Undecoded`; go-toml, `DisallowUnknownFields`.

| Input, for a field `pool_id` | go-toml | BurntSushi | Rust `toml` |
|---|---|---|---|
| `Pool_ID = "x"` | accepted as `pool_id` | accepted as `pool_id` | refused |
| `[Layout]`, `ROOT = "x"`, `[[DISK]]` | accepted | accepted | refused |
| `pool_id = "x"` and `Pool_ID = "y"` | accepted, `y` | accepted, `y` | refused |
| `Pool_ID = "y"` and `pool_id = "x"` | accepted, `x` | accepted, `y` | refused |
| `[disk]` for a list of tables | accepted, a list of one | refused | refused |
| Unknown key, at any depth | refused | listed as undecoded | refused |
| `replicas = "1"`, `1.0` or `true` | refused | refused | refused |
| 300 into a signed 8-bit field | refused | refused | refused |

So neither Go library can tell `Pool_ID` from `pool_id`, and go-toml keeps whichever came last. Atoll checks the generic tree against the schema before anything converts it, with keys compared byte for byte (`specs.md` §24.3). That tree keeps every key as written, in all three.

## Choice

go-toml, because it agrees with the library the Rust version will use on every file measured, where BurntSushi would let a file through that the Rust version refuses. Its one difference, the byte order mark, is one line to remove.

Atoll strips one leading byte order mark before parsing, as Rust's `toml` does on its own. A second one is a syntax error in both.

## Not measured

- Whether go-toml or Rust's `toml` has a setting that keeps to TOML 1.0. The contract moved to 1.1 whatever the answer, as nothing holds a library to an older version forever.
- Later versions of these libraries. `conformance/vectors/config.json` pins what they must still do, including the byte order mark and the cases above.
