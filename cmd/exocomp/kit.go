package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"exocomp.dev/exocomp/internal/backup"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

func init() {
	registerCommand(newKitCmd)
	registerCommand(newIdentityCmd)
}

func newKitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "kit",
		Short: "Derive from the recovery kit and check a printed page",
	}
	cmd.AddCommand(newKitDeriveCmd(), newKitVerifyCmd())
	return cmd
}

// derivation is one purpose:id in use, with the purpose it belongs to.
type derivation struct {
	purpose secrets.Purpose
	id      string
}

func newKitDeriveCmd() *cobra.Command {
	var (
		list     bool
		identity string
		keyring  string
		salt     string
	)

	cmd := &cobra.Command{
		Use:   "derive [<purpose>/<id>]",
		Short: "Derive one secret the way the recovery card describes",
		Long: "Runs the card's one rule — HKDF-SHA256 over the DEK with the printed salt " +
			"and an info string of \"<purpose>:<id>\" — and prints the result in the form " +
			"its consumer takes. `kit derive restic/nas` is the restic repository " +
			"password; `kit derive ansible-vault/homelab` is the vault password.\n\n" +
			"With --identity it works offline, against a keyring.age from any backup " +
			"target root, with no envelope and no daemon: the recovery key and the salt " +
			"are the only inputs.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if list {
				if len(args) > 0 {
					return errors.New("kit derive: --list takes no purpose")
				}
				return listDerivations(cmd.Context(), cmd.OutOrStdout())
			}
			if len(args) != 1 {
				return errors.New("kit derive: name one <purpose>/<id>, or pass --list")
			}
			purpose, id, err := parseDerivation(args[0])
			if err != nil {
				return err
			}

			key, err := deriveKey(purpose, id, salt, identity, keyring)
			if err != nil {
				return err
			}
			defer clear(key)
			_, err = fmt.Fprintln(cmd.OutOrStdout(), purpose.Encode(key))
			return err
		},
	}

	cmd.Flags().BoolVar(&list, "list", false, "list every purpose:id in use, from the store")
	cmd.Flags().StringVar(&identity, "identity", "",
		"age identity file holding the recovery key; derives offline, without the envelope")
	cmd.Flags().StringVar(&keyring, "keyring", "",
		"keyring.age to unwrap with --identity (default: the one in the data dir)")
	cmd.Flags().StringVar(&salt, "salt", secrets.Salt,
		"HKDF salt, as printed in section 3 of the card")
	return cmd
}

// parseDerivation reads the card's "<purpose>/<id>" grammar.
func parseDerivation(arg string) (secrets.Purpose, string, error) {
	name, id, ok := strings.Cut(strings.TrimSpace(arg), "/")
	if !ok || id == "" {
		return secrets.Purpose{}, "", fmt.Errorf("kit derive: %q is not <purpose>/<id>", arg)
	}
	purpose, ok := secrets.LookupPurpose(name)
	if !ok {
		names := make([]string, 0, len(secrets.Purposes()))
		for _, p := range secrets.Purposes() {
			names = append(names, p.Name)
		}
		return secrets.Purpose{}, "", fmt.Errorf("kit derive: unknown purpose %q; the card lists %s",
			name, strings.Join(names, ", "))
	}
	return purpose, id, nil
}

// deriveKey runs the card's rule, through the envelope when there is one and
// through a keyring.age unwrapped with the recovery key when there is not.
func deriveKey(purpose secrets.Purpose, id, salt, identityFile, keyringFile string) ([]byte, error) {
	dir, err := dataDir()
	if err != nil {
		return nil, fmt.Errorf("kit derive: resolve data dir: %w", err)
	}
	if identityFile == "" {
		env, err := secrets.Open(dir)
		if err != nil {
			return nil, fmt.Errorf("kit derive: open envelope (or pass --identity to derive offline): %w", err)
		}
		defer func() { _ = env.Close() }()
		key, err := env.DeriveWithSalt(salt, purpose.Name, id)
		if err != nil {
			return nil, fmt.Errorf("kit derive: %w", err)
		}
		return key, nil
	}

	if keyringFile == "" {
		keyringFile = filepath.Join(dir, secrets.KeyringName)
	}
	body, err := os.ReadFile(keyringFile)
	if err != nil {
		return nil, fmt.Errorf("kit derive: read keyring (pass --keyring): %w", err)
	}
	ids, err := backup.ReadIdentities(identityFile)
	if err != nil {
		return nil, fmt.Errorf("kit derive: %w", err)
	}
	dek, err := backup.UnwrapDEK(body, ids)
	if err != nil {
		return nil, fmt.Errorf("kit derive: %w", err)
	}
	defer clear(dek)

	key, err := secrets.DeriveWithSalt(dek, salt, purpose.Name, id)
	if err != nil {
		return nil, fmt.Errorf("kit derive: %w", err)
	}
	return key, nil
}

