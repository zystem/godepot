# The format pypi

Reference. The simple repository API of Python packaging for hosted, proxy and group repositories of the format `pypi`: the index in HTML (PEP 503) and in JSON (PEP 691) with hashes, the metadata files of wheels (PEP 658), yanked marks (PEP 592), the upload that `twine` makes, and what a proxy and a group do. How a proxy and a group behave in general (time to live, stale answers, pauses, claims) is in [proxy.md](proxy.md); this page says what is particular to Python. The settings are in [settings-file.md](settings-file.md). Tested by `internal/proxytest` (false PyPI servers) and `internal/pypi`, and run with the real `twine` and `pip` against pypi.org by `tools/test/pypi-run.sh` ([testing.md](testing.md)).

Like [gomod.md](gomod.md) this format is built on `raw`: a file is a file of the repository, at `files/<project>/<filename>`, and the **project** (its normalised name) is the name that claims and groups work with.

## Use it

```bash
pip install --index-url http://HOST/repository/<repository>/simple/ <package>
twine upload --repository-url http://HOST/repository/<repository>/legacy/ -u <any name> -p <TOKEN> dist/*
```

The token is the password; the user name is not looked at. `pip` sends credentials in the address (`http://user:TOKEN@HOST/...`) also over plain HTTP, and says so; put TLS in front where that matters. With plain HTTP `pip` needs `--trusted-host HOST` unless the host is `localhost`.

## The requests

| Request | Answer |
|---|---|
| `GET simple/<project>/` | the index of the project. **JSON** (`application/vnd.pypi.simple.v1+json`) when the `Accept` header asks for it at least as much as for any HTML, else **HTML** (`application/vnd.pypi.simple.v1+html`); `Vary: Accept`. `404` for a project with no files. A name that is not normalised (`Foo_Bar`) is `301` to the normalised one (`foo-bar`: runs of `-`, `_`, `.` are one `-`, lower case) |
| `GET simple/` | hosted only: the list of projects. A proxy and a group have none (`404`): the list of pypi.org is not copied |
| `GET files/<project>/<filename>` | the file (with `Range`, `ETag`, `X-Checksum-Sha256`, as [raw.md](raw.md)) |
| `GET files/<project>/<filename>.metadata` | the `METADATA` of a wheel (PEP 658): hosted, made at upload; a proxy: fetched from the upstream when its index names one |
| `POST legacy/` | hosted (or a group with `write_to`): upload, below. `201`; `200` for the same bytes again; `409` for other bytes under a file name; `400`; `413` over 1 GiB; `401` with no token; `403` when the account may not write that file |
| `POST yank/<project>/<filename>` | hosted: mark the file yanked; the body is the reason (up to 1024 bytes, may be empty). `204`; `404` |
| `DELETE yank/<project>/<filename>` | lift the mark. `204` |
| `DELETE files/<project>/<filename>` | hosted: delete the file (its `.metadata` goes too). `204`; `404` |

Every file link in an index is **relative** (`../../files/<project>/<filename>#sha256=...`), so a client stays on the address it used, a group's for example, and the program needs no setting for its own external address. Each link has the SHA-256 of the file; a wheel with a metadata file has `data-core-metadata` (and `data-dist-info-metadata`, its older name) with the hash of the metadata; `data-requires-python` is there when the file says it; `data-yanked` is the reason of a yanked file (an empty value is a yanked file with no reason). The JSON has the same, with `versions`, `size`, `upload-time` and `meta.api-version` `1.1`.

## Upload

`twine upload` sends a form (`multipart/form-data`) with the fields `:action` (`file_upload`), `name`, `version`, `sha256_digest` (when it is sent), `requires_python`, and the file as the part `content`. The file is ingested as it comes, after the account is found to be allowed to write `files/<project>/<filename>` (known from the file name before the first byte). It is refused with `400` unless:

- the file name is that of a **wheel** (`name-version(-build)-python-abi-platform.whl`) or a **source distribution** (`name-version.tar.gz` or `.zip`); other kinds (`.egg`, `.exe`) are not taken;
- `name` is the project of the file name (normalised) and `version` is its version (case and `_`/`-` not told apart);
- `sha256_digest`, when sent, is the SHA-256 of the bytes;
- for a wheel: it is a zip with at most 50 000 files and no name that has `..`, a backslash or a leading `/`; it has exactly one `<name>-<version>.dist-info/METADATA` of at most 10 MiB; and that `METADATA` names the same project and version. (The wheel is read where it is kept, not unpacked; a source distribution is not opened.)

