package main

import (
	"bytes"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureSources are the digest sources for the fixture repository, which
// fetches three of the image's tools: one with a checksum file GitHub also
// records digests for (tofu), one with GitHub's digests only (age), and one
// with a checksum file only and an arch name map (nono).
var fixtureSources = map[string]source{
	"tofu": sources["tofu"],
	"age":  sources["age"],
	"nono": sources["nono"],
}

// fakeGitHub serves the release lists and checksum files under testdata/github
// over TLS. overrides replaces a served path's body. An empty one is a 404,
// and one starting "redirect " redirects to the URL after it.
func fakeGitHub(t *testing.T, overrides map[string]string) *client {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if body, ok := overrides[path]; ok {
			if body == "" {
				http.NotFound(w, r)
				return
			}
			if to, ok := strings.CutPrefix(body, "redirect "); ok {
				http.Redirect(w, r, to, http.StatusFound)
				return
			}
			_, _ = w.Write([]byte(strings.ReplaceAll(body, "{{server}}", srv.URL)))
			return
		}
		var file string
		switch {
		case strings.HasPrefix(path, "/repos/") && strings.HasSuffix(path, "/releases"):
			parts := strings.Split(strings.TrimPrefix(path, "/repos/"), "/")
			file = parts[0] + "_" + parts[1] + ".json"
		case strings.HasPrefix(path, "/dl/"):
			file = strings.TrimPrefix(path, "/dl/")
		}
		body, err := os.ReadFile(filepath.Join("testdata", "github", filepath.Base(file)))
		if file == "" || err != nil {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(strings.ReplaceAll(string(body), "{{server}}", srv.URL)))
	}))
	t.Cleanup(srv.Close)
	return newClient(srv.URL, "", srv.Client())
}

// fixtureRepo copies testdata/repo into a temporary root.
func fixtureRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, path := range []string{containerfilePath, digestsPath, manifestPath, bomPath} {
		body, err := os.ReadFile(filepath.Join("testdata", "repo", path))
		if err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, path := range []string{containerfilePath, digestsPath, manifestPath, bomPath} {
		body, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			t.Fatal(err)
		}
		out[path] = string(body)
	}
	return out
}

