package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/store"
)

// litestreamBinary is the sidecar exocomp shells out to for a fast-RPO restore.
// It is never compiled in and never run inside exocompd (Decision 0001): the daemon
// generates the sidecar's config and, here, drives a one-shot restore beside it.
const litestreamBinary = "litestream"

// SettingReplicaURL is the setting key the replicate lane writes the configured
// Litestream replica URL under, and the restore path reads back when no
// --replica is passed. It is a non-secret locator; a replica's credentials live
// in the sealed secret plane, never in a plaintext value here.
const SettingReplicaURL = "litestream.replica_url"

// RecoveryGoal names why a restore is running, which is what decides the source.
type RecoveryGoal int

const (
	// GoalFreshestDB wants exocomp.db as current as possible — the seconds-RPO
	// case Litestream exists for. It recovers the DATABASE ONLY.
	GoalFreshestDB RecoveryGoal = iota
	// GoalAuthoritative wants the full, encrypted DR chain — keyring, secrets,
	// everything. That is always restic; Litestream never substitutes for it.
	GoalAuthoritative
)

const (
	sourceLitestream = "litestream"
	sourceRestic     = "restic"
	scopeDBOnly      = "db-only"
	scopeFull        = "full"
)

// SourceAvailability is what the restore path found on the box: which of the two
// chains can actually run right now.
type SourceAvailability struct {
	// LitestreamBinary is true when the sidecar binary is on PATH.
	LitestreamBinary bool
	// Replica is true when a replica URL is configured (setting or flag).
	Replica bool
	// Restic is true when a restic target is reachable for a full restore.
	Restic bool
	// Keyring is true when a keyring/DEK is on hand to OPEN a restored DB — from
	// a prior restic restore or an existing data dir. A Litestream restore ships
	// the database alone, so without this the recovered bytes cannot be trusted.
	Keyring bool
}

// RestorePlan is the decided route: which source, how much it recovers, and
// whether the recovered DB must survive a decrypt round-trip before trust.
type RestorePlan struct {
	// Source is "litestream" or "restic".
	Source string
	// Scope is "db-only" (Litestream) or "full" (restic).
	Scope string
	// VerifyDecrypt requires opening the envelope against the restored DB before
	// trusting it. Litestream ships unauthenticated, unencrypted DB bytes, so a
	// decrypt round-trip is the only proof they are yours.
	VerifyDecrypt bool
	// Reason is a one-line explanation for the operator and the logs.
	Reason string
}

// ErrNoRestoreSource reports that neither chain can run for the requested goal.
var ErrNoRestoreSource = errors.New("backup: no usable restore source")

// ErrNoKeyringForDBOnly reports a fast-RPO restore with nothing to open the
// recovered database with. Litestream recovers exocomp.db alone; the keyring
// that unwraps its sealed rows comes from restic or an existing data dir, and
// without it the bytes can be neither trusted nor read.
var ErrNoKeyringForDBOnly = errors.New(
	"backup: litestream recovers the database only, but no keyring is available to open it")

// PlanRestore decides the source and scope for a recovery goal given what is
// available. It is the precedence rule in one place — Litestream is the
// fast-RPO complement, restic stays the authoritative encrypted chain:
//
//   - Authoritative recovery is ALWAYS restic. Litestream never stands in for the
//     encrypted, self-describing chain that carries the keyring and secrets.
//   - Freshest-DB recovery prefers Litestream (seconds RPO), but for the database
//     only, and only when a keyring is on hand to open the result. With no
//     litestream binary or no replica it falls back to the restic chain, trading
//     seconds of RPO for restic's cadence.
func PlanRestore(goal RecoveryGoal, avail SourceAvailability) (RestorePlan, error) {
	switch goal {
	case GoalAuthoritative:
		if !avail.Restic {
			return RestorePlan{}, fmt.Errorf("%w: authoritative recovery needs a restic target", ErrNoRestoreSource)
		}
		return RestorePlan{
			Source: sourceRestic,
			Scope:  scopeFull,
			Reason: "authoritative encrypted DR: keyring, secrets, and database from restic",
		}, nil
	case GoalFreshestDB:
		if avail.LitestreamBinary && avail.Replica {
			if !avail.Keyring {
				return RestorePlan{}, ErrNoKeyringForDBOnly
			}
			return RestorePlan{
				Source:        sourceLitestream,
				Scope:         scopeDBOnly,
				VerifyDecrypt: true,
				Reason:        "fast-RPO database recovery from the Litestream replica, verified by decrypt round-trip",
			}, nil
		}
		if avail.Restic {
			return RestorePlan{
				Source: sourceRestic,
				Scope:  scopeFull,
				Reason: litestreamGapReason(avail) +
					"; falling back to the restic chain (RPO degrades to restic's cadence)",
			}, nil
		}
		return RestorePlan{}, fmt.Errorf(
			"%w: neither a litestream replica nor a restic target is available", ErrNoRestoreSource)
	default:
		return RestorePlan{}, fmt.Errorf("backup: unknown recovery goal %d", goal)
	}
}

