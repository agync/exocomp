package executor_test

import (
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/executor"
)

// emptyInventoryRecap is what ansible prints for a run whose inventory parses to
// zero hosts: the play skips with the no-hosts banner and the PLAY RECAP has no
// host rows. The process still exits 0 — the silent-green case BUG 2 guards.
const emptyInventoryRecap = `echo "PLAY [all] ****"
echo "skipping: no hosts matched"
echo ""
echo "PLAY RECAP ****"
exit 0
`

// populatedRecap is an ordinary successful run: one host in the PLAY RECAP.
const populatedRecap = `echo "PLAY [all] ****"
echo "TASK [ping] ****"
echo "ok: [node0]"
echo ""
echo "PLAY RECAP ****"
echo "node0                      : ok=1    changed=0    unreachable=0    failed=0    skipped=0    rescued=0    ignored=0"
exit 0
`

func hasNoHostsWarning(msgs []string) bool {
	for _, m := range msgs {
		if strings.Contains(m, "no hosts matched") {
			return true
		}
	}
	return false
}

// TestNoHostsMatchedWarns proves a clean run that matched zero hosts surfaces the
// warning while keeping exit 0: a mis-pointed inventory must not pass as silent
// green.
func TestNoHostsMatchedWarns(t *testing.T) {
	t.Parallel()
	res, sink := runAnsible(t, executor.AnsibleOptions{
		Binary:         fakeAnsible(t, emptyInventoryRecap),
		PrivateDirRoot: t.TempDir(),
	}, executor.AnsibleArgv("/p/site.yml", "/p/hosts.ini"))

	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0 (the warning must not change the exit code)", res.ExitCode)
	}
	if !hasNoHostsWarning(sink.msgs(executor.LevelSystem)) {
		t.Fatalf("no zero-host warning emitted: %v", sink.msgs(""))
	}
}

// TestHostsMatchedNoWarning proves an ordinary run with a host in the recap does
// not warn: the guard fires only when nothing matched.
func TestHostsMatchedNoWarning(t *testing.T) {
	t.Parallel()
	_, sink := runAnsible(t, executor.AnsibleOptions{
		Binary:         fakeAnsible(t, populatedRecap),
		PrivateDirRoot: t.TempDir(),
	}, executor.AnsibleArgv("/p/site.yml", "/p/hosts.ini"))

	if hasNoHostsWarning(sink.msgs("")) {
		t.Fatalf("a run that matched a host emitted the zero-host warning: %v", sink.msgs(""))
	}
}

// TestNoHostsFailedRunNoWarning proves the guard is scoped to exit 0: a run that
// failed (non-zero) is not reclassified as a no-hosts warning, so a real failure
// keeps its own signal.
func TestNoHostsFailedRunNoWarning(t *testing.T) {
	t.Parallel()
	_, sink := runAnsible(t, executor.AnsibleOptions{
		Binary:         fakeAnsible(t, "echo 'boom' >&2\nexit 2\n"),
		PrivateDirRoot: t.TempDir(),
	}, executor.AnsibleArgv("/p/site.yml", "/p/hosts.ini"))

	if hasNoHostsWarning(sink.msgs("")) {
		t.Fatalf("a failed run emitted the zero-host warning: %v", sink.msgs(""))
	}
}

// TestLocalhostRecapNoWarning proves a run whose recap shows localhost as a real
// host row (localhost reached through ansible's own inventory discovery) does not
// falsely emit the zero-host warning.
func TestLocalhostRecapNoWarning(t *testing.T) {
	t.Parallel()
	const localhostRecap = `echo "PLAY RECAP ****"
echo "localhost                  : ok=1    changed=0    unreachable=0    failed=0    skipped=0    rescued=0    ignored=0"
exit 0
`
	_, sink := runAnsible(t, executor.AnsibleOptions{
		Binary:         fakeAnsible(t, localhostRecap),
		PrivateDirRoot: t.TempDir(),
	}, executor.AnsibleArgv("/p/site.yml", ""))

	if hasNoHostsWarning(sink.msgs("")) {
		t.Fatalf("the localhost default run emitted the zero-host warning: %v", sink.msgs(""))
	}
}
