# Which file gets the room first

This report backs the order of files in the placement plan (`specs.md` §13.3, §13.4). It is research, not a contract: `conformance/vectors/plan.json` is the contract.

## The question

When a pool cannot hold a replica of everything, the order in which files are placed decides who is protected. `specs.md` §13.3 says smaller volumes first. Measured, against the two alternatives.

## Method

A throwaway simulation, with 5 synthetic libraries per row, each a mix by count:

- 12% movies: 2 to 100 GB, lognormal around 12 GB;
- 38% episodes: 0.3 to 6 GB;
- 20% photos: 2 to 30 MB;
- 30% subtitles, NFO files and artwork: 2 to 800 KB.

The pool has 4 + 6 + 8 + 12 TB. Each file already sits on one disk, placed in proportion to the room there, and wants x2. `min_free` is the smaller of 5% and 100 GiB. Placement is the greedy rule of `specs.md` §12.4: the disk with the most room after `min_free`, holding no copy of the file. There are no domains, tags or rule priorities.

## Results

| Pool filled to | Order | Files protected | Bytes protected | Room left unused |
|---|---|---|---|---|
| 45% | any | 100% | 100% | 2.5 TB |
| 60% | smallest first | 98.0% | 63.8% | 55 GB |
| 60% | library order | 67.7% | 64.1% | 0 |
| 60% | largest first | 6.5% | 64.1% | 0 |
| 75% | smallest first | 92.4% | 31.2% | 16 GB |
| 75% | library order | 34.5% | 31.3% | 0 |
| 75% | largest first | 1.8% | 31.3% | 0 |
| 90% | smallest first | 74.6% | 9.5% | 3 GB |
| 90% | library order | 12.4% | 9.5% | 0 |
| 90% | largest first | 0.4% | 9.5% | 0 |

## Reading

- Bytes protected barely depend on the order: the free room is the limit, and every order fills it. The difference is at most 0.3 of a percentage point.
- Files protected depend on it enormously. Smallest first protects 15 to 190 times more files than largest first.
- A file protected is a file saved whatever its size, and a single movie takes the room of thousands of small files.
- The cost is that the largest files are the ones left unprotected when room is short. `priority` on a rule puts a file first whatever its size, which is how to protect a library's large movies.

## Not measured

Real libraries, and the effect of domains and tags on these figures.
