# Assurance case

Explanation. Why the requirements of [security-requirements.md](security-requirements.md) are believed to be met, with the evidence in the repository. Reviewed 2026-11-28 (0.7.1). It is the maintainer's argument, not an audit.

## 1. Threat model

[threat-model.md](threat-model.md): assets, actors, threats, answers, what is left, the attack surface.

## 2. Trust boundaries

1. **Client to godepot**: nothing from a client is trusted: every request is authenticated and authorized, names and paths are checked.
2. **godepot to upstreams, issuers, the directory**: their answers are data to check (hashes, signatures, claims, certificates, limits).
3. **godepot to rqlite and blobgate**: **trusted**. The operator must keep others out.
4. **Operator to godepot**: the settings file and the environment are trusted.
5. **Build to user**: one workflow builds and signs; the user verifies.

## 3. Secure design principles

| Principle | How it is applied |
|---|---|
| Fail-safe defaults | deny unless granted; anonymous read off; private addresses refused; a version, once written, is not overwritten unless allowed (an OCI tag may move unless `immutable_tags` names it); an unknown word in the settings or a body is an error |
| Least privilege | a token narrows its account; CI tokens live 15 minutes; a password gives data actions only; the image runs as a user that is not root, with a read-only root |
| Complete mediation | one check of account and scope for every format and call; directory accounts asked again while their tokens are used |
| Economy of mechanism | one static program; one database; credentials of every kind become the same token |
| Open design | the code and the documents are public; secrets are only keys and passwords |
| Separation of privilege | account **and** scope must both allow |
| Least common mechanism | a CI job exchanges its token rather than every door accepting foreign tokens |
| Psychological acceptability | clients use their own way in (`npm login`, basic authentication, a bearer token) |

## 4. Common weaknesses

| Weakness | Answer | Evidence |
|---|---|---|
| Injection into SQL | statements with arguments | `internal/meta` and its callers |
| Path traversal | names and paths checked; archives are not unpacked to disk | tests of each format |
| SSRF | address checked at dial time; redirects limited | `internal/upstream`, `internal/proxytest` |
| Secrets in logs | not logged; tests search the logs for them | `internal/server` tests |
| Weak or home-made cryptography | Go standard library only | `go.mod` |
| Broken access control | tests of refusals for each door; a found hole (a password giving admin) has a regression test | `internal/server` |
| Memory safety | Go | — |
| Known-vulnerable dependencies | govulncheck on every change | CI |

## 5. Evidence that is checked automatically

The tests with the race detector and 80 % or more statement coverage, `staticcheck`, `gosec`, `govulncheck`, the repeatable build: `.github/workflows/ci.yml`.

## 6. What is weak

No external audit; one maintainer and no second reviewer; no TLS of its own; one fuzz target.
