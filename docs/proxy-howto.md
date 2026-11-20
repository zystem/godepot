# Put Docker Hub and your own images behind one address

How-to. One task: make `images/...` serve both what you push and what Docker Hub has, so that a build needs one address; and keep an image of yours from ever being taken from the public registry by a name clash. The commands were run on 2026-11-20 against godepot at `127.0.0.1:18390` with the stand of [testing.md](testing.md) (a real rqlite and a real blobgate 0.6.1) and the real Docker Hub, with `crane` 0.20.6; the output is what they printed, with the lines cut where it says so. What each setting does is in [proxy.md](proxy.md) and [settings-file.md](settings-file.md); how a client logs in is in [oci-clients.md](oci-clients.md).

## 1. The settings

Three repositories of the format `oci`: `mine` (yours), `hub` (a proxy of Docker Hub) and `images`, a group of the two. A push to `images` goes to `mine`; a name that begins with `corp/` is yours and is never asked of Docker Hub.

```json
{
  "repositories": [
    {"name": "mine", "format": "oci", "kind": "hosted"},
    {"name": "hub", "format": "oci", "kind": "proxy", "upstream": {"url": "https://registry-1.docker.io"}},
    {"name": "images", "format": "oci", "kind": "group", "members": ["mine", "hub"], "write_to": "mine", "claims": ["corp/"]}
  ],
  "grants": []
}
```

```bash
godepot validate
```

```
settings: ok
settings file: ok (3 repositories, 0 grants)
database: ok
store: ok
```

Make an account and a token that may read and write (and `admin`, for the last step), and log the client in as in [oci-clients.md](oci-clients.md). The address of a Docker Hub image is `HOST/images/library/alpine`: `images` is the group, and the rest is the name on Docker Hub (its official images are under `library/`).

## 2. Pull a public image

The first pull asks Docker Hub; the second does not:

```bash
crane digest 127.0.0.1:18390/images/library/alpine:3.20 --insecure
crane digest 127.0.0.1:18390/images/library/alpine:3.20 --insecure
curl -s localhost:18392/metrics | grep godepot_upstream_requests_total
```

```
sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc
godepot_upstream_requests_total{repo="hub"} 2
sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc
godepot_upstream_requests_total{repo="hub"} 2
```

(The `curl` is run after each `crane`. The two requests of the first pull: does Docker Hub have the name, and the manifest of the tag.) The digest is the one Docker Hub has for `alpine:3.20`. The layers are fetched when a client pulls them, streamed to the client while they are kept; a tag is trusted for five minutes (`ttl`), and after that a `HEAD` asks whether it moved. If Docker Hub is down the tag is served from the cache with `Warning: 110` ([proxy.md](proxy.md)).

The list of tags is Docker Hub's own:

```bash
crane ls 127.0.0.1:18390/images/library/alpine --insecure | head -4
```

```
2.6
2.7
20190228
20190408
```

## 3. Push an image of yours

A push to the group goes to `mine`:

```bash
crane copy alpine:3.19 127.0.0.1:18390/images/corp/app:1 --insecure
crane ls 127.0.0.1:18390/images/corp/app --insecure
```

```
2026/11/20 19:11:24 Copying from alpine:3.19 to 127.0.0.1:18390/images/corp/app:1
2026/11/20 19:11:28 127.0.0.1:18390/images/corp/app@sha256:12bd48f39929e06326042c34d1666c98222d6a5559786b74f72baae76ac94948: digest: sha256:12bd48f399
...
2026/11/20 19:11:31 127.0.0.1:18390/images/corp/app:1: digest: sha256:6baf43584bcb78f2e5847d1de515f23499913ac9f12bdf834811a3145eb11ca1 size: 8077
1
```

(Cut: the lines for the other platforms of the image, and the blobs. The tag list of `corp/app` is `mine`'s, `1`: not Docker Hub's, which has no such image.) A name that is under a claim and that nobody has pushed is not found, and Docker Hub is not asked:

```bash
crane digest 127.0.0.1:18390/images/corp/other:1 --insecure
```

```
2026/11/20 19:11:31 HEAD request failed, falling back on GET: HEAD http://127.0.0.1:18390/v2/images/corp/other/manifests/1: unexpected status code 404
Error: GET http://127.0.0.1:18390/v2/images/corp/other/manifests/1: NAME_UNKNOWN: no member of this group has this name
```

(Cut at the end of the first line.) This is the guard against **dependency confusion**: if an image called `corp/app` were published to Docker Hub tomorrow, `images/corp/app` would still be yours. The same holds for a name you have pushed and then deleted, though it is in no `claims`: the claim is written when the name is published and stays.

## 4. What a proxy does not take

```bash
crane copy alpine:3.19 127.0.0.1:18390/hub/x/y:1 --insecure
```

```
2026/11/20 19:11:31 Copying from alpine:3.19 to 127.0.0.1:18390/hub/x/y:1
2026/11/20 19:11:37 retrying without mount: POST http://127.0.0.1:18390/v2/hub/x/y/blobs/uploads/?from=library%2Falpine&mount=sha256%3Ae7aa81ce22c7c92
2026/11/20 19:11:37 retrying without mount: Post "http://127.0.0.1:18390/v2/hub/x/y/blobs/uploads/": ...
Error: POST http://127.0.0.1:18390/v2/hub/x/y/blobs/uploads/: UNSUPPORTED: a proxy repository is read-only: push to a hosted repository, or to a group
```

(The second line is cut.) A proxy is read-only. And an image that Docker Hub does not give to an anonymous client is "not found" here: Docker Hub answers `401` for a name that is not there, and godepot takes that for "no such name", remembers it for ten minutes and does **not** stop asking Docker Hub for other names.

## 5. When something is remembered that should not be

A "not found" is remembered for ten minutes (`negative_ttl`). If you have just published the name upstream and cannot wait, clear it:

```bash
curl -s -X POST -H "Authorization: Bearer $T" http://127.0.0.1:18390/api/v1/repositories/hub/forget
```

```
{"forgotten":11,"repository":"hub"}
```

(11 entries were remembered as missing at that moment; the number is what the steps above happened to ask for.) It also ends a pause, if the upstream was paused after errors.

## Credentials for a private upstream

For an upstream that needs a user and a password, name the **environment variable** that holds the password; the password is not in the file:

```json
{"name": "hub", "format": "oci", "kind": "proxy", "upstream": {"url": "https://registry-1.docker.io", "username": "mybot", "password_env": "HUB_PASSWORD", "token_hosts": ["auth.docker.io"]}}
```

`token_hosts` lets the credentials go to the token service of Docker Hub, which is on another host; without it the service is asked for an anonymous token. **Not run** with a real account: the examples above are anonymous. What was run is a false registry with a token service ([proxy.md](proxy.md)).
