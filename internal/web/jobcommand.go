package web

import (
	"net/http"
	"path/filepath"
	"strings"
	"sync"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// cmdGlanceRunes caps the command text a jobs-list row carries. The row also
// ellipsizes in CSS, so this only bounds the markup and keeps a pasted script
// from becoming a page of hidden text.
const cmdGlanceRunes = 100

// cmdFullRunes caps the hover and detail text, the whole command in practice.
const cmdFullRunes = 2000

// userSessionExport is the prefix `exocomp command --user-session` puts in
// front of the operator's command. Every such row would otherwise open with
// the same environment export and hide what was run.
const userSessionExport = "export XDG_RUNTIME_DIR="

// adhocParts is an ad-hoc ansible job read back out of its argv:
// `ansible <pattern> [--inventory I] --module-name M [--args A] [flags]`.
type adhocParts struct {
	Pattern string
	Module  string
	Args    string
}

// adhocValueFlags are the ansible options that consume the next argv element,
// so a value such as an inventory path is never mistaken for the host pattern.
var adhocValueFlags = map[string]struct{}{
	"--inventory": {}, "-i": {}, "--module-name": {}, "-m": {}, "--args": {}, "-a": {},
	"--limit": {}, "-l": {}, "--extra-vars": {}, "-e": {}, "--user": {}, "-u": {},
	"--connection": {}, "-c": {}, "--forks": {}, "-f": {}, "--timeout": {}, "-T": {},
	"--private-key": {}, "--key-file": {}, "--ssh-common-args": {}, "--ssh-extra-args": {},
	"--vault-id": {}, "--vault-password-file": {}, "--become-user": {}, "--become-method": {},
	"--tree": {}, "-t": {}, "--poll": {}, "-P": {}, "--background": {}, "-B": {},
	"--module-path": {}, "-M": {}, "--playbook-dir": {}, "--task-timeout": {},
	"--become-password-file": {}, "--become-pass-file": {}, "--connection-password-file": {},
	"--conn-pass-file": {},
}

// title is the PLAYBOOK cell's text for the run: "module · pattern", or the
// module alone when the argv names no pattern.
func (p adhocParts) title() string {
	if p.Pattern == "" {
		return p.Module
	}
	return p.Module + " · " + p.Pattern
}

// parseAdhoc reads an ad-hoc ansible argv by flag name, never by position:
// the host pattern is the first non-flag token and the module and args are
// the values of their own flags, so a trailing `--become` or a jinja `msg=...`
// argument cannot be mistaken for any of them. ok is false when argv is not an
// ad-hoc ansible invocation.
func parseAdhoc(argv []string) (p adhocParts, ok bool) {
	if len(argv) == 0 || filepath.Base(argv[0]) != executor.AnsibleAdhocBinary {
		return adhocParts{}, false
	}
	rest := argv[1:]
	for i := 0; i < len(rest); i++ {
		tok := rest[i]
		if tok == "--" {
			if p.Pattern == "" && i+1 < len(rest) {
				p.Pattern = rest[i+1]
			}
			break
		}
		if !strings.HasPrefix(tok, "-") || tok == "-" {
			if p.Pattern == "" {
				p.Pattern = tok
			}
			continue
		}
		name, val, hasVal := tok, "", false
		if strings.HasPrefix(tok, "--") {
			if k, v, found := strings.Cut(tok, "="); found {
				name, val, hasVal = k, v, true
			}
		}
		if _, takes := adhocValueFlags[name]; takes && !hasVal && i+1 < len(rest) {
			i++
			val, hasVal = rest[i], true
		}
		if !hasVal {
			continue
		}
		switch name {
		case "--module-name", "-m":
			p.Module = val
		case "--args", "-a":
			p.Args = val
		}
	}
	if p.Module == "" {
		p.Module = "command"
	}
	return p, true
}

// oneLine collapses every run of whitespace to a single space, so a multi-line
// shell script reads as one glanceable line.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// clipRunes cuts s to at most n runes and marks the cut with an ellipsis.
func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimRight(string(r[:n-1]), " ") + "…"
}

// stripUserSession drops the `export XDG_RUNTIME_DIR=...; ` prefix that
// `command --user-session` prepends, so the row names the operator's command.
func stripUserSession(args string) string {
	if !strings.HasPrefix(args, userSessionExport) {
		return args
	}
	if _, after, found := strings.Cut(args, "; "); found {
		return after
	}
	return args
}

// jobCommand is what a jobs-list row shows of the command a job ran.
type jobCommand struct {
	// Title replaces the PLAYBOOK cell for an ad-hoc run: "module · pattern".
	// Empty when the job is not an ad-hoc run, which keeps its runbook title.
	Title string
	// Glance is the redacted, one-line, clipped command text under the title.
	Glance string
	// Full is the redacted command text for the hover and the detail tile.
	Full string
}

// jobTitleOf is the PLAYBOOK cell's text for an ad-hoc ansible run, "" for
// every other job. It carries no command text, so it needs no redaction.
func jobTitleOf(j store.Job) string {
	if j.Kind != executor.KindAnsible {
		return ""
	}
	p, ok := parseAdhoc(j.Args.Argv)
	if !ok {
		return ""
	}
	return p.title()
}

// commandOf derives a job's command text from its recorded argv. Ad-hoc
// ansible runs yield the module args, CLI runs yield the tool's arguments, and
// everything else yields nothing. redact scrubs known secret values before the
// text is clipped, so a value is never half-cut into something the redactor
// would no longer recognise.
func commandOf(j store.Job, redact func(string) string) jobCommand {
	var cmd jobCommand
	var text string
	switch j.Kind {
	case executor.KindAnsible:
		p, ok := parseAdhoc(j.Args.Argv)
		if !ok {
			return cmd
		}
		cmd.Title = p.title()
		text = stripUserSession(p.Args)
	case executor.KindCLI:
		if len(j.Args.Argv) < 2 {
			return cmd
		}
		text = strings.Join(j.Args.Argv[1:], " ")
	default:
		return cmd
	}
	text = oneLine(redact(text))
	if text == "" {
		return cmd
	}
	cmd.Glance = clipRunes(text, cmdGlanceRunes)
	cmd.Full = clipRunes(text, cmdFullRunes)
	return cmd
}

// hideCommand is the scrubber used when command text must not be shown: it
// drops the text rather than risk a secret in it.
func hideCommand(string) string { return "" }

// commandRedactor returns the scrubber one request applies to job command
// text, and it fails closed. The text is argv the operator typed, so it is
// shown only to a viewer who may also run jobs (the job API omits Args for
// the same reason, and the rerun and schedule dialogs that echo the argv are
// operator only). For that viewer it is shown only once a redactor has been
// resolved from a complete secret set: no redactor, a resolve error or a nil
// filter all drop the text. The redactor is resolved on first use, because
// opening every sealed secret is wasted on a page with no command to show,
// and then reused for the rest of the request.
func (s *Server) commandRedactor(r *http.Request) func(string) string {
	ident, _ := identityOf(r.Context())
	if s.redactor == nil || !canWrite(ident) {
		return hideCommand
	}
	var (
		once sync.Once
		red  *secrets.Redactor
	)
	return func(text string) string {
		once.Do(func() {
			if got, err := s.redactor(r.Context()); err == nil {
				red = got
			}
		})
		if red == nil {
			return hideCommand(text)
		}
		return string(red.Redact([]byte(text)))
	}
}
