# Contributing

Use the Go toolchain specified in go.mod. Generated protobuf code is checked in.
Run `go fmt ./...`, `go vet ./...` and `go test -race ./...` before opening a pull request.
Include a regression test when changing a ledger invariant. Describe the failure mode, the change and how you verified it.

For PostgreSQL, provision an empty disposable database using both SQL files in deploy/postgres, in the order used in compose.yaml. Set LEDGER_TEST_DSN to the ledger_demo connection and run `make integration`. The suite creates isolated accounts and keeps the records for inspection. Use a separate database for benchmarks.

The SQLite demo creates its own temporary directory. The PostgreSQL quickstart in the README exercises the container entry points.
