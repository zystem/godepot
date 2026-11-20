# Metrics

Reference. Served at `/metrics` on `GODEPOT_METRICS_PORT` ([configuration.md](configuration.md)), in the text format of Prometheus. The collector metrics were checked by running two replicas on 2026-11-17: `godepot_leader` was 1 on one and 0 on the other, and 1 on the other one 3 seconds after the first was stopped with `SIGTERM`.

| Metric | Type | Meaning |
|---|---|---|
| `godepot_up` | gauge | 1 while the program is running |
| `godepot_leader` | gauge | 1 while this replica holds the lease of the background work, else 0. Over all replicas it is 1, except for up to the lease time after the leader dies |
| `godepot_collect_passes_total` | counter | passes of the collector that this replica made while it held the lease |
| `godepot_collect_errors_total` | counter | passes that ended with an error (the next pass goes on) |
| `godepot_collected_total{kind}` | counter | what the collector removed: `graves` (keys deleted from the store), `uploads` (swept: too old), `blobs` (nothing named them for the grace time), `oci_uploads` (uploads in parts of the format oci that took too long) |
| `godepot_reads_total{repo}` | counter | files, blobs and manifests read (a `GET` that sent bytes), by repository |
| `godepot_writes_total{repo}` | counter | files written, new or changed, by repository (the same bytes again are not counted) |
| `godepot_deletes_total{repo}` | counter | files deleted, by repository |
| `godepot_refused_total{reason}` | counter | requests refused: `unauthenticated` (401), `forbidden` (403), `conflict` (a file is immutable), `quota`, `digest` (the bytes were not the ones the client named), `busy` (more downloads at once than `GODEPOT_MAX_DOWNLOADS`), `upstream` (a proxy could not be served by its upstream: paused, failed, or refused its credentials) |
| `godepot_downloads_active` | gauge | blobs and files being sent now; never above `GODEPOT_MAX_DOWNLOADS`. Set when a download starts and when it ends |
| `godepot_upstream_requests_total{repo}` | counter | requests sent to the upstream of a proxy repository (not the ones held back by a pause) |
| `godepot_upstream_errors_total{repo}` | counter | requests that ended in an error, a `429` or a `5xx` (each starts or lengthens the pause of the upstream), and requests not sent because the upstream was paused ([proxy.md](proxy.md)) |
| `godepot_upstream_stale_total{repo}` | counter | answers served from the cache, with `Warning: 110`, because the upstream could not be asked |
| `godepot_upstream_changed_total{repo}` | counter | times an upstream gave other bytes for what cannot change: a blob or a manifest by digest, a raw file in `immutable_paths`. The cached bytes stay. It is an alarm |

Not there yet: bytes and latency of requests, the size of the store, the age of the oldest unnamed blob, the result of `reconcile`.
