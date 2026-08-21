package executor

import (
	"slices"
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
