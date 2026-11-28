# Changelog

All notable changes. The form is that of [Keep a Changelog](https://keepachangelog.com), the versions are [Semantic Versions](https://semver.org). Before 1.0 every release is a pre-release and may change settings and tables. The section of a version is the text of its release ([docs/release.md](docs/release.md)).

## [Unreleased]

### Changed
* **`serve` and `validate` need `GODEPOT_SEAL_KEY_FILE`**: the file of the key that passwords in the database are sealed with, made at the first start. Keep it on a disk that lasts and keep a copy ([docs/secrets.md](docs/secrets.md)). The schema is 9.

### Added
* The passwords of upstreams and of the directory can be given with the API (`PUT /api/v1/credentials/...`) and are kept sealed in the database; `godepot reseal` moves them to another key, `godepot reset-seal-key` gets back to work when the key is lost ([docs/secrets.md](docs/secrets.md)).
* The passwords and tokens that `password_env` and `token_env` name (upstreams, the directory) may be in a file, `NAME_FILE`; a new one in the file is used with no restart ([docs/configuration.md](docs/configuration.md)).
* The project's documents: [CONTRIBUTING.md](CONTRIBUTING.md), [SECURITY.md](SECURITY.md), [GOVERNANCE.md](GOVERNANCE.md), the security requirements, threat model, assurance case and policies; dependency review and a sign-off check on pull requests.

### Fixed
* The release script builds both programs before it writes an archive: the arm64 program of 0.7.1 is stamped as built from a changed tree (the source was that of the tag), so it cannot be made again byte for byte ([docs/verifying-releases.md](docs/verifying-releases.md)).

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