func runPinbump(t *testing.T, c *client, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), args, c, fixtureSources, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestParsePinsReadsTheCheckedDownloads(t *testing.T) {
	body, err := os.ReadFile("testdata/repo/packaging/Containerfile")
	if err != nil {
		t.Fatal(err)
	}
	pins, err := parsePins(string(body), fixtureSources)
	if err != nil {
		t.Fatal(err)
	}
	if len(pins) != 3 {
		t.Fatalf("got %d pins, want 3", len(pins))
	}
	nono := pins[2]
	if nono.tool != "nono" || nono.arg != "NONO_VERSION" || nono.version != "0.74.0" ||
		nono.owner != "nolabs-ai" || nono.repo != "nono" {
		t.Errorf("nono pin = %+v", nono)
	}
	got, err := nono.render(nono.asset, "1.2.3", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if want := "nono-v1.2.3-aarch64-unknown-linux-gnu.tar.gz"; got != want {
		t.Errorf("nono arm64 asset = %q, want %q", got, want)
	}
}

func TestParsePinsWantsASourceForEveryFetchedTool(t *testing.T) {
	body, err := os.ReadFile("testdata/repo/packaging/Containerfile")
	if err != nil {
		t.Fatal(err)
	}
	missing := map[string]source{"tofu": sources["tofu"], "age": sources["age"]}
	if _, err := parsePins(string(body), missing); err == nil || !strings.Contains(err.Error(), "nono") {
		t.Errorf("a fetched tool with no source parsed, err = %v", err)
	}
	extra := map[string]source{"restic": sources["restic"]}
	for k, v := range fixtureSources {
		extra[k] = v
	}
	if _, err := parsePins(string(body), extra); err == nil || !strings.Contains(err.Error(), "restic") {
		t.Errorf("a source for a tool nothing fetches parsed, err = %v", err)
	}
}

func TestVersionFromTagTakesOnlyPlainVersions(t *testing.T) {
	p := pin{arg: "SOPS_VERSION", tag: "v${SOPS_VERSION}"}
	for tag, want := range map[string]string{
		"v3.13.3":     "3.13.3",
		"v3.14.0-rc1": "",
		"3.13.3":      "",
		"v3.13":       "",
		"sops-v3.0.0": "",
	} {
		got, ok := p.versionFromTag(tag)
		if ok != (want != "") || (ok && got != want) {
			t.Errorf("versionFromTag(%q) = %q, %v, want %q", tag, got, ok, want)
		}
	}
}

func TestCompareVersionsIsNumeric(t *testing.T) {
	if compareVersions("0.80.0", "0.9.0") <= 0 {
		t.Error("0.80.0 sorted below 0.9.0")
	}
	if compareVersions("1.13.0", "1.13.0") != 0 {
		t.Error("equal versions compare unequal")
	}
}

// TestLatestIsTheHighestStableVersion: the list carries a draft, a
// prerelease, a suffixed tag, and a 1.12 patch published after 1.13.0. Only
// 1.13.0 qualifies as the newest.
func TestLatestIsTheHighestStableVersion(t *testing.T) {
	c := fakeGitHub(t, nil)
	p := pin{tool: "tofu", arg: "TOFU_VERSION", owner: "opentofu", repo: "opentofu", tag: "v${TOFU_VERSION}"}
	_, v, err := c.latest(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	if v != "1.13.0" {
		t.Errorf("latest = %s, want 1.13.0", v)
	}
}

func TestDryRunPrintsTheDiffAndWritesNothing(t *testing.T) {
	root := fixtureRepo(t)
	before := snapshot(t, root)

	code, out, errOut := runPinbump(t, fakeGitHub(t, nil), "-root", root, "-dry-run")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range []string{
		"--- a/packaging/Containerfile",
		"-ARG TOFU_VERSION=1.11.5\n+ARG TOFU_VERSION=1.13.0",
		"+tofu arm64 bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"+age amd64 dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		"+nono arm64 9999999999999999999999999999999999999999999999999999999999999999",
		`+  "nono": "0.80.0",`,
		"+| nono | 0.80.0, floor ≥ 0.55.0 (sha256-pinned) | Apache-2.0 |",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run diff lacks %q:\n%s", want, out)
		}
	}
	if after := snapshot(t, root); !maps.Equal(before, after) {
		t.Error("dry-run changed files")
	}
}

func TestApplyRewritesEveryRecordAndIsIdempotent(t *testing.T) {
	root := fixtureRepo(t)
	c := fakeGitHub(t, nil)
	body := filepath.Join(t.TempDir(), "body.md")

	if code, _, errOut := runPinbump(t, c, "-root", root, "-tool", "tofu", "-body", body); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	got := snapshot(t, root)
	for path, want := range map[string]string{
		containerfilePath: "ARG TOFU_VERSION=1.13.0\nARG AGE_VERSION=1.3.1\n",
		digestsPath:       "tofu amd64 aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n",
		manifestPath:      `"tofu": "1.13.0",`,
		bomPath:           "| OpenTofu | 1.13.0 (sha256-pinned) | MPL-2.0 |\n| age (CLI) | 1.3.1 ",
	} {
		if !strings.Contains(got[path], want) {
			t.Errorf("%s lacks %q:\n%s", path, want, got[path])
		}
	}
	var manifest map[string]string
	if err := json.Unmarshal([]byte(got[manifestPath]), &manifest); err != nil {
		t.Errorf("rewritten MANIFEST.json is not JSON: %v", err)
	}

	pr, err := os.ReadFile(body)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"from 1.11.5 to 1.13.0",
		"https://github.com/opentofu/opentofu/releases/tag/v1.13.0",
		"`tofu_1.13.0_SHA256SUMS` and match the digests GitHub records",
	} {
		if !strings.Contains(string(pr), want) {
			t.Errorf("PR body lacks %q:\n%s", want, pr)
		}
	}

	if code, _, errOut := runPinbump(t, c, "-root", root); code != 0 {
		t.Fatalf("second run exit %d: %s", code, errOut)
	}
	all := snapshot(t, root)
	code, out, errOut := runPinbump(t, c, "-root", root, "-dry-run")
	if code != 0 || out != "" {
		t.Fatalf("third run exit %d, diff %q: %s", code, out, errOut)
	}
	if again := snapshot(t, root); !maps.Equal(all, again) {
		t.Error("a run with every pin current changed files")
	}
}

func TestPlanListsPendingBumps(t *testing.T) {
	root := fixtureRepo(t)
	before := snapshot(t, root)
	code, out, errOut := runPinbump(t, fakeGitHub(t, nil), "-root", root, "-plan")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	var plan []bump
	if err := json.Unmarshal([]byte(out), &plan); err != nil {
		t.Fatalf("plan is not JSON: %v\n%s", err, out)
	}
	if len(plan) != 3 || plan[1].Tool != "age" || plan[1].To != "1.3.2" || plan[1].DigestSource != "GitHub asset digest" {
		t.Errorf("plan = %+v", plan)
	}
	if !maps.Equal(before, snapshot(t, root)) {
		t.Error("-plan changed files")
	}
}

