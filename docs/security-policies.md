# The project's security policies

Reference. The rules the project holds itself to; the automatic parts are in [`.github/workflows/`](../.github/workflows/).

## Dependencies: how they are chosen, obtained and tracked

- **Chosen**: the Go standard library first. A new module needs a reason in its pull request, must be maintained, and be under a permissive license (below).
- **Obtained**: Go modules, with their hashes in `go.sum` checked against the Go checksum database; GitHub actions pinned by commit; the build and runtime images by tag.
- **Tracked**: [`go.mod`](../go.mod) lists them; Dependabot opens updates weekly; every release has a software bill of materials.

## Findings in dependencies and licenses

Tools: `govulncheck` (on every change, and again for a release), dependency review (on every pull request).

| Severity of a vulnerability | A fix exists | No fix exists |
|---|---|---|
| Critical, High | fixed before the next release, at once if the vulnerable code is called | recorded with the reason it cannot be exploited, or mitigated |
| Medium | within 30 days | recorded |
| Low | at the next routine update | none |

**Licenses**: MIT, Apache-2.0, BSD-2-Clause, BSD-3-Clause, ISC, MPL-2.0, 0BSD, Unlicense and CC0-1.0 are accepted; dependency review refuses others, copyleft in particular.

**Malicious dependencies**: dependency review and the checksum database; a module that is reported as malicious is removed at once.

**Before a release**: CI on the tagged commit must pass, `govulncheck` included; no release is made with a Critical or High finding that has a fix.

**The gate**: dependency review fails a pull request on a vulnerability of High or Critical severity or on a license not listed above; `govulncheck` fails CI.

## Vulnerabilities that do not affect godepot (VEX)

A finding of a scanner in a component godepot ships is fixed, or is recorded in an OpenVEX document in `vex/` with the status and why the vulnerable code cannot be reached. There is none today: on 2026-11-28 `govulncheck` reported no vulnerability for the modules (the images were not scanned).

## Findings of static analysis

`staticcheck` (all checks), `gosec` (without G104, unchecked errors) and `go vet`. A finding of any severity is fixed before the change is merged, or suppressed in the code with its reason (`// #nosec Gxxx -- reason`). CI fails on a finding.

## Secrets and credentials

- No secret is committed; GitHub secret scanning and push protection are on.
- godepot takes its own secrets, and the upstream and directory passwords and tokens that the settings name, from the environment or from a file (`NAME_FILE`). A password or token in a file can be changed with no restart ([configuration.md](configuration.md)). They are not logged.
- CI has no long-lived secret: a release uses the token of the run and the identity of the workflow. A new secret in CI needs a reason in the pull request and is limited to the job that needs it.
- A secret is replaced when someone who knew it leaves, when it may have been seen, and at least once a year. One that leaked is replaced first, and the audit log is then read for its use.
