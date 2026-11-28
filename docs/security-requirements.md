# What you can and cannot expect, in security

Reference. As of 0.7.1 (2026-11-28).

## You can expect

- **Deny by default.** A request is allowed only when the account's grants **and** the token's scope allow it ([access.md](access.md)). Anonymous read is off unless a repository turns it on.
- **Tokens end, and only a digest is kept.** Every token has an owner, a scope and an end date; the database holds its SHA-256, compared in constant time.
- **Passwords are stored slowly hashed** (PBKDF2-HMAC-SHA256, 600 000 rounds, a salt each); five failures lock the account; a password never gives the admin API.
- **A directory is asked only over TLS** (LDAPS or StartTLS), with the certificate checked ([ldap.md](ldap.md)).
- **Bytes are what their digest says.** An upload with a named hash, and a proxied file whose upstream names one, is kept only if it matches. A released version is not overwritten unless the repository allows it; an OCI tag may move unless `immutable_tags` names it.
- **A proxy does not reach inside your network** unless told to (`allow_private`): loopback, private and link-local addresses are refused after the name is resolved; credentials are not sent to another host after a redirect.
- **Your names are not fetched from the public** once a hosted repository has them or they are claimed ([proxy.md](proxy.md)).
- **Passwords of other systems that godepot keeps are sealed** (AES-256-GCM, bound to their name) with a key that is never in the database; no call returns them ([secrets.md](secrets.md)).
- **Writes, deletes and refusals are in the audit log**; a write and its line are one transaction. Passwords and tokens are not logged.
- **Releases you can check** from 0.7.1: signed, with build provenance and a bill of materials ([verifying-releases.md](verifying-releases.md)).

## You cannot expect

- **TLS on the port of godepot.** It serves plain HTTP: put a reverse proxy with TLS in front, or tokens and passwords cross the network in the clear.
- **Protection if the database, blobgate or the host is taken.** The sealed passwords are not readable without the seal key, but the rest of the database is. Who can write rqlite can make an admin; who holds blobgate's secret can change bytes. Keep both where only godepot reaches them.
- **Protection of the metrics port**: it asks for no credential.
- **Reads in the audit log**: they are counted, not audited.
- **A check of what packages do.** godepot does not scan for malware or vulnerabilities (a scanner hook is planned, not built).
- **A limit on requests.** Downloads at once are limited and failed logins are throttled; other floods are for the reverse proxy.
- **Availability** beyond that of its database and blobgate. Two replicas at once have not been run.
- **Fixes for old releases** ([SECURITY.md](../SECURITY.md)), or any certification. No external audit has been made.
