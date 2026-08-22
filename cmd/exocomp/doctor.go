package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"exocomp.dev/exocomp/internal/api"
	"exocomp.dev/exocomp/internal/litestream"
	"exocomp.dev/exocomp/internal/metrics"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// optionalTool is a tool exocompd shells out to that is not required in M0.
type optionalTool struct {
	name string
	args []string
}

var optionalTools = []optionalTool{
	{name: "ansible-playbook", args: []string{"--version"}},
	{name: "tofu", args: []string{"version"}},
	{name: "restic", args: []string{"version"}},
	{name: "age", args: []string{"--version"}},
	{name: "sops", args: []string{"--version"}},
	// uv resolves project dependency manifests at runtime and git syncs git
	// sources; both are declared runtime dependencies (ADR 0011), so a missing
	// one is a real gap the operator should see rather than discover mid-run.
	{name: "uv", args: []string{"--version"}},
	{name: "git", args: []string{"--version"}},
}

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Run environment checks",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDoctor(cmd.OutOrStdout())
		},
	}
}

func runDoctor(w io.Writer) error {
	p := func(format string, a ...any) {
		_, _ = fmt.Fprintf(w, format, a...)
	}
	p("go runtime:   %s\n", runtime.Version())

	dataDir, err := dataDir()
	if err != nil {
		return fmt.Errorf("doctor: resolve data dir: %w", err)
	}
	report, fault := inspectDataDir(dataDir)
	p("data dir:     %s — %s\n", dataDir, report)
	if fault != nil {
		return fmt.Errorf("doctor: %w", fault)
	}

	reportSecretsProtection(w, dataDir)

	p("optional tools (M0 probe subset):\n")
	for _, t := range optionalTools {
		p("  %-18s %s\n", t.name, probeTool(t))
	}

	reportSandbox(context.Background(), w)
	reportSelfObservability(context.Background(), w, dataDir)
	reportLitestream(context.Background(), w)
	// The report prints first, so an operator sees why; then an explicit
	// local-sandboxed request that cannot be enforced is a failing check.
	return enforceSandboxTier(context.Background())
}

// reportSecretsProtection prints the real at-rest and in-memory posture of the
// key material: whether the DEK can be pinned out of swap (E5). The host-key
// at-rest state is added alongside it.
func reportSecretsProtection(w io.Writer, dataDir string) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	p("secrets protection:\n")

	switch prot := secrets.InspectHostKeyProtection(dataDir); prot {
	case secrets.HostKeySystemdCredential:
		p("  %-18s %s\n", "host key at rest",
			"systemd credential — unwrap secret is outside the data volume"+tpmSuffix())
	case secrets.HostKeyPassphrase:
		p("  %-18s %s\n", "host key at rest",
			"passphrase-wrapped — a stolen data volume alone does not yield it")
	case secrets.HostKeyPlaintext:
		p("  %-18s %s\n", "host key at rest",
			"PLAINTEXT in the data dir — a volume read alone yields every secret; "+hostKeyFixHint)
	default:
		p("  %-18s %s\n", "host key at rest", "absent — run `exocomp init`")
	}

	if lock := secrets.ProbeMemoryLock(); lock.Locked {
		p("  %-18s %s\n", "memory lock", "mlock available — the DEK is pinned out of swap and core dumps")
	} else {
		p("  %-18s %s\n", "memory lock",
			"UNAVAILABLE — the DEK may reach swap or a core dump ("+lock.Err.Error()+")")
	}
}

// hostKeyFixHint names the two ways to move a plaintext host key to a secure
// tier. It is one line so init and doctor say the same thing.
const hostKeyFixHint = "wrap it (set " + secrets.EnvHostKeyPassphrase +
	" and re-init, or install via the Quadlet unit's systemd credential)"

// tpmSuffix notes TPM backing only when a TPM is actually present, so the
// report never claims hardware sealing that is not there.
func tpmSuffix() string {
	if secrets.TPMPresent() {
		return " (TPM 2.0 present)"
	}
	return ""
}

