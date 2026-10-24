package main

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/client"
)

func init() {
	registerCommand(newLoginCmd)
}

// tokenFlag and tokenEnv are the bearer-token inputs, shared by `login` and the
// global client-mode resolution. They are named here (login registers the env
// name too) so the two paths agree on precedence.
const (
	tokenFlag = "token"
	tokenEnv  = "EXOCOMP_TOKEN"
)

func newLoginCmd() *cobra.Command {
	var (
		name       string
		noDefault  bool
		replacePin bool
	)
	cmd := &cobra.Command{
		Use:   "login <endpoint>",
		Short: "Pin a remote exocompd's certificate and store its token (TOFU)",
		Long: "Connects to a remote daemon over TLS, captures the self-signed certificate " +
			"fingerprint, shows it, and — on confirmation — records {endpoint, fingerprint, " +
			"token} in ~/.config/exocomp/hosts.toml (0600), SSH-known-hosts style. Later remote " +
			"commands verify the pinned fingerprint. Re-login updates the entry; a CHANGED " +
			"fingerprint (or an alias pointed at a different endpoint) warns loudly and is " +
			"REFUSED unless --replace-pin is passed. --assume-yes never replaces a pin: it is " +
			"the flag every script must already pass to pin at all, so accepting it as consent " +
			"to re-pin would make a machine-in-the-middle a silent success.",
		Example: "exocomp login hub.tailnet.ts.net:8443 --token exo_...",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLogin(cmd, args[0], name, !noDefault, replacePin)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "alias to store the remote under (default: the endpoint's first host label)")
	cmd.Flags().BoolVar(&noDefault, "no-default", false,
		"do not make this the default remote, which every command uses unless it passes --remote or --local")
	cmd.Flags().BoolVar(&replacePin, "replace-pin", false,
		"replace an existing DIFFERENT fingerprint or endpoint for this alias (a re-key "+
			"you confirmed out of band; --assume-yes does not grant this)")
	return cmd
}

func runLogin(cmd *cobra.Command, endpoint, name string, makeDefault, replacePin bool) error {
	out := cmd.OutOrStdout()
	endpoint = cleanEndpoint(endpoint)

	alias := name
	if alias == "" {
		var err error
		alias, err = defaultAlias(endpoint)
		if err != nil {
			return err
		}
	}

	token := resolveToken(cmd, hostEntry{})
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("login: no token; pass --token or set %s "+
			"(mint one on the box with `exocomp token new`)", tokenEnv)
	}

	fp, err := client.FetchFingerprint(cmd.Context(), endpoint)
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}

	cfg, err := loadHostsConfig()
	if err != nil {
		return err
	}
	existing, had := cfg.Hosts[alias]

	_, _ = fmt.Fprintf(out, "endpoint    %s\n", endpoint)
	_, _ = fmt.Fprintf(out, "fingerprint %s\n", fp)

	// The MITM guard: an alias already pinned to a DIFFERENT fingerprint is the
	// dangerous case. --assume-yes must not clear it: -y is a root persistent
	// flag and the ONLY way to pin at all on non-interactive stdin, so every
	// script, CI step and agent harness already passes it — accepting it as
	// consent to re-pin would silently bless whatever certificate answers.
	// Replacing a pin takes the deliberate, login-only
	// --replace-pin, exactly as a changed SSH host key takes a deliberate edit.
	switch {
	case had && existing.Fingerprint != "" && !sameFP(existing.Fingerprint, fp):
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
			"WARNING: the fingerprint for %q CHANGED\n  pinned:    %s\n  presented: %s\n"+
				"this is a re-key you must confirm out of band, or a machine-in-the-middle.\n",
			alias, existing.Fingerprint, fp)
		if !replacePin {
			return fmt.Errorf("login: refusing to overwrite the changed fingerprint pinned for %q; "+
				"confirm the new fingerprint out of band and re-run with --replace-pin", alias)
		}
	case had && existing.Endpoint != "" && existing.Endpoint != endpoint:
		// An alias silently re-pointed at another host sends the stored token
		// somewhere it was never minted for.
		if !replacePin {
			return fmt.Errorf("login: alias %q is pinned to %s, not %s; use --name for a new alias "+
				"or --replace-pin to re-point this one", alias, existing.Endpoint, endpoint)
		}
	case !boolFlag(cmd, assumeYesFlag):
		ok, err := confirmPin(cmd)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("login: not confirmed; nothing stored")
		}
	}

	cfg.Hosts[alias] = hostEntry{Endpoint: endpoint, Fingerprint: fp, Token: token}
	if makeDefault && cfg.Default == "" {
		// Only fill an empty default: every command without --remote, the env,
		// or --local then goes to this first-pinned remote, and an operator's
		// existing choice is never silently changed.
		cfg.Default = alias
	}
	if err := saveHostsConfig(cfg); err != nil {
		return err
	}

	verb := "pinned"
	if had {
		verb = "updated"
	}
	if cfg.Default == alias {
		_, _ = fmt.Fprintf(out, "%s remote %q; it is the default remote, so commands go there unless they pass --local\n",
			verb, alias)
		return nil
	}
	_, _ = fmt.Fprintf(out, "%s remote %q; use it with --remote %s\n", verb, alias, alias)
	return nil
}

// confirmPin asks the operator to accept the fingerprint. A non-interactive
// session (no TTY, or --no-input) refuses rather than blocking on stdin, so a
// script must pass --assume-yes to pin — the same discipline as confirmDestructive.
func confirmPin(cmd *cobra.Command) (bool, error) {
	if boolFlag(cmd, noInputFlag) || !stdinIsInteractive(cmd) {
		return false, fmt.Errorf("login: stdin is non-interactive: pass --assume-yes to pin this fingerprint")
	}
	_, _ = fmt.Fprint(cmd.OutOrStdout(), "pin this fingerprint? [y/N] ")
	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && line == "" {
		return false, nil
	}
	ans := strings.ToLower(strings.TrimSpace(line))
	return ans == "y" || ans == "yes", nil
}

// defaultAlias derives a short name from an endpoint: the first label of its
// host (hub.tailnet.ts.net -> hub), or the whole host for an IP/short
// name. It exists so a login without --name still stores under a memorable key.
func defaultAlias(endpoint string) (string, error) {
	raw := strings.TrimSpace(endpoint)
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return "", fmt.Errorf("login: cannot derive a name from %q; pass --name", endpoint)
	}
	host := u.Hostname()
	if label, _, ok := strings.Cut(host, "."); ok && label != "" {
		return label, nil
	}
	return host, nil
}

// resolveToken picks the bearer token: --token flag, then EXOCOMP_TOKEN, then the
// stored host entry's token. login passes a zero entry (nothing stored yet); the
// client-mode path passes the resolved entry so a stored token is the fallback.
func resolveToken(cmd *cobra.Command, entry hostEntry) string {
	if f := cmd.Flags().Lookup(tokenFlag); f != nil && f.Changed {
		return f.Value.String()
	}
	if env := strings.TrimSpace(os.Getenv(tokenEnv)); env != "" {
		return env
	}
	return entry.Token
}

func sameFP(a, b string) bool {
	norm := func(s string) string {
		return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(s), ":", ""))
	}
	return norm(a) == norm(b)
}
