package executor

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
)

// TestAnsibleEnvAppendsCollectionsPath proves ansibleEnv adds
// CollectionsPath to whatever ANSIBLE_COLLECTIONS_PATH the operator's own
// environment already contributed, rather than overwriting it, and that an
// unset ANSIBLE_COLLECTIONS_PATH gets a plain new entry. This is a pure
// function of its arguments: no process environment is read here, so the
// result cannot depend on what happens to be set outside the test.
func TestAnsibleEnvAppendsCollectionsPath(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name            string
		parent          []string
		explicit        []string
		collectionsPath string
		want            string
		wantAbsent      bool
	}{
		{
			name:            "appends to a path from the parent environment",
			parent:          []string{"ANSIBLE_COLLECTIONS_PATH=/opt/existing/collections"},
			collectionsPath: "/data/cache/collections",
			want:            "ANSIBLE_COLLECTIONS_PATH=/opt/existing/collections:/data/cache/collections",
		},
		{
			name:            "appends to a path from an explicit override",
			explicit:        []string{"ANSIBLE_COLLECTIONS_PATH=/explicit/collections"},
			collectionsPath: "/data/cache/collections",
			want:            "ANSIBLE_COLLECTIONS_PATH=/explicit/collections:/data/cache/collections",
		},
		{
			name:            "adds a new entry when nothing set one",
			collectionsPath: "/data/cache/collections",
			want:            "ANSIBLE_COLLECTIONS_PATH=/data/cache/collections",
		},
		{
			name:       "leaves the environment untouched when empty",
			parent:     []string{"ANSIBLE_COLLECTIONS_PATH=/opt/existing/collections"},
			wantAbsent: false,
			want:       "ANSIBLE_COLLECTIONS_PATH=/opt/existing/collections",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ansibleEnv(tc.parent, tc.explicit, tc.collectionsPath)
			if !slices.Contains(got, tc.want) {
				t.Errorf("ansibleEnv(...) = %v, want an entry %q", got, tc.want)
			}
			count := 0
			for _, kv := range got {
				if len(kv) >= len(ansibleCollectionsPathVar) && kv[:len(ansibleCollectionsPathVar)] == ansibleCollectionsPathVar {
					count++
				}
			}
			if count != 1 {
				t.Errorf("ansibleEnv(...) has %d ANSIBLE_COLLECTIONS_PATH entries, want exactly 1: %v", count, got)
			}
		})
	}
}

// TestPrependPath puts the venv bin at the front of PATH so the venv's own
// executables win, and synthesizes a PATH when the run inherited none.
func TestPrependPath(t *testing.T) {
	t.Parallel()
	got := prependPath([]string{"PATH=/usr/bin:/bin", "HOME=/h"}, "/venv/bin")
	if !slices.Contains(got, "PATH=/venv/bin:/usr/bin:/bin") {
		t.Errorf("prependPath did not prepend: %v", got)
	}
	got = prependPath([]string{"HOME=/h"}, "/venv/bin")
	if !slices.Contains(got, "PATH=/venv/bin") {
		t.Errorf("prependPath did not synthesize PATH: %v", got)
	}
}

// TestWritePrivateForcesInterpreter proves the venv interpreter is written into
// the extra-vars file (the highest-precedence var source) for a localhost-only
// run and merged with, not substituted for, the caller's own extra-vars — while
// a non-local run drops the interpreter and keeps only the caller's vars, so the
// controller venv path never reaches a remote host.
func TestWritePrivateForcesInterpreter(t *testing.T) {
	const interp = "/data/cache/python/demo/abc/bin/python"
	readExtraVars := func(t *testing.T, args []string) map[string]string {
		t.Helper()
		var path string
		for i, arg := range args {
			if arg == "--extra-vars" && i+1 < len(args) {
				path = strings.TrimPrefix(args[i+1], "@")
			}
		}
		if path == "" {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read extra-vars: %v", err)
		}
		var vars map[string]string
		if err := json.Unmarshal(body, &vars); err != nil {
			t.Fatalf("decode extra-vars: %v", err)
		}
		return vars
	}

	newAnsible := func() *Ansible {
		return NewAnsible(AnsibleOptions{
			Interpreter: interp,
			ExtraVars:   map[string]string{"region": "eu"},
		})
	}

	t.Run("local run forces the interpreter", func(t *testing.T) {
		args, err := newAnsible().writePrivate(t.TempDir(), true, "")
		if err != nil {
			t.Fatalf("writePrivate: %v", err)
		}
		vars := readExtraVars(t, args)
		if vars["ansible_python_interpreter"] != interp {
			t.Errorf("interpreter not forced: %v", vars)
		}
		if vars["region"] != "eu" {
			t.Errorf("caller extra-vars lost: %v", vars)
		}
	})

	t.Run("non-local run drops the interpreter", func(t *testing.T) {
		args, err := newAnsible().writePrivate(t.TempDir(), false, "")
		if err != nil {
			t.Fatalf("writePrivate: %v", err)
		}
		vars := readExtraVars(t, args)
		if _, ok := vars["ansible_python_interpreter"]; ok {
			t.Errorf("interpreter forced on a non-local run: %v", vars)
		}
		if vars["region"] != "eu" {
			t.Errorf("caller extra-vars lost: %v", vars)
		}
	})
}
