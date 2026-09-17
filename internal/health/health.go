// Package health is the single definition of exocomp's doctor subsystem
// probes: data dir, secrets protection, optional tools, self-observability,
// and litestream. `exocomp doctor` (text and --output json) and GET
// /api/v1/health both call Probe, so a resource's CLI and API reads can never
// diverge (docs/design/agent-interface.md §3.2, §3.4).
//
// Every probe takes its inputs explicitly — a data dir, a store opener — and
// touches no process global, so the CLI (a short-lived process resolving its
// own data dir) and the web API (a long-running daemon with an already-open
// store) each supply their own without importing cmd/exocomp.
package health

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"exocomp.dev/exocomp/internal/api"
	"exocomp.dev/exocomp/internal/litestream"
	"exocomp.dev/exocomp/internal/metrics"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

// dbFileName is the store file every probe that needs persisted state checks
// for before opening a connection, so a probe never creates the database as a
// side effect of reporting on it.
const dbFileName = "exocomp.db"

// Store is the read-only slice of *store.Store the hold-status and litestream
// probes need.
type Store interface {
	ListSilences(ctx context.Context, activeOnly bool) ([]store.Silence, error)
	ListNotifyMutes(ctx context.Context, activeOnly bool) ([]store.NotifyMute, error)
	GetSetting(ctx context.Context, key string) (string, error)
}

// OpenStore opens (or hands back) a Store for the probes that read persisted
// state. It is called only after a probe has confirmed the database file
// exists. The CLI opens a fresh connection per call and returns its Close; the
// web API reuses its long-lived store and returns a no-op close.
type OpenStore func() (Store, func() error, error)

