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

[dependency-groups.docs]
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

func TestParsePyprojectUnterminatedIsError(t *testing.T) {
	_, err := parsePyprojectDeps([]byte("[project]\ndependencies = [\n  \"jmespath\",\n"))
	if err == nil {
		t.Fatal("unterminated array parsed without error")
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
