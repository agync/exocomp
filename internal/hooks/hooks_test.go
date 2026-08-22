package hooks

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseValid(t *testing.T) {
	data := []byte(`
pre_run:
  - name: prep
    run: "test -f ready"
  - name: seed
    script: scripts/seed.sh
    secret: proxmox
post_run:
  - name: notify
    run: "echo done"
gate:
  - name: web-up
    http:
      url: http://127.0.0.1:8080/health
      expect_status: 200
  - name: cert
    tls:
      address: example.test:443
      min_days: 14
`)
	h, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.PreRun) != 2 || len(h.PostRun) != 1 || len(h.Gate) != 2 {
		t.Fatalf("unexpected shape: %+v", h)
	}
	if h.PreRun[1].Secret != "proxmox" || h.PreRun[1].Script != "scripts/seed.sh" {
		t.Fatalf("secret/script not parsed: %+v", h.PreRun[1])
	}
	if h.Empty() {
		t.Fatal("a populated hooks.yml reports Empty")
	}
}

func TestParseEmptyIsNoHooks(t *testing.T) {
	for _, in := range []string{"", "\n", "# just a comment\n"} {
		h, err := Parse([]byte(in))
		if err != nil {
			t.Fatalf("empty doc %q should parse: %v", in, err)
		}
		if !h.Empty() {
			t.Fatalf("empty doc %q should be Empty", in)
		}
	}
}

func TestParseRefusesMalformed(t *testing.T) {
	cases := map[string]string{
		"unknown-key": `
pre_run:
  - name: x
    run: "true"
    typo: nope
`,
		"unknown-top-key": `
pre_runn:
  - name: x
    run: "true"
`,
		"neither-run-nor-script": `
pre_run:
  - name: x
`,
		"both-run-and-script": `
pre_run:
  - name: x
    run: "true"
    script: s.sh
`,
		"no-name": `
post_run:
  - run: "true"
`,
		"dup-name": `
pre_run:
  - name: x
    run: "true"
  - name: x
    run: "false"
`,
		"absolute-script": `
pre_run:
  - name: x
    script: /etc/passwd
`,
		"script-escapes": `
pre_run:
  - name: x
    script: ../../etc/passwd
`,
		"gate-no-check": `
gate:
  - name: x
`,
		"gate-bad-url": `
gate:
  - name: x
    http:
      url: "ftp://nope/"
`,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(in)); err == nil {
				t.Fatalf("expected %s to be refused", name)
			}
		})
	}

	// Companion: a well-formed file with all the shapes the cases mangle is
	// accepted, so the refusals above are the semantics, not a parser that
	// rejects everything.
	ok := `
pre_run:
  - name: x
    run: "true"
post_run:
  - name: y
    script: ok.sh
gate:
  - name: z
    http:
      url: "http://host/"
`
	if _, err := Parse([]byte(ok)); err != nil {
		t.Fatalf("expected the valid file to parse, got %v", err)
	}
}

func TestResolveOverrideWins(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, RepoRelPath)
	if err := os.MkdirAll(filepath.Dir(repoDir), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, repoDir, "pre_run:\n  - name: repo\n    run: \"true\"\n")

	override := filepath.Join(root, "override.yml")
	writeFile(t, override, "pre_run:\n  - name: op\n    run: \"true\"\n")

	// Override present -> it wins.
	h, err := Resolve(root, override)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.PreRun) != 1 || h.PreRun[0].Name != "op" {
		t.Fatalf("override did not win: %+v", h)
	}

	// Override absent -> the repo-side file is used (companion: proves the
	// override path is what changed the answer, not that repo is never read).
	h, err = Resolve(root, filepath.Join(root, "missing.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(h.PreRun) != 1 || h.PreRun[0].Name != "repo" {
		t.Fatalf("repo-side hooks not used: %+v", h)
	}
}

func TestResolveNoFileIsEmpty(t *testing.T) {
	h, err := Resolve(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !h.Empty() {
		t.Fatalf("no hooks.yml should resolve to Empty, got %+v", h)
	}
}

func TestResolveMalformedRepoFileErrors(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, RepoRelPath)
	if err := os.MkdirAll(filepath.Dir(repoDir), 0o700); err != nil {
		t.Fatal(err)
	}
	// A present-but-broken repo file must fail loudly, never be skipped.
	writeFile(t, repoDir, "pre_run:\n  - name: x\n")
	if _, err := Resolve(root, ""); err == nil {
		t.Fatal("expected a malformed repo hooks.yml to error, not be skipped")
	}
}

func TestResolveWithSourceProvenance(t *testing.T) {
	root := t.TempDir()
	repoDir := filepath.Join(root, RepoRelPath)
	if err := os.MkdirAll(filepath.Dir(repoDir), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, repoDir, "pre_run:\n  - name: repo\n    run: \"true\"\n")

	_, prov, err := ResolveWithSource(root, filepath.Join(root, "missing.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if prov.Path != repoDir || prov.IsOverride {
		t.Fatalf("expected repo provenance, got %+v", prov)
	}

	override := filepath.Join(root, "override.yml")
	writeFile(t, override, "pre_run:\n  - name: op\n    run: \"true\"\n")
	_, prov, err = ResolveWithSource(root, override)
	if err != nil {
		t.Fatal(err)
	}
	if prov.Path != override || !prov.IsOverride {
		t.Fatalf("expected override provenance, got %+v", prov)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
