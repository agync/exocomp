package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/deps"
	"exocomp.dev/exocomp/internal/naming"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/source"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

func init() {
	registerCommand(newSourceCmd)
	registerCommand(newRunSecretCmd)
	registerCommand(newInventoryCmd)
}

// deployKeyKind and deployKeyName build the sealed-secret coordinates a git
// source's generated deploy key lives under.
const deployKeyKind = "deploy-key"

func deployKeySecretName(source string) string { return "source/" + source + "/deploy-key" }

func newSourceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "source",
		Short: "Register and sync the git repos and paths runs execute from (Decision 0010)",
	}
	cmd.AddCommand(newSourceAddCmd(), newSourceListCmd(), newSourceSyncCmd(), newSourceRmCmd(), newSourceEgressCmd(), newSourceRunSecretsCmd())
	return cmd
}

func newSourceAddCmd() *cobra.Command {
	var (
		url         string
		ref         string
		path        string
		writable    bool
		signed      bool
		signersFile string
		signers     []string
	)
	add := &cobra.Command{
		Use:   "add <name>",
		Short: "Register a git repo (--url) or a path (--path) as a source",
		Long: "A git source generates an ed25519 deploy key sealed into the envelope and " +
			"prints the public half to add to the forge; `source sync` clones and pins it. " +
			"A path source (--path) points at a tree already on the box: an absolute path, or " +
			"a path relative to the data volume. A path source is read-only unless --writable. " +
			"The credential planes are identical across kinds; only how the tree arrives differs.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if (url == "") == (path == "") {
				return errors.New("source add: give exactly one of --url or --path")
			}
			allowed, err := resolveAllowedSigners(signersFile, signers)
			if err != nil {
				return err
			}
			if allowed != "" && !signed {
				return errors.New("source add: --allowed-signers/--signer require --signed")
			}
			// Fail closed at registration: a --signed source with no exocomp-owned
			// signers would otherwise verify against the ambient host git config at
			// sync. Refuse now rather than record a source that can
			// never sync — the trust must be exocomp's own.
			if signed && allowed == "" {
				return errors.New("source add: --signed requires --allowed-signers or --signer; " +
					"exocomp verifies a signed ref against its own signers, never the host git config")
			}
			return runSourceAdd(cmd, args[0], sourceAddSpec{
				url: url, ref: ref, path: path, writable: writable, signed: signed, allowedSigners: allowed,
			})
		},
	}
	add.Flags().StringVar(&url, "url", "", "git clone URL for a git source")
	add.Flags().StringVar(&ref, "ref", "main", "ref/tag a git source pins to")
	add.Flags().StringVar(&path, "path", "",
		"tree path for a path source: absolute, or relative to the data volume")
	add.Flags().BoolVar(&writable, "writable", false,
		"grant a path source a read+write working directory (default: read-only)")
	add.Flags().BoolVar(&signed, "signed", false, "require a verified signature on the pinned ref before a sync advances")
	add.Flags().StringVar(&signersFile, "allowed-signers", "",
		"SSH allowed-signers file whose keys a --signed ref is verified against (exocomp-owned trust, not the host git config)")
	add.Flags().StringArrayVar(&signers, "signer", nil,
		"an allowed-signers line (`principal ssh-ed25519 AAAA...`, or `*` as principal), repeatable; appended to --allowed-signers")
	return add
}

// resolveAllowedSigners builds the stored allowed-signers content from a file
// and/or inline --signer lines, in git's allowed-signers format. Empty when
// neither is given, which a --signed source refuses at add time.
func resolveAllowedSigners(file string, signers []string) (string, error) {
	var b strings.Builder
	if file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("source add: read allowed-signers: %w", err)
		}
		b.Write(data)
		if len(data) > 0 && data[len(data)-1] != '\n' {
			b.WriteByte('\n')
		}
	}
	for _, line := range signers {
		b.WriteString(strings.TrimRight(line, "\n"))
		b.WriteByte('\n')
	}
	return b.String(), nil
}

type sourceAddSpec struct {
	url, ref, path string
	writable       bool
	signed         bool
	allowedSigners string
}

