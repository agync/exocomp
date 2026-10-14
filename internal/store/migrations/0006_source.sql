-- Source and credential model (Decision 0010). Three tables realise the git-centric
-- flows the M1 `repo` sketch only named.
--
-- A source is where a run's tree comes from: a registered git repo (auto-synced,
-- deploy-keyed, optionally signed-tag-pinned), a host path bind-mounted into the
-- container, or an in-container path in the data volume. Only how the tree
-- arrives differs; the credential planes are identical across all three.

CREATE TABLE source (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    name              TEXT    NOT NULL UNIQUE,
    kind              TEXT    NOT NULL CHECK (kind IN ('git', 'hostpath', 'volpath')),
    -- git: the clone URL. hostpath/volpath: the tree's path. Never a secret.
    uri               TEXT    NOT NULL DEFAULT '',
    -- git: the ref/tag the control plane pins to and advances only after a gated
    -- run (the ansible-pull trust anchor). Empty for the path localities.
    ref               TEXT    NOT NULL DEFAULT '',
    -- The sealed-secret name holding the ed25519 deploy key; empty for a path
    -- source or a public repo. The key itself never lands here.
    deploy_key_secret TEXT    NOT NULL DEFAULT '',
    -- The commit the last successful sync landed on, so a run records exactly
    -- what tree it executed.
    last_sync_sha     TEXT    NOT NULL DEFAULT '',
    -- 1 requires a good signature on the pinned tag before a sync advances.
    signed            INTEGER NOT NULL DEFAULT 0,
    enabled           INTEGER NOT NULL DEFAULT 1,
    created_at        INTEGER NOT NULL
);

-- An inventory is what an ansible run targets: a static file inside a source, or
-- a dynamic-inventory plugin config whose auth env is bound here. env_binding_json
-- maps an env name the plugin reads (PROXMOX_PASSWORD, AWS_ACCESS_KEY_ID) to the
-- sealed-secret name it resolves from — the mapping only, never the value.
CREATE TABLE inventory (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    name             TEXT    NOT NULL UNIQUE,
    source_id        INTEGER REFERENCES source (id) ON DELETE CASCADE,
    -- The inventory path relative to the source checkout root.
    path             TEXT    NOT NULL DEFAULT '',
    kind             TEXT    NOT NULL CHECK (kind IN ('static', 'dynamic')),
    env_binding_json TEXT    NOT NULL DEFAULT '{}',
    created_at       INTEGER NOT NULL
);

-- The convergent credential-plane must-fix: a named sealed-secret set bound to a
-- run and exported as named env (provider env for tofu, plugin env for ansible
-- dynamic inventory, TF_VAR_). One row is one (env name -> sealed-secret name)
-- pair under a binding name; a workspace or a run references the binding by name.
--
-- The row holds no secret material — only the mapping. Values are resolved from
-- the envelope at execution time, exported into the job's own env allowlist for
-- that run alone, and never persisted, put in argv, or written to an event.
CREATE TABLE run_secret (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    binding     TEXT    NOT NULL,
    env_name    TEXT    NOT NULL,
    secret_name TEXT    NOT NULL,
    created_at  INTEGER NOT NULL
);

-- One env name resolves to one secret within a binding; naming it twice is an
-- operator error, not a last-write-wins race.
CREATE UNIQUE INDEX run_secret_binding_env ON run_secret (binding, env_name);
CREATE INDEX run_secret_binding ON run_secret (binding);
