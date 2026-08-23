package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// kubeCaptureSentinel is the sensitive value the bootstrap play "fetches" and
// the chain seals. It may reach the apply's environment by LENGTH and nothing
// else — not an argv, not an event, not a fake tool's recorded command line.
const kubeCaptureSentinel = "KUBECONFIG-CANARY-8b1f4e-apiVersion-v1-clusters-server-https-token-abc123-END"

// fakeCapturePlay is a stand-in ansible-playbook that reads the driver-set
// exocomp_capture_dir out of its --extra-vars file and fetches the sentinel into
// the declared capture file, the way a real play's `fetch` would. It prints a
// non-secret marker and never the value.
func fakeCapturePlay(t *testing.T, captureFile string) string {
	t.Helper()
	body := fmt.Sprintf(`prev=""
ev=""
for a in "$@"; do
  if [ "$prev" = "--extra-vars" ]; then ev="${a#@}"; fi
  prev="$a"
done
if [ -n "$ev" ]; then
  dir=$(sed -n 's/.*"exocomp_capture_dir":"\([^"]*\)".*/\1/p' "$ev")
  if [ -n "$dir" ]; then
    mkdir -p "$(dirname "$dir/%s")"
    printf '%%s' '%s' > "$dir/%s"
  fi
fi
echo BOOTSTRAP_PLAY_RAN
exit 0`, captureFile, kubeCaptureSentinel, captureFile)
	return fakeTool(t, executor.AnsibleBinary, body)
}

// fakeApplyRecordingEnv is a stand-in tofu that records, on apply, the LENGTH of
// the KUBECONFIG env it received (never the value) and any KUBE-named env, to a
// log the test reads. `output -json` prints an empty object so the inventory
// capture is a no-op.
func fakeApplyRecordingEnv(t *testing.T, lenLog string) string {
	t.Helper()
	body := fmt.Sprintf(`case "$1" in
  output) printf '{}' ;;
  apply)
    printf 'KCFG_LEN=%%s\n' "${#KUBECONFIG}" >> %q
    env | sed -n 's/^\(KUBE[A-Z_]*\)=.*/ENVNAME=\1/p' >> %q
    ;;
esac
exit 0`, lenLog, lenLog)
	return fakeTool(t, executor.TofuBinary, body)
}

