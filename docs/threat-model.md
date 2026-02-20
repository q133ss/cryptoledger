# Threat model

The local demo trusts its host, certificate issuer, database administrator and software signer. mTLS identifies one calling service. Every holder of an accepted client certificate can invoke the ledger RPCs. A production deployment needs authorization by operation and account, certificate lifecycle management, and protected database transport.

The runtime database login can create accounts, insert postings and maintain the balance cache. PostgreSQL privileges and triggers reject ordinary history mutation. Integrity verification recomputes signed digests, journal totals, cache values and audit links. The checks run on one snapshot and verify one signing key. Old databases signed with an ephemeral key require that original public key for verification.

A database owner controls stored history. A linked hash chain cannot independently prove completeness after rollback of an entire database. External checkpoints and signing-key rotation remain open work. The Compose signing seed and generated TLS credentials live on a local named volume. Store a backup together with the associated database for restart tests. Treat the Compose passwords as public local examples.
