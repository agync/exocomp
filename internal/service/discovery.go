package service

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// MaxSourcePlaybooks caps how many candidate playbook paths
// ListSourcePlaybooks returns. A UI offering the list renders this as "and
// more" rather than an unbounded table, and the cap is also what keeps a
// pathological checkout from growing the daemon's response without bound.
const MaxSourcePlaybooks = 500

// maxDiscoveryEntries caps how many directory entries the walk visits, so an
// oversized or adversarial tree cannot make discovery itself hang or spin the
// daemon regardless of how few (or many) entries turn out to look like
// playbooks.
const maxDiscoveryEntries = 20000

// maxDiscoveryDepth caps how many path components deep the walk descends from
// the source root.
const maxDiscoveryDepth = 12

// maxDiscoveryFileBytes skips content-sniffing (and so excludes) any
// candidate file larger than this: a real playbook is a short list of plays,
// and reading an arbitrarily large file just to decide "probably not a
// playbook" is not worth the I/O.
const maxDiscoveryFileBytes = 1 << 20 // 1 MiB

// excludedDiscoveryDirs are directory names that never hold a runnable
// playbook, so the walk does not descend into them at all: role internals
// (roles and the subdirectories a role has under it), inventory variable
// trees, and scenario/test fixtures. Matching is case-insensitive.
var excludedDiscoveryDirs = map[string]bool{
	"roles":          true,
	"group_vars":     true,
	"host_vars":      true,
	"molecule":       true,
	"tasks":          true,
	"handlers":       true,
	"defaults":       true,
	"vars":           true,
	"meta":           true,
	"templates":      true,
	"files":          true,
	"library":        true,
	"module_utils":   true,
	"filter_plugins": true,
	"test":           true,
	"tests":          true,
	"node_modules":   true,
}

// excludedDiscoveryFiles are filenames that are valid *.yml/*.yaml but are
// never themselves a runnable playbook, even though some (requirements.yml)
// are also YAML lists and would otherwise pass the content sniff.
var excludedDiscoveryFiles = map[string]bool{
	"requirements.yml":  true,
	"requirements.yaml": true,
	"galaxy.yml":        true,
	"galaxy.yaml":       true,
}

// errDiscoveryCapped stops the walk early once a cap is hit. It is never
// returned to the caller: ListSourcePlaybooks treats it the same as a clean
// end of the walk, returning what it found so far.
var errDiscoveryCapped = errors.New("service: discovery cap reached")

// ListSourcePlaybooks walks a source checkout root and returns candidate
// Ansible playbook paths relative to it, for a UI to offer as run choices.
// Results are sorted, deterministic, and relative (slash-separated) so they
// can be joined straight back onto the same root or handed to
// Service.ResolveSourcePlaybook.
//
// The heuristic is deliberately conservative: it skips dot-directories and
// well-known non-playbook directories (roles/, group_vars/, host_vars/,
// molecule/, and role-internal dirs like tasks/ or vars/ wherever they
// appear), skips well-known non-playbook filenames (requirements.yml,
// galaxy.yml), and only keeps a *.yml/*.yaml file whose top-level YAML
// document is a list — a playbook is a list of plays, where a vars/defaults
// file (or one this heuristic otherwise let through) is a map. A UI that also
// offers a free-path fallback makes a missed playbook cheap to recover from; a
// false positive an operator picks and runs is not, so the walk favors
// precision over recall.
//
// The walk is capped independent of the result cap — depth, total entries
// visited, and per-file bytes sniffed — so an oversized or adversarial tree
// cannot hang the daemon; hitting a cap ends the walk early and returns
// whatever was found, not an error.
//
// A root that does not exist yet (a registered source not synced) is not an
// error: it returns an empty, non-nil slice. An empty root path, or a root
// that exists but is not a directory, is an error.
func ListSourcePlaybooks(root string) ([]string, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("service: empty source root")
	}
	info, err := os.Stat(root)
	if errors.Is(err, fs.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("service: stat source root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("service: source root %q is not a directory", root)
	}

	found := []string{}
	visited := 0
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A transient stat error (permission, race with a concurrent
			// sync) skips just that entry rather than aborting discovery.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if path == root {
			return nil
		}
		visited++
		if visited > maxDiscoveryEntries {
			return errDiscoveryCapped
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}

		if d.IsDir() {
			name := d.Name()
			if strings.HasPrefix(name, ".") || excludedDiscoveryDirs[strings.ToLower(name)] {
				return fs.SkipDir
			}
			if strings.Count(rel, string(filepath.Separator))+1 >= maxDiscoveryDepth {
				return fs.SkipDir
			}
			return nil
		}

		if len(found) >= MaxSourcePlaybooks {
			return errDiscoveryCapped
		}
		if looksLikePlaybook(path, d.Name()) {
			found = append(found, filepath.ToSlash(rel))
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, errDiscoveryCapped) {
		return nil, fmt.Errorf("service: walk source root: %w", walkErr)
	}

	sort.Strings(found)
	return found, nil
}

// looksLikePlaybook reports whether a file name and its content pass the
// conservative playbook heuristic documented on ListSourcePlaybooks.
func looksLikePlaybook(path, name string) bool {
	if strings.HasPrefix(name, ".") {
		return false
	}
	ext := strings.ToLower(filepath.Ext(name))
	if ext != ".yml" && ext != ".yaml" {
		return false
	}
	if excludedDiscoveryFiles[strings.ToLower(name)] {
		return false
	}

	info, err := os.Stat(path)
	if err != nil || info.Size() == 0 || info.Size() > maxDiscoveryFileBytes {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	if bytes.HasPrefix(bytes.TrimSpace(data), []byte("$ANSIBLE_VAULT")) {
		// Encrypted content cannot be sniffed; exclude it rather than guess.
		return false
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return false
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return false
	}
	// A playbook's top-level document is a list of plays; a vars/defaults/
	// group_vars-shaped file (or anything else that slipped past the
	// directory/filename filters) is a map and is excluded here.
	return doc.Content[0].Kind == yaml.SequenceNode
}
