# Log in with a directory (LDAP, Active Directory)

How-to. People log in with the name and password of your directory, and get rights by their groups. Why it is shaped so: [access.md](access.md); every word of the settings: [settings-file.md](settings-file.md). What was run is in [Checked](#checked).

## 1. A service account and its certificate

godepot searches the directory as a **service account** that may only read. **Nothing is sent in the clear**: `ldaps`, or `ldap` with `start_tls`, and **the certificate of the directory is always checked**. Unless the machine already trusts it, give godepot one of these (PEM); both work:

- **`ca.crt`**, the certificate of the authority that signed the directory's certificate. Get it from whoever runs that authority.
- **`server.crt`**, the directory's own certificate, when it is **self-signed** and there is no authority. When the server gets a new certificate, give godepot the new one.

**What is checked depends on which one you gave:**

| The server shows | The name | Also checked |
|---|---|---|
| exactly the certificate you gave (`server.crt`) | **not checked**: an address or an alias works, even with no name in the certificate | its time, and that it may be used by a server |
| a certificate signed by the one you gave (`ca.crt`) | **checked**: the host name or address of `url`, or `server_name`, must be among its subject alternative names | the chain, its time, and its use |

The name matters for an authority because it signs for many machines: without the check any of them, taken over, could pose as the directory and receive people's passwords. How to give the certificate: [step 3](#3-trust-the-certificate). `server_name`, when set, is asked for in both cases.

Without a given certificate (the machine's own list of authorities) the name is always checked.

The password of the service account comes from an environment variable of godepot, never from the settings file:

```bash
export GODEPOT_LDAP_PASSWORD='the password of the service account'
```

## 2. The settings

Active Directory:

```json
{
  "repositories": [{"name": "files", "format": "raw", "kind": "hosted"}],
  "ldap": {
    "url": "ldaps://dc1.example.com:636",
    "bind_dn": "svc-godepot@example.com",
    "password_env": "GODEPOT_LDAP_PASSWORD",
    "user_base": "OU=People,DC=example,DC=com",
    "user_filter": "(&(objectCategory=person)(sAMAccountName={user})(!(userAccountControl:1.2.840.113556.1.4.803:=2)))",
    "group_base": "DC=example,DC=com",
    "group_filter": "(member:1.2.840.113556.1.4.1941:={dn})"
  },
  "grants": [
    {"group": "developers", "repositories": ["files"], "paths": ["dev/**"], "actions": ["read", "write"]}
  ]
}
```

- **The user filter must not take disabled people.** The last part above does that for Active Directory. Without it a disabled person cannot log in with a password, but **their tokens go on working**.
- The group filter above follows groups inside groups (Active Directory only). The default, `(member={dn})`, sees direct members only. The primary group (`Domain Users`) is not in `member` and is not seen.

AD LDS: `"user_filter": "(&(objectClass=user)(cn={user})(!(msDS-UserAccountDisabled=TRUE)))"`.

OpenLDAP (not run): `"user_filter": "(&(objectClass=inetOrgPerson)(uid={user}))"`, with `group_filter` `(member={dn})` for `groupOfNames` or `(memberUid={user})` for `posixGroup`. OpenLDAP has no common mark of a disabled person: add the one your directory uses to the filter.

## 3. Trust the certificate

An admin sends the certificate to godepot, which keeps it in its database, so every replica has it and no file is mounted. `$TOKEN` is a token of an admin account with the action `admin` on `*` ([api.md](api.md)). `--data-binary` matters: `-d` would drop the line ends.

```bash
curl -s -X PUT -H "Authorization: Bearer $TOKEN" --data-binary @server.crt http://HOST/api/v1/ldap/ca
```

```json
{"certificates":[{"subject":"CN=dc1.example.com","issuer":"CN=dc1.example.com","sha256":"af16e4b691412e1484b705e8e08651a6bcbdfa3e862ad7c0f66af975c6de25a2","not_before":"2026-11-26T23:58:20Z","not_after":"2027-11-26T23:58:20Z"}]}
```

**Compare `sha256` with the certificate you meant to trust**, as the server's owner shows it:

```bash
openssl x509 -in server.crt -noout -fingerprint -sha256
```

```
sha256 Fingerprint=AF:16:E4:B6:91:41:2E:14:84:B7:05:E8:E0:86:51:A6:BC:BD:FA:3E:86:2A:D7:C0:F6:6A:F9:75:C6:DE:25:A2
```

If you have no file, a server's own certificate can be read from the server (`-starttls ldap` for StartTLS). **What arrives is only what answered**: compare the fingerprint on the server itself before you trust it.

```bash
openssl s_client -connect dc1.example.com:636 </dev/null 2>/dev/null | openssl x509 > server.crt
```

It prints nothing.

- A `PUT` **replaces** what was there. Several certificates may be in one body (an old and a new one during a change).
- `GET` on the same address shows what is kept; `DELETE` removes it. A change takes effect at the next question to the directory, with no restart.
- Only certificates are taken: a body with a private key, with nothing readable, with an ended certificate or longer than 64 KiB is `400`.
- Instead, or as well, the settings may name a file: `"ca_file": "/etc/godepot/ca.pem"`. With a certificate given either way, **only those are trusted**, not the system's.

## 4. Log in

```bash
curl -s http://HOST/api/v1/login -d '{"account": "alice", "password": "..."}'
```

The answer has this form:

```json
{"account":"alice","expires_at":1795700000,"note":"this is the only time the token is shown","token":"gd_..."}
```

`npm login` and basic authentication (`curl -u alice`, `mvn`, `pip`) work with the same name and password. The name is the short one (`alice`, not `alice@example.com` or `EXAMPLE\alice`); capitals do not matter.

## What godepot does with the directory

- **The first good login makes an account** with the name in small letters: a user, never an admin. `GET /api/v1/accounts` shows it with `"source": "ldap"`. An admin can disable it.
- **A local account always wins.** A name that godepot has made itself (a user, a robot, an admin) is never asked of the directory.
- **A grant for `account`** applies to the person of that name too. Do not grant a name you have not made and that the directory might give to somebody else.
- **Groups are compared without regard to capitals**, by the `group_name` attribute (`cn`). Two groups of the same name in different places of `group_base` are one to godepot: narrow `group_base` when that can happen.
- **People are asked again** every `refresh` (default 5 minutes) while their tokens or password are used: one removed, disabled or moved between groups is so in godepot within that time. A password changed in the directory may be accepted for up to 5 minutes more in basic authentication.
- **When the directory cannot be asked**, what it said last is used for **one hour**; after that, and for any new login, the answer is `503`. Local accounts and robots do not need the directory.
- **Five wrong passwords lock the name** in godepot (as for local accounts) before they use up the tries of the directory's own lock. Check that the directory's limit is higher than five.

## When it fails

The answer says little. The audit log has the reason: `GET /api/v1/audit?action=login` and `?action=directory`.

| Answer | `detail` in the audit log | What to do |
|---|---|---|
| `401` | `no entry for the name "..."` | the person is not under `user_base`, or `user_filter` does not take them (disabled?) |
| `401` | `the directory refuses the password of "..."` | wrong or expired password |
| `401` | `more than one entry fits the name` | make `user_filter` or `user_base` narrower |
| `403` after login | `forbidden` | no grant fits the person's groups: look at `group_base` and `group_filter` |
| `503` `the directory cannot be asked` | `... certificate signed by unknown authority` | give godepot the `ca.crt` or `server.crt` (step 3); after a new certificate on the server, the new one |
| `503` | `... certificate has expired or is not yet valid` | a new certificate on the server (give it to godepot if it is self-signed); look at the clocks |
| `503` | `... certificate is valid for ..., not ...`, `... doesn't contain any IP SANs` or `... legacy Common Name field ...` | the certificate lacks the name: with a `ca.crt` use in `url` a name it has, or set `server_name`; or give the server's own certificate instead |
| `503` | `the bind of the service account: ...` | `bind_dn` or the password variable; Active Directory refuses a bind without TLS |
| `503` | `... has no password (the environment variable is empty)` | set the variable that `password_env` names, and restart |

## Checked

- 2026-11-27, `tools/test/ldap-run.sh` ([testing.md](testing.md)) against Windows Server 2025: **Active Directory** (LDAPS) and **AD LDS** (LDAPS and StartTLS): login, grants by group, groups inside groups (AD), disabled and expired-password users not let in, no secret in the log.
- 2026-11-27, the same script with `L_CA_BY_API=1`: a self-signed `server.crt` (AD LDS, LDAPS and StartTLS) and the `ca.crt` of Active Directory, each given with `PUT /api/v1/ldap/ca`. The answers shown in step 3 are of a certificate made with `openssl` for this page.
- 2026-11-27, the `openssl s_client` command of step 3 against AD LDS, plain TLS and StartTLS: the fingerprint was that of the server's certificate.
- 2026-11-27, by address, with no `server_name`: AD LDS with its `server.crt` logs in. Active Directory with the `ca.crt` does **not** (`doesn't contain any IP SANs`: its certificate names only its host), and does with `server_name`. With the certificate of another server a login did not work.
- 2026-11-27, tests with a false directory (`internal/ldapauth`, `internal/server/ldap_test.go`): also removal, a move between groups, the directory down, the lock.
- **Not run:** OpenLDAP or any other server; a person removed or moved on a real directory (the stand was not changed); a user with very many groups; the `curl` command of step 4 as written.

## Limits

- One directory. No referrals are followed, no client certificate, no Kerberos.
- The login name must be 1 to 63 of `a-z 0-9 . _ -`. A person whose name has other characters cannot log in.
- Single sign-on in a browser (OIDC) is not built.
