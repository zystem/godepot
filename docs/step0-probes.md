# Step 0: what was asked of blobgate, and what it answered

Record. Run on 2026-11-17 on one workstation (32 CPUs, 31 GiB), with the program `tools/probe` ([../tools/probe/main.go](../tools/probe/main.go)) against the stand of [../tools/test/blobgate-stand.sh](../tools/test/blobgate-stand.sh): the **program** of blobgate built from the working tree at `v0.6.1-3-ge5c2562` (not the image: the probes read its memory and kill it), one store of **one node** (a directory on the local disk, through rclone), `dedup: "tenant"`, blobgate's own settings left at their defaults (`BLOBGATE_THREADS` 4), and one rqlite 10.5.1 for blobgate. godepot's blob layer and its adapter were on the other side, as in the real use. Each figure is one run, not a median; the workstation was doing other work.

## Questions and answers

| Question (plan, step 0 and T39) | Answer |
|---|---|
| Can a 10 GiB object be taken in as a stream of unknown length, made a blob by a link, and read back? | Yes. Ingest (write of the stream, link, delete of the staged key) took 49.4 s = 207 MiB/s; the whole object read back in 22.1 s = 463 MiB/s with the digest equal; 20 reads of 8 MiB at random offsets: p50 37 ms, max 52 ms. |
| What does that cost blobgate in memory? | Its peak resident memory was **38 MiB** during the write and **52 MiB** during the read of the whole object. |
| Is the link a record, not a copy? | The same 10 GiB sent again took 54.6 s (the bytes are sent again and are written before they are found to be a copy) and the free space on the disk changed by 0.15 GiB: the copy was dropped at once. |
| Does deleting free the disk? | **Not at once.** After the collector had deleted the key (28 ms), the free space was unchanged; about 70 seconds later it was back (blobgate frees blocks in its own pass, `BLOBGATE_REPAIR_SECONDS`, default 60). Trap T1 is therefore avoided in godepot's layer and still true one level down for a minute. |
| Do many slow readers starve the others (T39)? | **No.** With 50, 200 and 500 readers each taking 1 MiB/s of a 64 MiB object for 40 s, every reader got its first byte (p50 92 ms / 409 ms / 723 ms; the slowest 150 ms / 690 ms / 1.7 s), and a fast read of a small blob meanwhile never failed and never took a second (p50 5 / 5 / 9 ms, max 29 / 636 / 994 ms). |
| Then what do they cost (T39)? | **Memory, about 10 MiB per reader that is going on.** Peak resident memory of blobgate: **562 MiB** with 50 readers, **2050 MiB** with 200, **5084 MiB** with 500, and it was not given back when the readers ended (the "now" figure equalled the peak). `BLOBGATE_THREADS` did not bound this (default 4, not changed here): it bounds the reads of a block at once, and a block that waits for its slow client is held. |
| A listing of many objects? | 200 000 objects of 100 bytes (not the million of the plan): a listing of all took 7.7 s (26 000 keys/s); a listing of one 1/256 shard (781 keys), as `Reconcile` does, p50 19 ms, max 23 ms; so a full `Reconcile` is about 5 s of listing at this size. 500 reads of random objects: p50 0.45 ms, p99 1.1 ms. |
| How fast can many small objects be written? | 375 objects/s on average with 16 writers (8 min 54 s for 200 000); the rate fell from about 530/s to about 235/s. **Cause found afterwards** (section "Why the writes of small objects slowed down"): the disk of blobgate's rqlite, not the number of objects. On a RAM disk, 757/s. blobgate's peak memory 235 MiB. Deleting all of them: 2000/s. |
| Is `kill -9` of blobgate between an upload and its link safe? | Yes, at each of the five points tried (during the write of the bytes; after the write; after the claim; after the link; after the row is live): the first try ended with an error, blobgate was started again, a second try succeeded and read back equal, and after the times ran out (grace and upload time 2 s) and a collect and a reconcile, **nothing was left** in the store or in the database. |

## One thing that was not blobgate's fault

The first runs of the many-objects probe failed after about 12 500 writes: blobgate answered `503 database_unavailable` with "connection reset by peer" from its rqlite, in bursts (13 answers at one moment, then 131 in all), and retrying six times did not get through. The rqlite was reached through **docker's port forwarding** (`docker-proxy`). With the same rqlite on the host network (`GODEPOT_STAND_HOSTNET=1`) the probe wrote all 200 000 with **no 503**. So the resets came from the stand, not from blobgate or rqlite; the cause inside the forwarding was not looked for. The adapter nevertheless tries a call again when blobgate says 502, 503 or 504 or the connection fails (and a write only when its source can be read again), with tests against a fake blobgate (`internal/blobgate/retry_test.go`), because a real network can reset a connection as well.

