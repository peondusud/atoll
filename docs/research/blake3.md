# Choosing a BLAKE3 library

This report backs the choice of `github.com/zeebo/blake3` (`specs.md` §29.7). It is research, not a contract: `conformance/vectors/blake3.json` is the contract.

## Candidates

Neither the Go 1.27 standard library nor `golang.org/x/crypto` has BLAKE3. cgo is reserved for the FUSE view, so a binding to the C library is out.

| | `github.com/zeebo/blake3` v0.2.4 | `github.com/zeebo/blake3` main | `lukechampine.com/blake3` v1.4.1 |
|---|---|---|---|
| Date | August 2024 | commit `28a397d`, October 2026, 29 commits past v0.2.4 | May 2025 |
| License | CC0 1.0 | CC0 1.0 | MIT |
| amd64 assembly | AVX2, SSE4.1 | AVX-512, AVX2, SSE4.1 | AVX-512, AVX2 |
| arm64 assembly | none | NEON, SVE2 (added July and August 2026) | none |
| Run-time dependency | `klauspost/cpuid/v2` | `golang.org/x/sys` | `klauspost/cpuid/v2` |
| Goroutines | none | none | `min(buffers, runtime.NumCPU())`, with no setting |

All these licenses are compatible with GPL-3.0-or-later.

`lukechampine.com/blake3` v1.4.0 was retracted: for certain inputs, `Sum` depended on how the input was split between writes (its pull request 26). Its later versions fix that.

## Correctness

All three candidates, on amd64 in each SIMD mode, and on arm64 under `qemu-aarch64`:

- the 35 cases of the official `test_vectors.json`, at once and in random pieces;
- a `Sum` in the middle of a stream, and `Reset`;
- 50 lengths of random bytes around the sizes where the tree changes shape (up to 31 MiB): the two libraries agree, and `b3sum` 1.8.1 agrees with them.

On zeebo's main branch, SVE2, NEON and pure Go were each forced in turn under `qemu-aarch64 -cpu max`. No candidate gave a wrong hash anywhere.

## Throughput

Go 1.27.2 on an Intel i7-12700K in a container (Linux 6.6, WSL2), with nothing else running. Median of five runs of one second, in MB/s. "Both" is the real indexing work: one read feeding the whole-file hash and the chunk hash.

| Library and mode | Cores | Chunk of 4 MiB | File, reads of 1 MiB | Both | Allocations per 64 MiB |
|---|---|---|---|---|---|
| zeebo main, AVX2 | 1 | 4846 | 3912 | 2102 | 2 |
| zeebo v0.2.4, AVX2 | 1 | 4784 | 3879 | 2091 | 2 |
| lukechampine, AVX2 | 1 | 3473 | 2876 | 1483 | 10,756 |
| lukechampine, AVX2 | 4 | 11382 | 7663 | 5182 | 10,756 |
| lukechampine, AVX2 | 20 | 14512 | 8090 | 6901 | 10,756 |
| zeebo main, SSE4.1 | 20 | 1239 | 1215 | 610 | 2 |
| zeebo main, pure Go | 1 | 980 | 968 | 483 | 2 |
| zeebo v0.2.4, pure Go | 1 | 829 | 811 | 410 | 2 |
| lukechampine, no AVX | 1 | 767 | 756 | 382 | 10,756 |

- The first run compared them at the default `GOMAXPROCS` and made lukechampine look three times faster. It was using every core. On one core, zeebo main is 36% to 42% faster, depending on the column.
- zeebo uses one core, as its table shows at 1 core and at the default. Feeding the two hashes from two goroutines gives 4197 MB/s for "Both" on zeebo main.
- A disk reading 250 MB/s needs 12% of one such core for "Both" with AVX2, and 52% in pure Go. So on amd64, throughput does not decide.

Not measured:

- AVX-512: this CPU has none.
- arm64 throughput: `qemu-user` says nothing about speed. Pure Go on this CPU is an upper bound for pure Go on a small arm64 board, and NEON on real hardware is unknown.
- CPUs slower than this one.

## Decision

The maintainer chose zeebo's main branch, pinned to commit `28a397d` (`v0.2.5-0.20261020032212-28a397d2ee4c`), for its NEON and SVE2 backends. The cost is assembly from 2026 with no tagged release. `conformance/vectors/blake3.json`, whose hashes come from `b3sum`, guards the code paths the machine running the tests can reach.

An empty file has no chunks (§16).

## Open

- Move to a tagged release when one holds these backends.
- Run the vectors on real arm64 and AVX-512 hardware, and measure throughput there.
