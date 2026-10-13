package executor

import (
	"testing"
)

func TestParseAnsibleAdhocOutput_Command(t *testing.T) {
	lines := []string{
		"[info] web1 | CHANGED | rc=0 >>",
		"nginx is running",
		"worker pid: 1234",
		"[info] db1 | CHANGED | rc=0 >>",
		"postgresql is running",
		"[info] app1 | FAILED | rc=1 >>",
		"service not found",
	}

	results := ParseAnsibleAdhocOutput(lines)
	if len(results) != 3 {
		t.Fatalf("expected 3 hosts, got %d", len(results))
	}

	web1, ok := results["web1"]
	if !ok {
		t.Fatal("missing web1")
	}
	if web1.Status != "CHANGED" || web1.RC != 0 || !web1.Changed {
		t.Errorf("web1 unexpected: %+v", web1)
	}
	if web1.Stdout != "nginx is running\nworker pid: 1234" {
		t.Errorf("web1 stdout: %q", web1.Stdout)
	}

	db1, ok := results["db1"]
	if !ok {
		t.Fatal("missing db1")
	}
	if db1.Status != "CHANGED" || db1.RC != 0 || db1.Stdout != "postgresql is running" {
		t.Errorf("db1 unexpected: %+v", db1)
	}

	app1, ok := results["app1"]
	if !ok {
		t.Fatal("missing app1")
	}
	if app1.Status != "FAILED" || app1.RC != 1 || app1.Stderr != "service not found" {
		t.Errorf("app1 unexpected: %+v", app1)
	}
}

func TestParseAnsibleAdhocOutput_JSON(t *testing.T) {
	lines := []string{
		"web1 | SUCCESS => {",
		`    "changed": false,`,
		`    "ping": "pong"`,
		"}",
		"nas | UNREACHABLE! => {",
		`    "changed": false,`,
		`    "msg": "Connection refused",`,
		`    "unreachable": true`,
		"}",
	}

	results := ParseAnsibleAdhocOutput(lines)
	if len(results) != 2 {
		t.Fatalf("expected 2 hosts, got %d", len(results))
	}

	web1 := results["web1"]
	if web1.Status != "SUCCESS" || web1.RC != 0 || web1.Changed {
		t.Errorf("web1 unexpected: %+v", web1)
	}

	nas := results["nas"]
	if nas.Status != "UNREACHABLE!" || nas.RC != 1 || nas.Stderr != "Connection refused" {
		t.Errorf("nas unexpected: %+v", nas)
	}
}

func TestParseAnsibleAdhocOutput_Empty(t *testing.T) {
	res := ParseAnsibleAdhocOutput(nil)
	if len(res) != 0 {
		t.Errorf("expected 0, got %d", len(res))
	}
}
