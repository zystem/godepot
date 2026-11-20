# The blob layer: what happens to bytes, and what a crash leaves

Explanation. Written 2026-11-16 with the code (`internal/blob`, `internal/meta`). It is the page to read before changing either package, and before writing a format: a format never writes to the store or to the tables of this layer; it calls `Ingest` and puts `Hold`, `Drop` and `Done` into its own transaction.

## The problem

The same bytes are kept once and are needed by many owners (assets of different repositories, uploads in progress, publications). Three things happen at the same time on several replicas: bytes come in, owners let go, and the collector removes what nobody needs. Any of them can die at any step. The bytes are in one system (blobgate) and the records in another (the database), and no transaction spans both.

Other registries get this wrong in known ways (plan.md, T1 to T3): space that is never freed, a collector that needs the registry stopped, a layer removed under a push.

## What the database gives

`internal/meta` gives one thing: `Exec(statements...)`, a list applied **together or not at all**. A caller cannot read inside it. A condition is a **guard**: a statement of the list that fails when the condition does not hold, which undoes the list (`meta.Guard`, `meta.Unless`; an `INSERT` of 0 into the table `guard`, whose `CHECK` wants 1). The error is a `*meta.Refused` that carries the word of the guard. A failure of the database itself is a `*meta.Unavailable`: the caller does not know whether the list was applied, so **every list of this layer changes nothing when sent a second time**.

Checked on rqlite (SQLite 3.53.4) on 2026-11-16: a failed guard undoes the statements before it and stops those after it (`TestAListIsAllOrNothing`).

## The tables

| Table | A row says |
|---|---|
| `blobs` | the digest has `size` bytes under `key` in the store; `state` is `incoming` (the row is there, the bytes may not be) or `live`; `unref_since` is the time since when no reference names it, or NULL |
| `refs` | `owner` needs `digest` |
| `uploads` | bytes are being written to `key` (`uploads/<id>`), or were, since `started` |
| `graves` | `key` of the store is to be deleted |

## The rules

1. **The row is the commit.** A blob exists when its row is `live`. A key of the store that no row and no grave names is a stray.
2. **`unref_since` is NULL exactly while a reference names the blob.** Every list that adds or removes a reference keeps this true in the same list.
3. **A reference is added only to a live blob, with the check in the same list** (`Hold`). The exception is the upload that is bringing the bytes: it holds the row while the row is `incoming`.
4. **A row is removed only when nothing has named it for the grace time, with the check in the same list**, which also writes the grave of its key (`Collect`).
5. **A key has one life.** The key of a blob ends in a random part that is new each time the digest comes back after being collected; the key of an upload is its random id. So a delete that comes late, or twice, cannot remove anything that is needed. This is what lets the collector run with no lock and on two replicas at once.
6. **Whatever is to be deleted from the store is written down first** (a grave), in the transaction that makes it unneeded.

## Taking bytes in (`Ingest`)

| Step | What is done | A process that dies after it leaves | Who cleans it |
|---|---|---|---|
| 1 `ingest:row` | the row of `uploads`, before the first byte | a row with no bytes | the sweep, after `UploadTTL` |
| 2 `ingest:put` | the bytes are written to `uploads/<id>` while their SHA-256 is made; other bytes than were named are refused | a row and its bytes | the sweep |
| 3 `ingest:claim` | one list: the upload must still be there; a row of `blobs` (`incoming`, with a new key) unless the digest has one; the size must agree; the reference of the upload; `unref_since` NULL | as above, and perhaps an `incoming` row held by the upload | the sweep lets go; the collector removes the row after the grace time |
| 4 `ingest:link` | only if the row is `incoming`: the store makes the blob's key from the bytes of the upload, sending nothing (blobgate `?link`) | as above, and the blob's key in the store | as above; the grave of the key is dug |
| 5 `ingest:live` | the row becomes `live` | a live blob held by a dead upload | the sweep lets go; then the grace time |
| 6 `ingest:unstaged` | the key of the upload is deleted (grave, delete, grave removed); the row of the upload stays and holds the blob | the same | the same |

The caller then commits what the bytes are for in **one list of its own**: its rows, `Hold(owner, digest)`, `Done(upload)`. Until then the upload holds the blob; if the caller never commits, the sweep ends the upload.

Two uploads of the same new bytes at once: both pass step 3 (one makes the row, both hold it), both find it `incoming`, both link (the second link is the same as the first), both make it live.

An upload that finds the row `incoming` always links its own bytes, because the upload that made the row may have died before step 4.

An upload that is slower than `UploadTTL` is swept while its bytes come: step 3 is then refused (`upload_gone`), the upload deletes its own key and answers `ErrExpired`.

## Naming a blob without sending it (`Hold`)

A client that has learned that a blob exists (an OCI `HEAD`, a mount from another repository) commits a manifest that names it. The list holds a guard per blob: the row must be `live`. If the collector removed the blob in between, the whole list is refused with `blob_not_live`, nothing is written, and the client is told to send the bytes. The grace time makes this rare; the guard makes it safe.

## Letting go (`Drop`)

One list: every blob that only this owner names gets `unref_since = now`; the references of the owner are removed. In a list that also holds, `Hold` comes first, so that a blob passed from an upload to an asset never becomes unnamed.

## Removing (`Collect`)

