package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/source"
	"exocomp.dev/exocomp/internal/store"
)

func init() {
	registerCommand(newHostCmd)
}

// onboardRunbook is the seeded fleet-onboarding playbook. exocomp writes it into
// the data volume so an operator runs it through the same sandboxed, gated queue
// as any other play, rather than pasting shell onto a box by hand.
//
//go:embed runbooks/onboard.yml
var onboardRunbook []byte

// fleetKeySecret is the sealed-secret name the fleet SSH private key lives under.
// One key for the whole fleet, sealed in the envelope and loaded into a per-job
// agent from memory (Decision 0010) — never written to disk on a run.
const fleetKeySecret = "fleet/ssh"

// fleetKeyKind labels the sealed fleet key.
const fleetKeyKind = "fleet-ssh-key"

// keyscanHost scans a host's SSH keys on the port it serves them from. It is a
// package variable so a test drives onboarding without a live sshd; production
// runs ssh-keyscan. A non-default port matters because a tofu-derived host
// carries whatever ansible_port its inventory declares, and ssh-keyscan records
// such a host under the `[host]:port` name known_hosts expects.
var keyscanHost = func(ctx context.Context, host string, port int) ([]byte, error) {
	args := []string{"-T", "5", "-t", "ed25519,rsa,ecdsa"}
	if port != 0 && port != executor.SSHPort {
		args = append(args, "-p", strconv.Itoa(port))
	}
	out, err := exec.CommandContext(ctx, "ssh-keyscan", append(args, host)...).Output()
	if err != nil {
		return nil, fmt.Errorf("ssh-keyscan %s: %w", host, err)
	}
	return out, nil
}

func newHostCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "host",
		Short: "Onboard and track managed fleet hosts (Decision 0010)",
	}
	cmd.AddCommand(newHostOnboardCmd(), newHostKnownHostsCmd())
	return cmd
}

func newHostOnboardCmd() *cobra.Command {
	var rotate bool
	onboard := &cobra.Command{
		Use:   "onboard <host>...",
		Short: "Record host keys, ensure the fleet key, seed the onboarding runbook",
		Long: "Prepares one or more hosts for fleet automation: it ssh-keyscans each host " +
			"and records its keys in the backed-up known_hosts store, ensures the sealed " +
			"fleet SSH key exists (generating it on first run), and writes the seeded " +
			"onboarding runbook into the data volume. It then prints the fleet public key " +
			"to authorize and the exact `playbook run` that pushes it, creates the " +
			"automation user and grants it passwordless become (broad by default; " +
			"restrict the seeded sudoers to a command set if you need least privilege) " +
			"— through the sandboxed, gated queue.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runHostOnboard(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), args, rotate)
		},
	}
	onboard.Flags().BoolVar(&rotate, "rotate", false,
		"replace an already-recorded host key that changed (confirm the new fingerprint out of band first)")
	return onboard
}

func newHostKnownHostsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "known-hosts",
		Short: "Print the recorded known_hosts file",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) (err error) {
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() {
				if cerr := st.Close(); cerr != nil && err == nil {
					err = cerr
				}
			}()
			hosts, err := st.ListKnownHosts(cmd.Context())
			if err != nil {
				return err
			}
			_, err = io.WriteString(cmd.OutOrStdout(), store.RenderKnownHosts(hosts))
			return err
		},
	}
}

