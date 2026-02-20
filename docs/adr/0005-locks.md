# Concurrency across writers

SQLite uses one connection per store. PostgreSQL takes a transaction-scoped advisory lock for the idempotency key and locks balance rows in account-ID order. Disjoint transfers can validate and sign concurrently. A separate transaction-scoped lock serializes audit appends so that two commits cannot choose the same chain tip. Hash collisions in the idempotency lock add waiting while the full key remains unique in the table. The audit lock and heavily used accounts limit throughput.
