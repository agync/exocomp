package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// bump is a newer stable release of one pinned tool, with the sha256 of each
// architecture's asset.
type bump struct {
	Tool         string `json:"tool"`
	From         string `json:"from"`
	To           string `json:"to"`
	Release      string `json:"release_url"`
	DigestSource string `json:"digest_source"`

	digests map[string]string
	// crossChecked is true when GitHub's recorded digest of every asset
	// matched the checksum file.
	crossChecked bool
}

type ghRelease struct {
	TagName    string    `json:"tag_name"`
	HTMLURL    string    `json:"html_url"`
	Draft      bool      `json:"draft"`
	Prerelease bool      `json:"prerelease"`
	Assets     []ghAsset `json:"assets"`
}

type ghAsset struct {
	Name   string `json:"name"`
	URL    string `json:"browser_download_url"`
	Digest string `json:"digest"`
}

var hexDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

// client reads releases from the GitHub API and downloads checksum files. It
// refuses any URL or redirect that is not HTTPS.
type client struct {
	api   string
	token string
	http  *http.Client
}

func newClient(api, token string, base *http.Client) *client {
	hc := *base
	hc.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" {
			return fmt.Errorf("refusing redirect to %s", req.URL.Redacted())
		}
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		return nil
	}
	if hc.Timeout == 0 {
		hc.Timeout = 60 * time.Second
	}
	return &client{api: strings.TrimSuffix(api, "/"), token: token, http: &hc}
}

func (c *client) get(ctx context.Context, rawURL string, limit int64, api bool) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("refusing non-HTTPS URL %s", u.Redacted())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if api {
		req.Header.Set("Accept", "application/vnd.github+json")
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", u.Redacted(), resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("GET %s: response over %d bytes", u.Redacted(), limit)
	}
	return body, nil
}

// latest returns the newest stable release of p by version, never a draft or
// a prerelease, and never a tag that is not a plain x.y.z in the pin's shape.
// Only the 30 most recently created releases are read, so a project with 30
// prereleases or drafts in a row has no candidate and fails. OpenTofu's 30
// come to about 10 MB of JSON.
func (c *client) latest(ctx context.Context, p pin) (ghRelease, string, error) {
	body, err := c.get(ctx, fmt.Sprintf("%s/repos/%s/%s/releases?per_page=30", c.api, p.owner, p.repo), 32<<20, true)
	if err != nil {
		return ghRelease{}, "", err
	}
	var releases []ghRelease
	if err := json.Unmarshal(body, &releases); err != nil {
		return ghRelease{}, "", fmt.Errorf("%s/%s releases: %w", p.owner, p.repo, err)
	}
	var best ghRelease
	var bestVersion string
	for _, r := range releases {
		if r.Draft || r.Prerelease {
			continue
		}
		v, ok := p.versionFromTag(r.TagName)
		if !ok {
			continue
		}
		if bestVersion == "" || compareVersions(v, bestVersion) > 0 {
			best, bestVersion = r, v
		}
	}
	if bestVersion == "" {
		return ghRelease{}, "", fmt.Errorf("%s/%s has no stable release tagged like %s", p.owner, p.repo, p.tag)
	}
	return best, bestVersion, nil
}

