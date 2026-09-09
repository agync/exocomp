-- Per-workspace opt-in for resolving tofu providers from the local filesystem
-- mirror alone. Default 0: a workspace without the flag keeps the built-in
-- online provider resolution (registry when the plugin cache is cold). The
-- executor requires PluginCacheDir when offline is set, which is always present
-- for exocomp runs (tofuDirs). The value is a boolean stored as INTEGER.
ALTER TABLE workspace ADD COLUMN offline_providers INTEGER NOT NULL DEFAULT 0;