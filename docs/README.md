# The pages

Each page is of one kind: **tutorial** (one path that works), **how-to** (one task), **reference** (complete and dry), **explanation** (why), **record** (what was run or decided, with dates). The first try is in the [README](../README.md#try-it-a-few-minutes-docker-only).

| Page | Kind | About |
|---|---|---|
| [proxy-howto.md](proxy-howto.md) | how-to | a proxy of Docker Hub and a group |
| [oci-clients.md](oci-clients.md) | how-to | `helm`, `oras`, `crane`, `skopeo`, `cosign` |
| [ci-jobs.md](ci-jobs.md) | how-to | publish from CI with no stored secret |
| [ldap.md](ldap.md) | how-to | log in with LDAP or Active Directory |
| [configuration.md](configuration.md) | reference | settings, commands, exit codes |
| [settings-file.md](settings-file.md) | reference | repositories, grants, trusts, the directory |
| [api.md](api.md) | reference | the API and the audit log |
| [metrics.md](metrics.md) | reference | the metrics |
| [raw.md](raw.md), [oci.md](oci.md), [gomod.md](gomod.md), [pypi.md](pypi.md), [npm.md](npm.md), [maven.md](maven.md) | reference | the requests of each format |
| [proxy.md](proxy.md) | reference | how proxy and group repositories behave |
| [cleanup.md](cleanup.md) | reference | cleanup policies and the trash |
| [policy.md](policy.md) | reference | cooldown and block lists |
| [access.md](access.md) | explanation | accounts, tokens, grants |
| [blob-layer.md](blob-layer.md) | explanation | how bytes are kept, and what a crash leaves |
| [testing.md](testing.md) | how-to | run the tests and the real clients |
| [release.md](release.md) | how-to | cut a release |
| [verifying-releases.md](verifying-releases.md) | how-to | check what a release published |
| [status.md](status.md) | record | what is built, what was run, what is not |
| [security-requirements.md](security-requirements.md), [security-policies.md](security-policies.md), [supported-versions.md](supported-versions.md) | reference | what to expect in security, the project's rules, which release gets fixes |
| [threat-model.md](threat-model.md), [assurance-case.md](assurance-case.md) | explanation | the threats, and why the requirements are met |
| [spec.md](spec.md), [plan.md](plan.md) | record | what it will be, and in which steps |
| [step0-probes.md](step0-probes.md) | record | the first probes of blobgate |
