-- Local Compose database only. The database port stays inside its project network.
CREATE ROLE ledger_demo LOGIN PASSWORD 'local-demo-only' IN ROLE ledger_service;
