-- A signed git source's trust anchor must be owned by exocomp, not the host's
-- ambient git config: allowed_signers holds the SSH allowed-signers content
-- (git's `gpg.ssh.allowedSignersFile` format) that `source sync` materializes to
-- a 0700 file and verifies the pinned ref against. Empty falls back to git's own
-- configuration, preserving existing signed sources.
ALTER TABLE source ADD COLUMN allowed_signers TEXT NOT NULL DEFAULT '';