func litestreamGapReason(avail SourceAvailability) string {
	switch {
	case !avail.LitestreamBinary:
		return "litestream binary absent"
	case !avail.Replica:
		return "no litestream replica configured"
	default:
		return "litestream unavailable"
	}
}

// ReplicaSafety classifies a Litestream replica URL by whether shipping WAL
// frames there is safe WITHOUT an explicit operator acknowledgement. Litestream
// does not encrypt (governing constraint): frames land as plaintext, so the
// target must be LAN-local or an already-encrypted store. A local path or a
// private/loopback host is LAN-local and safe; a public remote is not, and needs
// the ack the operator gives only when the store itself is encrypted.
func ReplicaSafety(replicaURL string) (safe bool, reason string) {
	s := strings.TrimSpace(replicaURL)
	if s == "" {
		return false, "empty replica url"
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" {
		// No scheme is a bare filesystem path — a local replica.
		return true, "local filesystem path"
	}
	if strings.EqualFold(u.Scheme, "file") {
		return true, "local file replica"
	}
	host := u.Hostname()
	if host == "" {
		return true, "no remote host"
	}
	if isLANHost(host) {
		return true, "LAN-local host " + host
	}
	return false, "public remote " + strings.ToLower(u.Scheme) + "://" + host +
		" would receive plaintext WAL frames"
}

