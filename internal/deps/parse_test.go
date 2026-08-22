package deps

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestParsePyprojectDeps(t *testing.T) {
	body := []byte(`
[build-system]
requires = ["hatchling"]

[project]
name = "demo"
dependencies = [
  "jmespath==1.1.0",
  "netaddr>=0.8",  # networking
]

[dependency-groups]
dev = ["pytest", "ruff"]
docs = ["mkdocs"]

[tool.black]
line-length = 100
`)
	got, err := parsePyprojectDeps(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"jmespath==1.1.0", "netaddr>=0.8", "pytest", "ruff", "mkdocs"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("deps = %v, want %v", got, want)
	}
}

func TestParsePyprojectInlineArray(t *testing.T) {
	got, err := parsePyprojectDeps([]byte("[project]\ndependencies = [\"boto3\", \"botocore\"]\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"boto3", "botocore"}) {
		t.Errorf("deps = %v", got)
	}
}

// The build-system requires array must not leak into project deps: only
// [project].dependencies and [dependency-groups] are collected.
func TestParsePyprojectIgnoresBuildRequires(t *testing.T) {
	got, err := parsePyprojectDeps([]byte("[build-system]\nrequires = [\"hatchling\"]\n[project]\ndependencies = [\"jmespath\"]\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"jmespath"}) {
		t.Errorf("deps = %v, want [jmespath]", got)
	}
}

// A PEP 735 include-group entry references another group, not a package: it
// must never be collected as a specifier. The line-scanning reader this
// replaced would have picked up the "test" string as a bogus dependency.
func TestParsePyprojectSkipsIncludeGroup(t *testing.T) {
	body := []byte(`
[dependency-groups]
test = ["pytest"]
all = ["coverage", {include-group = "test"}]
`)
	got, err := parsePyprojectDeps(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"coverage", "pytest"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("deps = %v, want %v (include-group must not become a spec)", got, want)
	}
}

// optional-dependencies are extras, installed on demand, not by default: they
// stay out of the resolved set so a run does not silently pull an extra in.
func TestParsePyprojectIgnoresOptionalDeps(t *testing.T) {
	body := []byte(`
[project]
dependencies = ["core-pkg"]
[project.optional-dependencies]
aws = ["boto3"]
`)
	got, err := parsePyprojectDeps(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"core-pkg"}) {
		t.Errorf("deps = %v, want [core-pkg]", got)
	}
}

// A structurally broken pyproject fails loudly rather than resolving to fewer
// deps: a malformed manifest must stop the run, not silently under-install.
func TestParsePyprojectMalformedIsError(t *testing.T) {
	if _, err := parsePyprojectDeps([]byte("[project]\ndependencies = [\n  \"jmespath\",\n")); err == nil {
		t.Fatal("unterminated array parsed without error")
	}
	if _, err := parsePyprojectDeps([]byte("[project\nname = \"broken\"\n")); err == nil {
		t.Fatal("malformed table header parsed without error")
	}
}

// A dependency specifier that begins with a dash is a flag smuggled through
// [project].dependencies (finding S4): --index-url redirects the resolver's
// index, -e installs an editable path. Parsing must refuse it so it never
// reaches uv as a flag, rather than dropping it silently.
func TestParsePyprojectRejectsFlagSpecifier(t *testing.T) {
	for _, spec := range []string{
		"--index-url=http://evil.example/simple",
		"-i http://evil.example/simple",
		"-e /etc",
		"  --extra-index-url=http://evil.example",
	} {
		body := []byte("[project]\ndependencies = [\"jmespath\", \"" + spec + "\"]\n")
		if _, err := parsePyprojectDeps(body); err == nil {
			t.Errorf("spec %q parsed without error; a flag reached the resolver", spec)
		}
	}
}

// The whole-manifest path fails closed too: a pyproject carrying a flag
// specifier errors at discovery, so no venv is built and no flag is passed.
func TestDiscoverPythonRejectsFlagSpecifier(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "pyproject.toml"),
		"[project]\ndependencies = [\"--index-url=http://evil.example\"]\n")
	if _, err := DiscoverPython(root); err == nil {
		t.Fatal("DiscoverPython accepted a pyproject with a flag specifier")
	}
}

// A valid pyproject's specifiers are passed after a `--` separator, so even a
// specifier that somehow began with a dash would be a package path to uv, never
// a flag — defence in depth behind the parse-time refusal.
func TestInstallArgsPassesPyprojectSpecsPastSeparator(t *testing.T) {
	m := PythonManifest{Kind: KindPyproject, content: []byte(
		"[project]\ndependencies = [\"jmespath==1.1.0\", \"netaddr\"]\n")}
	args := installArgs(m)
	sep := -1
	for i, a := range args {
		if a == "--" {
			sep = i
			break
		}
	}
	if sep < 0 {
		t.Fatalf("installArgs did not separate specifiers with --: %v", args)
	}
	for _, a := range args[:sep] {
		if a == "jmespath==1.1.0" || a == "netaddr" {
			t.Errorf("specifier %q appears before the -- separator: %v", a, args)
		}
	}
}

func TestRequirementLines(t *testing.T) {
	body := []byte("# header\njmespath==1.1.0\n\n-r other.txt\n--hash=sha256:abc\nnetaddr  # inline\n")
	got := requirementLines(body)
	want := []string{"jmespath==1.1.0", "netaddr"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("lines = %v, want %v", got, want)
	}
}