## Why the writes of small objects slowed down

On 2026-11-18 the same probe was run again, 150 000 objects of 100 bytes, 16 writers, one node, rqlite on the host network, the rate said every 10 s (`probe many -every 10s`, new flags `-seq` and `-unique`). One change at a time; each run once, on a machine that was doing other work (load average 8 to 11 of 32 cores).

| Run | What was different | Average | What the rate did |
|---|---|---|---|
| A, 100 000 | as in the first probe: random keys, all bodies equal | 340/s | about 500, then 260 for about 140 s, then 500 |
| C, 100 000 | keys in order | 368/s | 600, then 260 to the end |
| D, 90 000 | keys in order | 526/s | 580, one dip of 20 s |
| E1, E2, 150 000 | keys in order; pressure on the disk and CPU recorded | 341/s, 352/s | 600, dips of 260 that start at different times; the kernel's pressure figures for the disk showed **no** stall |
| G, 150 000 | keys in order, bodies equal | 341/s | the same, the queue of removals (`storage_repair`) never above 11 rows |
| F, 150 000 | keys in order, **a body of its own** for each object | 485/s | 700, dips to 330 to 600 |
| H, 150 000 | as F, the directory of the node of blobgate **on a RAM disk** | 471/s | the same dips |
| I, 150 000 | as F, the directory of the node **and the data of rqlite** on a RAM disk | **757/s** | **no dips**; a slow fall from 890 to 680 |

What this says:

- **The dips are the disk of rqlite** (its log and its write-ahead file, which grows to 400 MB between two snapshots and is cut back every 60 s). Moving only blobgate's node to a RAM disk (H) changed nothing; moving rqlite as well (I) removed the dips.
- **It is not the order of keys, the number of objects (D had 90 000 and one dip), the queue of removals, or a noisy disk.**
- **A small fall is left even on a RAM disk** (890 to 680 in 3 minutes, 150 000 objects): the cost of an insert grows with the table. It is a quarter, not a half.
- Equal bodies make it worse (G against F): every copy is written and its removal is queued, which godepot, with bodies of its own, does not cause. The first probe used equal bodies, which was a fault of the probe, not of blobgate.
- blobgate uses much CPU for a small write: 5 to 10 cores of 32 at 260 to 600 writes/s (about 10 ms of CPU for one 100-byte object), measured with `top`, not profiled (`perf` was not allowed on this machine).

What it means for godepot: the speed of small writes is set by the disk under blobgate's rqlite (a fast disk, not a network disk; spec S5 keeps small documents as rows in godepot's own rqlite anyway). The planned sizing page says so. **Not run:** the dips with rqlite on a disk of a kind other than this machine's NVMe, and a profile of blobgate's CPU.

## What it changes in the plan

- **T39 is about memory, not about waiting.** The memory of a replica of godepot is not the issue (it streams); the memory of **blobgate** is: about 10 MiB for each download that is going on, kept. So: godepot limits the downloads of blobs going on at once (a setting) and ends a read that is slower than a minimum rate; the sizing page says "10 MiB of blobgate for each open download"; blobgate's roadmap item 2 (stream a block with the check at its end) would remove the cost and is the better fix. The presigned redirect (spec S7) does **not** help: the S3 door of blobgate reads the same way.
- **The deletion of a blob frees the disk one pass of blobgate later**: the page on capacity says so.
- **Writes of many small objects come in two speeds** (about 600/s and about 260/s, switching): the slow speed comes from the disk of the rqlite behind blobgate, so the rqlite of blobgate needs a fast disk and the capacity page says so. Documents under 256 KiB stay rows in the database (spec S5), as planned.

## What was not run

- **A million small objects** (200 000 were). The fall of the write rate means the million would be slower than a straight extrapolation says.
- **Nodes other than a local directory** (S3, SFTP, WebDAV, FTP), a mirror of two nodes (the 10 GiB probe used one node, because the disk had 54 GiB free), and a node that is slow.
- **A kill of rqlite, or of godepot and blobgate together**, and a change of leader of rqlite.
- **The reads of the slow readers against an S3 node** (the 10 MiB per reader is for a local node).
- **Repeats**: each probe was run once.