// isLANHost reports whether a host is on the local network — a loopback,
// private, or link-local address, or a .local/.lan/localhost name.
func isLANHost(host string) bool {
	h := strings.ToLower(host)
	if h == "localhost" || strings.HasSuffix(h, ".local") || strings.HasSuffix(h, ".lan") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

// LitestreamConfig drives one fast-RPO restore of exocomp.db from a replica.
type LitestreamConfig struct {
	// ReplicaURL is the Litestream replica to restore from.
	ReplicaURL string
	// DestDB is the exocomp.db path to write. It sits inside a data dir that
	// ALREADY holds the keyring (from restic or a live install): Litestream
	// recovers the database only, and the keyring is what opens it.
	DestDB string
	// Ack acknowledges a plaintext public remote — the operator asserting the
	// store is itself encrypted. Without it a public remote is refused.
	Ack bool
	// Driver runs the subprocess. Empty uses the local shell driver.
	Driver executor.Driver
	// Env is extra environment for litestream. Replica credentials come from the
	// sealed secret plane via the caller, never a plaintext config on disk.
	Env map[string]string
	// Warn receives non-fatal findings (a plaintext remote accepted under Ack).
	Warn func(string)
}

// ErrLitestreamAbsent reports the sidecar binary is not installed. The fast-RPO
// path is optional; its absence is an honest refusal, not a crash.
var ErrLitestreamAbsent = errors.New("backup: litestream binary not found on PATH")

// ErrReplicaUnsafe reports a plaintext public remote replica used without the
// encryption acknowledgement.
var ErrReplicaUnsafe = errors.New("backup: refusing a plaintext remote replica without acknowledgement")

// LitestreamAvailable reports whether the sidecar binary is on PATH. The restore
// path calls it to refuse cleanly rather than shell a missing tool.
func LitestreamAvailable() bool {
	_, err := exec.LookPath(litestreamBinary)
	return err == nil
}

// LitestreamRestore recovers exocomp.db from the replica by shelling
// `litestream restore`. It recovers the DATABASE ONLY — never the keyring or
// secrets — and gates on the exit code alone (invariant 3). The caller MUST run
// the decrypt round-trip (VerifyEnvelopeOpens) before trusting the result: the
// bytes arrive unauthenticated and unencrypted.
func LitestreamRestore(ctx context.Context, cfg LitestreamConfig) error {
	if cfg.ReplicaURL == "" {
		return errors.New("backup: litestream restore needs a replica url")
	}
	if cfg.DestDB == "" {
		return errors.New("backup: litestream restore needs a destination db path")
	}
	if !LitestreamAvailable() {
		return ErrLitestreamAbsent
	}
	if safe, reason := ReplicaSafety(cfg.ReplicaURL); !safe {
		if !cfg.Ack {
			return fmt.Errorf("%w: %s; encrypt the store or pass the acknowledgement", ErrReplicaUnsafe, reason)
		}
		emitWarn(cfg.Warn, "restoring from an unencrypted remote replica under explicit ack: "+reason)
	}

	driver := cfg.Driver
	if driver == nil {
		driver = executor.NewShell()
	}
	res, err := runTool(ctx, driver, executor.Spec{
		Kind: "shell",
		Argv: []string{litestreamBinary, "restore", "-o", cfg.DestDB, cfg.ReplicaURL},
		Env:  toolEnv(cfg.Env),
	}, nil)
	if err != nil {
		return err
	}
	if res.exitCode != 0 {
		return fmt.Errorf("backup: litestream restore exited %d: %s", res.exitCode, res.tail(5))
	}
	return nil
}

// VerifyEnvelopeOpens is the decrypt round-trip gate a fast-RPO restore must
// pass before its database is trusted, over a whole data dir: it opens the
// database at RestoredDBName using the keyring beside it. See VerifyDBDecrypts
// for the check itself.
func VerifyEnvelopeOpens(ctx context.Context, dataDir, identityFile string) (string, error) {
	return VerifyDBDecrypts(ctx,
		filepath.Join(dataDir, RestoredDBName),
		filepath.Join(dataDir, KeyringName),
		identityFile)
}

// VerifyDBDecrypts is the decrypt round-trip gate a fast-RPO restore must pass
// before its database is trusted. Litestream ships the database alone,
// unauthenticated and unencrypted, so the only proof the bytes are yours is that
// keyringFile unwraps the DEK AND that DEK opens a sealed row inside the database
// at dbPath. It opens one row and returns its name; the plaintext is wiped and
// never returned. A database with no sealed rows cannot be proven this way and is
// reported as such rather than trusted silently. Taking the paths explicitly lets
// a caller verify a freshly restored file before promoting it over the live one.
func VerifyDBDecrypts(ctx context.Context, dbPath, keyringFile, identityFile string) (string, error) {
	ids, err := ReadIdentities(identityFile)
	if err != nil {
		return "", err
	}
	keyring, err := os.ReadFile(keyringFile)
	if err != nil {
		return "", fmt.Errorf("backup: read keyring beside the restored db: %w", err)
	}
	dek, err := UnwrapDEK(keyring, ids)
	if err != nil {
		return "", err
	}
	defer clear(dek)

	st, err := store.Open(dbPath)
	if err != nil {
		return "", err
	}
	defer func() { _ = st.Close() }()

	rows, err := st.ListSecrets(ctx)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", errors.New("backup: restored database holds no sealed rows to prove the decrypt round-trip")
	}
	row := rows[0]
	full, err := st.GetSecret(ctx, row.Name)
	if err != nil {
		return "", err
	}
	pt, err := OpenSealed(dek, full.Name, full.Kind, full.Nonce, full.Ciphertext)
	if err != nil {
		return "", fmt.Errorf("backup: decrypt round-trip failed on %q: %w", full.Name, err)
	}
	clear(pt)
	return full.Name, nil
}

func emitWarn(f func(string), msg string) {
	if f != nil {
		f(msg)
		return
	}
	slog.Default().Warn(msg)
}
