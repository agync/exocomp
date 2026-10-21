package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

// TestAPIPlaybookRunGrantsAnAllowedIdentity: an identity file the operator
// allowed lands on the enqueued job row, the seam the sandboxed daemon reads
// its single-file grant from.
func TestAPIPlaybookRunGrantsAnAllowedIdentity(t *testing.T) {
	h := newHarness(t)
	key := writeIdentity(t, filepath.Join(t.TempDir(), "id_ed25519"))
	h.server.sshIdentityAllow = []string{key}
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "op", "operator")

	code, _, body := bearerPost(t, ts.URL, "/api/v1/playbooks/run", token,
		`{"playbook":"/srv/site.yml","ssh_identity_files":["`+key+`"]}`, nil)
	if code != http.StatusOK {
		t.Fatalf("run status = %d, want 200 (%s)", code, body)
	}
	_, _, data := decodeEnvelope(t, body)
	var jv jobEnqueuedView
	if err := json.Unmarshal(data, &jv); err != nil || jv.JobID == 0 {
		t.Fatalf("no job_id: %v (%s)", err, body)
	}
	job, err := h.store.GetJob(context.Background(), jv.JobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if !slices.Equal(job.Args.SSHIdentityFiles, []string{key}) {
		t.Fatalf("job SSHIdentityFiles = %v, want the allowed file", job.Args.SSHIdentityFiles)
	}
}

// TestAPIPlaybookRunRefusesAnIdentityNotAllowed: a caller names paths on a
// host it does not control, so any file the operator did not allow, a
// relative path, or any file at all on a daemon that allows none, is a 400
// before anything is enqueued.
func TestAPIPlaybookRunRefusesAnIdentityNotAllowed(t *testing.T) {
	for name, allow := range map[string][]string{
		"none allowed":  nil,
		"other allowed": {"/run/keys/fleet/id_ed25519"},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.server.sshIdentityAllow = allow
			ts := httptest.NewServer(h.server.Handler())
			defer ts.Close()
			token := h.mint(t, "op", "operator")

			for _, ids := range []string{`["/etc/ssh/ssh_host_ed25519_key"]`, `["keys/id"]`,
				`["/run/keys/fleet/id_ed25519","/run/keys/other/id"]`} {
				code, _, body := bearerPost(t, ts.URL, "/api/v1/playbooks/run", token,
					`{"playbook":"/srv/site.yml","ssh_identity_files":`+ids+`}`, nil)
				if code != http.StatusBadRequest {
					t.Fatalf("ssh_identity_files=%s status = %d, want 400 (%s)", ids, code, body)
				}
			}
			jobs, err := h.store.ListJobs(context.Background(), store.JobFilter{Runbook: "playbook"})
			if err != nil {
				t.Fatalf("ListJobs: %v", err)
			}
			if len(jobs) != 0 {
				t.Fatalf("a refused identity still created %d job(s)", len(jobs))
			}
		})
	}
}

// TestAPIPlaybookRunRefusesAListedIdentitySwappedIntoTheDataDir: the list is
// checked when serve starts, and again on every request, so a listed path
// replaced by a symlink into the data dir afterwards is refused.
func TestAPIPlaybookRunRefusesAListedIdentitySwappedIntoTheDataDir(t *testing.T) {
	h := newHarness(t)
	target := writeIdentity(t, filepath.Join(h.dir, "keys", "id"))
	key := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.Symlink(target, key); err != nil {
		t.Fatal(err)
	}
	h.server.sshIdentityAllow = []string{key}
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	token := h.mint(t, "op", "operator")

	code, _, body := bearerPost(t, ts.URL, "/api/v1/playbooks/run", token,
		`{"playbook":"/srv/site.yml","ssh_identity_files":["`+key+`"]}`, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("swapped identity status = %d, want 400 (%s)", code, body)
	}
}

func writeIdentity(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not-a-real-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