One pass, in this order: dig the graves; sweep the uploads older than `UploadTTL`; then, for every blob unnamed for longer than `Grace`, one list that writes the grave of its key and deletes the row, **if it is still the same key, still unnamed since the same moment, and no reference names it**. Then the key is deleted and the grave removed.

| Step | A process that dies after it leaves | Who cleans it |
|---|---|---|
| `collect:grave` (the row is gone, the grave is written) | bytes in the store with a grave | the next pass |
| `dig:deleted` (the bytes are gone) | a grave of a key that is not there | the next pass (deleting what is not there is no error) |

From the moment the row is gone the digest can come back: a push finds no row, makes a new one with a new key, and the delete of the old key, whenever it comes, does not touch it.

## Looking from outside (`Reconcile`)

Lists the store and compares, in 256 parts by the first byte of the digest. A live blob whose key is not in the store, or has another size, is **missing**: data is lost and only a restore of the store brings it back. A key under `blobs/` or `uploads/` that no row and no grave names, and that is older than the grace time, is a **stray** and is removed (never with `dry`). Strays come from a restore of an older database over a newer store, which is therefore the safe direction (spec O6).

## What the calls return

| Return | When |
|---|---|
| `ErrDigest` | the bytes of an upload are not the bytes the sender named, or the named digest is not a SHA-256 in lower-case hex; nothing is kept |
| `ErrExpired` | the sweep took the upload while its bytes were coming (guard `upload_gone`): it took longer than the upload time |
| `ErrNotFound` | no live blob has the digest (`Stat`, `Open`), or a key is not in the store |
| `ErrNotLinked` | the store wrote the bytes and then said it does not keep them: it is not set to link (blobgate: `dedup: "tenant"`) |
| a `*meta.Refused` with `blob_not_live` | `Hold`: a blob of the list is not there; nothing of the list was written |
| a `*meta.Unavailable` | the database failed; every list of this layer is safe to send again |

## The times

| Setting of the layer | Default | It must be |
|---|---|---|
| `Grace` | 24 hours | longer than the longest time between a client learning that a blob exists and committing what names it; and longer than a read lasts, since a read of an unnamed blob is not protected |
| `UploadTTL` | 24 hours | longer than the slowest upload, from its first byte to the commit of what it is for |

## What was run

On 2026-11-16 and 2026-11-17, on one workstation, `go test -race` with `GODEPOT_RQLITE_URL` at one rqlite (the image `rqlite/rqlite`, SQLite 3.53.4), first over a store in memory that links as blobgate's `dedup: "tenant"` does, and from 2026-11-17 the same suite again over a real blobgate 0.6.1 (the image, two directory nodes, [testing.md](testing.md)); every test passed on both:

- a death at each of the 7 points of `Ingest`, with and without a second try, and at each of the 2 points of `Collect`; after each, the rules hold at once, and after the times run out the database and the store are empty (`TestCrashAtEveryStepOfIngest`, `TestCrashAtEveryStepOfCollect`);
- a collector stalled between removing a row and deleting its bytes while a push brings the digest back (`TestALateDeleteDoesNotRemoveABlobThatCameBack`);
- a push against the collector, and a hold against the collector, 3000 rounds each; 12 pushes of the same bytes at once, 200 rounds (`TestRaceOfPushAndCollect`, `TestRaceOfHoldAndCollect`, `TestManyPushesOfTheSameBytesAtOnce`). In the hold race the hold won 2484 times and was refused 516 times, and the test fails if either side never wins. The default of a plain `go test` is 150 rounds (`GODEPOT_RACE_N` sets it).
- the answer of the database lost after each of the 6 lists of a push (the list applied, the caller told it failed), then the push tried again; a commit sent three times (`TestAnswerLostAtEveryList`, `TestCommitSentTwice`);
- a 2048 MiB stream through the whole path over blobgate (`TestBigObject`, [testing.md](testing.md));
- three faults put into the code on purpose, each of which made a test fail: a key that is the same for every life; `Hold` without its guard; a collector that does not check that the blob is still unnamed.

Statement coverage of that run: `internal/blob` 81.3 %, `internal/meta` 68.5 %.

Not run, not built:

- **A 10 GiB object.** 2 GiB was run; the disk had room for no more.
- **Many slow readers of ranges against blobgate** (plan T39), and a million small objects in a listing.
- **blobgate killed (`kill -9`) between an upload and its link.** The crash tests stop godepot at each step, not blobgate.
- **A change of leader of rqlite**, more than one node of it, or a second replica of godepot as a separate process (the races are goroutines of one process).
- **An upload in several requests** (an OCI upload in chunks). `Ingest` reads one stream and the store interface has no parts, so the format `oci` takes each chunk in as a blob held by the upload (`ociupload:<id>`) and, at the end, reads them in order into one `Ingest` that checks the digest of the whole ([oci.md](oci.md)). It costs one more read and write of the layer; a client that sends the layer in one `PATCH` does not pay it. Streaming parts to blobgate's multipart upload would avoid it, but an object made by a multipart upload has no digest that `?link` can find (blobgate's deduplication page), so the layer would have to be sent again anyway.
- **A sign of life of a long upload.** An upload slower than `UploadTTL` is swept.
- **The ceiling of rqlite** (plan, T38): nothing was measured. A pass of `Collect` makes three calls per blob.
- The plan asks for 10 000 rounds of the race; 3000 were run.
