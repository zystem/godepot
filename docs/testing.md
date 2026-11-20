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

## The OCI door: the conformance suite and the clients

With the stand running and its variables set, `tools/test/oci-run.sh` builds the program, starts it on port 18190 with a repository of the format `oci`, makes an admin account and a token of its own, and runs the **conformance suite** of the OCI distribution specification (fetched and built once into `GODEPOT_OCI_CACHE`, default `$TMPDIR/godepot-oci-cache`, at the commit written in the script), then every client it finds (`crane`, `skopeo`, `helm`, `oras`, `cosign`, `docker`, `podman`). It prints one line for each: `ok`, `skipped` with the reason, or `FAILED`; the exit code is 1 when one that ran failed.

```bash
eval "$(tools/test/blobgate-stand.sh start)"
tools/test/oci-run.sh
```

```
conformance  ok       SUCCESS! -- 73 Passed | 0 Failed | 0 Pending | 6 Skipped
crane        ok       copy of alpine:3.20 in, the digest of the tag is the same as at the source
skopeo       ok       copy of alpine:3.19 in and out
helm         ok       a chart pushed and pulled, the file is the same
oras         ok       an artifact pushed, a note attached (found by the referrers API), pulled
cosign       ok       an image signed with a key and verified
docker       skipped  the daemon does not take 127.0.0.1:18190 on plain HTTP (add it to insecure-registries): Error response from daemon: ...
podman       skipped  not installed
```

(That is the output of 2026-11-20; the `docker` line is cut. The clients need the network: they copy `alpine` from a public registry.) `tools/ocibig` pushes and pulls a big layer through a running program and reads its memory ([oci.md](oci.md) has the figures): `go run ./tools/ocibig -url http://127.0.0.1:18100 -token $T -name reg/big -pid $(pgrep -f 'godepot serve') -mib 10240`.

## Proxy and group: false upstreams, and the program against real ones

`go test -p 1 ./internal/proxytest` (with the stand's variables) runs the proxy and group repositories of `raw` and `oci` against **false upstreams**, servers of the test that count what they are asked: one that is slow and streams, one that dies in the middle of a body, one that answers `429` with `Retry-After` or `500`, one that gives other bytes under a digest, one that answers `401` for a name, one that asks for a bearer token from a service on another host. The clock of the test is moved to cross a pause or a time to live. Each guard was **taken out on purpose** to see that its test fails (claims, joined fetches, the refusal of private addresses, the held-back last byte, the growing pause, the immutable paths, the negative cache). 100 clients that ask one cold layer make one fetch.

`tools/test/proxy-run.sh` runs the program against a real blobgate and rqlite (the stand) and prints one line for each of: a layer of `PROXY_BIG_MIB` MiB (default 5120) through a false registry that sends `PROXY_RATE` MiB a second (default 100); `PROXY_CLIENTS` clients (default 100) that ask a cold layer of `PROXY_COLD_MIB` MiB (default 512); `crane` through a proxy of **Docker Hub**, twice; a group; a raw proxy of `raw.githubusercontent.com`. `PROXY_SKIP_BIG=1` skips the first two. It uses names of repositories of its own for each run, because the stand keeps what an earlier run cached.

```bash
eval "$(tools/test/blobgate-stand.sh start)"
tools/test/proxy-run.sh
```

```
big      ok       cold pull: first byte after 11ms, the whole layer after 51.287s, digest equal: true; the false registry was asked for the layer 1 time(s); memory of godepot: peak 15708 kB ...
cold     ok       100 clients asked the cold layer: 100 got the whole layer with the right digest, 0 did not (35 answers of 503 with Retry-After were retried); the false registry was asked for the layer 1 time(s); all done after 23.089s; memory of godepot: peak 33180 kB
hub      ok       alpine:3.20 pulled through the proxy of Docker Hub (digest sha256:d9e853e8...); the second pull sent no request upstream (4 requests in all)
group    ok       crane copy to the group went to its write_to (mine); the same image and a public one are read through the group
raw      ok       a file of raw.githubusercontent.com asked twice: one request upstream
```

That is the output of 2026-11-20 (the lines are cut where they end in `...`). Read it as: a **5 GiB** layer sent at 100 MiB a second by the upstream reached the client's first byte after **11 ms** and its last after 51 s, with the digest equal, while godepot held **16 MiB**; asked again it came from the store in 11 s with no word to the upstream. **100 clients** asking one cold 512 MiB layer made **one** fetch; every one got the right bytes; 35 were told `503`, `Retry-After: 5` (the limit of 64 downloads at once, [configuration.md](configuration.md)) and did as a client does, and godepot peaked at 33 MiB. **Not run:** the same through two replicas, a slow or failing node of blobgate, `docker` and `podman`.

## The format go: the real go command

`tools/test/go-run.sh` starts the program with a hosted, a proxy (of `proxy.golang.org`) and a group repository of the format `go`, and runs the real `go` command against them: a module zipped and published through the group, built by `go run` from it; `github.com/google/uuid` through the proxy with the real checksum database, and again with an empty module cache; a name under a claimed prefix. It needs the stand and the network.

```
hosted   ok       a module zipped, published to the group (write_to), built by go run from the group: "hello from godepot"
proxy    ok       github.com/google/uuid through the proxy of proxy.golang.org with the real checksum database; again with an empty module cache: no request upstream (6 in all)
group    ok       corp.example.com/internal is not found, and the public proxy was not asked (claimed prefix)
```

(The output of 2026-11-20.) The `go` command refuses to send credentials over plain HTTP, so these repositories are read anonymously.

## The format pypi: twine and pip

`tools/test/pypi-run.sh` starts the program with a hosted, a proxy (of pypi.org) and a group repository of the format `pypi`. It installs `twine` into a temporary virtual environment, uploads a wheel through the group, installs it with `pip` from the group, downloads `six` through the proxy twice with no `pip` cache, and asks for a claimed name. It needs the stand and the network.

```
hosted   ok       twine uploaded a wheel to the group (write_to), pip installed it from the group: "hello from godepot"
proxy    ok       six==1.16.0 downloaded through the proxy of pypi.org (files.pythonhosted.org is the file host), the hash checked; again with no pip cache: no request upstream (4 in all)
group    ok       corp-internal is not found, and pypi.org was not asked (claimed prefix)
```

(The output of 2026-11-20, pip 24.0, Python 3.12.3.)

## What the variables do

| Variable | Meaning |
|---|---|
| `GODEPOT_RQLITE_URL` | The rqlite of the tests. Without it the tests that need a database skip. |
| `GODEPOT_BLOBGATE_URL`, `GODEPOT_BLOBGATE_TENANT`, `GODEPOT_BLOBGATE_SECRET`, `GODEPOT_BLOBGATE_STORE` | A real blobgate for the tests of the blob layer. Without them the layer is tested over a store in memory that links as blobgate does. With them the whole suite runs over blobgate, and each test first removes every key of the tenant. |
| `GODEPOT_RACE_N` | Rounds of the race tests (default 150). |
| `GODEPOT_BIG_MIB` | Runs `TestBigObject`: a stream of this many MiB through the layer. Skipped without it. |
| `GODEPOT_OCI_CACHE` | Where `tools/test/oci-run.sh` keeps the conformance suite it fetched and built (default `$TMPDIR/godepot-oci-cache`). |
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
