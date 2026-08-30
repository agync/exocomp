-- Netgroups name CIDR sets so a rule classifies an address with the
-- operator's own vocabulary ("home", "office") instead of a prefix it would
-- have to hardcode. Prefixes are comma-separated and validated in Go at write
-- time by netip.ParsePrefix, so a row that reads is a row that parses.
CREATE TABLE netgroup (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT    NOT NULL UNIQUE,
    prefixes   TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);