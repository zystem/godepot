# Security policy

godepot keeps what others build and install, so a report of a weakness is welcome and is read first.

## Reporting

- Use GitHub's private vulnerability reporting of this repository: [Security, Report a vulnerability](https://github.com/zystem/godepot/security/advisories/new). This is the security contact.
- Do not put details of an unfixed problem into a public issue. If there is no answer within 7 days, open a public issue that holds **no details** ("security contact request").
- Say what you saw, on which version or commit, and how to see it again; a failing test is the best report.

## Coordinated disclosure, and the timeframe

- **Acknowledgement** within 7 days; an assessment (confirmed, not reproducible, not a vulnerability) within 14 days.
- **Fix**: a confirmed problem is fixed with a test. For High and Critical severity the target is a release within 30 days, for the rest the next release.
- **Disclosure**: after the fix is released, or 90 days after the report if no fix is possible, whichever is first (earlier by agreement).
- **Where found vulnerabilities are published**: in [CHANGELOG.md](CHANGELOG.md), under "Security" of the release that fixes them, and as a [GitHub security advisory](https://github.com/zystem/godepot/security/advisories) for a problem that a release had. Reporters are credited if they wish.

## What is supported

- **Scope**: the latest release and the head of `main`. godepot is before 1.0: there is no long-term line.
- **Duration**: a release is supported until the next one is made.
- **End**: from then on it gets no security fixes; update. Which release is supported: [docs/supported-versions.md](docs/supported-versions.md).

## More

What you can and cannot expect: [docs/security-requirements.md](docs/security-requirements.md). The threats: [docs/threat-model.md](docs/threat-model.md). Why we think the requirements are met: [docs/assurance-case.md](docs/assurance-case.md). The project's own rules for dependencies, analysis and secrets: [docs/security-policies.md](docs/security-policies.md). Checking a release: [docs/verifying-releases.md](docs/verifying-releases.md).
