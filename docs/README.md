# The documents

Every page is of one kind (Diátaxis). The README at the root is the way in.

| Page | Kind | What is in it |
|---|---|---|
| [../README.md](../README.md) | tutorial | the try-it, with the output of every command |
| [configuration.md](configuration.md) | reference | every environment variable, command and exit code, with default and whether a restart is needed |
| [metrics.md](metrics.md) | reference | every metric |
| [testing.md](testing.md) | how-to | run the tests, with a real rqlite and a real blobgate; what was run |
| [blob-layer.md](blob-layer.md) | explanation | what happens to bytes, and what a crash leaves |
| [spec.md](spec.md) | explanation and reference | what godepot is to be: the requirements, the data model, what it needs of blobgate |
| [plan.md](plan.md) | explanation | the steps with their proofs, and the traps of other products with the way round each |
| [status.md](status.md) | record | what is built, what is not, what was run |

A setting, a metric, a command or an exit code that the program has and its page does not mention fails `go test ./cmd/godepot` (`docs_test.go`).