func TestPRBodySaysWhereTheDigestsCameFrom(t *testing.T) {
	root := fixtureRepo(t)
	c := fakeGitHub(t, nil)
	for tool, want := range map[string]string{
		"age":  "are the digests GitHub records for the release assets. The release publishes no checksum file.",
		"nono": "come from the release's `SHA256SUMS.txt`. GitHub records no digest for the assets to compare against.",
	} {
		body := filepath.Join(t.TempDir(), "body.md")
		if code, _, errOut := runPinbump(t, c, "-root", root, "-tool", tool, "-body", body); code != 0 {
			t.Fatalf("%s: exit %d: %s", tool, code, errOut)
		}
		pr, err := os.ReadFile(body)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(pr), want) {
			t.Errorf("%s PR body lacks %q:\n%s", tool, want, pr)
		}
		for _, bad := range []string{"—", "–", ";"} {
			if strings.Contains(string(pr), bad) {
				t.Errorf("%s PR body contains %q", tool, bad)
			}
		}
	}
}

func TestInconsistentPinsFailBeforeAnyRequest(t *testing.T) {
	root := fixtureRepo(t)
	path := filepath.Join(root, manifestPath)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.Replace(body, []byte(`"age": "1.3.1"`), []byte(`"age": "1.3.0"`), 1), 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, root)
	refuse := newClient("https://127.0.0.1:1", "", http.DefaultClient)
	code, _, errOut := runPinbump(t, refuse, "-root", root)
	if code != 1 || !strings.Contains(errOut, `"age" = "1.3.0", Containerfile AGE_VERSION = "1.3.1"`) {
		t.Errorf("exit %d: %s", code, errOut)
	}
	if !maps.Equal(before, snapshot(t, root)) {
		t.Error("a failed run changed files")
	}
}

// TestDigestProblemsFail: each case breaks one link between the release and
// the digest the image would trust, and the run must fail and write nothing.
func TestDigestProblemsFail(t *testing.T) {
	tofuSums := "/dl/tofu_1.13.0_SHA256SUMS"
	tofuList := "/repos/opentofu/opentofu/releases"
	listing, err := os.ReadFile("testdata/github/opentofu_opentofu.json")
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		overrides map[string]string
		want      string
	}{
		"checksum file disagrees with GitHub": {
			overrides: map[string]string{tofuSums: strings.Repeat("a", 64) + "  tofu_1.13.0_linux_amd64.zip\n" +
				strings.Repeat("c", 64) + "  tofu_1.13.0_linux_arm64.zip\n"},
			want: "GitHub records bbbb",
		},
		"checksum file omits an asset": {
			overrides: map[string]string{tofuSums: strings.Repeat("a", 64) + "  tofu_1.13.0_linux_amd64.zip\n"},
			want:      "does not list tofu_1.13.0_linux_arm64.zip",
		},
		"checksum file is not a checksum list": {
			overrides: map[string]string{tofuSums: "<html>rate limited</html>\n"},
			want:      "not a sha256 checksum line",
		},
		"checksum file missing": {
			overrides: map[string]string{tofuSums: ""},
			want:      "404",
		},
		"checksum file over plain HTTP": {
			overrides: map[string]string{tofuList: strings.ReplaceAll(string(listing),
				"{{server}}/dl/tofu_1.13.0_SHA256SUMS", "http://example.invalid/tofu_1.13.0_SHA256SUMS")},
			want: "refusing non-HTTPS URL",
		},
		"checksum file redirects to plain HTTP": {
			overrides: map[string]string{tofuSums: "redirect http://example.invalid/tofu_1.13.0_SHA256SUMS"},
			want:      "refusing redirect to http://example.invalid",
		},
		"checksum file is not the one GitHub records": {
			overrides: map[string]string{tofuList: strings.Replace(string(listing),
				`"{{server}}/dl/tofu_1.13.0_SHA256SUMS", "digest": ""`,
				`"{{server}}/dl/tofu_1.13.0_SHA256SUMS", "digest": "sha256:`+strings.Repeat("0", 64)+`"`, 1)},
			want: "downloaded tofu_1.13.0_SHA256SUMS has sha256",
		},
		"GitHub records a digest that is not sha256": {
			overrides: map[string]string{tofuList: strings.Replace(string(listing),
				`"digest": "sha256:aaaa`, `"digest": "md5:aaaa`, 1)},
			want: `GitHub records digest "md5:aaaa`,
		},
		"release lacks an architecture": {
			overrides: map[string]string{tofuList: strings.Replace(string(listing),
				`"name": "tofu_1.13.0_linux_arm64.zip"`, `"name": "tofu_1.13.0_linux_riscv64.zip"`, 1)},
			want: "has no asset tofu_1.13.0_linux_arm64.zip",
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := fixtureRepo(t)
			before := snapshot(t, root)
			code, _, errOut := runPinbump(t, fakeGitHub(t, tc.overrides), "-root", root)
			if code != 1 || !strings.Contains(errOut, tc.want) {
				t.Errorf("exit %d, want 1 with %q: %s", code, tc.want, errOut)
			}
			if !maps.Equal(before, snapshot(t, root)) {
				t.Error("a failed run changed files")
			}
		})
	}
}

