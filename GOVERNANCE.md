# Governance

godepot is a small project with **one maintainer**. This page says how it is run, so that a contributor knows what to expect and a user knows what the controls are.

## Members and roles

| Member | Role and duties | Access to sensitive resources |
|---|---|---|
| `zystem` (owner of the repository) | maintainer: decides, reviews and merges, makes releases, answers security reports, keeps the repository's settings, the package and the badge entry | admin of the repository and its settings, the container package |
| contributors | send issues and pull requests | none |

This is the complete list of those with access to sensitive resources; it is changed here when access is given or removed.

## How it is run

- **Decisions** are made by the maintainer, in the open: in issues and pull requests, with the reasons in commit messages or in `docs/`.
- **Changes** come as pull requests ([CONTRIBUTING.md](CONTRIBUTING.md)) and are merged when the automatic checks pass.
- **No second person approves a change.** With one active author a required approval would stop every merge.
- **Giving access** (write access, publishing releases or packages, the settings): only to a person who has had changes accepted over time, whose identity the owner knows, who uses two-factor authentication, and whom the owner has reviewed before granting it. The list is reviewed when a release is made.
- **Releases** are made by the release workflow from a tag ([docs/release.md](docs/release.md)). The project holds no signing key: signatures are bound to the workflow.

## Continuity

**There is no second maintainer, so the project cannot promise to go on without interruption if the owner cannot.** What lets someone else carry it on: the MIT license, everything needed to build and release in the repository, and no secret of the project that could be lost.
