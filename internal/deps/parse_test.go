package deps

import (
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

func TestRequirementLines(t *testing.T) {
	body := []byte("# header\njmespath==1.1.0\n\n-r other.txt\n--hash=sha256:abc\nnetaddr  # inline\n")
	got := requirementLines(body)
	want := []string{"jmespath==1.1.0", "netaddr"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("lines = %v, want %v", got, want)
	}
}