func TestPinNewerThanAnyStableReleaseFails(t *testing.T) {
	root := fixtureRepo(t)
	c := fakeGitHub(t, map[string]string{"/repos/FiloSottile/age/releases": `[
		{"tag_name": "v1.3.0", "draft": false, "prerelease": false, "html_url": "https://github.com/FiloSottile/age/releases/tag/v1.3.0", "assets": []}
	]`})
	code, _, errOut := runPinbump(t, c, "-root", root, "-tool", "age")
	if code != 1 || !strings.Contains(errOut, "newer than its newest stable release 1.3.0") {
		t.Errorf("exit %d: %s", code, errOut)
	}
}

func TestListNamesTheFetchedTools(t *testing.T) {
	refuse := newClient("https://127.0.0.1:1", "", http.DefaultClient)
	code, out, errOut := runPinbump(t, refuse, "-root", fixtureRepo(t), "-list")
	if code != 0 || out != `["tofu","age","nono"]`+"\n" {
		t.Errorf("exit %d, out %q: %s", code, out, errOut)
	}
}

// TestBOMRowWantsTheExactVersion: a row for 1.3.1-dev or 1.3.10 does not
// record 1.3.1.
func TestBOMRowWantsTheExactVersion(t *testing.T) {
	for _, row := range []string{"| age (CLI) | 1.3.1-dev (sha256-pinned) |", "| age (CLI) | 1.3.10 (sha256-pinned) |"} {
		if got := bomLines(row, "age (CLI)", "1.3.1"); len(got) != 0 {
			t.Errorf("%q matched 1.3.1", row)
		}
	}
	for _, row := range []string{"| age (CLI) | 1.3.1 (sha256-pinned) |", "| nono | 1.3.1, floor ≥ 0.55.0 |"} {
		label := strings.Split(row, " | ")[0][2:]
		if got := bomLines(row, label, "1.3.1"); len(got) != 1 {
			t.Errorf("%q did not match 1.3.1", row)
		}
	}
}

func TestParseChecksums(t *testing.T) {
	sums, err := parseChecksums(strings.Repeat("a", 64) + "  one\n" + strings.Repeat("b", 64) + " *two\n\n")
	if err != nil {
		t.Fatal(err)
	}
	if sums["one"] != strings.Repeat("a", 64) || sums["two"] != strings.Repeat("b", 64) {
		t.Errorf("sums = %v", sums)
	}
	if _, err := parseChecksums(strings.Repeat("a", 64) + "  one\n" + strings.Repeat("b", 64) + "  one\n"); err == nil {
		t.Error("two digests for one name parsed")
	}
	if _, err := parseChecksums(strings.Repeat("A", 64) + "  one\n"); err == nil {
		t.Error("an uppercase digest parsed")
	}
}

// TestRepoPinsAreConsistent: the committed Containerfile, tool-digests.txt,
// MANIFEST.json and bill of materials agree, and every fetched tool has a
// digest source, so the scheduled bump starts from a consistent tree.
func TestRepoPinsAreConsistent(t *testing.T) {
	files := repoFiles{}
	for _, path := range []string{containerfilePath, digestsPath, manifestPath, bomPath} {
		body, err := os.ReadFile(filepath.Join("..", "..", path))
		if err != nil {
			t.Fatal(err)
		}
		files[path] = string(body)
	}
	pins, err := parsePins(files[containerfilePath], sources)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkConsistency(files, pins); err != nil {
		t.Error(err)
	}
}
