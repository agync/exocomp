// Command pinbump moves the image's release-fetched tool pins to their newest
// stable upstream release. For each tool the Containerfile fetches through its
// checked download, it resolves the newest non-draft, non-prerelease release on
// GitHub, takes each architecture's sha256 from the release's checksum file
// (cross-checked against the digest GitHub records for the asset), and
// rewrites the ARG, tool-digests.txt, MANIFEST.json and the bill of materials
// row together. It exits non-zero when those files disagree before or after.
//
//	go run ./packaging/pinbump -dry-run      print the diff, write nothing
//	go run ./packaging/pinbump               apply every bump
//	go run ./packaging/pinbump -tool tofu    apply one tool's bump
//	go run ./packaging/pinbump -plan         print pending bumps as JSON
//	go run ./packaging/pinbump -list         print the bumpable tools as JSON
//
// GITHUB_TOKEN, when set, authenticates the API reads.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func main() {
	c := newClient("https://api.github.com", os.Getenv("GITHUB_TOKEN"), http.DefaultClient)
	os.Exit(run(context.Background(), os.Args[1:], c, sources, os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, c *client, srcs map[string]source, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pinbump", flag.ContinueOnError)
	fs.SetOutput(stderr)
	root := fs.String("root", ".", "repository root")
	tool := fs.String("tool", "", "bump only this tool")
	dryRun := fs.Bool("dry-run", false, "print the diff and write nothing")
	plan := fs.Bool("plan", false, "print pending bumps as JSON and write nothing")
	body := fs.String("body", "", "write a pull request body for the applied bumps to this file")
	list := fs.Bool("list", false, "print the tools pinbump can bump as JSON, with no network access")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if (*plan || *list) && (*dryRun || *body != "") || *plan && *list {
		_, _ = fmt.Fprintln(stderr, "pinbump: -plan and -list take neither each other, -dry-run nor -body")
		return 2
	}
	if err := bumpPins(ctx, c, srcs, *root, *tool, *dryRun, *plan, *list, *body, stdout, stderr); err != nil {
		_, _ = fmt.Fprintf(stderr, "pinbump: %v\n", err)
		return 1
	}
	return 0
}

func bumpPins(ctx context.Context, c *client, srcs map[string]source, root, only string, dryRun, plan, list bool, bodyPath string, stdout, stderr io.Writer) error {
	files := repoFiles{}
	for _, path := range []string{containerfilePath, digestsPath, manifestPath, bomPath} {
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			return err
		}
		files[path] = string(data)
	}
	pins, err := parsePins(files[containerfilePath], srcs)
	if err != nil {
		return err
	}
	if err := checkConsistency(files, pins); err != nil {
		return err
	}

	if list {
		tools := make([]string, 0, len(pins))
		for _, p := range pins {
			tools = append(tools, p.tool)
		}
		return json.NewEncoder(stdout).Encode(tools)
	}

	var selected []pin
	for _, p := range pins {
		if only == "" || p.tool == only {
			selected = append(selected, p)
		}
	}
	if len(selected) == 0 {
		return fmt.Errorf("the Containerfile fetches no tool %q", only)
	}

	bumps := []bump{}
	after := files
	for _, p := range selected {
		b, err := c.resolve(ctx, p)
		if err != nil {
			return err
		}
		if b == nil {
			_, _ = fmt.Fprintf(stderr, "%s %s is current\n", p.tool, p.version)
			continue
		}
		_, _ = fmt.Fprintf(stderr, "%s %s -> %s\n", b.Tool, b.From, b.To)
		if after, err = apply(after, p, *b); err != nil {
			return err
		}
		bumps = append(bumps, *b)
	}

	if plan {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(bumps)
	}

	newPins, err := parsePins(after[containerfilePath], srcs)
	if err != nil {
		return err
	}
	if err := checkConsistency(after, newPins); err != nil {
		return fmt.Errorf("after the rewrite: %w", err)
	}

	var changed []string
	for path := range after {
		if after[path] != files[path] {
			changed = append(changed, path)
		}
	}
	sort.Strings(changed)
	if dryRun {
		for _, path := range changed {
			_, _ = fmt.Fprint(stdout, diff(path, files[path], after[path]))
		}
		return nil
	}

	if bodyPath != "" && len(bumps) > 0 {
		var sb strings.Builder
		for i, b := range bumps {
			if i > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(prBody(b))
		}
		if err := os.WriteFile(bodyPath, []byte(sb.String()), 0o644); err != nil {
			return err
		}
	}

	// Every new file is staged beside its target before any target is
	// replaced, so a failed write leaves the pins as they were.
	staged := map[string]string{}
	defer func() {
		for _, tmp := range staged {
			_ = os.Remove(tmp)
		}
	}()
	for _, path := range changed {
		tmp, err := stage(filepath.Join(root, path), after[path])
		if err != nil {
			return err
		}
		staged[path] = tmp
	}
	for _, path := range changed {
		if err := os.Rename(staged[path], filepath.Join(root, path)); err != nil {
			return err
		}
		delete(staged, path)
	}
	return nil
}

// stage writes content to a temporary file beside path, with path's mode.
func stage(path, content string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New(path + " is not a regular file")
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".pinbump-*")
	if err != nil {
		return "", err
	}
	_, werr := f.WriteString(content)
	cerr := f.Close()
	if err := errors.Join(werr, cerr, os.Chmod(f.Name(), info.Mode().Perm())); err != nil {
		_ = os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}
