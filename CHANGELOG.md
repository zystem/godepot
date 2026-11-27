# Changelog

All notable changes. The form is that of [Keep a Changelog](https://keepachangelog.com), the versions are [Semantic Versions](https://semver.org). Before 1.0 every release is a pre-release and may change settings and tables. The section of a version is the text of its release ([docs/release.md](docs/release.md)).

## [Unreleased]

## [0.7.1] - 2026-11-28

The first release with files. The program is that of 0.7.0.

### Added
* An image (`ghcr.io/zystem/godepot`), binaries for linux/amd64 and linux/arm64, checksums, a bill of materials, signatures and build provenance, made by the release workflow ([docs/verifying-releases.md](docs/verifying-releases.md)).

## [0.7.0] - 2026-11-28

Parts of steps 7 and 8 of [docs/plan.md](docs/plan.md). No image and no signed files.

### Security
* `POST /api/v1/login` took the action `admin`, so an admin account's password could reach the admin API. It was in `main` only: no release had it.

### Added
* Cleanup policies, which report first and remove only when told to, and a trash from which what was removed can be restored ([docs/cleanup.md](docs/cleanup.md)).
* Cooldown and block lists of a proxy ([docs/policy.md](docs/policy.md)).
* Passwords of local accounts, `POST /api/v1/login`, basic authentication and `npm login` ([docs/api.md](docs/api.md)).
* Tokens for CI jobs by OIDC ([docs/ci-jobs.md](docs/ci-jobs.md)).
* Logins with LDAP and Active Directory, grants by group ([docs/ldap.md](docs/ldap.md)).

### Changed
* **The database moves from schema 5 to 8, with no way back.** Back up rqlite, and stop all replicas of the old version first.

## [0.6.0] - 2026-11-21

Steps 4 to 6 of the plan: the format `oci`, proxy and group repositories, and the formats `go`, `pypi`, `npm` and `maven`. The notes are with the release on GitHub.
