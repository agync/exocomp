package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// isolatedEnv marks a process whose environment TestMain already isolated. A
// helper process a test re-executes inherits it, and keeps the environment its
// parent test set rather than being isolated again.
const isolatedEnv = "EXOCOMP_TEST_ISOLATED"

// TestMain gives the whole package, and every binary it spawns, an empty client
// config and data dir: a command without --local reads hosts.toml, so a test
// that forgot to isolate it would otherwise follow the operator's real default
// remote, or write the operator's real data dir.
func TestMain(m *testing.M) {
	if os.Getenv(isolatedEnv) != "" {
		os.Exit(m.Run())
	}
	dir, err := os.MkdirTemp("", "exocomp-test-env-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for k, v := range map[string]string{
		"XDG_CONFIG_HOME": filepath.Join(dir, "config"),
		"XDG_DATA_HOME":   filepath.Join(dir, "data"),
		"XDG_STATE_HOME":  filepath.Join(dir, "state"),
		remoteEnv:         "",
		tokenEnv:          "",
		isolatedEnv:       "1",
	} {
		if err := os.Setenv(k, v); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
