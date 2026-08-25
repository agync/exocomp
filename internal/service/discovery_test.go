package service_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"exocomp.dev/exocomp/internal/service"
)

const playbookYAML = `---
- hosts: all
  tasks:
    - name: ping
      ping:
`

const varsYAML = `---
foo: bar
nested:
  baz: qux
`

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestListSourcePlaybooksFindsTopLevelAndNested(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "site.yml"), playbookYAML)
	writeFile(t, filepath.Join(root, "deploy", "app.yaml"), playbookYAML)
	writeFile(t, filepath.Join(root, "deploy", "nested", "sub.yml"), playbookYAML)

	got, err := service.ListSourcePlaybooks(root)
	if err != nil {
		t.Fatalf("ListSourcePlaybooks: %v", err)
	}
	want := []string{"deploy/app.yaml", "deploy/nested/sub.yml", "site.yml"}
	assertEqual(t, got, want)
}

func TestListSourcePlaybooksExcludesNonPlaybookContent(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "site.yml"), playbookYAML)
	writeFile(t, filepath.Join(root, "vars.yml"), varsYAML)
	writeFile(t, filepath.Join(root, "requirements.yml"), "- name: geerlingguy.docker\n")
	writeFile(t, filepath.Join(root, "galaxy.yml"), "- foo\n")
	writeFile(t, filepath.Join(root, "empty.yml"), "")
	writeFile(t, filepath.Join(root, "secret.yml"), "$ANSIBLE_VAULT;1.1;AES256\nsomeciphertext\n")
	writeFile(t, filepath.Join(root, "notes.txt"), "- this is not yaml at all as far as we care\n")

	got, err := service.ListSourcePlaybooks(root)
	if err != nil {
		t.Fatalf("ListSourcePlaybooks: %v", err)
	}
	assertEqual(t, got, []string{"site.yml"})
}

func TestListSourcePlaybooksExcludesRolesGroupVarsAndDotDirs(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "site.yml"), playbookYAML)
	// All of these are syntactically list-shaped YAML (so they'd pass the
	// content sniff) but live under directories that are never playbooks.
	writeFile(t, filepath.Join(root, "roles", "web", "tasks", "main.yml"), playbookYAML)
	writeFile(t, filepath.Join(root, "group_vars", "all.yml"), playbookYAML)
	writeFile(t, filepath.Join(root, "host_vars", "host1.yml"), playbookYAML)
	writeFile(t, filepath.Join(root, "molecule", "default", "molecule.yml"), playbookYAML)
	writeFile(t, filepath.Join(root, ".git", "hooks", "pre-commit.yml"), playbookYAML)
	writeFile(t, filepath.Join(root, ".exocomp", "hooks.yml"), playbookYAML)
	writeFile(t, filepath.Join(root, "tasks", "main.yml"), playbookYAML)
	writeFile(t, filepath.Join(root, "defaults", "main.yml"), playbookYAML)

	got, err := service.ListSourcePlaybooks(root)
	if err != nil {
		t.Fatalf("ListSourcePlaybooks: %v", err)
	}
	assertEqual(t, got, []string{"site.yml"})
}

func TestListSourcePlaybooksRespectsCap(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < service.MaxSourcePlaybooks+50; i++ {
		writeFile(t, filepath.Join(root, fmt.Sprintf("play%05d.yml", i)), playbookYAML)
	}

	got, err := service.ListSourcePlaybooks(root)
	if err != nil {
		t.Fatalf("ListSourcePlaybooks: %v", err)
	}
	if len(got) != service.MaxSourcePlaybooks {
		t.Fatalf("len(got) = %d, want %d", len(got), service.MaxSourcePlaybooks)
	}
}

func TestListSourcePlaybooksMissingRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "does-not-exist")

	got, err := service.ListSourcePlaybooks(root)
	if err != nil {
		t.Fatalf("ListSourcePlaybooks: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %v, want empty", got)
	}
}

func TestListSourcePlaybooksEmptyDirectory(t *testing.T) {
	root := t.TempDir()

	got, err := service.ListSourcePlaybooks(root)
	if err != nil {
		t.Fatalf("ListSourcePlaybooks: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %v, want empty", got)
	}
}

func TestListSourcePlaybooksEmptyRootPath(t *testing.T) {
	if _, err := service.ListSourcePlaybooks(""); err == nil {
		t.Fatal("ListSourcePlaybooks(\"\"): want error, got nil")
	}
	if _, err := service.ListSourcePlaybooks("   "); err == nil {
		t.Fatal("ListSourcePlaybooks(\"   \"): want error, got nil")
	}
}

func TestListSourcePlaybooksRootIsAFile(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "not-a-dir")
	writeFile(t, file, playbookYAML)

	if _, err := service.ListSourcePlaybooks(file); err == nil {
		t.Fatal("ListSourcePlaybooks(file): want error, got nil")
	}
}

func assertEqual(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