// resolve returns the bump for p, or nil when the pin is already the newest
// stable release.
func (c *client) resolve(ctx context.Context, p pin) (*bump, error) {
	rel, version, err := c.latest(ctx, p)
	if err != nil {
		return nil, err
	}
	switch cmp := compareVersions(version, p.version); {
	case cmp == 0:
		return nil, nil
	case cmp < 0:
		return nil, fmt.Errorf("%s is pinned at %s, newer than its newest stable release %s", p.tool, p.version, version)
	}
	b := &bump{Tool: p.tool, From: p.version, To: version, Release: rel.HTMLURL, digests: map[string]string{}}
	if !strings.HasPrefix(b.Release, "https://github.com/") {
		return nil, fmt.Errorf("%s: release URL %q is not on github.com", p.tool, b.Release)
	}

	assets := map[string]ghAsset{}
	for _, a := range rel.Assets {
		assets[a.Name] = a
	}
	var sums map[string]string
	if name := strings.ReplaceAll(p.src.checksums, "{version}", version); name != "" {
		a, ok := assets[name]
		if !ok {
			return nil, fmt.Errorf("%s %s publishes no %s", p.tool, version, name)
		}
		body, err := c.get(ctx, a.URL, 1<<20, false)
		if err != nil {
			return nil, err
		}
		if hex, ok := strings.CutPrefix(a.Digest, "sha256:"); ok {
			if got := fmt.Sprintf("%x", sha256.Sum256(body)); got != hex {
				return nil, fmt.Errorf("%s: downloaded %s has sha256 %s, GitHub records %s", p.tool, name, got, hex)
			}
		}
		if sums, err = parseChecksums(string(body)); err != nil {
			return nil, fmt.Errorf("%s %s: %w", name, version, err)
		}
		b.DigestSource = name
	} else {
		b.DigestSource = "GitHub asset digest"
	}

	b.crossChecked = true
	for _, arch := range arches {
		name, err := p.render(p.asset, version, arch)
		if err != nil {
			return nil, err
		}
		a, ok := assets[name]
		if !ok {
			return nil, fmt.Errorf("%s %s has no asset %s", p.tool, version, name)
		}
		recorded := ""
		if a.Digest != "" {
			hex, ok := strings.CutPrefix(a.Digest, "sha256:")
			if !ok || !hexDigest.MatchString(hex) {
				return nil, fmt.Errorf("%s: GitHub records digest %q for %s", p.tool, a.Digest, name)
			}
			recorded = hex
		}
		digest := recorded
		if sums != nil {
			digest = sums[name]
			if digest == "" {
				return nil, fmt.Errorf("%s does not list %s", b.DigestSource, name)
			}
			if recorded == "" {
				b.crossChecked = false
			} else if recorded != digest {
				return nil, fmt.Errorf("%s: %s lists %s for %s, GitHub records %s",
					p.tool, b.DigestSource, digest, name, recorded)
			}
		}
		if digest == "" {
			return nil, fmt.Errorf("%s: no digest for %s, the release has no checksum file and GitHub records none", p.tool, name)
		}
		b.digests[arch] = digest
	}
	return b, nil
}

// parseChecksums reads a sha256sum-style list: "<hex>  <name>", with an
// optional "*" before the name for binary mode.
func parseChecksums(body string) (map[string]string, error) {
	sums := map[string]string{}
	for _, line := range strings.Split(body, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 || !hexDigest.MatchString(fields[0]) {
			return nil, fmt.Errorf("not a sha256 checksum line: %q", line)
		}
		name := strings.TrimPrefix(fields[1], "*")
		if prev, dup := sums[name]; dup && prev != fields[0] {
			return nil, fmt.Errorf("two different digests for %s", name)
		}
		sums[name] = fields[0]
	}
	if len(sums) == 0 {
		return nil, errors.New("no checksums listed")
	}
	return sums, nil
}

// prBody describes a bump for its pull request.
func prBody(b bump) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "The image's %s pin moves from %s to %s.\n\n", b.Tool, b.From, b.To)
	fmt.Fprintf(&sb, "**Release.** %s\n\n", b.Release)
	sb.WriteString("**Digests.** The amd64 and arm64 sha256 in `packaging/tool-digests.txt` ")
	switch {
	case b.DigestSource == "GitHub asset digest":
		sb.WriteString("are the digests GitHub records for the release assets. The release publishes no checksum file.")
	case b.crossChecked:
		fmt.Fprintf(&sb, "come from the release's `%s` and match the digests GitHub records for the assets.", b.DigestSource)
	default:
		fmt.Fprintf(&sb, "come from the release's `%s`. GitHub records no digest for the assets to compare against.", b.DigestSource)
	}
	sb.WriteString(" `MANIFEST.json` and the bill of materials record the new version.\n")
	return sb.String()
}