// reportSelfObservability prints whether the daemon can be watched: the
// metrics endpoint reachable, a dead-man ping configured, and any active
// silence or mute. None of these fail the check — they are what an operator
// reads to answer "is anything holding it back, and would I be told if it
// broke".
func reportSelfObservability(ctx context.Context, w io.Writer, dataDir string) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	p("self-observability:\n")
	p("  %-18s %s\n", "metrics", probeMetrics(ctx, dataDir))
	p("  %-18s %s\n", "dead-man", deadManStatus())
	p("  %-18s %s\n", "silence/mute", holdStatus(ctx))
}

// probeMetrics reads the daemon's recorded address and scrapes /metrics. No
// address file means no running daemon, which is a state, not a fault.
func probeMetrics(ctx context.Context, dataDir string) string {
	addr, err := api.ReadAddress(filepath.Join(dataDir, api.AddressFile))
	if err != nil {
		return "unknown (" + err.Error() + ")"
	}
	if addr == "" {
		return "daemon not running (no address file)"
	}
	url := "http://" + addr + "/metrics"
	reqCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return "unreachable (" + err.Error() + ")"
	}
	// /metrics is bearer-gated (finding D3); present the envelope-derived scrape
	// token so the probe reads gauges instead of a 401.
	if env, oerr := secrets.Open(dataDir); oerr == nil {
		req.Header.Set("Authorization", "Bearer "+metrics.ScrapeToken(env))
		_ = env.Close()
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "unreachable at " + url + " (" + err.Error() + ")"
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "exocomp_up") {
		return fmt.Sprintf("listener up but /metrics returned %d", resp.StatusCode)
	}
	return "reachable at " + url
}

// deadManStatus reports the dead-man ping. The ping is not yet a subsystem, so
// the honest answer is that nothing is configured — said plainly rather than
// implied by silence.
func deadManStatus() string {
	return "not configured — no external heartbeat wired yet"
}

// holdStatus opens the store and reports what is currently holding the daemon
// back: a global silence is a pause, an active notify mute is a mute. The two
// are named separately because they mean different things. It opens through
// openStore (never store.Open directly) so the one sweeping opener stays the
// only door to the database; absent a database, there is nothing to report.
func holdStatus(ctx context.Context) string {
	dir, err := dataDir()
	if err != nil {
		return "unknown (" + err.Error() + ")"
	}
	if _, err := os.Stat(filepath.Join(dir, "exocomp.db")); err != nil {
		return "no database yet"
	}
	st, err := openStore()
	if err != nil {
		return "unknown (" + err.Error() + ")"
	}
	defer func() { _ = st.Close() }()

	var parts []string
	if silences, err := st.ListSilences(ctx, true); err == nil {
		for _, s := range silences {
			if s.ScopeKind == store.ScopeGlobal {
				parts = append(parts, "PAUSED (global silence)")
				break
			}
		}
	}
	if mutes, err := st.ListNotifyMutes(ctx, true); err == nil && len(mutes) > 0 {
		parts = append(parts, fmt.Sprintf("MUTED (%d active)", len(mutes)))
	}
	if len(parts) == 0 {
		return "none active"
	}
	return strings.Join(parts, " · ")
}

