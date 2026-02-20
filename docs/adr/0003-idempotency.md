# Retry after a lost reply

The caller chooses the idempotency key before sending. A unique row binds the key to the financial payload. The service compares the canonical request hash, authenticated actor and signed note. Compatible retries return the original identity, signature and account totals through the original posting sequences. Reusing a key with a changed amount fails. PostgreSQL serializes contenders on the key before it checks available funds, so an already committed debit remains replayable after the account is empty.