func runSourceAdd(cmd *cobra.Command, name string, spec sourceAddSpec) error {
	if err := naming.ValidateComponent("source", name); err != nil {
		return fmt.Errorf("source add: %w", err)
	}

	st, err := openStore()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	row := store.Source{Name: name, Ref: spec.ref, Signed: spec.signed, AllowedSigners: spec.allowedSigners, Enabled: true}
	out := cmd.OutOrStdout()

	if spec.path != "" {
		// An absolute path is cleaned as-is; a relative path is kept verbatim
		// (cleaned) so it resolves against the data volume at run time. filepath.Abs
		// would bind a relative path to the CLI's cwd, defeating that.
		uri := filepath.Clean(spec.path)
		row.Kind = store.SourcePath
		row.URI = uri
		row.Writable = spec.writable
		row.Ref = ""
		if _, err := st.CreateSource(cmd.Context(), row); err != nil {
			return fmt.Errorf("source add: %w", err)
		}
		access := "read-only"
		if spec.writable {
			access = "writable"
		}
		_, _ = fmt.Fprintf(out, "registered %s path source %q at %s\n", access, name, uri)
		return nil
	}

	// A git source: generate the deploy key, seal it, then record the source.
	dir, err := dataDir()
	if err != nil {
		return err
	}
	env, err := secrets.Open(dir)
	if err != nil {
		return fmt.Errorf("source add: open envelope (run `exocomp init` first): %w", err)
	}
	defer func() { _ = env.Close() }()

	key, err := source.GenerateDeployKey(name)
	if err != nil {
		return fmt.Errorf("source add: %w", err)
	}
	secretName := deployKeySecretName(name)

	// The source row is created first, so a duplicate name is refused here,
	// before any secret material is ever sealed or stored — a colliding name
	// must never leave a sealed deploy key with nothing referencing it (no
	// recovery UI exists for that). If a later step fails, the row created
	// here is rolled back on a best-effort basis.
	row.Kind = store.SourceGit
	row.URI = spec.url
	row.DeployKeySecret = secretName
	if _, err := st.CreateSource(cmd.Context(), row); err != nil {
		return fmt.Errorf("source add: %w", err)
	}

	sealed, err := env.Seal(secretName, deployKeyKind, key.PrivatePEM)
	if err != nil {
		if derr := st.DeleteSource(cmd.Context(), name); derr != nil {
			return fmt.Errorf("source add: seal deploy key: %w (rollback also failed: %v)", err, derr)
		}
		return fmt.Errorf("source add: seal deploy key: %w", err)
	}
	if _, err := st.CreateSecret(cmd.Context(), store.SecretRow{
		Name:       sealed.Name,
		Kind:       sealed.Kind,
		Nonce:      sealed.Nonce,
		Ciphertext: sealed.Ciphertext,
		AAD:        sealed.Name + "|" + sealed.Kind,
	}); err != nil {
		if derr := st.DeleteSource(cmd.Context(), name); derr != nil {
			return fmt.Errorf("source add: store deploy key: %w (rollback also failed: %v)", err, derr)
		}
		return fmt.Errorf("source add: store deploy key: %w", err)
	}

	_, _ = fmt.Fprintf(out, "registered git source %q (%s, ref %s)\n", name, spec.url, spec.ref)
	_, _ = fmt.Fprintln(out, "add this deploy key to the forge as a read-only key:")
	_, _ = fmt.Fprintln(out, key.PublicAuthorizedKey)
	return nil
}

func newSourceListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Short:   "List registered sources",
		Example: "exocomp source list --output json",
		RunE: func(cmd *cobra.Command, _ []string) error {
			mode, err := outputMode(cmd)
			if err != nil {
				return err
			}
			if rc, ok, err := remoteClient(cmd); err != nil {
				return err
			} else if ok {
				rows, err := rc.ListSources(cmd.Context())
				if err != nil {
					return remoteFail(cmd, mode, err)
				}
				return renderRemoteSources(cmd, mode, rows)
			}
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			rows, err := st.ListSources(cmd.Context())
			if err != nil {
				return err
			}
			if mode == outputJSON {
				return emitJSON(cmd, wire.KindSourceList, wire.NewSourceList(rows))
			}
			if len(rows) == 0 {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "no sources")
				return nil
			}
			for _, s := range rows {
				loc := s.URI
				pin := s.Ref
				if s.LastSyncSHA != "" {
					pin = fmt.Sprintf("%s@%s", s.Ref, short(s.LastSyncSHA))
				}
				flags := ""
				if s.Kind == store.SourcePath {
					if s.Writable {
						flags += " rw"
					} else {
						flags += " ro"
					}
				}
				if s.Signed {
					flags += " signed"
				}
				if !s.Enabled {
					flags += " disabled"
				}
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%-20s %-9s %-8s %s%s\n",
					s.Name, s.Kind, pin, loc, flags)
			}
			return nil
		},
	}
}

