# Accounts, tokens and the audit log: how it works and why

Explanation. Who may do what, how a request is checked, and the reasons. The calls, headers and fields are in [api.md](api.md); the grants are in [settings-file.md](settings-file.md). Checked by tests with a real rqlite (`internal/auth`, `internal/server`) and by the program run against a real blobgate (`cmd/godepot/raw_test.go`).

## The model

- An **account** is a **user** or a **robot** (an account for a program, with a plain name such as `ci-build`: no `$`, no spaces, so that no shell eats it). A **user** may have a **password** ([api.md](api.md)); a robot has none. An account gets in with **tokens**, or with its password, which gives a token (login) or is the password of basic authentication for the data of the account.
- A **token** has an owner (an account), a **scope** (a list of grants: repositories, path patterns, actions) and an **end date**. It always ends; the longest life is `GODEPOT_TOKEN_MAX_DAYS` (90 by default). It looks like `gd_<12 hex>_<43 characters>`: the first part is its **id** (public: it is in the audit log and in listings), the second its secret. The secret is **shown once**, when the token is made, and only its SHA-256 is kept.
- What an account may do is in the settings file as **grants** ([settings-file.md](settings-file.md)), or is everything for an **admin** account.
- A request is allowed when **both** the account and the token's scope allow it. A token therefore never has more than its account: making a token with a wide scope gives nothing the account lacks, so a user may make tokens for oneself.
- **Anonymous read** is a setting of a repository, off by default.
- Accounts can be **disabled**: all their tokens stop at once, and come back when the account is enabled. A token can be **revoked** for good.

A **CI job** may get in with no stored secret: the signed token that its CI gives it is exchanged for a token of a robot that lives 15 minutes ([ci-jobs.md](ci-jobs.md)).

Single sign-on of people (OIDC in a browser) and LDAP are not here yet (step 8 of the plan, the next parts). A user logs in with a password or a token that an admin made.

## Why it is shaped so

- **A job's token is exchanged, not accepted everywhere.** Every format and call then sees one kind of credential, with an owner, a scope, an end and a line in the audit log. The settings say who is trusted, the account says the most it may do, and the trust narrows it; a job is never an admin, and never a user.
- **Passwords came after tokens, and give tokens.** Tokens with an end date, a scope and an owner are what every client can send (`curl -u`, `pip`, Maven, `apt`), so they came first. A password is for a person at a terminal (`npm login`, `docker login`, `POST /api/v1/login`): it is checked with a hash that costs time, locks the account after five failures, tells nothing about which names exist, and **never opens the admin API**, so a password that leaks gives the data actions of the account and not the control of the program. A script uses a token.
- **The account and the token must both allow.** The settings file says what an account may do; a token can only narrow it. Making a token is then safe to let a user do for oneself, and a token in a CI job can be narrow without a second place to keep rules.
- **A token always ends.** Tokens that never end, shared by a team and pasted into CI settings, are a trap of other products ([plan.md](plan.md), T31).
- **The secret is in the header and nowhere else.** An address is logged by every proxy; a header is not ([plan.md](plan.md), T32).
- **The audit row of a change is in the change's transaction.** A write or a delete and its row are applied together or not at all. The rows of refusals and of changes to accounts and tokens are written after the thing they describe, by themselves: a crash between the two loses the line, not the change. This is the one place where the audit is not atomic.
- **Failing to write the row of a refusal does not stop the refusal.** It is logged.
- **`X-Forwarded-For` is not trusted** for `remote` in the audit log: anyone could write it. The address is the one the connection came from, which behind a reverse proxy is the proxy's.

Reads are counted ([metrics.md](metrics.md)) and are not audited yet (spec A6 says "when the repository says so").
