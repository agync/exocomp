package backup

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"exocomp.dev/exocomp/internal/executor"
)

// credSnapshot is what a leg's subprocess could see about its own credential
// at the moment it ran.
type credSnapshot struct {
	path     string
	body     string
	fileMode os.FileMode
	dirMode  os.FileMode
	inEnv    bool
}

// recordingDriver stands in for the shell driver so a leg's process
// environment can be inspected instead of guessed at.
type recordingDriver struct {
	mu    sync.Mutex
	specs []executor.Spec
	creds []credSnapshot
}

func (d *recordingDriver) Run(_ context.Context, spec executor.Spec, sink executor.EventSink) (executor.Result, error) {
	d.mu.Lock()
	d.specs = append(d.specs, spec)
	d.creds = append(d.creds, inspectCred(spec.Env))
	d.mu.Unlock()

	if slices.Contains(spec.Argv, "backup") {
		sink.Emit(executor.Event{
			Level: executor.LevelInfo,
			Msg:   `{"message_type":"summary","snapshot_id":"deadbeefcafe","data_added":42}`,
		})
	}
	return executor.Result{ExitCode: 0}, nil
}

func inspectCred(env []string) credSnapshot {
	var c credSnapshot
	for _, kv := range env {
		switch key, value, _ := strings.Cut(kv, "="); key {
		case "RESTIC_PASSWORD":
			c.inEnv = true
		case "RESTIC_PASSWORD_FILE":
			c.path = value
			body, err := os.ReadFile(value)
			if err != nil {
				return c
			}
			c.body = strings.TrimSpace(string(body))
			if info, err := os.Stat(value); err == nil {
				c.fileMode = info.Mode().Perm()
			}
			if info, err := os.Stat(filepath.Dir(value)); err == nil {
				c.dirMode = info.Mode().Perm()
			}
		}
	}
	return c
}

// TestResticLegKeepsThePasswordOutOfTheEnvironment pins where the derived
// repository password may live: a 0600 file in a private directory that is
// neither the staging tree nor the process environment, gone when the leg is.
func TestResticLegKeepsThePasswordOutOfTheEnvironment(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	stage := t.TempDir()
	if _, err := f.engine.stage(t.Context(), stage); err != nil {
		t.Fatalf("stage: %v", err)
	}

	drv := &recordingDriver{}
	stageRoot := t.TempDir()
	engine := New(f.store, f.env, Options{Driver: drv, StageRoot: stageRoot})
	target := f.target(t, "nas", KindResticLocal, filepath.Join(t.TempDir(), "repo"))

	if _, _, err := engine.resticLeg(t.Context(), target, stage); err != nil {
		t.Fatalf("restic leg: %v", err)
	}
	if len(drv.creds) == 0 {
		t.Fatal("the leg ran no subprocess")
	}

	want := base64.StdEncoding.EncodeToString(f.env.Derive("restic-repo", "nas"))
	for i, c := range drv.creds {
		argv := strings.Join(drv.specs[i].Argv, " ")
		if c.inEnv {
			t.Errorf("%s: RESTIC_PASSWORD is in the child environment", argv)
		}
		if c.path == "" {
			t.Errorf("%s: no RESTIC_PASSWORD_FILE in the child environment", argv)
			continue
		}
		if c.body != want {
			t.Errorf("%s: password file does not hold the derived password", argv)
		}
		if c.fileMode != 0o600 {
			t.Errorf("%s: password file is %04o, want 0600", argv, c.fileMode)
		}
		if c.dirMode != 0o700 {
			t.Errorf("%s: password file's directory is %04o, want 0700", argv, c.dirMode)
		}
		if within(stage, c.path) {
			t.Errorf("%s: password file %s sits in the staging tree, which is what gets backed up",
				argv, c.path)
		}
		if _, err := os.Stat(c.path); err == nil {
			t.Errorf("%s: password file %s outlived the leg", argv, c.path)
		}
	}

	entries, err := os.ReadDir(stageRoot)
	if err != nil {
		t.Fatalf("read stage root: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("the leg left %d director(ies) behind under %s", len(entries), stageRoot)
	}
}

func within(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && !strings.HasPrefix(rel, "..")
}

// hasTag reports whether argv carries "--tag <value>" as an adjacent pair.
func hasTag(argv []string, value string) bool {
	for i, a := range argv {
		if a == "--tag" && i+1 < len(argv) && argv[i+1] == value {
			return true
		}
	}
	return false
}

// TestResticRestoreScopesLatestToExocompTags pins finding B2: the backup leg
// tags every snapshot "exocomp" and its target name (resticLeg, "backup"),
// so "restic restore latest" with no tag filter can resolve to a foreign
// snapshot on a repository this program does not own exclusively, or to the
// wrong target's snapshot on a repository URI shared between targets.
func TestResticRestoreScopesLatestToExocompTags(t *testing.T) {
	t.Parallel()

	drv := &recordingDriver{}
	src := Source{
		Kind:      KindResticLocal,
		URI:       filepath.Join(t.TempDir(), "repo"),
		Name:      "nas",
		Password:  "test-password",
		Driver:    drv,
		StageRoot: t.TempDir(),
	}

	// The fake driver writes nothing, so the restore leg fails past the
	// restic invocation this test inspects; only the captured argv matters.
	if _, err := restoreRestic(t.Context(), src, t.TempDir(), t.TempDir()); err == nil {
		t.Fatal("restoreRestic succeeded against a driver that wrote no manifest")
	}
	if len(drv.specs) == 0 {
		t.Fatal("restoreRestic ran no subprocess")
	}

	argv := drv.specs[0].Argv
	if !slices.Contains(argv, "restore") {
		t.Fatalf("first subprocess argv = %v, want a restic restore", argv)
	}
	if !hasTag(argv, "exocomp") {
		t.Errorf("restore argv %v: missing --tag exocomp; an untagged latest "+
			"can resolve to a foreign snapshot on a shared repository", argv)
	}
	if !hasTag(argv, "nas") {
		t.Errorf("restore argv %v: missing --tag nas; a shared repository uri "+
			"can hold more than one exocomp target's snapshots", argv)
	}
}

// TestResticRestoreOmitsNameTagWhenTargetUnknown covers the bare-password
// restore path (Source.Password set, Source.Name empty): the leg has no
// target name to scope by, so it must still filter to exocomp's own
// snapshots without inventing a name tag that would match nothing.
func TestResticRestoreOmitsNameTagWhenTargetUnknown(t *testing.T) {
	t.Parallel()

	drv := &recordingDriver{}
	src := Source{
		Kind:      KindResticLocal,
		URI:       filepath.Join(t.TempDir(), "repo"),
		Password:  "test-password",
		Driver:    drv,
		StageRoot: t.TempDir(),
	}

	if _, err := restoreRestic(t.Context(), src, t.TempDir(), t.TempDir()); err == nil {
		t.Fatal("restoreRestic succeeded against a driver that wrote no manifest")
	}
	if len(drv.specs) == 0 {
		t.Fatal("restoreRestic ran no subprocess")
	}

	argv := drv.specs[0].Argv
	if !hasTag(argv, "exocomp") {
		t.Errorf("restore argv %v: missing --tag exocomp", argv)
	}
	count := 0
	for i, a := range argv {
		if a == "--tag" {
			count++
			_ = i
		}
	}
	if count != 1 {
		t.Errorf("restore argv %v: want exactly one --tag (exocomp) when the target name is unknown, got %d", argv, count)
	}
}
