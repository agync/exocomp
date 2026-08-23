-- Registered runner definitions (byo-runner design §2). A runner is a *recipe*
-- for an execution-environment image: a base, a bootstrap script or a
-- Containerfile path, a package list, and the resource caps a run would ask
-- for. The container tier that builds and runs those images is deferred, so a
-- row here is inert data — registered, listed, hashed, and backed up with the
-- data volume, never executed.
--
-- The DB is the native home (design §2 "Registration"): the same place sources,
-- inventories, schedules, and plan-policies live, so a runner is covered by the
-- one backup and reachable by the audit machinery, rather than split into a
-- /etc or ~/.config tree the volume does not carry.

CREATE TABLE runner (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    name           TEXT    NOT NULL UNIQUE,
    -- cached builds the image once and reuses it until the recipe changes;
    -- onthefly rebuilds every run against the layer cache. Both feed the same
    -- content_hash, so switching mode never invalidates a built image.
    mode           TEXT    NOT NULL CHECK (mode IN ('cached', 'onthefly')),
    -- The base image reference. Stored as the operator wrote it; the build tier
    -- is what would resolve a tag to @sha256 (design §7.2).
    base           TEXT    NOT NULL,
    -- Model A: the script body run as one layer. Not a secret (invariant 4
    -- material never lands in a recipe), but long, so the projections carry its
    -- digest rather than its body.
    bootstrap      TEXT    NOT NULL DEFAULT '',
    -- Model B: a path to a Containerfile, relative to the source checkout.
    containerfile  TEXT    NOT NULL DEFAULT '',
    -- Sorted, deduplicated JSON array — canonical so the content hash is stable
    -- against the order the operator happened to type.
    packages_json  TEXT    NOT NULL DEFAULT '[]',
    ansible_core   TEXT    NOT NULL DEFAULT '',
    -- Caps the deferred tier would enforce (design §6). Free-form systemd
    -- syntax ('2G', '200%'); this tier stores them, nothing parses them yet.
    memory_max     TEXT    NOT NULL DEFAULT '',
    cpu_quota      TEXT    NOT NULL DEFAULT '',
    -- The design §7.1 runner-hash over the definition, recomputed on every
    -- write so a row can never carry a hash its own columns disagree with.
    content_hash   TEXT    NOT NULL,
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL,
    -- The two build models are alternatives, not a merge: a recipe naming both
    -- a bootstrap and a Containerfile has no defined build.
    CHECK (bootstrap = '' OR containerfile = '')
);
