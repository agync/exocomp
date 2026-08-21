package backup

import (
	"context"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/store"
)

// pushToken is the credential the leg must never write down.
const pushToken = "ghp-s3cr3t-token-1f4a"

func TestScrubURIUserinfo(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want string
	}{
		{
			"fatal: could not read Password for 'https://bot:" + pushToken + "@github.com'",
			"fatal: could not read Password for 'https://***@github.com'",
		},
		{
			"remote: Authentication failed for 'https://" + pushToken + "@github.com/o/r.git/'",
			"remote: Authentication failed for 'https://***@github.com/o/r.git/'",
		},
		{
			"backup: restic_s3 uri \"s3:https://KEY:" + pushToken + "@nas.lan/bucket\" is bad",
			"backup: restic_s3 uri \"s3:https://***@nas.lan/bucket\" is bad",
		},
		{"ssh://git@host:2222/repo.git unreachable", "ssh://***@host:2222/repo.git unreachable"},
		// No userinfo, nothing to do: a scrub that rewrote these would make
		// every other error harder to read.
		{"fatal: unable to access 'https://github.com/o/r.git/'", "fatal: unable to access 'https://github.com/o/r.git/'"},
		{"git@github.com:o/r.git", "git@github.com:o/r.git"},
		{"", ""},
	}
	for _, c := range cases {
		if got := ScrubURIUserinfo(c.in); got != c.want {
			t.Errorf("ScrubURIUserinfo(%q) =\n  %q\nwant\n  %q", c.in, got, c.want)
		}
	}
}

func TestHasURIUserinfo(t *testing.T) {
	t.Parallel()

	with := []string{
		"https://bot:" + pushToken + "@github.com/o/r.git",
		"https://" + pushToken + "@github.com/o/r.git",
		"s3:https://KEY:SECRET@nas.lan/bucket",
	}
	without := []string{
		"https://github.com/o/r.git",
		"git@github.com:o/r.git",
		"/srv/backups/nas",
		"s3:https://nas.lan/bucket",
	}
	for _, uri := range with {
		if !HasURIUserinfo(uri) {
			t.Errorf("HasURIUserinfo(%q) = false, want true", uri)
		}
	}
	for _, uri := range without {
		if HasURIUserinfo(uri) {
			t.Errorf("HasURIUserinfo(%q) = true, want false", uri)
		}
	}
}

// echoingDriver is a subprocess that fails while quoting the remote it could
// not reach — the shape that puts a token into backup_run.error.
type echoingDriver struct{ line string }

func (d echoingDriver) Run(_ context.Context, _ executor.Spec, sink executor.EventSink) (executor.Result, error) {
	sink.Emit(executor.Event{Level: executor.LevelError, Msg: d.line})
	return executor.Result{ExitCode: 128}, nil
}

// TestGitLegErrorCarriesNoCredentials runs a leg whose subprocess echoes the
// credential-bearing remote and checks both the returned error and the row the
// engine persists.
func TestGitLegErrorCarriesNoCredentials(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	stage := t.TempDir()
	if _, err := f.engine.stage(t.Context(), stage); err != nil {
		t.Fatalf("stage: %v", err)
	}

	uri := "https://bot:" + pushToken + "@git.invalid/exocomp-backup.git"
	drv := echoingDriver{line: "fatal: unable to access '" + uri + "': could not resolve host"}
	engine := New(f.store, f.env, Options{Driver: drv, StageRoot: t.TempDir()})

	row, err := f.store.CreateBackupTarget(t.Context(), store.BackupTarget{
		Name: "offsite", Kind: KindGit, URI: uri, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}

	leg := engine.runLeg(t.Context(), Target{BackupTarget: row}, stage)
	if leg.Err == nil {
		t.Fatal("the leg reported success against an unreachable remote")
	}
	if strings.Contains(leg.Err.Error(), pushToken) {
		t.Errorf("leg error leaked the push credential: %v", leg.Err)
	}
	if !strings.Contains(leg.Err.Error(), "git.invalid") {
		t.Errorf("scrubbing swallowed the host too: %v", leg.Err)
	}

	runs, err := f.store.ListBackupRuns(t.Context(), row.ID, 1)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("the leg wrote %d rows, want 1", len(runs))
	}
	if strings.Contains(runs[0].Error, pushToken) {
		t.Errorf("backup_run.error leaked the push credential: %s", runs[0].Error)
	}
}
