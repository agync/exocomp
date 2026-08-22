// Package hooks reads a source's pre/post-run steps and its post-run health
// gate from `.exocomp/hooks.yml`. A run becomes a step-ledger workflow —
// [pre_run...] -> the main play/plan -> [post_run...] -> the gate — so a
// pre-hook that fails blocks the main step (the sequencer runs step N+1 only
// once step N succeeded), and the gate can fail an otherwise-zero-exit run.
//
// The file is discovered repo-side by default with an operator override, the
// same precedence the source model uses everywhere: an operator-registered
// override wins, otherwise the checkout's own `.exocomp/hooks.yml`. A malformed
// file is refused at define time (hooks validate / hooks show), never
// discovered silently at run time where a bad hook would already have run.
package hooks

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"

	"exocomp.dev/exocomp/internal/verify"
)

// JobKind is the queue/ledger kind a hook step runs under: a sandboxed shell
// job, distinct from the scheduler's own shell kind so wiring a hook does not
// change how a scheduled shell fires.
const JobKind = "hook"

// RepoRelPath is where a source declares its hooks by default, relative to the
// checkout root.
const RepoRelPath = ".exocomp/hooks.yml"

// Hook is one pre- or post-run step. Exactly one of Run and Script is set: Run
// is an inline shell command, Script is a path (relative to the checkout root)
// executed with /bin/sh. Secret optionally names a run_secret binding whose
// sealed values are injected as named environment variables for this step —
// the same credential plane a play or a provider uses, so a hook that has to
// authenticate can, without the value ever touching the argv or the job row.
type Hook struct {
	Name   string `yaml:"name"`
	Run    string `yaml:"run,omitempty"`
	Script string `yaml:"script,omitempty"`
	Secret string `yaml:"secret,omitempty"`
}

// Hooks is a source's resolved lifecycle: ordered pre_run and post_run steps
// and a post-run gate. An empty Hooks (no file, or an empty file) means a run
// is just its main step, unchanged.
type Hooks struct {
	PreRun  []Hook         `yaml:"pre_run,omitempty"`
	PostRun []Hook         `yaml:"post_run,omitempty"`
	Gate    []verify.Probe `yaml:"gate,omitempty"`
}

// Empty reports whether a run would be composed exactly as its main step: no
// hooks and no gate.
func (h Hooks) Empty() bool {
	return len(h.PreRun) == 0 && len(h.PostRun) == 0 && len(h.Gate) == 0
}

// Parse decodes and validates a hooks.yml. Unknown keys are refused (a typo in
// `pre_run` is a mistake worth failing on, not silently dropping), each hook
// must name exactly one of run/script, and each gate probe must be able to
// decide. A parse that returns nil error is safe to run as written.
func Parse(data []byte) (Hooks, error) {
	var h Hooks
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&h); err != nil {
		// An empty document decodes as io.EOF; treat it as no hooks.
		if err.Error() == "EOF" {
			return Hooks{}, nil
		}
		return Hooks{}, fmt.Errorf("hooks: parse: %w", err)
	}
	if err := h.validate(); err != nil {
		return Hooks{}, err
	}
	return h, nil
}

// validate enforces the semantic rules the YAML shape cannot.
func (h Hooks) validate() error {
	if err := validateHooks("pre_run", h.PreRun); err != nil {
		return err
	}
	if err := validateHooks("post_run", h.PostRun); err != nil {
		return err
	}
	if len(h.Gate) > 0 {
		if err := verify.ValidateAll(h.Gate); err != nil {
			return err
		}
	}
	return nil
}

func validateHooks(phase string, list []Hook) error {
	seen := map[string]bool{}
	for i, hk := range list {
		if strings.TrimSpace(hk.Name) == "" {
			return fmt.Errorf("hooks: %s step %d has no name", phase, i+1)
		}
		if seen[hk.Name] {
			return fmt.Errorf("hooks: %s declares two steps named %q", phase, hk.Name)
		}
		seen[hk.Name] = true

		hasRun := strings.TrimSpace(hk.Run) != ""
		hasScript := strings.TrimSpace(hk.Script) != ""
		switch {
		case !hasRun && !hasScript:
			return fmt.Errorf("hooks: %s step %q declares neither run nor script", phase, hk.Name)
		case hasRun && hasScript:
			return fmt.Errorf("hooks: %s step %q declares both run and script; a step does one thing", phase, hk.Name)
		}
		if hasScript && filepath.IsAbs(hk.Script) {
			return fmt.Errorf("hooks: %s step %q: script %q must be relative to the checkout, not absolute",
				phase, hk.Name, hk.Script)
		}
		if hasScript && strings.Contains(hk.Script, "..") {
			return fmt.Errorf("hooks: %s step %q: script %q must stay inside the checkout (no parent refs)",
				phase, hk.Name, hk.Script)
		}
	}
	return nil
}

// Resolve reads a source's hooks with the override-wins precedence: the
// operator override at overridePath when it exists, otherwise the checkout's
// own .exocomp/hooks.yml. Neither present is not an error — it is a source with
// no lifecycle, whose run is just its main step. A present-but-malformed file
// IS an error, so a broken hooks.yml fails loudly rather than being skipped.
func Resolve(checkoutRoot, overridePath string) (Hooks, error) {
	if overridePath != "" {
		if data, err := os.ReadFile(overridePath); err == nil {
			h, perr := Parse(data)
			if perr != nil {
				return Hooks{}, fmt.Errorf("hooks: override %s: %w", overridePath, perr)
			}
			return h, nil
		} else if !os.IsNotExist(err) {
			return Hooks{}, fmt.Errorf("hooks: read override %s: %w", overridePath, err)
		}
	}

	repoPath := filepath.Join(checkoutRoot, RepoRelPath)
	data, err := os.ReadFile(repoPath)
	if os.IsNotExist(err) {
		return Hooks{}, nil
	}
	if err != nil {
		return Hooks{}, fmt.Errorf("hooks: read %s: %w", repoPath, err)
	}
	h, perr := Parse(data)
	if perr != nil {
		return Hooks{}, fmt.Errorf("hooks: %s: %w", repoPath, perr)
	}
	return h, nil
}

// Source names where a resolved Hooks came from, for the lister to show the
// operator which file is in effect.
type Source struct {
	Path       string // the file resolved, "" when none
	IsOverride bool   // true when the operator override won
}

// ResolveWithSource is Resolve plus the provenance of the file it read.
func ResolveWithSource(checkoutRoot, overridePath string) (Hooks, Source, error) {
	if overridePath != "" {
		if _, err := os.Stat(overridePath); err == nil {
			h, rerr := Resolve(checkoutRoot, overridePath)
			return h, Source{Path: overridePath, IsOverride: true}, rerr
		} else if !os.IsNotExist(err) {
			return Hooks{}, Source{}, fmt.Errorf("hooks: stat override %s: %w", overridePath, err)
		}
	}
	repoPath := filepath.Join(checkoutRoot, RepoRelPath)
	if _, err := os.Stat(repoPath); err == nil {
		h, rerr := Resolve(checkoutRoot, "")
		return h, Source{Path: repoPath}, rerr
	}
	return Hooks{}, Source{}, nil
}
