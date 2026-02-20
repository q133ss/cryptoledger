# Append-only journal

A cached account total cannot explain a customer's balance. Each posting persists a signed amount and links to one transaction. The runtime role can append journal rows. Triggers reject updates and deletes. Corrections use a new transaction with a business reference. The cache is updated in the same commit, and the verifier reconciles it against the journal. Storage and verification work grow with history.