// TestBootstrapCaptureHandoffEnv is the Phase-2 gate: a bootstrap chain whose
// play fetches a sentinel kubeconfig seals it under ab:<workflow> and the tofu
// apply of the SAME chain receives it as $KUBECONFIG — proven by length, with the
// value absent from every argv and every job event of both steps. A re-run is
// idempotent, and an unrelated bootstrap run (no --capture) inherits nothing.
func TestBootstrapCaptureHandoffEnv(t *testing.T) {
	useDataDir(t, t.TempDir())
	bin := buildExocomp(t)

	lenLog := t.TempDir() + "/apply-env.log"
	ansibleLog := fakeCapturePlay(t, "kubeconfig")
	tofuLog := fakeApplyRecordingEnv(t, lenLog)

	runCLI(t, bin, "init")
	module := t.TempDir()
	writeFile(t, module+"/main.tf", "# empty root module\n")
	runCLI(t, bin, "workspace", "add", "wsp", "--dir", module)
	other := t.TempDir()
	writeFile(t, other+"/main.tf", "# empty root module\n")
	runCLI(t, bin, "workspace", "add", "wsp-other", "--dir", other)
	playbook := module + "/site.yml"
	writeFile(t, playbook, "- hosts: all\n")

	d := startDaemon(t, bin, "serve", "--port", "0", "--reload-interval", "0", "--sandbox", "local")
	d.waitForLine(t, &d.stdout, "scheduler started", 30*time.Second)

	// --- Run 1: capture -> seal -> apply inherits it as env. ---
	out, code := runCLIExit(t, bin, "bootstrap", "run", playbook, "wsp",
		"--capture", "KUBECONFIG=kubeconfig", "--wait")
	if code != 0 || !strings.Contains(out, "succeeded") {
		t.Fatalf("bootstrap run --capture exited %d, want success:\n%s\n%s", code, out, d.dump())
	}
	wf := parseBootstrapWorkflowID(t, out)

	// The apply received $KUBECONFIG at the sentinel's exact length.
	wantLen := "KCFG_LEN=" + strconv.Itoa(len(kubeCaptureSentinel))
	if got := readFile(t, lenLog); !strings.Contains(got, wantLen) {
		t.Fatalf("apply did not receive $KUBECONFIG (%s):\n%s\n%s", wantLen, got, d.dump())
	}
	// The env name reached the child verbatim: KUBECONFIG, never a mangled twin.
	if got := readFile(t, lenLog); !strings.Contains(got, "ENVNAME=KUBECONFIG") {
		t.Errorf("apply env did not carry KUBECONFIG:\n%s", got)
	}

	// The sealed row opens back to the exact sentinel bytes (compared, never
	// printed), proving the value survived the envelope round-trip.
	assertSealedEquals(t, executor.AnsibleOutputSecretName(wf, "KUBECONFIG"), kubeCaptureSentinel)

	// The binding maps KUBECONFIG to the deterministic row, exactly once.
	listing, code := runCLIExit(t, bin, "run-secret", "list", executor.AnsibleOutputBinding(wf))
	if code != 0 {
		t.Fatalf("run-secret list exited %d:\n%s", code, listing)
	}
	if !strings.Contains(listing, "KUBECONFIG") ||
		!strings.Contains(listing, executor.AnsibleOutputSecretName(wf, "KUBECONFIG")) {
		t.Fatalf("ab:<workflow> binding does not map the sealed capture:\n%s", listing)
	}
	// Exactly one pair under the binding: the seal+bind did not duplicate a row.
	assertBindingPairCount(t, executor.AnsibleOutputBinding(wf), "KUBECONFIG", 1)

	// The value is in neither step's recorded argv nor any of their job events.
	steps := workflowSteps(t, wf)
	if len(steps) != 2 {
		t.Fatalf("workflow has %d steps, want 2", len(steps))
	}
	for _, s := range steps {
		if s.Status != store.StepSucceeded {
			t.Fatalf("step %d (%s) is %s, want succeeded\n%s", s.StepNo, s.Kind, s.Status, d.dump())
		}
		if s.JobID == nil {
			t.Fatalf("step %d has no job id", s.StepNo)
		}
		if ev := jobEventText(t, *s.JobID); strings.Contains(ev, kubeCaptureSentinel) {
			t.Errorf("step %d (%s) leaked the sentinel into its events:\n%s", s.StepNo, s.Kind, ev)
		}
	}
	// The fake tools' recorded command lines never carried the value.
	if got := readFile(t, ansibleLog); strings.Contains(got, kubeCaptureSentinel) {
		t.Errorf("the play's argv carried the sentinel:\n%s", got)
	}
	if got := readFile(t, tofuLog); strings.Contains(got, kubeCaptureSentinel) {
		t.Errorf("the apply's argv carried the sentinel:\n%s", got)
	}

	// --- Run 2: a re-run is idempotent — the apply again inherits the value. ---
	writeFile(t, lenLog, "")
	out, code = runCLIExit(t, bin, "bootstrap", "run", playbook, "wsp",
		"--capture", "KUBECONFIG=kubeconfig", "--wait")
	if code != 0 || !strings.Contains(out, "succeeded") {
		t.Fatalf("re-run exited %d, want success:\n%s\n%s", code, out, d.dump())
	}
	if got := readFile(t, lenLog); !strings.Contains(got, wantLen) {
		t.Fatalf("re-run's apply did not receive $KUBECONFIG (%s):\n%s", wantLen, got)
	}

	// --- Unrelated run: a bootstrap without --capture inherits nothing. ---
	writeFile(t, lenLog, "")
	out, code = runCLIExit(t, bin, "bootstrap", "run", playbook, "wsp-other", "--wait")
	if code != 0 || !strings.Contains(out, "succeeded") {
		t.Fatalf("unrelated bootstrap exited %d, want success:\n%s\n%s", code, out, d.dump())
	}
	if got := readFile(t, lenLog); !strings.Contains(got, "KCFG_LEN=0") {
		t.Errorf("an unrelated run inherited a KUBECONFIG:\n%s", got)
	}
	if got := readFile(t, lenLog); strings.Contains(got, "ENVNAME=KUBECONFIG") {
		t.Errorf("an unrelated run's apply carried a KUBECONFIG env:\n%s", got)
	}

	d.signal(t, syscall.SIGTERM)
	d.waitExit(t, 60*time.Second)
	assertNothingLeftRunning(t, d)
}