func newSourceSyncCmd() *cobra.Command {
	var ref string
	sync := &cobra.Command{
		Use:   "sync <name>",
		Short: "Clone/fetch a git source and pin it to its ref",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSourceSync(cmd, args[0], ref)
		},
	}
	sync.Flags().StringVar(&ref, "ref", "", "override the pinned ref for this sync (advances the pin)")
	return sync
}

func runSourceSync(cmd *cobra.Command, name, refOverride string) error {
	st, err := openStore()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	src, err := st.SourceByName(cmd.Context(), name)
	if err != nil {
		return fmt.Errorf("source sync: %w", err)
	}
	if src.Kind != store.SourceGit {
		return fmt.Errorf("source sync: %q is a %s source; only git sources sync (the tree is already present)",
			name, src.Kind)
	}

	dir, err := dataDir()
	if err != nil {
		return err
	}
	loc, err := source.Resolve(dir, src.Kind, src.Name, src.URI, src.Writable)
	if err != nil {
		return fmt.Errorf("source sync: %w", err)
	}

	ref := src.Ref
	if refOverride != "" {
		ref = refOverride
	}

	opts := source.SyncOptions{Root: loc.Root, URL: src.URI, Ref: ref, Signed: src.Signed}
	if src.DeployKeySecret != "" {
		key, err := openSealed(cmd.Context(), st, dir, src.DeployKeySecret)
		if err != nil {
			return fmt.Errorf("source sync: deploy key: %w", err)
		}
		defer zero(key)
		opts.DeployKeyPEM = key
	}

	// Pin the forge's host key from the recorded known_hosts store (Decision 0010):
	// without it the ssh transport trusts whatever key the forge presents on
	// first contact. When the forge host has a recorded key, verify
	// against it strictly; a mismatch fails the sync rather than trusting a new
	// key. A forge not yet recorded stays trust-on-first-use — record it (host
	// onboard / ssh-keyscan into the store) to get the pin.
	if host := source.ForgeHost(src.URI); host != "" {
		khFile, cleanup, err := forgeKnownHosts(cmd.Context(), st, host)
		if err != nil {
			return fmt.Errorf("source sync: known_hosts: %w", err)
		}
		defer cleanup()
		opts.KnownHostsFile = khFile
	}
	if src.Signed && src.AllowedSigners != "" {
		// Materialize exocomp's own allowed-signers outside the checkout (git
		// clean -fdx wipes the tree) so verify-tag/commit trusts these keys, not
		// whatever the host's git config happens to name.
		f, err := os.CreateTemp("", "exocomp-signers-*")
		if err != nil {
			return fmt.Errorf("source sync: allowed-signers: %w", err)
		}
		defer func() { _ = os.Remove(f.Name()) }()
		if _, err := f.WriteString(src.AllowedSigners); err != nil {
			_ = f.Close()
			return fmt.Errorf("source sync: allowed-signers: %w", err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("source sync: allowed-signers: %w", err)
		}
		opts.AllowedSignersFile = f.Name()
	}

	res, err := source.Sync(cmd.Context(), opts)
	if err != nil {
		return fmt.Errorf("source sync: %w", err)
	}
	if err := st.RecordSourceSync(cmd.Context(), src.ID, res.Ref, res.SHA); err != nil {
		return fmt.Errorf("source sync: record: %w", err)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "synced %q to %s (%s)\n", name, res.Ref, short(res.SHA))
	return nil
}

// forgeKnownHosts materializes the recorded host keys for one forge host into a
// temporary known_hosts file for the sync's ssh transport, returning the path
// and a cleanup. When the host has no recorded key it returns an empty path and
// a no-op cleanup: the sync stays trust-on-first-use rather than failing to
// clone a forge the operator has not yet recorded.
func forgeKnownHosts(ctx context.Context, st *store.Store, host string) (string, func(), error) {
	noop := func() {}
	rows, err := st.ListKnownHosts(ctx)
	if err != nil {
		return "", noop, err
	}
	var forge []store.KnownHost
	for _, kh := range rows {
		if kh.Host == host {
			forge = append(forge, kh)
		}
	}
	if len(forge) == 0 {
		return "", noop, nil
	}
	f, err := os.CreateTemp("", "exocomp-knownhosts-*")
	if err != nil {
		return "", noop, err
	}
	cleanup := func() { _ = os.Remove(f.Name()) }
	if _, err := f.WriteString(store.RenderKnownHosts(forge)); err != nil {
		_ = f.Close()
		cleanup()
		return "", noop, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", noop, err
	}
	return f.Name(), cleanup, nil
}

func newSourceRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <name>",
		Short: "Remove a source (its inventories cascade; a git deploy key is left sealed)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSourceRm(cmd, args[0])
		},
	}
}

