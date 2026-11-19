# Running the tests

How-to. For a contributor. Written 2026-11-17.

The tests that matter need a real rqlite, and some a real blobgate. Without them they skip, and say so.

```bash
BLOBGATE_IMAGE=ghcr.io/zystem/blobgate:0.6.1 tools/test/blobgate-stand.sh start
```

It starts two rqlites (for godepot on `127.0.0.1:24801`, for blobgate on `:24802`) and a blobgate on `:24803` with two directories for nodes and one store with `dedup: "tenant"`, and prints the `export` lines that the tests read. Take them into your shell:

```bash
eval "$(BLOBGATE_IMAGE=ghcr.io/zystem/blobgate:0.6.1 tools/test/blobgate-stand.sh start)"
go test -race -count=1 -p 1 ./...
tools/test/blobgate-stand.sh stop
```

`-p 1` is **required**: the packages share one database, and each test drops and makes the tables, so two packages at once break each other.

Instead of an image the stand can use the program: `BLOBGATE_BIN=/path/to/blobgate`. Only then can the stand kill and start it again (`blobgate-stand.sh kill-blobgate`, `start-blobgate`), and the probes of step 0 (`go run ./tools/probe big|slow|many|kill`, [step0-probes.md](step0-probes.md)) need it.

## What the variables do

| Variable | Meaning |
|---|---|
| `GODEPOT_RQLITE_URL` | The rqlite of the tests. Without it the tests that need a database skip. |
| `GODEPOT_BLOBGATE_URL`, `GODEPOT_BLOBGATE_TENANT`, `GODEPOT_BLOBGATE_SECRET`, `GODEPOT_BLOBGATE_STORE` | A real blobgate for the tests of the blob layer. Without them the layer is tested over a store in memory that links as blobgate does. With them the whole suite runs over blobgate, and each test first removes every key of the tenant. |
| `GODEPOT_RACE_N` | Rounds of the race tests (default 150). |
| `GODEPOT_BIG_MIB` | Runs `TestBigObject`: a stream of this many MiB through the layer. Skipped without it. |
| `GODEPOT_STAND_DIR` | Where the stand keeps its directories (default `$TMPDIR/godepot-bg-stand`). |
| `GODEPOT_STAND_NODES` | `1`: the store of the stand has one node, not two (for the probes that write a lot). |
| `GODEPOT_STAND_BLOBGATE_ENV` | More settings for the program of blobgate, separated by spaces (`BLOBGATE_THREADS=64`). |
| `GODEPOT_STAND_HOSTNET` | `1`: the two rqlites run on the host network. Through docker's port forwarding many connections were reset: [step0-probes.md](step0-probes.md). |
| `GODEPOT_STAND_SCRIPT` | The path of the stand script, for `tools/probe kill` (default `tools/test/blobgate-stand.sh`). |

## What was run

On 2026-11-17, on one workstation with the stand above (blobgate 0.6.1 as the image, two directory nodes, rqlite 10.x as the image `rqlite/rqlite`):

- the whole suite of the blob layer over the store in memory, and over blobgate: all passed;
- `TestBigObject` with 2048 MiB over blobgate: the stream was taken in, linked and made live in 13.6 seconds, read back whole in 3.6 seconds with the digest equal, and a range from the middle was right; the test process held 3 MiB of heap at the end. blobgate's resident memory was 35 MiB when looked at afterwards (not its peak). 10 GiB was **not** run (the disk had 45 GiB free).

Not run: any of this on a second machine, on a cluster of rqlite, or on a slow node.
