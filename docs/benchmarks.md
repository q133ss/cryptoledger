# Posting benchmarks

Go 1.26.0 on Windows amd64, AMD Ryzen 5 5600X, 16 GB installed RAM and an NVMe SSD. SQLite WAL used a local file. PostgreSQL 17.12 ran in Docker Desktop on the same host with a TCP connection from Windows. Each transaction moves one minor unit between two repeatedly used accounts, signs it and appends one audit record. Each parallel run uses four workers. Measurements include service and storage time. The gRPC transport is outside the timed section.

| Backend | Clients | Transactions/s | p50 ms | p99 ms | Allocations/tx |
|---|---|---:|---:|---:|---:|
| SQLite | Serial | 608 | 1.56 | 5.99 | 542 |
| SQLite | Parallel | 618 | 5.07 | 22.56 | 544 |
| PostgreSQL | Serial | 75 | 13.23 | 16.41 | 540 |
| PostgreSQL | Parallel | 98 | 30.53 | 154.38 | 542 |

Each cell is the median of five five-second runs. Percentiles are sampled per call within each run. Some measurements overlapped local container builds and race tests. PostgreSQL latency includes repeated round trips across the Docker Desktop boundary and lock contention on these same two accounts. Use the raw data for the range of results.

Set LEDGER_BENCH_DSN to a disposable provisioned PostgreSQL database to include both engines. Then run the following command from the repo root.

```sh
go test -run '^$' -bench '^BenchmarkPostTransaction$' -benchtime=5s -count=5 -cpu=4 -benchmem ./internal/ledger
```

Run `make bench` to include a separate SQLite verification benchmark over 1,000 signed transactions. [Raw observations](benchmarks.txt)
