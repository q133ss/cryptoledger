# Changelog

## v0.1.0 — 2026-11-20

- Added a PostgreSQL 17 backend with a restricted runtime role, account locks, a serialized audit chain and commit-time double-entry checks.
- Preserved original transaction balances on replay and tested ten independent callers against one database.
- Bound posted audit identity to mTLS and persisted the local Ed25519 signing seed across server restarts.
- Added the Compose quickstart, ledgerctl, health and metrics endpoints, benchmark measurements, and Windows/Linux CI.
