# Contributing

- **Bugs**: open an [issue](https://github.com/zystem/godepot/issues). Say the version (`godepot version`), what you did, what you expected and what happened; the line of the audit log helps. Cut tokens and passwords out of what you paste.
- **Ideas and questions**: an issue too.
- **Security problems**: **not** in an issue; see [SECURITY.md](SECURITY.md).
- **Changes**: a pull request against `main`. What it takes to be accepted is below.

## Build and test

You need Go as `go.mod` says, and Docker for what the tests start (a real rqlite and a real blobgate).

```bash
go build ./cmd/godepot
```

It prints nothing and leaves the program `godepot` in the directory. How to run the tests, which share one database and so run one package at a time: [docs/testing.md](docs/testing.md).

## What a change needs

- **Coding standard**: [Effective Go](https://go.dev/doc/effective_go) and the [Go Code Review Comments](https://go.dev/wiki/CodeReviewComments), formatted with `gofmt`. CI enforces it: it fails on unformatted code, on `go vet`, on `staticcheck` with all its checks and on `gosec` findings. Comments say why, not what.
- **Tests (policy)**: **every change of behaviour adds or updates a test in the automatic suite, and new major functionality comes with its tests in the same pull request.** A fixed bug gets a test that fails without the fix. A parser of input from outside gets a fuzz test. Statement coverage stays at 80 % or more (CI fails below).
- **Documents**: a new setting, flag, command, metric or message gets its line in the documents in the same change (tests compare them). A command is shown with what it printed; what was not run is said.
- **Dependencies**: a new one needs a reason in the pull request; the rules are in [docs/security-policies.md](docs/security-policies.md).
- **Cryptography**: no primitive of our own; the Go standard library.
- **Commits**: a short subject that says what changed and why. A change a user can see gets a line in [CHANGELOG.md](CHANGELOG.md).
- **Sign-off (DCO)**: every commit carries `Signed-off-by: Name <email>` (`git commit -s`): you certify that you may submit the work under the project's license, as [DCO.md](DCO.md) says. The `DCO` workflow checks each commit of a pull request (bots are exempt).
- **License**: your contribution is under the [MIT license](LICENSE) of the project.

## When the tests run

- **By you, before a pull request**: `gofmt -l .`, `go vet ./...` and the tests ([docs/testing.md](docs/testing.md)).
- **Automatically**, on every pull request and push to `main` (`.github/workflows/ci.yml`): `gofmt`, `go vet`, the tests with the race detector against a real rqlite and blobgate, the coverage floor, `govulncheck`, `staticcheck`, `gosec`, a repeatable build and the image build; on pull requests also dependency review and the sign-off check. A release runs `ci.yml` again before it publishes.
- The scripts that run the real clients (`tools/test/*-run.sh`) are run by hand, not by CI.

## Conduct and governance

[CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md), [GOVERNANCE.md](GOVERNANCE.md). What comes next: [docs/plan.md](docs/plan.md).
