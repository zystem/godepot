# Connect an OCI client

How-to. Push and pull with `helm`, `oras`, `crane`, `skopeo`, `cosign`, `docker` and `podman`. The commands of the first four were run on 2026-11-20 against godepot at `localhost:18100` (the stand of [testing.md](testing.md)) and print what is shown, with the long lines cut as said; `docker` and `podman` were **not run** (see the end). Every request is in [oci.md](oci.md).

## Before any client

You need a repository of the format `oci` in the settings file, an account, and a token ([settings-file.md](settings-file.md), [access.md](access.md)). With `reg` as the repository, `HOST` as the address of godepot, and a token in `$T`:

```json
{"repositories": [{"name": "reg", "format": "oci", "kind": "hosted"}], "grants": []}
```

```bash
godepot new-account ci -robot
T=$(godepot new-token ci -life 24h -repositories reg -actions read,write)
```

(`ci` needs a grant in the file, or to be an admin account made with `new-account -admin`; the token narrows an account, it does not widen one.) The name of an image is `HOST/reg/<image>`: **`reg`, the first part, is the repository**, and the rest is yours.

Every client below sends the token as the **password**; the user name is not looked at (use the account's name). The address has no scheme in `docker` and `podman` and `crane`; `helm` writes `oci://`.

**TLS.** godepot serves plain HTTP on its port ([configuration.md](configuration.md)). The clients refuse that for a registry that is not on their list of insecure ones, so the examples below use the flag of each client for it (`--plain-http`, `--insecure`, `--tls-verify=false`). On a real network put a proxy that serves TLS in front.

## helm

```bash
echo $T | helm registry login localhost:18100 -u ci --password-stdin --plain-http
helm push mychart-0.1.0.tgz oci://localhost:18100/reg/charts --plain-http
helm pull oci://localhost:18100/reg/charts/mychart --version 0.1.0 --plain-http
```

```
Login Succeeded
Pushed: localhost:18100/reg/charts/mychart:0.1.0
Digest: sha256:a336270248a022518fe940aeb1c04e44f7331ce2568d79dc711d4ab81d43355f
Pulled: localhost:18100/reg/charts/mychart:0.1.0
Digest: sha256:a336270248a022518fe940aeb1c04e44f7331ce2568d79dc711d4ab81d43355f
```

The chart is the image `charts/mychart` and its version is the tag. The `.tgz` that comes back is the one that went in (compared byte for byte in `tools/test/oci-run.sh`).

## oras: any file, and what is attached to it

```bash
echo $T | oras push --plain-http -u ci --password-stdin localhost:18100/reg/hello:v1 hello.txt:text/plain
echo $T | oras attach --plain-http -u ci --password-stdin --artifact-type application/vnd.example.note localhost:18100/reg/hello:v1 note.txt:text/plain
echo $T | oras discover --plain-http -u ci --password-stdin localhost:18100/reg/hello:v1
```

```
Uploading 5891b5b522d5 hello.txt
Uploaded  5891b5b522d5 hello.txt
Pushed [registry] localhost:18100/reg/hello:v1
ArtifactType: application/vnd.unknown.artifact.v1
Digest: sha256:e81de7da375109772cf2ccf5723ee518514f7b5b089fba2dddb99da18dce95df
Uploading 389ed6887e49 note.txt
Uploaded  389ed6887e49 note.txt
Attached to [registry] localhost:18100/reg/hello@sha256:e81de7da375109772cf2ccf5723ee518514f7b5b089fba2dddb99da18dce95df
Digest: sha256:1c9867cc6d8291c7c14b76a0fcd08feb298b2265af65b7af6ec7143ea44a49f1
localhost:18100/reg/hello@sha256:e81de7da375109772cf2ccf5723ee518514f7b5b089fba2dddb99da18dce95df
└── application/vnd.example.note
    └── sha256:1c9867cc6d8291c7c14b76a0fcd08feb298b2265af65b7af6ec7143ea44a49f1
```

`discover` is the referrers API: the note is listed under the artifact it names as its `subject`.

## crane

```bash
echo $T | crane auth login localhost:18100 -u ci --password-stdin
crane copy alpine:3.20 localhost:18100/reg/alpine:3.20 --insecure
crane ls localhost:18100/reg/alpine --insecure
crane digest localhost:18100/reg/alpine:3.20 --insecure
```

```
2026/11/20 15:16:32 logged in via /home/me/.docker/config.json
2026/11/20 15:16:44 pushed blob: sha256:d38f020baf1fa2c87122ac85c7c1277244aec5f8a4a3f4c8a78bf2d10851e1e4
2026/11/20 15:16:44 localhost:18100/reg/alpine@sha256:856b20bf4946b5c8f43fc5b2ce46f39109914d9213a2832eb24cc01c76f27bbe: digest: sha256:856b20bf4946b5c8f43fc5b2ce46f39109914d9213a2832eb24cc01c76f27bbe size: 1022
2026/11/20 15:16:44 localhost:18100/reg/alpine:3.20: digest: sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc size: 9226
3.20
sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc
```

(Cut: the warning about credentials kept in the clear and the lines for the other blobs. The path in the first line is the one of the machine of the run, replaced.) The digest of the tag in godepot is the digest of `alpine:3.20` at its source: the multi-platform index and the attestation manifests that came with it were taken in as they were.

## skopeo

```bash
skopeo copy --dest-tls-verify=false --dest-creds ci:$T docker://alpine:3.19 docker://localhost:18100/reg/alpine:3.19
skopeo list-tags --tls-verify=false --creds ci:$T docker://localhost:18100/reg/alpine
```

```
Copying blob sha256:17a39c0ba978cc27001e9c56a480f98106e1ab74bd56eb302f9fd4cf758ea43f
Copying config sha256:83b2b6703a620bf2e001ab57f7adc414d891787b3c59859b1b62909e48dd2242
Writing manifest to image destination
{
    "Repository": "localhost:18100/reg/alpine",
    "Tags": [
        "3.19",
        "3.20"
    ]
}
```

(The first lines of the copy are cut.) `--creds ci:$T` puts the token in the list of processes of the machine: use `--authfile` where that matters.

## cosign

```bash
COSIGN_PASSWORD="" cosign generate-key-pair
cosign sign --yes --key cosign.key --allow-insecure-registry --tlog-upload=false localhost:18100/reg/alpine:3.20
cosign verify --key cosign.pub --allow-insecure-registry --insecure-ignore-tlog localhost:18100/reg/alpine:3.20
```

`sign` pushes the signature as a manifest whose `subject` is the image, and `verify` finds it through the referrers API; it printed, on 2026-11-20 (cut to the end): `The signatures were verified against the specified public key`, and the claim `{"critical":{"identity":{"docker-reference":"localhost:18100/reg/alpine:3.20"},...}}`. The two flags about the transparency log are for a registry that has no network to Sigstore; leave them out where there is. The `COSIGN_PASSWORD=""` is for the example: give the key a password.

## docker and podman: not run

`docker login`, `docker push` and `docker pull` speak the same calls as `skopeo` and `crane` and are expected to work: they are a `POST`, one `PATCH` of the whole layer and a `PUT`, the path that `crane` and `skopeo` take ([oci.md](oci.md)). **They were not run:** on the machine of 2026-11-20 the daemon of `docker` refused `127.0.0.1:18100` as a registry that speaks plain HTTP (`server gave HTTP response to HTTPS client`), and its `insecure-registries` are the owner's to change; `podman` is not installed there. For both, the address of godepot has to be on the list of insecure registries (`/etc/docker/daemon.json`, `/etc/containers/registries.conf`) or behind TLS.

```bash
echo $T | docker login HOST -u ci --password-stdin
docker tag alpine:3.20 HOST/reg/alpine:3.20
docker push HOST/reg/alpine:3.20
```

`tools/test/oci-run.sh` runs all of these that are installed, and says `skipped` for the others and why.