func runSourceRm(cmd *cobra.Command, name string) error {
	st, err := openStore()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	dir, err := dataDir()
	if err != nil {
		return err
	}

	// Fetch the row before delete: the rm needs it to resolve the caches the
	// source may own, and it is gone after.
	src, err := st.SourceByName(cmd.Context(), name)
	if err != nil {
		return fmt.Errorf("source rm: %w", err)
	}
	if err := st.DeleteSource(cmd.Context(), name); err != nil {
		return fmt.Errorf("source rm: %w", err)
	}
	purged := purgeSourceCachesIfUnreferenced(cmd.Context(), st, dir, src)
	if purged {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "removed source %q (cached project deps removed)\n", name)
		return nil
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "removed source %q\n", name)
	return nil
}

func newSourceEgressCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "egress <name> [port...]",
		Short: "Declare the default egress ports a source's ansible runs inherit",
		Long: "Records a default egress set that every ansible run naming this source " +
			"inherits, unioned with whatever that run declares, replacing any previous " +
			"declaration. With no ports it clears the declaration. This is a deliberate " +
			"widening: because the sandbox filters egress by port and not by destination, " +
			"a granted port reaches any host on that port, not only the intended one. It " +
			"is the ansible counterpart of `workspace egress` " +
			"(docs/operations/serve.md).",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSourceEgress(cmd, args[0], args[1:])
		},
	}
}

func runSourceEgress(cmd *cobra.Command, name string, portArgs []string) (err error) {
	ports := make([]int, 0, len(portArgs))
	for _, a := range portArgs {
		p, perr := strconv.Atoi(a)
		if perr != nil {
			return fmt.Errorf("source egress: %q is not a port number", a)
		}
		ports = append(ports, p)
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

	ctx := cmd.Context()
	src, err := st.SourceByName(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("source egress: no source %q", name)
	}
	if err != nil {
		return err
	}

	updated, err := st.SetSourceEgressPorts(ctx, src.ID, ports)
	if errors.Is(err, store.ErrInvalidEgressPort) {
		return fmt.Errorf("source egress: %w", err)
	}
	if err != nil {
		return err
	}

	if len(updated.ExtraEgressPorts) == 0 {
		_, err = fmt.Fprintf(cmd.OutOrStdout(),
			"source %s default egress ports: none (ansible kind default only)\n", updated.Name)
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(),
		"source %s default egress ports: %v (any host on these ports)\n",
		updated.Name, updated.ExtraEgressPorts)
	return err
}

func newSourceRunSecretsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "run-secrets <name> [binding]",
		Short: "Set the default run-secret binding a source's ansible runs inherit",
		Long: "Records a run-secret binding that every ansible run naming this source " +
			"(a manual playbook run, an ad-hoc command, a rule-fired playbook) injects as " +
			"env unless the run passes its own --run-secrets, which wins. With no binding " +
			"it clears the default. The binding must already hold at least one pair " +
			"(`exocomp run-secret add`). Only the binding name is stored; the sealed values " +
			"are resolved from the envelope when the run starts. Use it when the source's " +
			"tree needs credentials on every run, such as the env var an encrypted " +
			"group_vars tree reads its vault password from. A scheduled play inherits it when " +
			"the schedule names the source (`schedule add --source`); a handoff or " +
			"bootstrap play names no source on its play step and does not. --vault-project stays per run and is not " +
			"defaulted (docs/operations/sources.md).",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			binding := ""
			if len(args) == 2 {
				binding = args[1]
			}
			return runSourceRunSecrets(cmd, args[0], binding)
		},
	}
}