// shmDataDir returns a data dir on a tmpfs mount, or skips: the file-staging
// path (Phase 3) refuses to run unless the per-job run dir is tmpfs, so an e2e
// that exercises it needs the daemon's data dir — and thus its run/ subtree — on
// RAM-only storage. /dev/shm is tmpfs on a normal Linux host.
func shmDataDir(t *testing.T) string {
	t.Helper()
	const base = "/dev/shm"
	if ok, err := executor.RunDirTmpfs(base); err != nil || !ok {
		t.Skipf("no tmpfs at %s (tmpfs=%v, err=%v)", base, ok, err)
	}
	dir, err := os.MkdirTemp(base, "exocomp-p3-data-")
	if err != nil {
		t.Fatalf("mkdir tmpfs data dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// fakeApplyRecordingFile is a stand-in tofu that, on apply, records the KUBECONFIG
// PATH it received, that file's mode and byte length, and whether its contents
// equal the sentinel — a 1/0, never the value. `output -json` is an empty object.
func fakeApplyRecordingFile(t *testing.T, kcfgLog string) string {
	t.Helper()
	body := fmt.Sprintf(`case "$1" in
  output) printf '{}' ;;
  apply)
    printf 'KCFG_PATH=%%s\n' "$KUBECONFIG" >> %q
    if [ -f "$KUBECONFIG" ]; then
      printf 'KCFG_MODE=%%s\n' "$(stat -c %%a "$KUBECONFIG")" >> %q
      printf 'KCFG_LEN=%%s\n' "$(wc -c < "$KUBECONFIG" | tr -d ' ')" >> %q
      if [ "$(cat "$KUBECONFIG")" = %q ]; then printf 'KCFG_MATCH=1\n' >> %q; else printf 'KCFG_MATCH=0\n' >> %q; fi
    else
      printf 'KCFG_MISSING=1\n' >> %q
    fi
    ;;
esac
exit 0`, kcfgLog, kcfgLog, kcfgLog, kubeCaptureSentinel, kcfgLog, kcfgLog, kcfgLog)
	return fakeTool(t, executor.TofuBinary, body)
}

// TestBootstrapCaptureHandoffFile is the Phase-3 gate: a bootstrap chain whose
// play fetches a sentinel kubeconfig, declared with --capture-file, delivers it
// to the apply as KUBECONFIG=<path> — a 0600 file on the tmpfs run/ tree whose
// contents are the sentinel — with the value absent from every argv and event,
// and the file gone once the job ends.
func TestBootstrapCaptureHandoffFile(t *testing.T) {
	dataRoot := shmDataDir(t)
	useDataDir(t, dataRoot)
	bin := buildExocomp(t)
	dataDir := filepath.Join(dataRoot, "exocomp")

	kcfgLog := t.TempDir() + "/apply-kcfg.log"
	ansibleLog := fakeCapturePlay(t, "kubeconfig")
	tofuLog := fakeApplyRecordingFile(t, kcfgLog)

	runCLI(t, bin, "init")
	module := t.TempDir()
	writeFile(t, module+"/main.tf", "# empty root module\n")
	runCLI(t, bin, "workspace", "add", "wsp", "--dir", module)
	playbook := module + "/site.yml"
	writeFile(t, playbook, "- hosts: all\n")

	d := startDaemon(t, bin, "serve", "--port", "0", "--reload-interval", "0", "--sandbox", "local")
	d.waitForLine(t, &d.stdout, "scheduler started", 30*time.Second)

	out, code := runCLIExit(t, bin, "bootstrap", "run", playbook, "wsp",
		"--capture-file", "KUBECONFIG=kubeconfig", "--wait")
	if code != 0 || !strings.Contains(out, "succeeded") {
		t.Fatalf("bootstrap run --capture-file exited %d, want success:\n%s\n%s", code, out, d.dump())
	}
	wf := parseBootstrapWorkflowID(t, out)

	kcfg := readFile(t, kcfgLog)
	kcfgPath := ""
	for _, l := range strings.Split(kcfg, "\n") {
		if v, ok := strings.CutPrefix(l, "KCFG_PATH="); ok {
			kcfgPath = v
		}
	}
	if kcfgPath == "" {
		t.Fatalf("apply received no KUBECONFIG path:\n%s\n%s", kcfg, d.dump())
	}
	// The path is a file in the run's tmpfs private tree, not a value.
	if !strings.HasPrefix(kcfgPath, filepath.Join(dataDir, "run")) {
		t.Errorf("KUBECONFIG path %q is not under the tmpfs run dir %q", kcfgPath, filepath.Join(dataDir, "run"))
	}
	if filepath.Base(kcfgPath) != "kubeconfig" {
		t.Errorf("staged file basename = %q, want kubeconfig", filepath.Base(kcfgPath))
	}
	// The file the apply read was 0600, the sentinel's length, and its contents.
	if !strings.Contains(kcfg, "KCFG_MODE=600") {
		t.Errorf("staged file was not 0600:\n%s", kcfg)
	}
	if !strings.Contains(kcfg, "KCFG_LEN="+strconv.Itoa(len(kubeCaptureSentinel))) {
		t.Errorf("staged file length != sentinel length:\n%s", kcfg)
	}
	if !strings.Contains(kcfg, "KCFG_MATCH=1") {
		t.Errorf("staged file contents did not equal the sentinel:\n%s", kcfg)
	}

	// The sealed row still round-trips (the seal is identical to env delivery).
	assertSealedEquals(t, executor.AnsibleOutputSecretName(wf, "KUBECONFIG"), kubeCaptureSentinel)

	// The value is in neither step's argv nor any of their job events.
	steps := workflowSteps(t, wf)
	for _, s := range steps {
		if s.Status != store.StepSucceeded {
			t.Fatalf("step %d (%s) is %s, want succeeded\n%s", s.StepNo, s.Kind, s.Status, d.dump())
		}
		if s.JobID != nil {
			if ev := jobEventText(t, *s.JobID); strings.Contains(ev, kubeCaptureSentinel) {
				t.Errorf("step %d (%s) leaked the sentinel into its events:\n%s", s.StepNo, s.Kind, ev)
			}
		}
	}
	if got := readFile(t, ansibleLog); strings.Contains(got, kubeCaptureSentinel) {
		t.Errorf("the play's argv carried the sentinel:\n%s", got)
	}
	if got := readFile(t, tofuLog); strings.Contains(got, kubeCaptureSentinel) {
		t.Errorf("the apply's argv carried the sentinel:\n%s", got)
	}

	// The job is over: the private dir's RemoveAll ran, so the staged file is gone.
	if _, err := os.Stat(kcfgPath); !os.IsNotExist(err) {
		t.Errorf("staged file survived the job: stat err = %v", err)
	}

	d.signal(t, syscall.SIGTERM)
	d.waitExit(t, 60*time.Second)
	assertNothingLeftRunning(t, d)
}

// TestBootstrapCaptureNonPosixSkipped proves Phase-1 skip-with-reason holds
// through the chain: a capture whose declared name folds to a non-POSIX env
// identifier is sealed but not bound and never mangled into a substitute name, so
// the apply inherits no such env.
func TestBootstrapCaptureNonPosixSkipped(t *testing.T) {
	useDataDir(t, t.TempDir())
	bin := buildExocomp(t)

	lenLog := t.TempDir() + "/apply-env.log"
	fakeCapturePlay(t, "kubeconfig")
	fakeApplyRecordingEnv(t, lenLog)

	runCLI(t, bin, "init")
	module := t.TempDir()
	writeFile(t, module+"/main.tf", "# empty root module\n")
	runCLI(t, bin, "workspace", "add", "wsp", "--dir", module)
	playbook := module + "/site.yml"
	writeFile(t, playbook, "- hosts: all\n")

	d := startDaemon(t, bin, "serve", "--port", "0", "--reload-interval", "0", "--sandbox", "local")
	d.waitForLine(t, &d.stdout, "scheduler started", 30*time.Second)

	// "kube-config" folds to "KUBE-CONFIG", not a POSIX identifier.
	out, code := runCLIExit(t, bin, "bootstrap", "run", playbook, "wsp",
		"--capture", "kube-config=kubeconfig", "--wait")
	if code != 0 || !strings.Contains(out, "succeeded") {
		t.Fatalf("bootstrap run exited %d, want success:\n%s\n%s", code, out, d.dump())
	}
	wf := parseBootstrapWorkflowID(t, out)

	// The play step reports the capture as sealed-but-not-bound, with the reason.
	steps := workflowSteps(t, wf)
	playEvents := jobEventText(t, *steps[0].JobID)
	if !strings.Contains(playEvents, "not bound") || !strings.Contains(playEvents, "POSIX identifier") {
		t.Errorf("play did not report the unbindable capture with a reason:\n%s", playEvents)
	}

	// The binding is empty: nothing was bound under a mangled or original name.
	listing, _ := runCLIExit(t, bin, "run-secret", "list", executor.AnsibleOutputBinding(wf))
	if strings.Contains(listing, "KUBE") {
		t.Errorf("an unbindable capture reached the ab:<workflow> binding:\n%s", listing)
	}

	// The apply inherited no KUBE-named env: not the original, not a substitute.
	if got := readFile(t, lenLog); strings.Contains(got, "ENVNAME=KUBE") {
		t.Errorf("the apply inherited a KUBE env from an unbindable capture:\n%s", got)
	}

	d.signal(t, syscall.SIGTERM)
	d.waitExit(t, 60*time.Second)
	assertNothingLeftRunning(t, d)
}

// assertBindingPairCount fails unless the binding maps envName exactly want
// times, read straight from the store so a secret name that embeds the env name
// cannot inflate the count the way a text listing would.
func assertBindingPairCount(t *testing.T, binding, envName string, want int) {
	t.Helper()
	st, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	pairs, err := st.RunSecretsByBinding(t.Context(), binding)
	if err != nil {
		t.Fatalf("run secrets by binding %q: %v", binding, err)
	}
	got := 0
	for _, p := range pairs {
		if p.EnvName == envName {
			got++
		}
	}
	if got != want {
		t.Errorf("binding %q maps %s %d time(s), want %d", binding, envName, got, want)
	}
}

// assertSealedEquals opens the named sealed secret through the data dir's
// envelope and fails unless its plaintext equals want. The value is compared in
// memory and never printed, the only assertion a test of a secret may make on it.
func assertSealedEquals(t *testing.T, secretName, want string) {
	t.Helper()
	env := openEnvelope(t)
	defer func() { _ = env.Close() }()
	st, err := openStore()
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	row, err := st.GetSecret(t.Context(), secretName)
	if err != nil {
		t.Fatalf("get sealed secret %q: %v", secretName, err)
	}
	pt, err := env.OpenSecret(secrets.SealedSecret{
		Name: row.Name, Kind: row.Kind, Nonce: row.Nonce, Ciphertext: row.Ciphertext,
	})
	if err != nil {
		t.Fatalf("open sealed secret %q: %v", secretName, err)
	}
	if string(pt) != want {
		t.Errorf("sealed %q did not round-trip: got %d bytes, want %d", secretName, len(pt), len(want))
	}
}
