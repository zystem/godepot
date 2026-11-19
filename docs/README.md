# The documents

Every page is of one kind (Diátaxis). The README at the root is the way in.

| Page | Kind | What is in it |
|---|---|---|
| [../README.md](../README.md) | tutorial | the try-it, with the output of every command |
| [configuration.md](configuration.md) | reference | every environment variable, command and exit code, with default and whether a restart is needed |
| [settings-file.md](settings-file.md) | reference | every word of the settings file: repositories, grants, patterns |
| [raw.md](raw.md) | reference | every request of the format `raw`, with its answers |
| [api.md](api.md) | reference | every call of the API, how a client sends a token, the fields of the audit log |
| [access.md](access.md) | explanation | accounts, tokens and the audit log: how a request is checked, and why |
| [metrics.md](metrics.md) | reference | every metric |
| [testing.md](testing.md) | how-to | run the tests, with a real rqlite and a real blobgate; what was run |
| [blob-layer.md](blob-layer.md) | explanation | what happens to bytes, and what a crash leaves |
| [spec.md](spec.md) | explanation and reference | what godepot is to be: the requirements, the data model, what it needs of blobgate |
| [plan.md](plan.md) | explanation | the steps with their proofs, and the traps of other products with the way round each |
| [step0-probes.md](step0-probes.md) | record | what was asked of blobgate in step 0, what it answered, what was not run |
| [status.md](status.md) | record | what is built, what is not, what was run |

A setting, a metric, a command or an exit code that the program has and its page does not mention fails `go test ./cmd/godepot` (`docs_test.go`).