`Requires-Python` of the `METADATA` (or the field of the form) is kept and shown in the index. **A file name is never overwritten** by other bytes (`409`, R4): the hash in somebody's lock file must stay true; publish a new version. The claim (R3) of the project is written in the same transaction as the file: **once a project has been uploaded to a repository in a group, the group never asks a proxy for that name**, also after the files are deleted.

## A proxy

A proxy keeps what it fetched from an upstream that serves the **JSON** index (`Accept: application/vnd.pypi.simple.v1+json`, which pypi.org does): the index of a project is cached as a file of the proxy (`index/<project>.json`, not an address of a client), and the files are fetched from the addresses that index names.

| What | Rule |
|---|---|
| the index of a project | trusted for `ttl` (default `5m`), then asked again; **served stale, with `Warning: 110`, when the upstream cannot answer** |
| a file, and a `.metadata` file | fetched once, when a client asks, from the address in the index, **with the hash that the index names checked** (P1): other bytes are not kept, the body is cut one byte short, the audit log has `upstream_changed`, and `godepot_upstream_changed_total` counts it |
| the host of a file | the upstream's own, or one of **`file_hosts`** (pypi.org names `files.pythonhosted.org`). An index that names a file on another host is not followed. Credentials go to the upstream's own host only; the address is checked as every other (private addresses refused, P9) |
| a project the upstream lacks | `404`, remembered for `negative_ttl` (default `10m`) |
| a file the index does not list | `404` |

The index that a client of the proxy gets is **made from** the upstream's, not copied: its links are relative addresses of the proxy and carry the hash. Entries with no SHA-256, or with a file name that is not a distribution of the project, are left out. **Not done:** an upstream that serves only the HTML index (a proxy of it answers `502`); the list of all projects; the JSON `upload-time` and `size` of an upstream whose index lacks them (they are left out).

## A group

A group answers for a project from **the first member that has it**: a hosted member that holds a file of it, or a proxy whose cache holds the index or whose upstream has the project. The files of one project are never mixed from two members: the index is one member's (`merge` is not built). A prefix in `claims` (`corp-` claims every project whose normalised name begins so) and every project uploaded to a hosted member are claimed (R3): the group does not ask a proxy for them. This is the guard against dependency confusion (T12), the attack in which a public package of the name of a private one, with a higher version, is installed first. `pip --extra-index-url` has that flaw; one group address has not. An upload or a yank sent to a group goes to `write_to` (`405` with none); the index of a group is `404` for `simple/` (no list).

## Checked

| What | How | Date |
|---|---|---|
| upload of a wheel and a sdist, the index in HTML and JSON with hashes, requires-python and metadata, redirect of a name that is not normalised, the metadata file, the same bytes again, other bytes, yank and lift, delete, the list of projects; the refusals above (name, version, file kind, digest, a wheel that is not a zip, with `..` or `/`, or with the `METADATA` of another project, a form with no file, another action, an upload with no token) | `internal/proxytest` (`pypi_test.go`) with a real rqlite; each check broken on purpose to see its test fail | 2026-11-20 |
| a proxy: the index from JSON that names files on another host, the file fetched once and its hash checked, the metadata file, the time to live, stale when down, a hash that is not the bytes, a file host that is not allowed; a group: our project not asked of the public index, a claimed prefix, `write_to` for an upload and a yank, the name that stays ours after a delete | the same, with false PyPI servers | 2026-11-20 |
| `twine` uploads a wheel to a group and `pip` installs it from the group; `six==1.16.0` through a proxy of pypi.org (files on files.pythonhosted.org), again with no `pip` cache (no request upstream); a claimed name not found and not asked | `tools/test/pypi-run.sh`, pip 24.0 and twine from a virtual environment | 2026-11-20 |

**Not run:** `pip` with `--extra-index-url` in the same command; `poetry`, `uv` and `pdm` (they use the same API); an upstream other than pypi.org (devpi, Nexus, Artifactory); a package of hundreds of files; the legacy JSON API (`/pypi/<name>/json`) and the XML-RPC search, which are not served; `twine` with a project description to check, and digital signatures (`.asc`).