// Probe runs every in-scope subsystem probe once and projects the result to
// the wire health view, in a fixed order. It never short-circuits on a
// data-dir fault: a caller asking for the summary wants the full picture in
// one read, and the per-subsystem status carries the fault. The sandbox tier
// and the go-runtime line are deliberately not subsystems here
// (docs/design/agent-interface.md §3.2 scopes health to data dir, secrets,
// tools, self-observability, and litestream).
func Probe(ctx context.Context, dataDir string, open OpenStore) wire.HealthView {
	report, fault := InspectDataDir(dataDir)
	probes := ProbeOptionalTools()
	return wire.HealthView{
		dataDirEntry(report, fault),
		secretsProtectionEntry(ProbeSecretsProtection(dataDir)),
		optionalToolsEntry(probes, ProbeToolchain(probes)),
		selfObservabilityEntry(ProbeSelfObservability(ctx, dataDir, open)),
		litestreamEntry(ProbeLitestream(ctx, dataDir, open)),
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

// dataDirEntry classifies InspectDataDir's report: a fault is fail, an absent
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

// DataDirMode is the only mode the data dir may have: it holds host.key and
// the keyring, so nothing outside this uid has any business reading it.
const DataDirMode os.FileMode = 0o700

// InspectDataDir describes the data dir without touching it, and returns the
// fault that should fail the run. A probe that created the dir would report
// on its own handiwork, and would create it under whatever mode it chose
// rather than the 0700 `exocomp init` uses.
func InspectDataDir(dir string) (string, error) {
	info, err := os.Stat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		parent := filepath.Dir(dir)
		if err := writableDir(parent); err != nil {
			return fmt.Sprintf("absent, and %s cannot hold it (%v)", parent, err),
				fmt.Errorf("data dir cannot be created: %w", err)
		}
		return fmt.Sprintf("absent — creatable, %s is writable; `exocomp init` creates it %04o",
			parent, DataDirMode.Perm()), nil
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
	if mode&^DataDirMode != 0 {
		return fmt.Sprintf("mode %04o — TOO PERMISSIVE, want %04o (chmod %04o %s)",
				mode, DataDirMode.Perm(), DataDirMode.Perm(), dir),
			fmt.Errorf("data dir mode %04o grants access beyond this user; it holds the host key",
				mode)
	}
	return fmt.Sprintf("mode %04o — writable", mode), nil
}

// writableDir reports whether this process can create entries in dir, without
// creating one. access(2) answers with the real uid, which for a process that
// is never setuid is the uid everything else here runs as.
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

// SecretsProtectionProbe is InspectHostKeyProtection and ProbeMemoryLock's raw
// findings, captured once so a text renderer and the health entry read the
// same probe instead of two.
type SecretsProtectionProbe struct {
	HostKey secrets.HostKeyProtection
	Lock    secrets.MemoryLock
}

// ProbeSecretsProtection reads the real at-rest and in-memory posture of the
// key material: whether the DEK can be pinned out of swap (E5), and whether
// the host key at rest is wrapped.
func ProbeSecretsProtection(dataDir string) SecretsProtectionProbe {
	return SecretsProtectionProbe{
		HostKey: secrets.InspectHostKeyProtection(dataDir),
		Lock:    secrets.ProbeMemoryLock(),
	}
}

// secretsProtectionEntry combines the host-key-at-rest and memory-lock probes
// into the subsystem's worst status: a plaintext host key or an unpinned DEK
// is worth an agent's attention even though neither aborts the command. The
// raw mlock error is omitted from the JSON detail — it is an OS-specific
// syscall message, not needed to branch on, and would make the bytes vary by
// host for no gain.
func secretsProtectionEntry(res SecretsProtectionProbe) wire.HealthEntry {
	hostStatus := wire.HealthOK
	switch res.HostKey {
	case secrets.HostKeyPlaintext:
		hostStatus = wire.HealthFail
	case secrets.HostKeyAbsent:
		hostStatus = wire.HealthWarn
	}
	hostDetail := "host key: " + res.HostKey.String()

	lockStatus, lockDetail := wire.HealthOK, "memory lock: locked"
	if !res.Lock.Locked {
		lockStatus, lockDetail = wire.HealthWarn, "memory lock: unavailable"
	}

	return wire.HealthEntry{
		Name:   "secrets_protection",
		Status: worseHealth(hostStatus, lockStatus),
		Detail: hostDetail + "; " + lockDetail,
	}
}

// HostKeyFixHint names the two ways to move a plaintext host key to a secure
// tier. It is one line so init and doctor say the same thing.
const HostKeyFixHint = "wrap it (set " + secrets.EnvHostKeyPassphrase +
	" and re-init, or install via the Quadlet unit's systemd credential)"

// TPMSuffix notes TPM backing only when a TPM is actually present, so a report
// never claims hardware sealing that is not there.
func TPMSuffix() string {
	if secrets.TPMPresent() {
		return " (TPM 2.0 present)"
	}
	return ""
}

// SelfObservabilityProbe is the metrics scrape and hold-status read, captured
// once so a text renderer and the health entry read the same probe.
type SelfObservabilityProbe struct {
	MetricsStatus, MetricsText string
	HoldStatus, HoldText       string
}

// ProbeSelfObservability reads whether the daemon can be watched: the metrics
// endpoint reachable, and any active silence or mute holding it back.
func ProbeSelfObservability(ctx context.Context, dataDir string, open OpenStore) SelfObservabilityProbe {
	metricsStatus, metricsText := ProbeMetricsResult(ctx, dataDir)
	holdStatus, holdText := ProbeHoldStatus(ctx, dataDir, open)
	return SelfObservabilityProbe{
		MetricsStatus: metricsStatus, MetricsText: metricsText,
		HoldStatus: holdStatus, HoldText: holdText,
	}
}

// selfObservabilityEntry rolls the metrics and hold probes into one subsystem
// status: dead-man is not yet a real subsystem (DeadManStatus is a constant
// until an external heartbeat is wired), so it is named in the detail for
// context but never moves the status.
func selfObservabilityEntry(res SelfObservabilityProbe) wire.HealthEntry {
	return wire.HealthEntry{
		Name:   "self_observability",
		Status: worseHealth(res.MetricsStatus, res.HoldStatus),
		Detail: fmt.Sprintf("metrics: %s; dead-man: %s; hold: %s", res.MetricsText, DeadManStatus(), res.HoldText),
	}
}

// ProbeMetricsResult reads the daemon's recorded address and scrapes /metrics
// once, classifying the outcome alongside the human text: no address file
// means no running daemon, which is unknown — a state, not a fault; a
// reachable listener that answers wrong is a warn. The scrape token itself
// never appears in the returned text, only in the outgoing request header.
func ProbeMetricsResult(ctx context.Context, dataDir string) (status, text string) {
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
	// /metrics is bearer-gated; present the envelope-derived scrape
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

// DeadManStatus reports the dead-man ping. The ping is not yet a subsystem, so
// the honest answer is that nothing is configured — said plainly rather than
// implied by silence.
func DeadManStatus() string {
	return "not configured — no external heartbeat wired yet"
}

// ProbeHoldStatus reports what is currently holding the daemon back — a global
// silence is a pause, an active notify mute is a mute — alongside a status.
// It stats the database file before opening a store, so a probe never creates
// the database as a side effect of reporting on it; absent a database, there
// is nothing to report.
func ProbeHoldStatus(ctx context.Context, dataDir string, open OpenStore) (status, text string) {
	if _, err := os.Stat(filepath.Join(dataDir, dbFileName)); err != nil {
		return wire.HealthUnknown, "no database yet"
	}
	st, closeFn, err := open()
	if err != nil {
		return wire.HealthUnknown, "unknown (" + err.Error() + ")"
	}
	defer func() { _ = closeFn() }()

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

// LitestreamProbe is ProbeLitestream's raw findings, captured once so a text
// renderer and the health entry read the same probe instead of two.
type LitestreamProbe struct {
	BinPath, Version string

	DirErr    error
	NoDB      bool
	StoreErr  error
	NoTarget  bool
	TargetErr error
	TargetRaw string
	ParseErr  error
	Target    litestream.Target

	FreshOK    bool
	FreshMtime time.Time
	FreshErr   error
}

// ProbeLitestream reports the optional WAL-replication sidecar's binary,
// configured target, and replication freshness in one pass. It stats the
// database file before opening a store, so a probe never creates the database
// as a side effect of reporting on it; absent a database, there is nothing to
// report on the target.
func ProbeLitestream(ctx context.Context, dataDir string, open OpenStore) LitestreamProbe {
	var res LitestreamProbe
	res.BinPath, res.Version = litestream.Presence()

	if _, err := os.Stat(filepath.Join(dataDir, dbFileName)); err != nil {
		res.NoDB = true
		return res
	}
	st, closeFn, err := open()
	if err != nil {
		res.StoreErr = err
		return res
	}
	defer func() { _ = closeFn() }()

	raw, err := st.GetSetting(ctx, litestream.SettingReplicaTarget)
	if errors.Is(err, store.ErrNotFound) || raw == "" {
		res.NoTarget = true
		return res
	}
	if err != nil {
		res.TargetErr = err
		return res
	}
	target, perr := litestream.ParseTarget(raw)
	if perr != nil {
		res.TargetRaw = raw
		res.ParseErr = perr
		return res
	}
	res.Target = target

	_, mtime, ok, ferr := litestream.ReplicaFreshness(target, time.Now().UTC())
	res.FreshOK, res.FreshMtime, res.FreshErr = ok, mtime, ferr
	return res
}

// litestreamEntry classifies the same probe result: an unparseable target is a
// real misconfiguration (fail); no target, no database, or a remote target's
// unobservable age is the sidecar's normal optional-and-unused state
// (unknown); a local target already replicating is healthy; one configured
// but producing nothing yet is worth a look (warn). The raw target URI and the
// RPO's relative age are both omitted from the detail: a URI can carry a host
// or path beyond an identifier, and a relative age is exactly the volatile
// field the wire contract never emits (internal/wire/doc.go).
func litestreamEntry(res LitestreamProbe) wire.HealthEntry {
	bin := "binary not installed (optional)"
	switch {
	case res.BinPath != "" && res.Version != "":
		bin = "binary present"
	case res.BinPath != "":
		bin = "binary found, version probe failed"
	}

	switch {
	case res.DirErr != nil, res.StoreErr != nil, res.TargetErr != nil:
		return wire.HealthEntry{Name: "litestream", Status: wire.HealthUnknown, Detail: bin + "; target unknown"}
	case res.NoDB:
		return wire.HealthEntry{Name: "litestream", Status: wire.HealthUnknown, Detail: bin + "; no database yet"}
	case res.NoTarget:
		return wire.HealthEntry{Name: "litestream", Status: wire.HealthUnknown, Detail: bin + "; no replica target configured"}
	case res.ParseErr != nil:
		return wire.HealthEntry{Name: "litestream", Status: wire.HealthFail, Detail: bin + "; target UNPARSEABLE"}
	}

	target := bin + "; target " + string(res.Target.Type) + " configured"
	switch {
	case res.FreshErr != nil:
		return wire.HealthEntry{Name: "litestream", Status: wire.HealthUnknown, Detail: target + "; RPO probe failed"}
	case res.FreshOK:
		return wire.HealthEntry{Name: "litestream", Status: wire.HealthOK, Detail: target + "; replica frames observed"}
	case res.Target.Remote():
		return wire.HealthEntry{Name: "litestream", Status: wire.HealthUnknown, Detail: target + "; remote RPO age needs the binary"}
	default:
		return wire.HealthEntry{Name: "litestream", Status: wire.HealthWarn, Detail: target + "; no replicated frames yet"}
	}
}

// Tool is an optional tool exocompd shells out to. It is not required.
type Tool struct {
	Name string
	Args []string
}

// OptionalTools is the fixed set the doctor text report, the JSON summary,
// GET /api/v1/health, and the sandbox grant-mapping (cmd/exocomp/sandbox.go)
// all read from — one list, so a tool added here is in scope everywhere at
// once.
var OptionalTools = []Tool{
	{Name: "ansible-playbook", Args: []string{"--version"}},
	{Name: "tofu", Args: []string{"version"}},
	{Name: "restic", Args: []string{"version"}},
	{Name: "age", Args: []string{"--version"}},
	{Name: "sops", Args: []string{"--version"}},
	// uv resolves project dependency manifests at runtime and git syncs git
	// sources; both are declared runtime dependencies (ADR 0011), so a missing
	// one is a real gap the operator should see rather than discover mid-run.
	{Name: "uv", Args: []string{"--version"}},
	{Name: "git", Args: []string{"--version"}},
}

// ManifestEnv names the environment variable pointing at the image toolchain
// manifest; the container sets it to /opt/exocomp/MANIFEST.json (ADR 0006).
const ManifestEnv = "EXOCOMP_TOOLCHAIN_MANIFEST"

// ToolProbe is one optional tool's resolved state: whether it is on PATH, the
// bare version token the extraction rule pulled from its version probe, and
// the human line the text report prints. Version is best-effort — empty means
// no rule matched, never that the tool is absent.
type ToolProbe struct {
	Tool    Tool
	Found   bool
	Version string
	Text    string
}

// toolVersionRule maps a probed binary to the manifest key that pins it and a
// regexp that extracts its bare version token from the version probe's first
// line. Only tools with both a probe and a stable token appear; ansible_runner
// and python have no probe, and nono's pin belongs to the sandbox section.
type toolVersionRule struct {
	manifestKey string
	pattern     *regexp.Regexp
}

var toolVersionRules = map[string]toolVersionRule{
	"ansible-playbook": {"ansible_core", regexp.MustCompile(`\[core ([0-9][^\]]*)\]`)},
	"tofu":             {"tofu", regexp.MustCompile(`OpenTofu v?([0-9][^\s]*)`)},
	"restic":           {"restic", regexp.MustCompile(`restic ([0-9][^\s]*)`)},
	"age":              {"age", regexp.MustCompile(`^v?([0-9][^\s]*)`)},
	"sops":             {"sops", regexp.MustCompile(`sops ([0-9][^\s]*)`)},
	"uv":               {"uv", regexp.MustCompile(`uv ([0-9][^\s]*)`)},
	"git":              {"git", regexp.MustCompile(`git version ([0-9][^\s]*)`)},
}

// toolVersionToken applies tool's extraction rule to a version-probe line,
// best-effort and deterministic. An unknown tool or a non-matching line
// reports no token rather than guessing.
func toolVersionToken(tool, line string) (string, bool) {
	rule, ok := toolVersionRules[tool]
	if !ok {
		return "", false
	}
	m := rule.pattern.FindStringSubmatch(line)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// ProbeTool resolves one optional tool: LookPath, then the version invocation
// with its extraction rule applied to the first line.
func ProbeTool(t Tool) ToolProbe {
	p := ToolProbe{Tool: t}
	path, err := exec.LookPath(t.Name)
	if err != nil {
		p.Text = "MISSING"
		return p
	}
	p.Found = true

	out, err := exec.Command(path, t.Args...).CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			p.Text = fmt.Sprintf("found at %s (version probe exited %d)", path, exitErr.ExitCode())
		} else {
			p.Text = fmt.Sprintf("found at %s (version probe failed: %v)", path, err)
		}
		return p
	}
	line := FirstLine(out)
	p.Text = fmt.Sprintf("found at %s (%s)", path, line)
	if v, ok := toolVersionToken(t.Name, line); ok {
		p.Version = v
	}
	return p
}

// ProbeOptionalTools probes every tool in the fixed OptionalTools set once.
func ProbeOptionalTools() []ToolProbe {
	out := make([]ToolProbe, 0, len(OptionalTools))
	for _, t := range OptionalTools {
		out = append(out, ProbeTool(t))
	}
	return out
}

// ToolDeviation is one tool whose resolved version differs from its pin.
type ToolDeviation struct {
	Tool string
	Got  string
	Pin  string
}

// ToolchainProbe is the manifest comparison: the active manifest path, the
// tools that deviate from their pins, and the tools whose version would not
// parse. Err is a manifest that could not be read; an unset manifest is the
// zero value, a state rather than an error.
type ToolchainProbe struct {
	Path       string
	Deviations []ToolDeviation
	Unparsed   []string
	Err        error
}

// Deviation returns the deviation recorded for tool, if any.
func (tc ToolchainProbe) Deviation(tool string) (ToolDeviation, bool) {
	for _, d := range tc.Deviations {
		if d.Tool == tool {
			return d, true
		}
	}
	return ToolDeviation{}, false
}

// ProbeToolchain reads the manifest at ManifestEnv and compares every probed
// tool it can parse against its pin. It is best-effort and never fatal: no
// manifest env is the empty probe, an unreadable manifest lands in Err, and a
// version that will not parse lands in Unparsed — none of them change a job's
// outcome, and a tool with no pin or no probe is simply skipped.
func ProbeToolchain(probes []ToolProbe) ToolchainProbe {
	path := os.Getenv(ManifestEnv)
	if path == "" {
		return ToolchainProbe{}
	}
	tc := ToolchainProbe{Path: path}
	body, err := os.ReadFile(path)
	if err != nil {
		tc.Err = err
		return tc
	}
	var pins map[string]string
	if err := json.Unmarshal(body, &pins); err != nil {
		tc.Err = err
		return tc
	}
	for _, p := range probes {
		rule, ok := toolVersionRules[p.Tool.Name]
		if !ok || !p.Found {
			continue
		}
		pin := pins[rule.manifestKey]
		if pin == "" {
			continue
		}
		if p.Version == "" {
			tc.Unparsed = append(tc.Unparsed, p.Tool.Name)
			continue
		}
		if p.Version != pin {
			tc.Deviations = append(tc.Deviations, ToolDeviation{Tool: p.Tool.Name, Got: p.Version, Pin: pin})
		}
	}
	return tc
}

// optionalToolsEntry rolls every tool probe and the toolchain-manifest
// comparison into one subsystem status: a missing tool is a warn (every one of
// them is still optional) and a version off its pin is a warn too — the ADR
// 0006 escape hatch is made visible, not refused. No manifest at all is
// unknown, a state to read.
func optionalToolsEntry(probes []ToolProbe, tc ToolchainProbe) wire.HealthEntry {
	var missing []string
	for _, p := range probes {
		if !p.Found {
			missing = append(missing, p.Tool.Name)
		}
	}

	status := wire.HealthOK
	var parts []string
	if len(missing) > 0 {
		status = worseHealth(status, wire.HealthWarn)
		parts = append(parts, "missing: "+strings.Join(missing, ", "))
	} else {
		parts = append(parts, "all present")
	}

	switch {
	case tc.Path == "":
		status = worseHealth(status, wire.HealthUnknown)
		parts = append(parts, "toolchain manifest not set")
	case tc.Err != nil:
		status = worseHealth(status, wire.HealthUnknown)
		parts = append(parts, "toolchain manifest unreadable")
	default:
		if len(tc.Deviations) > 0 {
			status = worseHealth(status, wire.HealthWarn)
			var devs []string
			for _, d := range tc.Deviations {
				devs = append(devs, fmt.Sprintf("%s %s (pinned %s)", d.Tool, d.Got, d.Pin))
			}
			parts = append(parts, "deviates: "+strings.Join(devs, ", "))
		}
		if len(tc.Unparsed) > 0 {
			parts = append(parts, "version unparsed: "+strings.Join(tc.Unparsed, ", "))
		}
	}
	return wire.HealthEntry{Name: "optional_tools", Status: status, Detail: strings.Join(parts, "; ")}
}

// FirstLine returns the first line of b, trimmed. It is shared by the tool
// version probe and by callers that need a one-line summary of a subprocess's
// output (cmd/exocomp/upgrade.go's error context).
func FirstLine(b []byte) string {
	line, _, _ := strings.Cut(string(b), "\n")
	return strings.TrimSpace(line)
}
