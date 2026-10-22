package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// The files a pin is recorded in, relative to the repository root.
const (
	containerfilePath = "packaging/Containerfile"
	digestsPath       = "packaging/tool-digests.txt"
	manifestPath      = "packaging/MANIFEST.json"
	bomPath           = "docs/sbom/bill-of-materials.md"
)

// arches are the architectures the image is published for (publish.yml).
var arches = []string{"amd64", "arm64"}

// source is what the Containerfile cannot say about a tool: which release
// asset lists its sha256, and the bill of materials row that records its
// version. An empty checksums means the release publishes no checksum file and
// the digest GitHub records for each asset is the source.
type source struct {
	checksums string
	bomRow    string
}

var sources = map[string]source{
	"tofu":       {checksums: "tofu_{version}_SHA256SUMS", bomRow: "OpenTofu"},
	"restic":     {checksums: "SHA256SUMS", bomRow: "restic"},
	"age":        {checksums: "", bomRow: "age (CLI)"},
	"sops":       {checksums: "sops-v{version}.checksums.txt", bomRow: "sops"},
	"nono":       {checksums: "SHA256SUMS.txt", bomRow: "nono"},
	"litestream": {checksums: "checksums.txt", bomRow: "Litestream *(optional sidecar)*"},
}

// pin is one tool the Containerfile fetches from a GitHub release.
type pin struct {
	tool    string
	arg     string // the ARG holding the version, e.g. TOFU_VERSION
	version string
	owner   string
	repo    string
	tag     string // tag template, e.g. v${TOFU_VERSION}
	asset   string // asset template, e.g. tofu_${TOFU_VERSION}_linux_${TARGETARCH}.zip
	archMap map[string]map[string]string
	src     source
}

