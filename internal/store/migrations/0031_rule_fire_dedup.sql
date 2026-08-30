-- A sweep-source rule's cooldown keys off the event's dedup key, so each target
-- of a multi-target check gets its own cooldown slot (check:<check>:<target>)
-- instead of sharing the rule's single budget. Webhook rules stay per-rule:
-- their keys are fingerprint-based, and per-key cooldown would hand a varying
-- body a fresh budget on every distinct body. Pre-existing rows default to '',
-- which per-rule lookups never match and per-key lookups never serve.
ALTER TABLE rule_fire ADD COLUMN dedup_key TEXT NOT NULL DEFAULT '';

CREATE INDEX rule_fire_rule_key ON rule_fire (rule_id, dedup_key);