// listDerivations enumerates what the store says is in use. It derives
// nothing: this answers "which ids exist", and `kit derive` answers "what is
// the key".
func listDerivations(ctx context.Context, out io.Writer) error {
	st, err := openStore()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	found, err := derivationsInUse(ctx, st)
	if err != nil {
		return err
	}
	if len(found) == 0 {
		_, err := fmt.Fprintln(out, "no derivations in use: no backup target, workspace, or vaulted play yet")
		return err
	}
	for _, d := range found {
		if _, err := fmt.Fprintf(out, "%-34s %s, %s\n",
			d.purpose.Name+":"+d.id, d.purpose.Consumer, d.purpose.Encoding); err != nil {
			return err
		}
	}
	return nil
}

func derivationsInUse(ctx context.Context, st *store.Store) ([]derivation, error) {
	var out []derivation
	add := func(name, id string) {
		purpose, ok := secrets.LookupPurpose(name)
		if !ok || id == "" {
			return
		}
		if slices.ContainsFunc(out, func(d derivation) bool {
			return d.purpose.Name == purpose.Name && d.id == id
		}) {
			return
		}
		out = append(out, derivation{purpose: purpose, id: id})
	}

	targets, err := st.ListBackupTargets(ctx, true)
	if err != nil {
		return nil, err
	}
	for _, t := range targets {
		if t.Kind != backup.KindGit {
			add("restic-repo", t.Name)
		}
		add(backup.ManifestMACPurpose, t.Name)
	}

	workspaces, err := st.ListWorkspaces(ctx)
	if err != nil {
		return nil, err
	}
	for _, w := range workspaces {
		add("tofu-state", w.Name)
		add("tf-backend", w.Name)
	}

	// Vault projects have no table of their own: a play names its project in
	// the job row that ran it, which is the only record that it exists.
	jobs, err := st.ListJobs(ctx, store.JobFilter{})
	if err != nil {
		return nil, err
	}
	for _, j := range jobs {
		add(vaultPurpose, j.Args.VaultProject)
	}

	order := secrets.Purposes()
	slices.SortStableFunc(out, func(a, b derivation) int {
		ai := slices.IndexFunc(order, func(p secrets.Purpose) bool { return p.Name == a.purpose.Name })
		bi := slices.IndexFunc(order, func(p secrets.Purpose) bool { return p.Name == b.purpose.Name })
		if ai != bi {
			return ai - bi
		}
		return strings.Compare(a.id, b.id)
	})
	return out, nil
}

func newKitVerifyCmd() *cobra.Command {
	var page string

	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Check a printed recovery page against the current kit",
		Long: "Renders the kit as it stands now and prints its sha256. With --page it " +
			"reads a printed page back in, recomputes the digest the page carries, and " +
			"compares both: a digest that does not match the page means the page was " +
			"edited, and a page that does not match the current kit means the kit moved " +
			"on and the page needs reprinting.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := dataDir()
			if err != nil {
				return fmt.Errorf("kit verify: resolve data dir: %w", err)
			}
			env, err := secrets.Open(dir)
			if err != nil {
				return fmt.Errorf("kit verify: %w", err)
			}
			defer func() { _ = env.Close() }()

			kit := env.KitStructure()
			decorateKit(cmd.Context(), kit, cmd.ErrOrStderr())
			current := secrets.PageSHA256(kit.Render())

			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "current kit  %s  sha256 %s\n", kit.Version, current)
			if page == "" {
				return nil
			}

			body, err := os.ReadFile(page)
			if err != nil {
				return fmt.Errorf("kit verify: read page: %w", err)
			}
			return verifyPage(out, string(body), current)
		},
	}

	cmd.Flags().StringVar(&page, "page", "", "a printed page, as text, to check against the current kit")
	return cmd
}

