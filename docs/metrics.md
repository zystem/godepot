# Metrics

Reference. Served at `/metrics` on `GODEPOT_METRICS_PORT` ([configuration.md](configuration.md)), in the text format of Prometheus. Checked by running two replicas on 2026-11-17: `godepot_leader` was 1 on one and 0 on the other, and 1 on the other one 3 seconds after the first was stopped with `SIGTERM`.

| Metric | Type | Meaning |
|---|---|---|
| `godepot_up` | gauge | 1 while the program is running |
| `godepot_leader` | gauge | 1 while this replica holds the lease of the background work, else 0. Over all replicas it is 1, except for up to the lease time after the leader dies |
| `godepot_collect_passes_total` | counter | passes of the collector that this replica made while it held the lease |
| `godepot_collect_errors_total` | counter | passes that ended with an error (the next pass goes on) |
| `godepot_collected_total{kind}` | counter | what the collector removed: `graves` (keys deleted from the store), `uploads` (swept: too old), `blobs` (nothing named them for the grace time) |

Not there yet: requests, bytes, the size of the store, the age of the oldest unnamed blob, the result of `reconcile`.