// reportLitestream reports the optional WAL-replication sidecar: whether the
// binary is installed, what replica target is configured, and the RPO age (the
// staleness of the newest replicated frame). Nothing here fails the check — the
// sidecar is optional, so an absent binary or an unconfigured target is a state
// to read, said plainly. It reads through openStore so the one sweeping opener
// stays the only door to the database; absent a database, there is nothing to
// report on the target.
func reportLitestream(ctx context.Context, w io.Writer) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	p("litestream sidecar:\n")

	binPath, version := litestream.Presence()
	switch {
	case binPath == "":
		p("  %-18s %s\n", "binary", "not installed (optional) — "+litestream.InstallHint)
	case version == "":
		p("  %-18s %s\n", "binary", "found at "+binPath+" (version probe failed)")
	default:
		p("  %-18s %s\n", "binary", "found at "+binPath+" ("+version+")")
	}

	dir, err := dataDir()
	if err != nil {
		p("  %-18s %s\n", "target", "unknown ("+err.Error()+")")
		return
	}
	if _, err := os.Stat(filepath.Join(dir, "exocomp.db")); err != nil {
		p("  %-18s %s\n", "target", "no database yet")
		return
	}
	st, err := openStore()
	if err != nil {
		p("  %-18s %s\n", "target", "unknown ("+err.Error()+")")
		return
	}
	defer func() { _ = st.Close() }()

	raw, err := st.GetSetting(ctx, litestream.SettingReplicaTarget)
	if errors.Is(err, store.ErrNotFound) || raw == "" {
		p("  %-18s %s\n", "target", "none configured — `exocomp litestream target <uri>`")
		p("  %-18s %s\n", "RPO age", "unknown (no replica target)")
		return
	}
	if err != nil {
		p("  %-18s %s\n", "target", "unknown ("+err.Error()+")")
		return
	}
	target, perr := litestream.ParseTarget(raw)
	if perr != nil {
		p("  %-18s %s\n", "target", raw+" (UNPARSEABLE: "+perr.Error()+")")
		return
	}
	p("  %-18s %s\n", "target", target.Raw+" ("+string(target.Type)+")")

	_, mtime, ok, ferr := litestream.ReplicaFreshness(target, time.Now().UTC())
	switch {
	case ferr != nil:
		p("  %-18s %s\n", "RPO age", "unknown ("+ferr.Error()+")")
	case ok:
		p("  %-18s %s\n", "RPO age", time.Since(mtime).Truncate(time.Second).String()+" (newest replicated frame)")
	case target.Remote():
		p("  %-18s %s\n", "RPO age", "unknown — a remote replica's frame age needs the litestream binary")
	default:
		p("  %-18s %s\n", "RPO age", "no replicated frames yet — sidecar not running?")
	}
}

func dataDir() (string, error) {
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "exocomp"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "exocomp"), nil
}

// dataDirMode is the only mode the data dir may have: it holds host.key and
// the keyring, so nothing outside this uid has any business reading it.
const dataDirMode os.FileMode = 0o700

// inspectDataDir describes the data dir without touching it, and returns the
// fault that should fail the run. A doctor that created the dir would report
// on its own handiwork, and would create it under whatever mode it chose
// rather than the 0700 `exocomp init` uses.
func inspectDataDir(dir string) (string, error) {
	info, err := os.Stat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		parent := filepath.Dir(dir)
		if err := writableDir(parent); err != nil {
			return fmt.Sprintf("absent, and %s cannot hold it (%v)", parent, err),
				fmt.Errorf("data dir cannot be created: %w", err)
		}
		return fmt.Sprintf("absent — creatable, %s is writable; `exocomp init` creates it %04o",
			parent, dataDirMode.Perm()), nil
	case err != nil:
		return fmt.Sprintf("UNREADABLE (%v)", err), fmt.Errorf("data dir not readable: %w", err)
	}

	if !info.IsDir() {
		return "NOT A DIRECTORY", errors.New("data dir is not a directory")
	}
	mode := info.Mode().Perm()
	if err := writableDir(dir); err != nil {
		return fmt.Sprintf("mode %04o — NOT WRITABLE (%v)", mode, err),
			fmt.Errorf("data dir not writable: %w", err)
	}
	if mode&^dataDirMode != 0 {
		return fmt.Sprintf("mode %04o — TOO PERMISSIVE, want %04o (chmod %04o %s)",
				mode, dataDirMode.Perm(), dataDirMode.Perm(), dir),
			fmt.Errorf("data dir mode %04o grants access beyond this user; it holds the host key",
				mode)
	}
	return fmt.Sprintf("mode %04o — writable", mode), nil
}

// writableDir reports whether this process can create entries in dir, without
// creating one. access(2) answers with the real uid, which for a process
// that is never setuid is the uid everything else here runs as.
func writableDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if err := unix.Access(dir, unix.W_OK|unix.X_OK); err != nil {
		return fmt.Errorf("no write access to %s: %w", dir, err)
	}
	return nil
}

func probeTool(t optionalTool) string {
	path, err := exec.LookPath(t.name)
	if err != nil {
		return "MISSING"
	}

	out, err := exec.Command(path, t.args...).CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return fmt.Sprintf("found at %s (version probe exited %d)", path, exitErr.ExitCode())
		}
		return fmt.Sprintf("found at %s (version probe failed: %v)", path, err)
	}

	return fmt.Sprintf("found at %s (%s)", path, firstLine(out))
}

func firstLine(b []byte) string {
	line, _, _ := strings.Cut(string(b), "\n")
	return strings.TrimSpace(line)
}