func runHostOnboard(ctx context.Context, out, warn io.Writer, hosts []string, rotate bool) (err error) {
	dir, err := dataDir()
	if err != nil {
		return fmt.Errorf("host onboard: resolve data dir: %w", err)
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer func() {
		if cerr := st.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	env, err := secrets.Open(dir)
	if err != nil {
		return fmt.Errorf("host onboard: open envelope (run `exocomp init` first): %w", err)
	}
	defer func() { _ = env.Close() }()

	pub, err := ensureFleetKey(ctx, st, env)
	if err != nil {
		return err
	}

	recorded := 0
	for _, host := range hosts {
		scanned, err := keyscanHost(ctx, host, executor.SSHPort)
		if err != nil {
			_, _ = fmt.Fprintf(warn, "WARNING: %v; %s not recorded\n", err, host)
			continue
		}
		n, err := recordScanned(ctx, st, scanned, rotate, out)
		if err != nil {
			return err
		}
		if n == 0 {
			_, _ = fmt.Fprintf(warn, "WARNING: no host keys returned for %s\n", host)
			continue
		}
		recorded += n
		_, _ = fmt.Fprintf(out, "recorded %d host key(s) for %s\n", n, host)
	}

	runbookPath, err := seedOnboardRunbook(dir)
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintf(out, "recorded %d host key(s) into the backed-up known_hosts store\n", recorded)
	_, _ = fmt.Fprintln(out, "authorize this fleet key on each host's initial admin account:")
	_, _ = fmt.Fprintln(out, pub)
	_, _ = fmt.Fprintf(out, "then push it, create the user, and grant sudo through the queue:\n")
	extra, err := extraVarsArgs(map[string]any{"exocomp_fleet_pubkey": pub})
	if err != nil {
		return err
	}
	// The extra-var is printed in JSON form on purpose: ansible's k=v form
	// splits on whitespace, and an authorized_keys line has two spaces in it.
	_, _ = fmt.Fprintf(out, "  exocomp playbook run %s --inventory %s \\\n", runbookPath, strings.Join(hosts, ","))
	_, _ = fmt.Fprintf(out, "    %s '%s'\n", extra[0], extra[1])
	return nil
}

// ensureFleetKey returns the fleet public key, generating and sealing the
// private half on first call. One fleet key serves every host; it is sealed in
// the envelope and never written to disk on a run.
func ensureFleetKey(ctx context.Context, st *store.Store, env *secrets.Envelope) (string, error) {
	if row, err := st.GetSecret(ctx, fleetKeySecret); err == nil {
		// A row under the fleet key's name that onboarding did not seal (one
		// added by hand or through another face) is not trusted as the key
		// every host will authorize.
		if row.Kind != fleetKeyKind {
			return "", fmt.Errorf("host onboard: %s holds a %s secret, not the fleet key; "+
				"remove it with `exocomp --local secrets rm %s` and onboard again", fleetKeySecret, row.Kind, fleetKeySecret)
		}
		pem, err := env.OpenSecret(secrets.SealedSecret{
			Name: row.Name, Kind: row.Kind, Nonce: row.Nonce, Ciphertext: row.Ciphertext,
		})
		if err != nil {
			return "", fmt.Errorf("host onboard: open fleet key: %w", err)
		}
		pub, err := source.PublicFromPrivatePEM(pem, "exocomp-fleet")
		if err != nil {
			return "", fmt.Errorf("host onboard: derive fleet public key: %w", err)
		}
		return pub, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}

	key, err := source.GenerateDeployKey("fleet")
	if err != nil {
		return "", fmt.Errorf("host onboard: generate fleet key: %w", err)
	}
	sealed, err := env.Seal(fleetKeySecret, fleetKeyKind, key.PrivatePEM)
	if err != nil {
		return "", fmt.Errorf("host onboard: seal fleet key: %w", err)
	}
	if _, err := st.CreateSecret(ctx, store.SecretRow{
		Name: sealed.Name, Kind: sealed.Kind, Nonce: sealed.Nonce,
		Ciphertext: sealed.Ciphertext, AAD: sealed.Name + "|" + sealed.Kind,
	}); err != nil {
		return "", fmt.Errorf("host onboard: store fleet key: %w", err)
	}
	return key.PublicAuthorizedKey, nil
}

// recordScanned parses ssh-keyscan output and records each host key on first
// sight, printing its fingerprint so first-use trust is a deliberate act.
// Comment and blank lines are skipped; a malformed line is skipped rather than
// failing the whole scan. A host key that DIFFERS from one already recorded is
// refused (ErrKnownHostChanged) unless rotate is set — a changed key is a rekey
// the operator confirms out of band, or an attacker at scan time.
func recordScanned(ctx context.Context, st *store.Store, scanned []byte, rotate bool, out io.Writer) (int, error) {
	n := 0
	sc := bufio.NewScanner(bytes.NewReader(scanned))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		kh := store.KnownHost{Host: fields[0], KeyType: fields[1], Key: fields[2]}
		record := st.RecordKnownHost
		if rotate {
			record = st.RotateKnownHost
		}
		if err := record(ctx, kh); err != nil {
			if errors.Is(err, store.ErrKnownHostChanged) {
				return n, fmt.Errorf("%w — verify the new key out of band, then re-run with --rotate", err)
			}
			return n, err
		}
		_, _ = fmt.Fprintf(out, "  %s %s %s\n", kh.Host, kh.KeyType, sshKeyFingerprint(kh.Key))
		n++
	}
	if err := sc.Err(); err != nil {
		return n, fmt.Errorf("host onboard: read scan output: %w", err)
	}
	return n, nil
}

// sshKeyFingerprint renders the SHA256 fingerprint OpenSSH shows, so an operator
// can verify a recorded key against the host's own `ssh-keygen -lf`.
func sshKeyFingerprint(base64Key string) string {
	raw, err := base64.StdEncoding.DecodeString(base64Key)
	if err != nil {
		return "(unparseable)"
	}
	sum := sha256.Sum256(raw)
	return "SHA256:" + strings.TrimRight(base64.StdEncoding.EncodeToString(sum[:]), "=")
}

// seedOnboardRunbook writes the embedded onboarding playbook into the data
// volume (a path source in the data volume) so it runs through the queue like
// any other play.
func seedOnboardRunbook(dir string) (string, error) {
	return seedRunbook(dir, "onboard.yml", onboardRunbook)
}
