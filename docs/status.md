# Status (2026-11-19)

Record. What is built, what was run, what is not. Dates are absolute; "not run" means it was not run.

## Built, and run

- **The database package** (`internal/meta`): a list of statements applied together or not at all, with guards; the rqlite client; the schema as numbered steps (3 so far); a program refuses a newer schema.
- **The blob layer** (`internal/blob`): taking bytes in, holding and letting go, the collector, `reconcile`. Run with a death at each step, with the answer of the database lost at each list, and with races, over a store in memory and over a real blobgate 0.6.1: [blob-layer.md](blob-layer.md), [testing.md](testing.md).
- **The store over blobgate** (`internal/blobgate`), with `Check`.
- **The lease** (`internal/lease`): one holder at a time; 12 replicas asking at once for 40 rounds, one wins each time.
- **The program** (`cmd/godepot`): `serve`, `validate`, `migrate`, `check`, `reconcile`, `version`; the settings with every problem named at once; `/healthz`, `/readyz`, `/metrics`; the image. Two replicas run by hand on 2026-11-17: one led, and the other led 3 seconds after the first was stopped with `SIGTERM`. `serve` with a port that is taken was found to hang, was fixed, and has a test.
- **Step 3, run 2026-11-19: the format `raw`, accounts, tokens, the settings file, the audit log.** `internal/auth` (accounts, tokens kept as a digest, scope patterns with a fuzz target), `internal/settings` (the file, every problem at once, grants), `internal/audit`, `internal/raw`, `internal/server` (the door and the API), the commands `new-account` and `new-token`, `SIGHUP` reads the file again. Tested against a real rqlite and (`cmd/godepot/raw_test.go`) a real blobgate with a 20 MiB file written and read back by range. Races: 12 writers of one path, in a repository that is immutable (one wins) and in one that overwrites (none leaves an unnamed reference). The secret of a token was searched for in the log and the audit rows after being sent four ways: it is in none (the test was checked to fail when a secret is put in a message). What is in the specification and **not** built for step 3: setting a property after the upload and searching by property across repositories (R6: at upload, and in a listing of a directory, only); promotion (R5); TLS.
- **The README try-it**, run as written on 2026-11-19 in containers, with blobgate 0.6.1 (this release's `raw` example). Not run by a test (blobgate's README is; this one is by hand): a test that runs it is still to do.
- **Documents kept true by a test**: every setting, metric, command, flag and exit code has its line (`cmd/godepot/docs_test.go`; checked to fail when a line is removed).

Statement coverage over `./cmd/godepot` and `./internal/...` with the stand: 87.8 % on 2026-11-17 (`go test -p 1 -coverpkg=./cmd/godepot,./internal/...`).

The checks that CI will run, run by hand on 2026-11-17: `gofmt`, `go vet`, staticcheck v0.8.2 with all checks (no findings), gosec v2.29.0 (one finding, an intended read of the file a setting names, annotated), govulncheck v1.9.0 (no vulnerabilities), the build repeated twice for amd64 and for arm64 (the same bytes each time), the image built and run read-only with `--cap-drop ALL`.

## Step 0, run 2026-11-17

A 10 GiB object, 50 to 500 slow readers, 200 000 small objects and `kill -9` of blobgate at five points, against a real blobgate: [step0-probes.md](step0-probes.md). Findings: blobgate's memory is about 10 MiB for each slow download; deleting frees the disk one pass of blobgate later; everything survived the kills; the adapter now tries a call again on 502, 503, 504 and on connection failures.

## Not built

- **Every format but `raw`**, the proxy, groups, policies, OIDC and LDAP, promotion, the web pages: steps 4 to 12 of the plan. Accounts have no passwords (tokens only) until step 8.
- Chunked and resumed uploads in the layer (it reads one stream).
- TLS on the port of the program.
- CI has run on GitHub (pull request 3, 2026-11-18: `test`, `analysis`, `reproducible` and `image` passed). The release workflow, the chart, signed releases, the supply-chain gates (Scorecard, dependency review, fuzzing), the security documents, `CONTRIBUTING`, the changelog and the OpenSSF badge are not there.

## Not run

- Anything on a second machine, on a cluster of rqlite, on a slow or failing node of blobgate, or with blobgate killed.
- The ceiling of rqlite (plan T38). A million small objects, a mirror of two nodes, nodes of other kinds: [step0-probes.md](step0-probes.md).
- The race tests at the 10 000 rounds the plan asks for (3000 were run on 2026-11-16).
