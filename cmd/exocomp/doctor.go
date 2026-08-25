package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/health"
	"exocomp.dev/exocomp/internal/litestream"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/wire"
)

// optionalTool mirrors health.Tool's name field with an unexported field, so
// sandbox.go's grant-mapping loop (t.name) keeps compiling against this
// package's own type; optionalTools is derived from health.OptionalTools once
// at init, so the tool set itself has exactly one definition.
type optionalTool struct{ name string }

var optionalTools = deriveOptionalTools()

func deriveOptionalTools() []optionalTool {
	out := make([]optionalTool, len(health.OptionalTools))
	for i, t := range health.OptionalTools {
		out[i] = optionalTool{name: t.Name}
	}
	return out
}

// dataDirMode is the data dir's required mode; servelock.go's
// acquireServeLock also creates the dir at this mode. Mirrors
// health.DataDirMode, the canonical definition.
const dataDirMode = health.DataDirMode

// firstLine is upgrade.go's one-line subprocess-output summarizer, forwarding
// to health.FirstLine so the tool-version probe text and this share one
// definition.
func firstLine(b []byte) string { return health.FirstLine(b) }

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
			// In client mode doctor reports the REMOTE daemon's health summary
			// (GET /api/v1/health), not this workstation's local environment: the
			// operator asked about the box the other verbs target. The JSON is the
			// same wire.HealthView the daemon's own `doctor --output json` emits.
			if rc, ok, err := remoteClient(cmd); err != nil {
				return err
			} else if ok {
				view, err := rc.Health(cmd.Context())
				if err != nil {
					return remoteFail(cmd, mode, err)
				}
				return renderRemoteHealth(cmd, mode, view)
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
	report, fault := health.InspectDataDir(dataDir)
	p("data dir:     %s — %s\n", dataDir, report)
	if fault != nil {
		return fmt.Errorf("doctor: %w", fault)
	}

	reportSecretsProtection(w, dataDir)

	p("optional tools (probe subset):\n")
	for _, t := range health.OptionalTools {
		_, text := health.ProbeToolResult(t)
		p("  %-18s %s\n", t.Name, text)
	}

	reportSandbox(context.Background(), w)
	reportCaps(w)
	reportRunSecretFileStaging(w, dataDir)
	reportSelfObservability(context.Background(), w, dataDir)
	reportLitestream(context.Background(), w, dataDir)
	// The report prints first, so an operator sees why; then an enforcing
	// request that cannot be enforced is a failing check.
	return enforceSandboxTier(context.Background())
}

// doctorHealth resolves the data dir and rewires into internal/health.Probe —
// the single probe definition `doctor --output json` and GET /api/v1/health
// both call, so a resource's CLI and API reads can never diverge. Unlike the
// text report, it never short-circuits on a data-dir fault: an agent asking
// for the summary wants the full picture in one call, and the per-subsystem
// status carries the fault — not the process exit code, which stays 0 as long
// as the probes themselves could run.
func doctorHealth(ctx context.Context) wire.HealthView {
	dir, err := dataDir()
	if err != nil {
		return wire.HealthView{{
			Name: "data_dir", Status: wire.HealthUnknown, Detail: "unknown (" + err.Error() + ")",
		}}
	}
	return health.Probe(ctx, dir, healthOpenStore)
}

// healthOpenStore adapts cmd/exocomp's own store opener (backup.go) to
// internal/health's OpenStore seam: a fresh connection per probe, closed
// after — the CLI's connection lifecycle before this package split.
func healthOpenStore() (health.Store, func() error, error) {
	st, err := openStore()
	if err != nil {
		return nil, nil, err
	}
	return st, st.Close, nil
}

// reportSecretsProtection prints the real at-rest and in-memory posture of the
// key material: whether the DEK can be pinned out of swap (E5). The host-key
// at-rest state is added alongside it.
func reportSecretsProtection(w io.Writer, dataDir string) {
	printSecretsProtection(w, health.ProbeSecretsProtection(dataDir))
}