// verifyPage reports both halves of the check and fails on either.
func verifyPage(out io.Writer, page, current string) error {
	computed := secrets.PageSHA256(page)
	_, _ = fmt.Fprintf(out, "page         sha256 %s\n", computed)

	var faults []string
	printed, ok := secrets.ParsePageSHA256(page)
	switch {
	case !ok:
		faults = append(faults, "the page carries no digest line")
	case !strings.HasPrefix(computed, strings.ReplaceAll(printed, " ", "")):
		faults = append(faults, fmt.Sprintf("the page prints digest %q, its text hashes to %s: "+
			"the page was edited after it was printed", printed, computed))
	}
	if computed != current {
		faults = append(faults, "the page does not match the current kit: "+
			"reprint it and destroy the old page")
	}
	if len(faults) > 0 {
		return errors.New("kit verify: " + strings.Join(faults, "; "))
	}
	_, err := fmt.Fprintln(out, "page matches the current kit")
	return err
}

// decorateKit fills in what the card carries but the envelope does not know:
// which host printed it, and where its data went.
func decorateKit(ctx context.Context, kit *secrets.RecoveryKit, warn io.Writer) {
	if host, err := os.Hostname(); err == nil {
		kit.Host = host
	}
	targets, err := kitTargets(ctx)
	if err != nil {
		_, _ = fmt.Fprintf(warn, "WARNING: kit: backup targets omitted: %v\n", err)
		return
	}
	kit.Targets = targets
}

// kitTargets reads the targets section out of the store, and reports none when
// there is no database yet — which is the state `exocomp init` prints in.
func kitTargets(ctx context.Context) ([]secrets.KitTarget, error) {
	dir, err := dataDir()
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(dir, "exocomp.db")); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	st, err := openStore()
	if err != nil {
		return nil, err
	}
	defer func() { _ = st.Close() }()

	rows, err := st.ListBackupTargets(ctx, true)
	if err != nil {
		return nil, err
	}
	out := make([]secrets.KitTarget, 0, len(rows))
	for _, r := range rows {
		out = append(out, secrets.KitTarget{Name: r.Name, Kind: r.Kind, URI: r.URI})
	}
	return out, nil
}

func newIdentityCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "identity",
		Short: "Manage the host identity and the keyring recipients",
	}
	cmd.AddCommand(newIdentityMigrateCmd())
	return cmd
}

func newIdentityMigrateCmd() *cobra.Command {
	var (
		add        []string
		drop       []string
		rotateHost bool
	)

	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Re-wrap the keyring for new recipients or a new host key",
		Long: "Wraps the same data encryption key to a changed recipient set, or to a " +
			"freshly minted host identity, and bumps the keyring generation. Sealed " +
			"secrets and every derived subkey are untouched — only who can unwrap the " +
			"DEK changes. The printed card is stale afterwards, and says so.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := dataDir()
			if err != nil {
				return fmt.Errorf("identity migrate: resolve data dir: %w", err)
			}
			res, err := secrets.Migrate(dir, secrets.MigrateOptions{
				Add: add, Drop: drop, RotateHost: rotateHost,
			})
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if res.HostRotated {
				_, _ = fmt.Fprintln(out, "host identity rotated; the previous host key no longer unwraps the keyring")
			}
			for _, r := range res.Added {
				_, _ = fmt.Fprintf(out, "added    %-10s %s\n", r.Label, r.Fingerprint)
			}
			for _, r := range res.Dropped {
				_, _ = fmt.Fprintf(out, "dropped  %-10s %s\n", r.Label, r.Fingerprint)
			}
			_, err = fmt.Fprintln(out, res.Nag())
			return err
		},
	}

	cmd.Flags().StringArrayVar(&add, "add-recipient", nil,
		"age recipient to wrap the DEK to, as age1… or label=age1…; repeatable")
	cmd.Flags().StringArrayVar(&drop, "drop-recipient", nil,
		"recipient to stop wrapping to, by label or public key; repeatable")
	cmd.Flags().BoolVar(&rotateHost, "rotate-host", false,
		"mint a new host identity and replace host.key")
	return cmd
}
