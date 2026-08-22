package executor

import (
	"strings"
	"testing"
)

// TestParseAnsibleRecap proves the PLAY RECAP parses into per-host changed
// counts: clean hosts are counted, changed>0 hosts become drift, and the
// values behind the diff never enter the structured outcome.
func TestParseAnsibleRecap(t *testing.T) {
	recap := strings.Split(strings.TrimSpace(`
PLAY [all] *********************************************************************
TASK [config] ******************************************************************
PLAY RECAP *********************************************************************
web1                       : ok=5    changed=2    unreachable=0    failed=0    skipped=1    rescued=0    ignored=0
web2                       : ok=5    changed=0    unreachable=0    failed=0    skipped=1    rescued=0    ignored=0
db1                        : ok=4    changed=3    unreachable=0    failed=0    skipped=0    rescued=0    ignored=0
`), "\n")

	outcome, found := ParseAnsibleRecap(recap)
	if !found {
		t.Fatal("recap not found")
	}
	if outcome.Operation != "check" {
		t.Errorf("operation = %q, want check", outcome.Operation)
	}
	if outcome.CleanHosts != 1 {
		t.Errorf("clean hosts = %d, want 1 (web2)", outcome.CleanHosts)
	}
	if !outcome.Changes {
		t.Error("Changes should be true with two drifting hosts")
	}
	if len(outcome.Drift) != 2 {
		t.Fatalf("drift = %+v, want web1 and db1", outcome.Drift)
	}
	byHost := map[string]int{}
	for _, d := range outcome.Drift {
		byHost[d.Host] = d.Changed
	}
	if byHost["web1"] != 2 || byHost["db1"] != 3 {
		t.Errorf("changed counts = %v, want web1:2 db1:3", byHost)
	}
}

// TestParseAnsibleRecapNoRecap proves a run whose output carries no recap is
// reported as not-found, so the driver does not pass it off as "all clean".
func TestParseAnsibleRecapNoRecap(t *testing.T) {
	_, found := ParseAnsibleRecap([]string{"ERROR! playbook not found", "fatal: ..."})
	if found {
		t.Fatal("no PLAY RECAP was printed, found should be false")
	}
}

// TestParseAnsibleRecapAllClean proves a dry run where nothing would change
// reports every host clean and no drift.
func TestParseAnsibleRecapAllClean(t *testing.T) {
	recap := []string{
		"PLAY RECAP *****",
		"web1 : ok=5    changed=0    unreachable=0    failed=0",
		"web2 : ok=5    changed=0    unreachable=0    failed=0",
	}
	outcome, found := ParseAnsibleRecap(recap)
	if !found {
		t.Fatal("recap not found")
	}
	if outcome.Changes || len(outcome.Drift) != 0 {
		t.Errorf("expected no drift, got %+v", outcome)
	}
	if outcome.CleanHosts != 2 {
		t.Errorf("clean hosts = %d, want 2", outcome.CleanHosts)
	}
}
