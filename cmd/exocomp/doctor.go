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
	"exocomp.dev/exocomp/internal/wire"
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
		Use:     "doctor",
		Short:   "Run environment checks",
		Example: "exocomp doctor --output json",
		RunE: func(cmd *cobra.Command, _ []string) error {
			mode, err := outputMode(cmd)
			if err != nil {
				return err
			}
			if mode == outputJSON {
				return emitJSON(cmd, wire.KindDoctor, doctorHealth(cmd.Context()))
			}
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

// doctorHealth runs every in-scope subsystem probe once and projects the
// result to the wire health view. Unlike the text report, it never
// short-circuits on a data-dir fault: an agent asking for the summary wants
// the full picture in one call, and the per-subsystem status carries the
// fault — not the process exit code, which stays 0 as long as the probes
// themselves could run. The sandbox tier and the go-runtime line are
// deliberately not subsystems here (docs/design/agent-interface.md §3.2
// scopes doctor's health entries to data dir, secrets, tools,
// self-observability, and litestream); the text report still covers them.
func doctorHealth(ctx context.Context) wire.HealthView {
	dir, err := dataDir()
	if err != nil {
		return wire.HealthView{{
			Name: "data_dir", Status: wire.HealthUnknown, Detail: "unknown (" + err.Error() + ")",
		}}
	}

	report, fault := inspectDataDir(dir)
	return wire.HealthView{
		dataDirEntry(report, fault),
		secretsProtectionEntry(probeSecretsProtection(dir)),
		optionalToolsEntry(),
		selfObservabilityEntry(probeSelfObservability(ctx, dir)),
		litestreamEntry(probeLitestream(ctx)),
	}
}

// healthSeverity ranks statuses from least to most concerning, so combining
// two independent probes into one subsystem entry can pick the worst.
var healthSeverity = map[string]int{
	wire.HealthOK:      0,
	wire.HealthUnknown: 1,
	wire.HealthWarn:    2,
	wire.HealthFail:    3,
}

func worseHealth(a, b string) string {
	if healthSeverity[b] > healthSeverity[a] {
		return b
	}
	return a
}

// dataDirEntry classifies inspectDataDir's report: a fault is fail, an absent
// but creatable dir is warn (not yet initialized — `exocomp init` fixes it),
// everything else is ok.
func dataDirEntry(report string, fault error) wire.HealthEntry {
	status := wire.HealthOK
	switch {
	case fault != nil:
		status = wire.HealthFail
	case strings.HasPrefix(report, "absent"):
		status = wire.HealthWarn
	}
	return wire.HealthEntry{Name: "data_dir", Status: status, Detail: report}
}

// secretsProtectionProbe is InspectHostKeyProtection and ProbeMemoryLock's raw
// findings, captured once so the text renderer and the health entry read the
// same probe instead of two.
type secretsProtectionProbe struct {
	hostKey secrets.HostKeyProtection
	lock    secrets.MemoryLock
}

func probeSecretsProtection(dataDir string) secretsProtectionProbe {
	return secretsProtectionProbe{
		hostKey: secrets.InspectHostKeyProtection(dataDir),
		lock:    secrets.ProbeMemoryLock(),
	}
}

// reportSecretsProtection prints the real at-rest and in-memory posture of the
// key material: whether the DEK can be pinned out of swap (E5). The host-key
// at-rest state is added alongside it.
func reportSecretsProtection(w io.Writer, dataDir string) {
	printSecretsProtection(w, probeSecretsProtection(dataDir))
}

func printSecretsProtection(w io.Writer, res secretsProtectionProbe) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	p("secrets protection:\n")

	switch res.hostKey {
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

	if res.lock.Locked {
		p("  %-18s %s\n", "memory lock", "mlock available — the DEK is pinned out of swap and core dumps")
	} else {
		p("  %-18s %s\n", "memory lock",
			"UNAVAILABLE — the DEK may reach swap or a core dump ("+res.lock.Err.Error()+")")
	}
}

// secretsProtectionEntry combines the host-key-at-rest and memory-lock probes
// into the subsystem's worst status: a plaintext host key or an unpinned DEK
// is worth an agent's attention even though neither aborts the command. The
// raw mlock error is omitted from the JSON detail — it is an OS-specific
// syscall message, not needed to branch on, and would make the bytes vary by
// host for no gain.
func secretsProtectionEntry(res secretsProtectionProbe) wire.HealthEntry {
	hostStatus := wire.HealthOK
	switch res.hostKey {
	case secrets.HostKeyPlaintext:
		hostStatus = wire.HealthFail
	case secrets.HostKeyAbsent:
		hostStatus = wire.HealthWarn
	}
	hostDetail := "host key: " + res.hostKey.String()

	lockStatus, lockDetail := wire.HealthOK, "memory lock: locked"
	if !res.lock.Locked {
		lockStatus, lockDetail = wire.HealthWarn, "memory lock: unavailable"
	}

	return wire.HealthEntry{
		Name:   "secrets_protection",
		Status: worseHealth(hostStatus, lockStatus),
		Detail: hostDetail + "; " + lockDetail,
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

// selfObservabilityProbe is the metrics scrape and hold-status read, captured
// once so the text renderer and the health entry read the same probe.
type selfObservabilityProbe struct {
	metricsStatus, metricsText string
	holdStatus, holdText       string
}

func probeSelfObservability(ctx context.Context, dataDir string) selfObservabilityProbe {
	metricsStatus, metricsText := probeMetricsResult(ctx, dataDir)
	holdStatus, holdText := holdStatusResult(ctx)
	return selfObservabilityProbe{
		metricsStatus: metricsStatus, metricsText: metricsText,
		holdStatus: holdStatus, holdText: holdText,
	}
}

// reportSelfObservability prints whether the daemon can be watched: the
// metrics endpoint reachable, a dead-man ping configured, and any active
// silence or mute. None of these fail the check — they are what an operator
// reads to answer "is anything holding it back, and would I be told if it
// broke".
func reportSelfObservability(ctx context.Context, w io.Writer, dataDir string) {
	printSelfObservability(w, probeSelfObservability(ctx, dataDir))
}

func printSelfObservability(w io.Writer, res selfObservabilityProbe) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	p("self-observability:\n")
	p("  %-18s %s\n", "metrics", res.metricsText)
	p("  %-18s %s\n", "dead-man", deadManStatus())
	p("  %-18s %s\n", "silence/mute", res.holdText)
}

// selfObservabilityEntry rolls the metrics and hold probes into one
// subsystem status: dead-man is not yet a real subsystem (deadManStatus is a
// constant until an external heartbeat is wired), so it is named in the
// detail for context but never moves the status.
func selfObservabilityEntry(res selfObservabilityProbe) wire.HealthEntry {
	return wire.HealthEntry{
		Name:   "self_observability",
		Status: worseHealth(res.metricsStatus, res.holdStatus),
		Detail: fmt.Sprintf("metrics: %s; dead-man: %s; hold: %s", res.metricsText, deadManStatus(), res.holdText),
	}
}

// probeMetricsResult reads the daemon's recorded address and scrapes
// /metrics once, classifying the outcome alongside the human text: no
// address file means no running daemon, which is unknown — a state, not a
// fault; a reachable listener that answers wrong is a warn. The scrape token
// itself never appears in the returned text, only in the outgoing request
// header.
func probeMetricsResult(ctx context.Context, dataDir string) (status, text string) {
	addr, err := api.ReadAddress(filepath.Join(dataDir, api.AddressFile))
	if err != nil {
		return wire.HealthUnknown, "unknown (" + err.Error() + ")"
	}
	if addr == "" {
		return wire.HealthUnknown, "daemon not running (no address file)"
	}
	url := "http://" + addr + "/metrics"
	reqCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return wire.HealthWarn, "unreachable (" + err.Error() + ")"
	}
	// /metrics is bearer-gated (finding D3); present the envelope-derived scrape
	// token so the probe reads gauges instead of a 401.
	if env, oerr := secrets.Open(dataDir); oerr == nil {
		req.Header.Set("Authorization", "Bearer "+metrics.ScrapeToken(env))
		_ = env.Close()
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return wire.HealthWarn, "unreachable at " + url + " (" + err.Error() + ")"
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "exocomp_up") {
		return wire.HealthWarn, fmt.Sprintf("listener up but /metrics returned %d", resp.StatusCode)
	}
	return wire.HealthOK, "reachable at " + url
}

// probeMetrics is the text-only view of probeMetricsResult.
func probeMetrics(ctx context.Context, dataDir string) string {
	_, text := probeMetricsResult(ctx, dataDir)
	return text
}

// deadManStatus reports the dead-man ping. The ping is not yet a subsystem, so
// the honest answer is that nothing is configured — said plainly rather than
// implied by silence.
func deadManStatus() string {
	return "not configured — no external heartbeat wired yet"
}

// holdStatusResult reports what is currently holding the daemon back — a
// global silence is a pause, an active notify mute is a mute — alongside a
// status for the JSON path. It opens through openStore (never store.Open
// directly) so the one sweeping opener stays the only door to the database;
// absent a database, there is nothing to report.
func holdStatusResult(ctx context.Context) (status, text string) {
	dir, err := dataDir()
	if err != nil {
		return wire.HealthUnknown, "unknown (" + err.Error() + ")"
	}
	if _, err := os.Stat(filepath.Join(dir, "exocomp.db")); err != nil {
		return wire.HealthUnknown, "no database yet"
	}
	st, err := openStore()
	if err != nil {
		return wire.HealthUnknown, "unknown (" + err.Error() + ")"
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
		return wire.HealthOK, "none active"
	}
	return wire.HealthWarn, strings.Join(parts, " · ")
}

// holdStatus is the text-only view of holdStatusResult.
func holdStatus(ctx context.Context) string {
	_, text := holdStatusResult(ctx)
	return text
}

// litestreamProbe is reportLitestream's raw findings, captured once so the
// text renderer and the health entry read the same probe instead of two.
type litestreamProbe struct {
	binPath, version string

	dirErr    error
	noDB      bool
	storeErr  error
	noTarget  bool
	targetErr error
	targetRaw string
	parseErr  error
	target    litestream.Target

	freshOK    bool
	freshMtime time.Time
	freshErr   error
}

// probeLitestream reports the optional WAL-replication sidecar's binary,
// configured target, and replication freshness in one pass. It reads through
// openStore so the one sweeping opener stays the only door to the database;
// absent a database, there is nothing to report on the target.
func probeLitestream(ctx context.Context) litestreamProbe {
	var res litestreamProbe
	res.binPath, res.version = litestream.Presence()

	dir, err := dataDir()
	if err != nil {
		res.dirErr = err
		return res
	}
	if _, err := os.Stat(filepath.Join(dir, "exocomp.db")); err != nil {
		res.noDB = true
		return res
	}
	st, err := openStore()
	if err != nil {
		res.storeErr = err
		return res
	}
	defer func() { _ = st.Close() }()

	raw, err := st.GetSetting(ctx, litestream.SettingReplicaTarget)
	if errors.Is(err, store.ErrNotFound) || raw == "" {
		res.noTarget = true
		return res
	}
	if err != nil {
		res.targetErr = err
		return res
	}
	target, perr := litestream.ParseTarget(raw)
	if perr != nil {
		res.targetRaw = raw
		res.parseErr = perr
		return res
	}
	res.target = target

	_, mtime, ok, ferr := litestream.ReplicaFreshness(target, time.Now().UTC())
	res.freshOK, res.freshMtime, res.freshErr = ok, mtime, ferr
	return res
}

// reportLitestream reports the optional WAL-replication sidecar: whether the
// binary is installed, what replica target is configured, and the RPO age (the
// staleness of the newest replicated frame). Nothing here fails the check — the
// sidecar is optional, so an absent binary or an unconfigured target is a state
// to read, said plainly.
func reportLitestream(ctx context.Context, w io.Writer) {
	printLitestream(w, probeLitestream(ctx))
}

func printLitestream(w io.Writer, res litestreamProbe) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	p("litestream sidecar:\n")

	switch {
	case res.binPath == "":
		p("  %-18s %s\n", "binary", "not installed (optional) — "+litestream.InstallHint)
	case res.version == "":
		p("  %-18s %s\n", "binary", "found at "+res.binPath+" (version probe failed)")
	default:
		p("  %-18s %s\n", "binary", "found at "+res.binPath+" ("+res.version+")")
	}

	switch {
	case res.dirErr != nil:
		p("  %-18s %s\n", "target", "unknown ("+res.dirErr.Error()+")")
		return
	case res.noDB:
		p("  %-18s %s\n", "target", "no database yet")
		return
	case res.storeErr != nil:
		p("  %-18s %s\n", "target", "unknown ("+res.storeErr.Error()+")")
		return
	case res.noTarget:
		p("  %-18s %s\n", "target", "none configured — `exocomp litestream target <uri>`")
		p("  %-18s %s\n", "RPO age", "unknown (no replica target)")
		return
	case res.targetErr != nil:
		p("  %-18s %s\n", "target", "unknown ("+res.targetErr.Error()+")")
		return
	case res.parseErr != nil:
		p("  %-18s %s\n", "target", res.targetRaw+" (UNPARSEABLE: "+res.parseErr.Error()+")")
		return
	}
	p("  %-18s %s\n", "target", res.target.Raw+" ("+string(res.target.Type)+")")

	switch {
	case res.freshErr != nil:
		p("  %-18s %s\n", "RPO age", "unknown ("+res.freshErr.Error()+")")
	case res.freshOK:
		p("  %-18s %s\n", "RPO age", time.Since(res.freshMtime).Truncate(time.Second).String()+" (newest replicated frame)")
	case res.target.Remote():
		p("  %-18s %s\n", "RPO age", "unknown — a remote replica's frame age needs the litestream binary")
	default:
		p("  %-18s %s\n", "RPO age", "no replicated frames yet — sidecar not running?")
	}
}

