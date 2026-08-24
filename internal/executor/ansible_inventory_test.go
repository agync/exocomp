package executor

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestWritePrivateCopiesInventoryArtifact proves the consumer half of the
// handoff: a materialized inventory is copied into the run's own private
// directory and the run is pointed at that copy, never the source artifact.
func TestWritePrivateCopiesInventoryArtifact(t *testing.T) {
	src := filepath.Join(t.TempDir(), "materialized.json")
	body := []byte(`{"all":{"hosts":{"webnode":{}}}}`)
	if err := os.WriteFile(src, body, 0o600); err != nil {
		t.Fatalf("write source inventory: %v", err)
	}

	private := t.TempDir()
	a := NewAnsible(AnsibleOptions{InventoryArtifact: src})
	args, err := a.writePrivate(private, false, "")
	if err != nil {
		t.Fatalf("writePrivate: %v", err)
	}

	i := slices.Index(args, "--inventory")
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("no --inventory in run args: %v", args)
	}
	copyPath := args[i+1]
	if filepath.Dir(copyPath) != private {
		t.Errorf("inventory copy %s is not in the private dir %s", copyPath, private)
	}
	if copyPath == src {
		t.Error("the run points at the source artifact, not a private copy")
	}
	got, err := os.ReadFile(copyPath)
	if err != nil {
		t.Fatalf("read copy: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("copy = %q, want %q", got, body)
	}
	if info, err := os.Stat(copyPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("copy mode = %v (err %v), want 0600", info.Mode().Perm(), err)
	}
}

// TestWritePrivateNoInventoryArtifact is the mutation check: with no artifact
// set, the driver adds no inventory of its own, so an existing --inventory in
// the argv is the only one and the test above cannot pass vacuously.
func TestWritePrivateNoInventoryArtifact(t *testing.T) {
	a := NewAnsible(AnsibleOptions{})
	args, err := a.writePrivate(t.TempDir(), false, "")
	if err != nil {
		t.Fatalf("writePrivate: %v", err)
	}
	if slices.Contains(args, "--inventory") {
		t.Errorf("driver added --inventory with no artifact set: %v", args)
	}
}

// TestArtifactArgvHasNoInventory proves the handoff argv carries no inventory of
// its own — the property that routes it through the private-copy path.
func TestArtifactArgvHasNoInventory(t *testing.T) {
	argv := AnsibleArtifactArgv("/repo/site.yml", "--check")
	if slices.Contains(argv, "--inventory") || slices.Contains(argv, "-i") {
		t.Errorf("AnsibleArtifactArgv injected an inventory: %v", argv)
	}
	if argv[len(argv)-1] != "/repo/site.yml" {
		t.Errorf("playbook is not last: %v", argv)
	}
}
