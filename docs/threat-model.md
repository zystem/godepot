# Threat model and security assessment

Explanation. The assets, who may attack, what answers it and what is left. Made by the maintainer from the code and the specification on 2026-11-28 (0.7.1); not an external audit.

**Assets**: the integrity of what is served (packages and images that others install and run); the credentials (tokens, passwords, upstream and directory secrets); private packages; the audit log.

**Actors**: a person or program with a token or password; an anonymous client; the operator (settings, environment); the upstreams; the directory and the CI issuers that are trusted; rqlite and blobgate behind godepot.

## Threats, most likely and most harmful first

| Attacker | Could | What answers it | What is left |
|---|---|---|---|
| Public package with the name of a private one (dependency confusion) | get its code installed | a name that a hosted repository has published, or that is claimed by prefix, is never asked of an upstream in the groups that hold it | a private name that is neither published nor claimed yet |
| Hostile or taken-over upstream | serve bad bytes; change a released file | hash checks where the upstream names one; a changed cached file is not replaced and is audited; cooldown and block lists | what the upstream publishes is trusted; no scan |
| Thief of a token | everything the token's scope and account allow, until it ends | narrow, short tokens; revoke; disable the account; audit | use until noticed |
| Guesser of passwords | get into an account | slow hash, lock after 5, 30 failures a minute per address, a password never gives admin | a lock says that a name exists; behind a proxy all share one address |
| Network attacker | read tokens and passwords; change answers | **nothing in godepot for its own port**: needs a TLS reverse proxy; outgoing LDAP is always encrypted, https upstreams are checked | an `http` rqlite, blobgate or upstream is readable on its network |
| Anyone who can name a URL (upstream redirect, token service, JWKS) | reach internal services (SSRF) | private addresses refused after resolving; the upstream's address is the operator's only | `allow_private` turns it off for that upstream or issuer |
| Writer of a hostile upload | exhaust memory or disk; escape a path | size and count limits per format, names and paths checked, archives read and never unpacked to disk, quotas | only one parser is fuzzed; no quota by default |
| Holder of a CI token of a trusted issuer | publish as the robot | a trust must pin the repository or project; exchange gives 15 minutes, never admin | who can run that job can publish |
| Taken-over directory server | become any directory user | local accounts are never taken over; directory users are never admin; certificate checked | its users' grants are the attacker's |
| Dependency or build of godepot itself | ship bad code | pinned actions, `go.sum` and the checksum database, govulncheck, dependency review, signed releases | base images are pinned by tag, not digest |
| Reader of the metrics port | see names of repositories and rates | keep the port inside | no authentication |
| Whoever can write the database or the store | anything | network rules; not defended by godepot | full control |

## Attack surface

| Entry | Who reaches it | Checks |
|---|---|---|
| The main port: the formats under `/repository/` and `/v2/`, the API under `/api/v1/` | anyone on the network | authentication on every request; without a token only what `anonymous_read` allows; `login`, `password`, `oidc/token` take their credential in the body; body limits |
| The metrics port (off by default) | anyone who reaches it | read only; no authentication |
| Outgoing: upstreams, JWKS of issuers, the directory, rqlite, blobgate | — | see the table above |
| The settings file and the environment | the operator | checked as a whole; secrets are named by variables, not written in the file |
| Build and release | who can push or tag | CI, one release workflow, signatures |

**The most critical code**: the permission check (`internal/auth`, `internal/settings`), the verification of signed tokens (`internal/oidc`), the directory login (`internal/ldapauth`), the upstream client (`internal/upstream`), and the references that keep and free bytes (`internal/blob`).

## To do

TLS on godepot's own port; fuzz tests for each parser; base images pinned by digest.