// litestreamEntry classifies the same probe result: an unparseable target is
// a real misconfiguration (fail); no target, no database, or a remote
// target's unobservable age is the sidecar's normal optional-and-unused state
// (unknown); a local target already replicating is healthy; one configured
// but producing nothing yet is worth a look (warn). The raw target URI and
// the RPO's relative age are both omitted from the detail: a URI can carry a
// host or path beyond an identifier, and a relative age is exactly the
// volatile field the wire contract never emits (internal/wire/doc.go).
func litestreamEntry(res litestreamProbe) wire.HealthEntry {
	bin := "binary not installed (optional)"
	switch {
	case res.binPath != "" && res.version != "":
		bin = "binary present"
	case res.binPath != "":
		bin = "binary found, version probe failed"
	}

	switch {
	case res.dirErr != nil, res.storeErr != nil, res.targetErr != nil:
		return wire.HealthEntry{Name: "litestream", Status: wire.HealthUnknown, Detail: bin + "; target unknown"}
	case res.noDB:
		return wire.HealthEntry{Name: "litestream", Status: wire.HealthUnknown, Detail: bin + "; no database yet"}
	case res.noTarget:
		return wire.HealthEntry{Name: "litestream", Status: wire.HealthUnknown, Detail: bin + "; no replica target configured"}
	case res.parseErr != nil:
		return wire.HealthEntry{Name: "litestream", Status: wire.HealthFail, Detail: bin + "; target UNPARSEABLE"}
	}

	target := bin + "; target " + string(res.target.Type) + " configured"
	switch {
	case res.freshErr != nil:
		return wire.HealthEntry{Name: "litestream", Status: wire.HealthUnknown, Detail: target + "; RPO probe failed"}
	case res.freshOK:
		return wire.HealthEntry{Name: "litestream", Status: wire.HealthOK, Detail: target + "; replica frames observed"}
	case res.target.Remote():
		return wire.HealthEntry{Name: "litestream", Status: wire.HealthUnknown, Detail: target + "; remote RPO age needs the binary"}
	default:
		return wire.HealthEntry{Name: "litestream", Status: wire.HealthWarn, Detail: target + "; no replicated frames yet"}
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

// probeToolResult looks up and version-probes one optional tool, reporting
// whether it was found alongside the same text the report prints.
func probeToolResult(t optionalTool) (found bool, text string) {
	path, err := exec.LookPath(t.name)
	if err != nil {
		return false, "MISSING"
	}

	out, err := exec.Command(path, t.args...).CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return true, fmt.Sprintf("found at %s (version probe exited %d)", path, exitErr.ExitCode())
		}
		return true, fmt.Sprintf("found at %s (version probe failed: %v)", path, err)
	}

	return true, fmt.Sprintf("found at %s (%s)", path, firstLine(out))
}

// probeTool is the text-only view of probeToolResult.
func probeTool(t optionalTool) string {
	_, text := probeToolResult(t)
	return text
}

// optionalToolsEntry rolls every tool probe into one subsystem status: a
// missing tool is a warn in M0 (every one of them is still optional), never a
// fail.
func optionalToolsEntry() wire.HealthEntry {
	var missing []string
	for _, t := range optionalTools {
		if found, _ := probeToolResult(t); !found {
			missing = append(missing, t.name)
		}
	}
	if len(missing) == 0 {
		return wire.HealthEntry{Name: "optional_tools", Status: wire.HealthOK, Detail: "all present"}
	}
	return wire.HealthEntry{
		Name: "optional_tools", Status: wire.HealthWarn,
		Detail: "missing: " + strings.Join(missing, ", "),
	}
}

func firstLine(b []byte) string {
	line, _, _ := strings.Cut(string(b), "\n")
	return strings.TrimSpace(line)
}
