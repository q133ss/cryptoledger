# Protect transaction contents and audit order

Ed25519 signs a canonical transaction digest. The audit separately hashes each row together with its predecessor. The verifier checks both and expects one posting event per transaction. A signature binds one operation to the signing key. A chain commits to the observed order. Independent evidence of the latest chain tip would require an external checkpoint. Local verification uses the configured signing key and one read snapshot.
