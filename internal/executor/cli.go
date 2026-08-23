package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
	"unicode"
)

// KindCLI is the Spec kind this driver serves: an allow-listed binary that is
// neither ansible nor tofu — a kubectl, a scoped token refresh, a vendor CLI —
// run under the same queue, sandbox, run-secret, and approval governance
// (v1.1 roadmap R1). It exists so such work stops escaping through an
// `ansible shell` wrapper, where none of those apply.
const KindCLI = "cli"

// cliEnvAllow is the environment a CLI run inherits. Nothing else passes: a
// governed run must not pick up whatever the operator happened to export, and
// every credential it legitimately needs arrives through RunSecrets instead.
var cliEnvAllow = []string{"HOME", "LANG", "LC_ALL", "PATH", "TZ"}

// CLIArgv builds the argv a queued CLI job carries: the registered tool name,
// then the arguments. argv[0] is the *registered name*, not a path — the job
// row records which allow-list entry was invoked, and the driver resolves that
// entry's executable at run time, so a row can never name a binary the
// allow-list does not.
//
// The argv is a list and stays a list. It is never joined into a string and
// never handed to a shell, so a metacharacter in an argument is one literal
// argv element with no meaning to anything.
func CLIArgv(tool string, args ...string) []string {
	argv := make([]string, 0, len(args)+1)
	argv = append(argv, tool)
	return append(argv, args...)
}

// ValidateCLIArgs is the structural guard every CLI argument passes regardless
// of what the registration allows: a NUL truncates an argv element and any
// other control character splits or hides it in an event line. It is not an
// injection guard — there is no shell to inject into — it keeps the recorded
// argv and the executed argv the same bytes.
func ValidateCLIArgs(args []string) error {
	for _, a := range args {
		if i := strings.IndexFunc(a, unicode.IsControl); i >= 0 {
			return fmt.Errorf("executor: cli: argument %q contains a control character at byte %d", a, i)
		}
	}
	return nil
}

// CLIOptions configures the driver. Like AnsibleOptions and TofuOptions it
// holds what a Spec must not: specs are persisted, options are resolved per
// execution from the allow-list entry and the envelope.
type CLIOptions struct {
	// Tool is the registered allow-list name. It must equal argv[0] of the
	// spec, which is the driver's half of the allow-list check: the CLI refuses
	// before enqueue, and the driver refuses to run a row it was not built for.
	Tool string
	// Binary is the executable the tool resolves to: an absolute path, or a
	// bare name looked up on PATH. Empty falls back to Tool.
	Binary string
	// PrivateDirRoot is where the per-run private directory is created. Empty
	// uses the system temp dir.
	PrivateDirRoot string
	// Env adds explicit KEY=VALUE entries on top of the inherited allowlist.
	Env []string
	// RunSecrets is the credential-plane binding: sealed values exported as
	// named env for this run alone. A provider token reaches the process this
	// way and never as an argument, because argv is recorded on the job row and
	// visible in the process table, while the environment is neither.
	RunSecrets RunSecrets
	// AllowHTTPS is the tool's declared egress. It reaches the sandbox profile,
	// not this driver: an unsandboxed tier has no egress policy to apply.
	AllowHTTPS bool
}

// CLI runs one allow-listed binary. It assembles argv and the environment,
// then hands the process to the local shell driver so streaming, process
// groups, and exit-code handling keep one implementation — and so exit code
// alone decides the job (AGENTS.md invariant 3): this driver reclassifies
// nothing.
type CLI struct {
	opts  CLIOptions
	inner Driver
}

// NewCLI returns the governed CLI driver.
func NewCLI(opts CLIOptions) *CLI { return &CLI{opts: opts, inner: NewShell()} }

// Run executes the tool named by spec.Argv[0] with the rest of the argv.
func (c *CLI) Run(ctx context.Context, spec Spec, sink EventSink) (Result, error) {
	bin, err := c.resolve(spec)
	if err != nil {
		return Result{}, err
	}
	if err := ValidateCLIArgs(spec.Argv[1:]); err != nil {
		return Result{}, err
	}

	private, err := os.MkdirTemp(c.opts.PrivateDirRoot, "exocomp-cli-")
	if err != nil {
		return Result{}, fmt.Errorf("executor: cli: private dir: %w", err)
	}
	defer func() {
		if rmErr := os.RemoveAll(private); rmErr != nil {
			sink.Emit(Event{
				Ts:    time.Now().UTC(),
				Level: LevelSystem,
				Msg:   fmt.Sprintf("private dir %s not removed: %v", private, rmErr),
			})
		}
	}()
	if err := os.Chmod(private, 0o700); err != nil {
		return Result{}, fmt.Errorf("executor: cli: private dir mode: %w", err)
	}

	env, err := c.env(private)
	if err != nil {
		return Result{}, err
	}

	argv := make([]string, 0, len(spec.Argv))
	argv = append(argv, bin)
	argv = append(argv, spec.Argv[1:]...)
	spec.Argv = argv
	spec.Env = env

	sink.Emit(Event{Ts: time.Now().UTC(), Level: LevelSystem, Msg: "cli " + c.opts.Tool})
	return c.inner.Run(ctx, spec, sink)
}

// resolve validates the spec against the allow-list entry this driver was
// built from and resolves the executable.
func (c *CLI) resolve(spec Spec) (string, error) {
	if spec.Kind != "" && spec.Kind != KindCLI {
		return "", fmt.Errorf("executor: cli: spec kind is %q, want %s", spec.Kind, KindCLI)
	}
	if len(spec.Argv) == 0 {
		return "", errors.New("executor: cli: argv needs a tool name")
	}
	if c.opts.Tool == "" {
		return "", errors.New("executor: cli: no allow-listed tool; refusing to run")
	}
	if spec.Argv[0] != c.opts.Tool {
		return "", fmt.Errorf("executor: cli: argv[0] is %q but this driver serves the allow-listed tool %q",
			spec.Argv[0], c.opts.Tool)
	}

	bin := c.opts.Binary
	if bin == "" {
		bin = c.opts.Tool
	}
	resolved, err := exec.LookPath(bin)
	if err != nil {
		return "", fmt.Errorf("executor: cli: %w", err)
	}
	return resolved, nil
}

// env builds the child environment: the allowlist over the parent, the run's
// own scratch, then the caller's explicit entries and the bound credentials.
// The credentials go last so a bound name wins over an inherited one.
func (c *CLI) env(private string) ([]string, error) {
	out := make([]string, 0, len(cliEnvAllow)+4)
	for _, kv := range os.Environ() {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if slices.Contains(cliEnvAllow, name) {
			out = append(out, kv)
		}
	}
	// A tool that needs to write something needs somewhere granted to write it,
	// and the system temp dir is not granted under the sandbox.
	out = append(out, "TMPDIR="+private)
	out = append(out, c.opts.Env...)

	secretEnv, err := runSecretEnv(c.opts.RunSecrets)
	if err != nil {
		return nil, err
	}
	return append(out, secretEnv...), nil
}
