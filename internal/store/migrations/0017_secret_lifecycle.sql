-- Secret lifecycle attributes: where a secret's value came from, and when it
-- expires. Both are additive ALTER TABLE ADD COLUMN, so there is no table
-- rebuild and no re-seal — the per-secret AAD stays 'name|kind' and every
-- ciphertext sealed before this migration still opens byte-for-byte.
--
-- source records how the value was minted: 'user' (the operator supplied it on
-- stdin) or 'generated' (exocomp minted a random value). Every pre-existing row
-- was operator-supplied, so the DEFAULT makes them 'user'.
ALTER TABLE secret ADD COLUMN source TEXT NOT NULL DEFAULT 'user';
-- expires_at is unix milliseconds; NULL means the secret never expires. Existing
-- rows have no expiry. It is a hygiene marker for the CLI and never gates
-- decryption — an expired secret still opens, it just prints as EXPIRED.
ALTER TABLE secret ADD COLUMN expires_at INTEGER;