var (
	argRe   = regexp.MustCompile(`(?m)^ARG ([A-Z0-9_]+_VERSION)=(\S+)$`)
	fetchRe = regexp.MustCompile(`fetch ([a-z0-9-]+) "https://github\.com/([^/"]+)/([^/"]+)/releases/download/([^/"]+)/([^/"]+)"`)
	caseRe  = regexp.MustCompile(`case "\$TARGETARCH" in amd64\) ([a-z_]+)=(\S+) ;; arm64\) ([a-z_]+)=(\S+) ;;`)
	semver  = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)$`)
)

// parsePins reads every checked download out of the Containerfile and pairs
// it with its source in srcs. A fetched tool with no source, or a source for a
// tool the Containerfile does not fetch, is an error.
func parsePins(containerfile string, srcs map[string]source) ([]pin, error) {
	args := map[string]string{}
	for _, m := range argRe.FindAllStringSubmatch(containerfile, -1) {
		if _, dup := args[m[1]]; dup {
			return nil, fmt.Errorf("the Containerfile declares ARG %s twice", m[1])
		}
		args[m[1]] = m[2]
	}
	archMap := map[string]map[string]string{}
	for _, m := range caseRe.FindAllStringSubmatch(containerfile, -1) {
		if m[1] != m[3] {
			return nil, fmt.Errorf("the Containerfile arch case sets %s and %s", m[1], m[3])
		}
		archMap[m[1]] = map[string]string{"amd64": m[2], "arm64": m[4]}
	}

	var pins []pin
	for _, m := range fetchRe.FindAllStringSubmatch(containerfile, -1) {
		src, ok := srcs[m[1]]
		if !ok {
			return nil, fmt.Errorf("the Containerfile fetches %s, which has no digest source in pinbump", m[1])
		}
		p := pin{tool: m[1], owner: m[2], repo: m[3], tag: m[4], asset: m[5], archMap: archMap, src: src}
		p.arg = strings.ToUpper(strings.ReplaceAll(p.tool, "-", "_")) + "_VERSION"
		v, ok := args[p.arg]
		if !ok {
			return nil, fmt.Errorf("the Containerfile fetches %s but declares no ARG %s", p.tool, p.arg)
		}
		if !semver.MatchString(v) {
			return nil, fmt.Errorf("ARG %s=%s is not a plain x.y.z version", p.arg, v)
		}
		p.version = v
		if slices.ContainsFunc(pins, func(q pin) bool { return q.tool == p.tool }) {
			return nil, fmt.Errorf("the Containerfile fetches %s twice", p.tool)
		}
		for _, arch := range arches {
			for _, tmpl := range []string{p.tag, p.asset} {
				if _, err := p.render(tmpl, v, arch); err != nil {
					return nil, err
				}
			}
		}
		pins = append(pins, p)
	}
	if len(pins) == 0 {
		return nil, fmt.Errorf("the Containerfile has no checked downloads")
	}
	var unused []string
	for tool := range srcs {
		if !slices.ContainsFunc(pins, func(p pin) bool { return p.tool == tool }) {
			unused = append(unused, tool)
		}
	}
	if len(unused) > 0 {
		sort.Strings(unused)
		return nil, fmt.Errorf("pinbump has a digest source for %s, which the Containerfile does not fetch",
			strings.Join(unused, ", "))
	}
	return pins, nil
}

// render expands a tag or asset template for one version and architecture.
func (p pin) render(tmpl, version, arch string) (string, error) {
	s := strings.ReplaceAll(tmpl, "${"+p.arg+"}", version)
	s = strings.ReplaceAll(s, "${TARGETARCH}", arch)
	for name, byArch := range p.archMap {
		s = strings.ReplaceAll(s, "${"+name+"}", byArch[arch])
	}
	if strings.Contains(s, "$") {
		return "", fmt.Errorf("%s: cannot expand %q", p.tool, tmpl)
	}
	return s, nil
}

// versionFromTag returns the version a release tag carries when the tag has
// the pin's tag shape and a plain x.y.z version, so a prerelease or an
// unrelated tag never becomes a candidate.
func (p pin) versionFromTag(tag string) (string, bool) {
	prefix, suffix, ok := strings.Cut(p.tag, "${"+p.arg+"}")
	if !ok || !strings.HasPrefix(tag, prefix) || !strings.HasSuffix(tag, suffix) {
		return "", false
	}
	v := strings.TrimSuffix(strings.TrimPrefix(tag, prefix), suffix)
	return v, semver.MatchString(v)
}

// compareVersions orders two x.y.z versions.
func compareVersions(a, b string) int {
	pa, pb := semver.FindStringSubmatch(a), semver.FindStringSubmatch(b)
	for i := 1; i <= 3; i++ {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// repoFiles holds the contents of every file a pin is recorded in.
type repoFiles map[string]string

// checkConsistency fails when the files disagree about any pin: a
// MANIFEST.json field, a digest line, or a bill of materials row out of step
// with the Containerfile.
func checkConsistency(files repoFiles, pins []pin) error {
	var manifest map[string]string
	if err := json.Unmarshal([]byte(files[manifestPath]), &manifest); err != nil {
		return fmt.Errorf("%s: %w", manifestPath, err)
	}
	var problems []string
	for _, p := range pins {
		if got := manifest[p.tool]; got != p.version {
			problems = append(problems, fmt.Sprintf("%s %q = %q, Containerfile %s = %q",
				manifestPath, p.tool, got, p.arg, p.version))
		}
		for _, arch := range arches {
			if n := len(digestLines(files[digestsPath], p.tool, arch)); n != 1 {
				problems = append(problems, fmt.Sprintf("%s has %d lines for %s %s, want 1",
					digestsPath, n, p.tool, arch))
			}
		}
		if n := len(bomLines(files[bomPath], p.src.bomRow, p.version)); n != 1 {
			problems = append(problems, fmt.Sprintf("%s has %d %q rows at %s, want 1",
				bomPath, n, p.src.bomRow, p.version))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("pins disagree:\n  %s", strings.Join(problems, "\n  "))
	}
	return nil
}

// digestLines returns the indexes of the lines pinning tool on arch.
func digestLines(body, tool, arch string) []int {
	var idx []int
	for i, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, tool+" "+arch+" ") {
			idx = append(idx, i)
		}
	}
	return idx
}

// bomLines returns the indexes of the table rows for label at version.
func bomLines(body, label, version string) []int {
	var idx []int
	for i, line := range strings.Split(body, "\n") {
		rest, ok := strings.CutPrefix(line, bomRowPrefix(label, version))
		if ok && (rest == "" || rest[0] == ' ' || rest[0] == ',') {
			idx = append(idx, i)
		}
	}
	return idx
}

func bomRowPrefix(label, version string) string {
	return "| " + label + " | " + version
}

// apply rewrites every file that records p's version to b. Each rewrite
// replaces whole lines in place, so the result has the same line count.
func apply(files repoFiles, p pin, b bump) (repoFiles, error) {
	out := repoFiles{}
	for k, v := range files {
		out[k] = v
	}
	edit := func(path string, idx []int, want int, fn func(string) string) error {
		if len(idx) != want {
			return fmt.Errorf("%s: %d lines to rewrite for %s, want %d", path, len(idx), p.tool, want)
		}
		lines := strings.Split(out[path], "\n")
		for _, i := range idx {
			lines[i] = fn(lines[i])
		}
		out[path] = strings.Join(lines, "\n")
		return nil
	}

	argLine := "ARG " + p.arg + "=" + p.version
	if err := edit(containerfilePath, exactLines(out[containerfilePath], argLine), 1, func(string) string {
		return "ARG " + p.arg + "=" + b.To
	}); err != nil {
		return nil, err
	}
	for _, arch := range arches {
		digest := b.digests[arch]
		if err := edit(digestsPath, digestLines(out[digestsPath], p.tool, arch), 1, func(string) string {
			return p.tool + " " + arch + " " + digest
		}); err != nil {
			return nil, err
		}
	}
	field := `"` + p.tool + `": "` + p.version + `"`
	if err := edit(manifestPath, fieldLines(out[manifestPath], field), 1, func(line string) string {
		return strings.Replace(line, field, `"`+p.tool+`": "`+b.To+`"`, 1)
	}); err != nil {
		return nil, err
	}
	label := p.src.bomRow
	if err := edit(bomPath, bomLines(out[bomPath], label, p.version), 1, func(line string) string {
		return bomRowPrefix(label, b.To) + strings.TrimPrefix(line, bomRowPrefix(label, p.version))
	}); err != nil {
		return nil, err
	}
	return out, nil
}

func exactLines(body, want string) []int {
	var idx []int
	for i, line := range strings.Split(body, "\n") {
		if line == want {
			idx = append(idx, i)
		}
	}
	return idx
}

// fieldLines finds a MANIFEST.json line holding exactly one field.
func fieldLines(body, field string) []int {
	var idx []int
	for i, line := range strings.Split(body, "\n") {
		if t := strings.TrimSuffix(strings.TrimSpace(line), ","); t == field {
			idx = append(idx, i)
		}
	}
	return idx
}

// diff renders the changed lines of two same-length texts as a unified diff
// with no context, which is exact for line-for-line rewrites.
func diff(path, before, after string) string {
	if before == after {
		return ""
	}
	a, b := strings.Split(before, "\n"), strings.Split(after, "\n")
	var sb strings.Builder
	fmt.Fprintf(&sb, "--- a/%s\n+++ b/%s\n", path, path)
	for i := range a {
		if a[i] != b[i] {
			fmt.Fprintf(&sb, "@@ -%d +%d @@\n-%s\n+%s\n", i+1, i+1, a[i], b[i])
		}
	}
	return sb.String()
}
