# Publish from a CI job with no stored secret

How-to. A CI that gives each job a signed OpenID Connect token (GitHub Actions, GitLab, Kubernetes) can have the job exchange that token for a token of godepot that lives **15 minutes**. Nothing secret is kept in the CI. Why it is shaped so: [access.md](access.md); every word of the settings: [settings-file.md](settings-file.md); the call: [api.md](api.md).

What was run is in [Checked](#checked): of the real CI systems, only GitHub Actions has done it.

## 1. Make the robot

The job acts as a **robot** account that is not an admin. The grants of the account are the outer limit of what a job can do.

```bash
godepot new-account ci-app -robot
```

```
{"time":"2026-11-26T23:07:56+02:00","level":"INFO","msg":"audit","actor":"cli",...,"action":"account.create","result":"ok","detail":"ci-app robot"}
account ci-app (robot) made
```

(The first line is the audit line, cut.)

## 2. Trust the issuer

In the settings file:

```json
{
  "repositories": [{"name": "files", "format": "raw", "kind": "hosted"}],
  "grants": [{"account": "ci-app", "repositories": ["files"], "actions": ["read", "write"]}],
  "issuers": [
    {"name": "github", "url": "https://token.actions.githubusercontent.com", "audience": "https://godepot.example.com"}
  ],
  "trusts": [
    {"issuer": "github", "account": "ci-app", "claims": {"repository": "corp/app", "ref": "refs/heads/main"}, "paths": ["releases/**"]}
  ]
}
```

`godepot validate` checks it:

```bash
godepot validate
```

```
settings: ok
settings file: ok (1 repositories, 1 grants)
database: ok
store: ok
```

`SIGHUP` makes a running program read it.

**The conditions are the lock.** GitHub signs tokens for every repository of every user, with any audience a workflow asks for: only `claims` keeps a stranger's job out. Pin the repository (or its owner), and the branch or environment when only that may publish. A pattern `corp/*` lets in every repository of `corp`; `sub: "repo:**"` would let in everybody, and godepot cannot tell.

**Prefer `repository` and `ref` to `sub` for GitHub.** The text of `sub` is not the same everywhere: the run of 2026-11-26 got `repo:<owner>@<owner id>/<repository>@<repository id>:ref:refs/heads/<branch>`, with the numbers, so a pattern `repo:corp/app:*` would not have fitted it. Look at the audit line of a refused try to see what your tokens say.

## 3. The job

GitHub Actions:

```yaml
permissions:
  id-token: write   # without it the job gets no token
  contents: read
steps:
  - name: publish
    env:
      GODEPOT: https://godepot.example.com
    run: |
      jwt=$(curl -sSf -H "Authorization: bearer $ACTIONS_ID_TOKEN_REQUEST_TOKEN" \
        "$ACTIONS_ID_TOKEN_REQUEST_URL&audience=$GODEPOT" | jq -r .value)
      token=$(jq -n --arg t "$jwt" '{token: $t}' | curl -sSf --data-binary @- "$GODEPOT/api/v1/oidc/token" | jq -r .token)
      echo "::add-mask::$token"
      curl -sSf -H "Authorization: Bearer $token" -T app.tar.gz "$GODEPOT/repository/files/releases/app.tar.gz"
```

The answer of the exchange has this form:

```json
{"account":"ci-app","expires_at":1795700000,"token":"gd_..."}
```

The token goes where any token goes: a bearer token, or the password of basic authentication (`npm`, `mvn`, `twine`, `docker login`: [api.md](api.md)).

GitLab: ask for the token with `id_tokens: {GODEPOT_JWT: {aud: https://godepot.example.com}}` in the job, and send `$GODEPOT_JWT` as `token`. The issuer is the address of the GitLab; pin `project_path`.

Kubernetes: mount a projected `serviceAccountToken` with an `audience`, and pin `sub` (`system:serviceaccount:<namespace>:<name>`). godepot reads the keys **with no credential**: the cluster must serve its discovery document and keys to anonymous requests (many do not by default), at an address godepot reaches (`allow_private` for one inside your network; a cluster's own certificate authority has to be trusted by the machine of godepot, for example with `SSL_CERT_FILE`). Not run.

Woodpecker: whether it gives a job such a token was not checked.

## When it is refused

The answer says little on purpose. The audit log (`GET /api/v1/audit?action=oidc.exchange`) says what the token was.

| Answer | `detail` in the audit log | What to do |
|---|---|---|
| `401` `the token is not accepted` | `no issuer of the settings is "..."` | add the issuer; its `url` is the `iss` of the token, to the character |
| `401` | `it is not for the audience "..."` | the job asked for another audience than the issuer's `audience` |
| `401` | `it has expired` | exchange sooner; check the clock (one minute apart is allowed) |
| `401` | `the signature is not right`, `the issuer has no key ...` | the token is not of this issuer, or the keys changed less than a minute ago: try again |
| `403` `no trust of this server fits the token` | `no trust fits: issuer ..., sub ...` | compare the claims with the `claims` of the trust; `*` stops at `/`, `**` does not |
| `403` `the account of the trust cannot be used` | `the account ... does not exist` or `... is not an enabled robot that is not an admin` | make the robot, enable it, or name another |
| `429` | | 30 refused tokens from one address in a minute: wait |
| `503` `the keys of the issuer cannot be read` | the reason | godepot cannot reach the issuer; a private address needs `allow_private` |

## Checked

- 2026-11-26: the two commands above, with the settings file of step 2.
- 2026-11-26, `tools/test/oidc-run.sh` ([testing.md](testing.md)): the exchange against an issuer whose tokens `openssl` signed, and the keys of GitHub's real issuer read (a token signed by us with a key id of GitHub's is refused for its signature).
- 2026-11-26, tests with a false issuer (`internal/oidc`, `internal/server/oidc_test.go`), RS256 and ES256.
- 2026-11-26, **a job of GitHub Actions** (a hosted runner, godepot inside it, a temporary workflow that was removed after): its real token, with `repository` and `ref` as the claims, gave a token for 900 s that published and read back a file under `releases/**` (201), was refused outside it (403); a token the job asked for another audience was refused (401).
- **Not run:** GitLab, Kubernetes, Woodpecker. The GitHub snippet above as written: the run asked for the token and exchanged it with the same two requests, with its own names and addresses.

## Limits

- Only `RS256` and `ES256`. Only claims that are a text, a number or a boolean can be conditions (not lists, not objects).
- The life of the token is 15 minutes and is not a setting. A longer job exchanges again.
- A job token can be exchanged more than once while it is valid.
- godepot reads the keys directly, **not through an HTTP proxy** of the environment.
- Each exchange adds a row to the tokens of the account. Ended tokens are not removed.