func runSourceRunSecrets(cmd *cobra.Command, name, binding string) (err error) {
	st, err := openStore()
	if err != nil {
		return err
	}
	defer func() {
		if cerr := st.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()

	ctx := cmd.Context()
	src, err := st.SourceByName(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("source run-secrets: no source %q", name)
	}
	if err != nil {
		return err
	}

	updated, err := st.SetSourceRunSecrets(ctx, src.ID, binding)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("source run-secrets: binding %q has no pairs (add one with `exocomp run-secret add`)", binding)
	}
	if err != nil {
		return err
	}

	if updated.DefaultRunSecrets == "" {
		_, err = fmt.Fprintf(cmd.OutOrStdout(),
			"source %s default run-secrets: none (a run injects only the binding it names)\n", updated.Name)
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(),
		"source %s default run-secrets: %s (every run of this source, unless it names its own)\n",
		updated.Name, updated.DefaultRunSecrets)
	return err
}

// purgeSourceCachesIfUnreferenced removes a deleted source's dependency caches
// (cache/python/<SourceKey> and cache/collections/<SourceKey> under dataDir)
// unless another registered source resolves to the same root and therefore
// shares the key (Decision 0015). A disabled source still owns its cache when
// re-enabled, so it counts as a reference. The source row is already deleted by
// the caller. Any failure leaves the caches in place: the failure mode is a
// stale cache, never a wrong purge. It reports whether it actually removed
// anything so the caller can say so.
func purgeSourceCachesIfUnreferenced(ctx context.Context, st *store.Store, dataDir string, src store.Source) bool {
	root, ok := sourceRoot(dataDir, src)
	if !ok {
		return false
	}
	key := deps.SourceKey(root)
	rows, err := st.ListSources(ctx)
	if err != nil {
		return false
	}
	for _, other := range rows {
		otherRoot, ok := sourceRoot(dataDir, other)
		if !ok {
			continue
		}
		if deps.SourceKey(otherRoot) == key {
			return false
		}
	}
	_ = deps.RemoveProjectCaches(dataDir, root)
	return true
}

// sourceRoot resolves a source row to its locality root; ok is false when the
// row cannot be resolved (an empty or malformed path), in which case the caller
// must not purge anything it cannot key.
func sourceRoot(dataDir string, src store.Source) (root string, ok bool) {
	loc, err := source.Resolve(dataDir, src.Kind, src.Name, src.URI, src.Writable)
	if err != nil {
		return "", false
	}
	return loc.Root, true
}

// --- run-secret bindings: the credential-plane mapping, no secret material ---

func newRunSecretCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run-secret",
		Short: "Bind sealed secrets to env names for a run (the credential plane)",
		Long: "A binding names a set of (ENV_NAME -> sealed-secret) pairs a run exports as " +
			"named env, so a dynamic-inventory plugin or a provider authenticates under the " +
			"sandbox. The rows hold the mapping only; the values are resolved from the " +
			"envelope when the job runs.",
	}
	cmd.AddCommand(newRunSecretAddCmd(), newRunSecretListCmd(), newRunSecretRmCmd())
	return cmd
}

func newRunSecretAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "add <binding> <ENV_NAME> <secret-name>",
		Short: "Map an env name to a sealed secret under a binding",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			binding, envName, secretName := args[0], args[1], args[2]
			if !isEnvName(envName) {
				return fmt.Errorf("run-secret add: %q is not a valid environment variable name", envName)
			}
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			// Refuse to bind a secret that does not exist: the failure belongs
			// here, not at run time when a plugin silently gets an empty token.
			if _, err := st.GetSecret(cmd.Context(), secretName); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("run-secret add: no sealed secret %q (add it with `exocomp secrets add`)", secretName)
				}
				return err
			}
			if _, err := st.AddRunSecret(cmd.Context(), store.RunSecret{
				Binding: binding, EnvName: envName, SecretName: secretName,
			}); err != nil {
				return fmt.Errorf("run-secret add: %w", err)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "bound %s -> %s under %q\n", envName, secretName, binding)
			return nil
		},
	}
}

func newRunSecretListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list [binding]",
		Short: "List bindings, or the pairs under one binding",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			out := cmd.OutOrStdout()

			if len(args) == 0 {
				names, err := st.ListRunSecretBindings(cmd.Context())
				if err != nil {
					return err
				}
				if len(names) == 0 {
					_, _ = fmt.Fprintln(out, "no run-secret bindings")
					return nil
				}
				for _, n := range names {
					_, _ = fmt.Fprintln(out, n)
				}
				return nil
			}

			pairs, err := st.RunSecretsByBinding(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if len(pairs) == 0 {
				_, _ = fmt.Fprintf(out, "no pairs under %q\n", args[0])
				return nil
			}
			for _, p := range pairs {
				_, _ = fmt.Fprintf(out, "%-24s -> %s\n", p.EnvName, p.SecretName)
			}
			return nil
		},
	}
}

func newRunSecretRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <binding>",
		Short: "Remove every pair under a binding",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			if err := st.DeleteRunSecretBinding(cmd.Context(), args[0]); err != nil {
				return fmt.Errorf("run-secret rm: %w", err)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "removed binding %q\n", args[0])
			return nil
		},
	}
}

// --- inventory registration ---

func newInventoryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "inventory",
		Short: "Register static and dynamic inventories runs target",
	}
	cmd.AddCommand(newInventoryAddCmd(), newInventoryListCmd(), newInventoryRmCmd())
	return cmd
}

func newInventoryAddCmd() *cobra.Command {
	var (
		sourceName string
		path       string
		kind       string
		binds      []string
	)
	add := &cobra.Command{
		Use:   "add <name>",
		Short: "Register an inventory, binding a dynamic plugin's auth env with --bind ENV=secret",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if kind != store.InventoryStatic && kind != store.InventoryDynamic {
				return fmt.Errorf("inventory add: --kind must be %s or %s", store.InventoryStatic, store.InventoryDynamic)
			}
			bindings, err := parseBinds(binds)
			if err != nil {
				return fmt.Errorf("inventory add: %w", err)
			}
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()

			inv := store.Inventory{Name: args[0], Path: path, Kind: kind, EnvBinding: bindings}
			if sourceName != "" {
				src, err := st.SourceByName(cmd.Context(), sourceName)
				if err != nil {
					return fmt.Errorf("inventory add: source %q: %w", sourceName, err)
				}
				inv.SourceID = &src.ID
			}
			if _, err := st.CreateInventory(cmd.Context(), inv); err != nil {
				return fmt.Errorf("inventory add: %w", err)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "registered %s inventory %q\n", kind, args[0])
			return nil
		},
	}
	add.Flags().StringVar(&sourceName, "source", "", "source the inventory lives in")
	add.Flags().StringVar(&path, "path", "", "inventory path relative to the source checkout")
	add.Flags().StringVar(&kind, "kind", store.InventoryStatic, "static or dynamic")
	add.Flags().StringArrayVar(&binds, "bind", nil, "dynamic-inventory plugin env binding, ENV_NAME=secret-name (repeatable)")
	return add
}

func newInventoryListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List registered inventories",
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			rows, err := st.ListInventories(cmd.Context())
			if err != nil {
				return err
			}
			if len(rows) == 0 {
				_, _ = fmt.Fprintln(cmd.OutOrStdout(), "no inventories")
				return nil
			}
			for _, inv := range rows {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%-20s %-8s %-24s binds:%d\n",
					inv.Name, inv.Kind, inv.Path, len(inv.EnvBinding))
			}
			return nil
		},
	}
}

func newInventoryRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rm <name>",
		Short: "Remove an inventory",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = st.Close() }()
			if err := st.DeleteInventory(cmd.Context(), args[0]); err != nil {
				return fmt.Errorf("inventory rm: %w", err)
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "removed inventory %q\n", args[0])
			return nil
		},
	}
}

// parseBinds turns --bind ENV=secret flags into an env->secret map, refusing a
// malformed env name so a bad binding fails at registration, not at run time.
func parseBinds(binds []string) (map[string]string, error) {
	if len(binds) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(binds))
	for _, b := range binds {
		name, secret, ok := strings.Cut(b, "=")
		if !ok || secret == "" {
			return nil, fmt.Errorf("bind %q is not ENV_NAME=secret-name", b)
		}
		if !isEnvName(name) {
			return nil, fmt.Errorf("bind env name %q is not a valid identifier", name)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("bind env name %q given twice", name)
		}
		out[name] = secret
	}
	return out, nil
}

// isEnvName reports whether s is a POSIX-shaped environment variable name.
func isEnvName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_':
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// openSealed opens a sealed secret's plaintext from the envelope.
func openSealed(ctx context.Context, st *store.Store, dataDir, name string) ([]byte, error) {
	env, err := secrets.Open(dataDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = env.Close() }()
	row, err := st.GetSecret(ctx, name)
	if err != nil {
		return nil, err
	}
	return env.OpenSecret(secrets.SealedSecret{
		Name:       row.Name,
		Kind:       row.Kind,
		Nonce:      row.Nonce,
		Ciphertext: row.Ciphertext,
	})
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
