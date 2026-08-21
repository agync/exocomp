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
