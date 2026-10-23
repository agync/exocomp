package web

import (
	"strings"
	"testing"
	"unicode/utf8"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/store"
)

func keep(s string) string { return s }

func adhocJob(runbook string, argv []string) store.Job {
	return store.Job{Runbook: runbook, Kind: executor.KindAnsible, Args: store.JobArgs{Argv: argv}}
}

func TestParseAdhoc(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want adhocParts
		ok   bool
	}{
		{
			name: "command with become last",
			argv: executor.AnsibleAdhocArgv("host-a", "/inv/hosts", "command", "systemctl status foo", "--become"),
			want: adhocParts{Pattern: "host-a", Module: "command", Args: "systemctl status foo"}, ok: true,
		},
		{
			name: "ping takes no args",
			argv: executor.AnsibleAdhocArgv("host-a", "", "ping", ""),
			want: adhocParts{Pattern: "host-a", Module: "ping"}, ok: true,
		},
		{
			name: "jinja msg is args, not a title",
			argv: executor.AnsibleAdhocArgv("host-a", "/inv", "debug", "msg={{ inventory_hostname }}"),
			want: adhocParts{Pattern: "host-a", Module: "debug", Args: "msg={{ inventory_hostname }}"}, ok: true,
		},
		{
			name: "host expression and dotted module",
			argv: executor.AnsibleAdhocArgv("group-a:!host-b", "", "ansible.builtin.setup", "filter=ansible_os_*"),
			want: adhocParts{Pattern: "group-a:!host-b", Module: "ansible.builtin.setup", Args: "filter=ansible_os_*"}, ok: true,
		},
		{
			name: "short flags and equals form",
			argv: []string{"ansible", "-i", "/inv", "-m", "shell", "--args=uptime | cat", "host-a"},
			want: adhocParts{Pattern: "host-a", Module: "shell", Args: "uptime | cat"}, ok: true,
		},
		{
			name: "value flags do not become the pattern",
			argv: []string{"ansible", "--limit", "host-b", "--forks", "5", "host-a", "-m", "ping"},
			want: adhocParts{Pattern: "host-a", Module: "ping"}, ok: true,
		},
		{
			name: "args value that looks like a flag",
			argv: []string{"ansible", "host-a", "-m", "command", "-a", "--version"},
			want: adhocParts{Pattern: "host-a", Module: "command", Args: "--version"}, ok: true,
		},
		{
			name: "double dash ends options",
			argv: []string{"ansible", "-m", "ping", "--", "host-a"},
			want: adhocParts{Pattern: "host-a", Module: "ping"}, ok: true,
		},
		{
			name: "trailing flag with no value",
			argv: []string{"ansible", "host-a", "-m"},
			want: adhocParts{Pattern: "host-a", Module: "command"}, ok: true,
		},
		{name: "bare binary", argv: []string{"ansible"}, want: adhocParts{Module: "command"}, ok: true},
		{name: "absolute binary path", argv: []string{"/usr/bin/ansible", "host-a"}, want: adhocParts{Pattern: "host-a", Module: "command"}, ok: true},
		{name: "playbook is not ad hoc", argv: []string{"ansible-playbook", "site.yml"}},
		{name: "empty argv", argv: nil},
		{name: "other tool", argv: []string{"kubectl", "get", "pods"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseAdhoc(tc.argv)
			if ok != tc.ok || got != tc.want {
				t.Errorf("parseAdhoc(%q) = %+v, %v; want %+v, %v", tc.argv, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestAdhocTitleWithoutPattern(t *testing.T) {
	if got := jobTitle(adhocJob("ansible", []string{"ansible", "-m", "ping"})); got != "ping" {
		t.Errorf("jobTitle = %q, want ping with no dangling separator", got)
	}
	argv := []string{"ansible", "--module-path", "/mods", "host-a", "-m", "ping"}
	if got := jobTitle(adhocJob("ansible", argv)); got != "ping · host-a" {
		t.Errorf("jobTitle = %q, want ping · host-a", got)
	}
}

func TestJobTitleAdhoc(t *testing.T) {
	// The runbook label is only "command" or "ansible", and the last argv
	// element is whatever came last: neither may name the row.
	for _, runbook := range []string{"command", "ansible", "ping"} {
		j := adhocJob(runbook, executor.AnsibleAdhocArgv("host-a", "/inv", "ping", "", "--become"))
		if got := jobTitle(j); got != "ping · host-a" {
			t.Errorf("runbook %q: jobTitle = %q, want %q", runbook, got, "ping · host-a")
		}
	}
	j := adhocJob("ansible", executor.AnsibleAdhocArgv("host-a", "", "debug", "msg=hello"))
	if got := jobTitle(j); got != "debug · host-a" {
		t.Errorf("jobTitle = %q, want debug · host-a", got)
	}
}

func TestJobTitlePlaybookUnchanged(t *testing.T) {
	j := store.Job{
		Runbook: "deploy", Kind: executor.KindAnsible,
		Args: store.JobArgs{Dir: "/work/src", Argv: []string{"ansible-playbook", "/work/src/deploy/app.yml"}},
	}
	if got := jobTitle(j); got != "deploy/app.yml" {
		t.Errorf("jobTitle = %q, want deploy/app.yml", got)
	}
	if c := commandOf(j, keep); c != (jobCommand{}) {
		t.Errorf("a playbook run carries no command text, got %+v", c)
	}
}

func TestCommandOf(t *testing.T) {
	t.Run("module args", func(t *testing.T) {
		j := adhocJob("command", executor.AnsibleAdhocArgv("host-a", "/inv", "command", "systemctl status foo", "--become"))
		got := commandOf(j, keep)
		want := jobCommand{Title: "command · host-a", Glance: "systemctl status foo", Full: "systemctl status foo"}
		if got != want {
			t.Errorf("commandOf = %+v, want %+v", got, want)
		}
	})
	t.Run("no args", func(t *testing.T) {
		got := commandOf(adhocJob("ansible", executor.AnsibleAdhocArgv("host-a", "", "ping", "")), keep)
		if got.Title != "ping · host-a" || got.Glance != "" || got.Full != "" {
			t.Errorf("commandOf = %+v, want a title and no command text", got)
		}
	})
	t.Run("whitespace collapses to one line", func(t *testing.T) {
		j := adhocJob("command", executor.AnsibleAdhocArgv("host-a", "", "shell", "set -e\n\techo  one\r\necho two"))
		if got := commandOf(j, keep).Glance; got != "set -e echo one echo two" {
			t.Errorf("Glance = %q", got)
		}
	})
	t.Run("glance is clipped and full is not", func(t *testing.T) {
		long := strings.Repeat("é", 250)
		got := commandOf(adhocJob("command", executor.AnsibleAdhocArgv("host-a", "", "shell", long)), keep)
		if n := utf8.RuneCountInString(got.Glance); n != cmdGlanceRunes || !strings.HasSuffix(got.Glance, "…") {
			t.Errorf("Glance has %d runes (%q), want %d ending in an ellipsis", n, got.Glance, cmdGlanceRunes)
		}
		if !utf8.ValidString(got.Glance) || got.Full != long {
			t.Errorf("Full = %d runes, valid glance = %v; want the whole command", utf8.RuneCountInString(got.Full), utf8.ValidString(got.Glance))
		}
	})
	t.Run("full is capped", func(t *testing.T) {
		got := commandOf(adhocJob("command", executor.AnsibleAdhocArgv("host-a", "", "shell", strings.Repeat("x", 5000))), keep)
		if n := utf8.RuneCountInString(got.Full); n != cmdFullRunes {
			t.Errorf("Full has %d runes, want %d", n, cmdFullRunes)
		}
	})
	t.Run("user-session prefix is dropped", func(t *testing.T) {
		args := `export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}" DBUS_SESSION_BUS_ADDRESS="x"; systemctl --user status foo`
		got := commandOf(adhocJob("command", executor.AnsibleAdhocArgv("host-a", "", "shell", args)), keep)
		if got.Glance != "systemctl --user status foo" {
			t.Errorf("Glance = %q", got.Glance)
		}
	})
	t.Run("cli run shows the tool arguments", func(t *testing.T) {
		j := store.Job{Runbook: "cli:kubectl", Kind: executor.KindCLI, Args: store.JobArgs{Argv: executor.CLIArgv("kubectl", "get", "pods", "-n", "ns-a")}}
		got := commandOf(j, keep)
		if got.Glance != "get pods -n ns-a" || got.Title != "" {
			t.Errorf("commandOf = %+v", got)
		}
	})
	t.Run("cli run with no arguments", func(t *testing.T) {
		j := store.Job{Runbook: "cli:kubectl", Kind: executor.KindCLI, Args: store.JobArgs{Argv: []string{"kubectl"}}}
		if got := commandOf(j, keep); got != (jobCommand{}) {
			t.Errorf("commandOf = %+v, want empty", got)
		}
	})
	t.Run("other kinds carry nothing", func(t *testing.T) {
		j := store.Job{Runbook: "nightly", Kind: "shell", Args: store.JobArgs{Argv: []string{"/bin/sh", "-c", "echo hi"}}}
		if got := commandOf(j, keep); got != (jobCommand{}) {
			t.Errorf("commandOf = %+v, want empty", got)
		}
	})
}

// TestCommandOfRedacts: a known secret is scrubbed before the text is clipped,
// so it is absent from the glance and the full text alike, including when it
// straddles the clip boundary.
func TestCommandOfRedacts(t *testing.T) {
	const secret = "hunter2-run-secret-value"
	redact := func(s string) string { return strings.ReplaceAll(s, secret, "[REDACTED]") }

	t.Run("short", func(t *testing.T) {
		j := adhocJob("command", executor.AnsibleAdhocArgv("host-a", "", "shell", "curl -H 'X-Key: "+secret+"' url"))
		got := commandOf(j, redact)
		for _, text := range []string{got.Glance, got.Full} {
			if strings.Contains(text, secret) || !strings.Contains(text, "[REDACTED]") {
				t.Errorf("secret handling wrong in %q", text)
			}
		}
	})
	t.Run("across the clip boundary", func(t *testing.T) {
		pad := strings.Repeat("a", cmdGlanceRunes-10)
		j := adhocJob("command", executor.AnsibleAdhocArgv("host-a", "", "shell", pad+secret))
		got := commandOf(j, redact)
		if strings.Contains(got.Glance, "hunter2") || strings.Contains(got.Full, secret) {
			t.Errorf("secret leaked: glance %q", got.Glance)
		}
	})
	t.Run("cli arguments", func(t *testing.T) {
		j := store.Job{Runbook: "cli:tool", Kind: executor.KindCLI, Args: store.JobArgs{Argv: []string{"tool", "--token", secret}}}
		if got := commandOf(j, redact); strings.Contains(got.Glance+got.Full, secret) {
			t.Errorf("secret leaked: %+v", got)
		}
	})
}
