-- The allow-list of binaries a governed CLI run (executor kind "cli") may
-- execute (v1.1 roadmap R1). Anything not registered here is refused before a
-- job row is written, which is the whole safety property of the kind: an
-- operation that is neither ansible nor tofu gets the queue, the sandbox, the
-- run-secret plane, and the approval gate instead of escaping through an
-- `ansible shell` wrapper, and it still cannot run an arbitrary program.
--
-- The database is the native home, as it is for sources, inventories, runners,
-- and plan policies: the allow-list is then covered by the one backup and
-- reachable by the audit machinery. A config tree would put the boundary that
-- decides what may execute outside the volume disaster recovery restores.

CREATE TABLE cli_tool (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    name            TEXT    NOT NULL UNIQUE,
    -- The executable. A bare name is resolved on PATH at run time; an absolute
    -- path pins the exact file. Never a shell string: it becomes argv[0] of a
    -- direct exec, and nothing here is ever handed to sh.
    binary          TEXT    NOT NULL,
    -- Arguments always prepended to the operator's own, so a registration can
    -- pin the dangerous part of an invocation (`--context staging`,
    -- `--namespace apps`) instead of trusting every caller to repeat it.
    fixed_args_json TEXT    NOT NULL DEFAULT '[]',
    -- An anchored regexp every operator-supplied argument must match. Empty
    -- keeps only the structural guard (no NUL, no control character): the
    -- pattern narrows a tool further, it is not what makes the run safe.
    arg_pattern     TEXT    NOT NULL DEFAULT '',
    -- The tool's declared network posture. Default-deny is the kind's baseline;
    -- 'https' adds port 443 to that one run's sandbox profile and nothing else.
    -- Landlock filters egress by port, not by destination, so this is the whole
    -- network policy a registration can express.
    egress          TEXT    NOT NULL DEFAULT 'none' CHECK (egress IN ('none', 'https')),
    description     TEXT    NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);