func printSecretsProtection(w io.Writer, res health.SecretsProtectionProbe) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	p("secrets protection:\n")

	switch res.HostKey {
	case secrets.HostKeySystemdCredential:
		p("  %-18s %s\n", "host key at rest",
			"systemd credential — unwrap secret is outside the data volume"+health.TPMSuffix())
	case secrets.HostKeyPassphrase:
		p("  %-18s %s\n", "host key at rest",
			"passphrase-wrapped — a stolen data volume alone does not yield it")
	case secrets.HostKeyPlaintext:
		p("  %-18s %s\n", "host key at rest",
			"PLAINTEXT in the data dir — a volume read alone yields every secret; "+health.HostKeyFixHint)
	default:
		p("  %-18s %s\n", "host key at rest", "absent — run `exocomp init`")
	}

	if res.Lock.Locked {
		p("  %-18s %s\n", "memory lock", "mlock available — the DEK is pinned out of swap and core dumps")
	} else {
		p("  %-18s %s\n", "memory lock",
			"UNAVAILABLE — the DEK may reach swap or a core dump ("+res.Lock.Err.Error()+")")
	}
}

// reportSelfObservability prints whether the daemon can be watched: the
// metrics endpoint reachable, a dead-man ping configured, and any active
// silence or mute. None of these fail the check — they are what an operator
// reads to answer "is anything holding it back, and would I be told if it
// broke".
func reportSelfObservability(ctx context.Context, w io.Writer, dataDir string) {
	printSelfObservability(w, health.ProbeSelfObservability(ctx, dataDir, healthOpenStore))
}

func printSelfObservability(w io.Writer, res health.SelfObservabilityProbe) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	p("self-observability:\n")
	p("  %-18s %s\n", "metrics", res.MetricsText)
	p("  %-18s %s\n", "dead-man", health.DeadManStatus())
	p("  %-18s %s\n", "silence/mute", res.HoldText)
}

// holdStatus is the text-only view of health.ProbeHoldStatus.
func holdStatus(ctx context.Context) string {
	dir, err := dataDir()
	if err != nil {
		return "unknown (" + err.Error() + ")"
	}
	_, text := health.ProbeHoldStatus(ctx, dir, healthOpenStore)
	return text
}

// probeMetrics is the text-only view of health.ProbeMetricsResult.
func probeMetrics(ctx context.Context, dataDir string) string {
	_, text := health.ProbeMetricsResult(ctx, dataDir)
	return text
}

// reportLitestream reports the optional WAL-replication sidecar: whether the
// binary is installed, what replica target is configured, and the RPO age (the
// staleness of the newest replicated frame). Nothing here fails the check — the
// sidecar is optional, so an absent binary or an unconfigured target is a state
// to read, said plainly.
func reportLitestream(ctx context.Context, w io.Writer, dataDir string) {
	printLitestream(w, health.ProbeLitestream(ctx, dataDir, healthOpenStore))
}

func printLitestream(w io.Writer, res health.LitestreamProbe) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }
	p("litestream sidecar:\n")

	switch {
	case res.BinPath == "":
		p("  %-18s %s\n", "binary", "not installed (optional) — "+litestream.InstallHint)
	case res.Version == "":
		p("  %-18s %s\n", "binary", "found at "+res.BinPath+" (version probe failed)")
	default:
		p("  %-18s %s\n", "binary", "found at "+res.BinPath+" ("+res.Version+")")
	}

	switch {
	case res.DirErr != nil:
		p("  %-18s %s\n", "target", "unknown ("+res.DirErr.Error()+")")
		return
	case res.NoDB:
		p("  %-18s %s\n", "target", "no database yet")
		return
	case res.StoreErr != nil:
		p("  %-18s %s\n", "target", "unknown ("+res.StoreErr.Error()+")")
		return
	case res.NoTarget:
		p("  %-18s %s\n", "target", "none configured — `exocomp litestream target <uri>`")
		p("  %-18s %s\n", "RPO age", "unknown (no replica target)")
		return
	case res.TargetErr != nil:
		p("  %-18s %s\n", "target", "unknown ("+res.TargetErr.Error()+")")
		return
	case res.ParseErr != nil:
		p("  %-18s %s\n", "target", res.TargetRaw+" (UNPARSEABLE: "+res.ParseErr.Error()+")")
		return
	}
	p("  %-18s %s\n", "target", res.Target.Raw+" ("+string(res.Target.Type)+")")

	switch {
	case res.FreshErr != nil:
		p("  %-18s %s\n", "RPO age", "unknown ("+res.FreshErr.Error()+")")
	case res.FreshOK:
		p("  %-18s %s\n", "RPO age", time.Since(res.FreshMtime).Truncate(time.Second).String()+" (newest replicated frame)")
	case res.Target.Remote():
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
