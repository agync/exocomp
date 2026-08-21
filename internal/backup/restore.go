package backup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// ErrDestInitialized reports a destination that already holds key material.
// Restoring over a live data dir would replace the keyring that unwraps its
// secrets, so the restore stops instead.
var ErrDestInitialized = errors.New("backup: destination already holds a keyring")

// Source is where a restore reads from. Name is the target name the repository
// password was derived under; empty means read it back from the RECOVERY.md at
// the target root.
type Source struct {
	Kind         string
	URI          string
	Name         string
	IdentityFile string
	// Password restores a restic repository whose password is already known,
	// for the case where no age identity is at hand.
	Password  string
	Env       map[string]string
	Driver    executor.Driver
	StageRoot string
}

// ParseSource reads the "--from" form: either "<kind>:<uri>" with an explicit
// kind, or a bare URI whose kind is inferred.
func ParseSource(spec string) (Source, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return Source{}, errors.New("backup: empty restore source")
	}
	for _, kind := range []string{KindResticLocal, KindResticS3, KindGit} {
		if uri, ok := strings.CutPrefix(spec, kind+":"); ok && uri != "" {
			return Source{Kind: kind, URI: uri}, nil
		}
	}
	return Source{Kind: inferKind(spec), URI: spec}, nil
}

func inferKind(uri string) string {
	switch {
	case strings.HasPrefix(uri, "s3:"):
		return KindResticS3
	case strings.HasSuffix(uri, ".git"),
		strings.HasPrefix(uri, "git@"),
		strings.HasPrefix(uri, "git://"),
		strings.HasPrefix(uri, "ssh://"),
		strings.HasPrefix(uri, "https://"):
		return KindGit
	}
	// A bare path is a restic repository unless it looks like a git one.
	if _, err := os.Stat(filepath.Join(localRepoPath(uri), "objects")); err == nil {
		return KindGit
	}
	return KindResticLocal
}

// RestoreLatest rebuilds a data dir at dest from the newest snapshot on one
// target. It verifies every sha256 in the manifest before writing anything
// into dest, and refuses a dest that already holds a keyring.
func RestoreLatest(ctx context.Context, src Source, dest string) (Manifest, error) {
	if src.Driver == nil {
		src.Driver = executor.NewShell()
	}
	if err := checkDest(dest); err != nil {
		return Manifest{}, err
	}

	work, err := os.MkdirTemp(src.StageRoot, "exocomp-restore-")
	if err != nil {
		return Manifest{}, fmt.Errorf("backup: create restore work dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(work) }()

	extracted := filepath.Join(work, "snapshot")
	if err := os.Mkdir(extracted, 0o700); err != nil {
		return Manifest{}, fmt.Errorf("backup: create restore work dir: %w", err)
	}

	switch src.Kind {
	case KindResticLocal, KindResticS3:
		err = restoreRestic(ctx, src, work, extracted)
	case KindGit:
		err = restoreGit(ctx, src, work, extracted)
	default:
		err = fmt.Errorf("backup: unknown target kind %q", src.Kind)
	}
	if err != nil {
		return Manifest{}, err
	}

	manifest, err := ReadManifest(extracted)
	if err != nil {
		return Manifest{}, err
	}
	if err := manifest.Verify(extracted); err != nil {
		return Manifest{}, err
	}
	if err := install(extracted, dest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func checkDest(dest string) error {
	for _, name := range []string{KeyringName, "host.key", RestoredDBName} {
		if _, err := os.Stat(filepath.Join(dest, name)); err == nil {
			return fmt.Errorf("%w: %s", ErrDestInitialized, filepath.Join(dest, name))
		}
	}
	return nil
}

// install moves the verified snapshot into the destination data dir. The
// snapshot keeps its backup name until here so the manifest verifies against
// exactly the bytes that were written.
func install(from, dest string) error {
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return fmt.Errorf("backup: create destination: %w", err)
	}
	files := map[string]string{
		SnapshotName: RestoredDBName,
		KeyringName:  KeyringName,
		ManifestName: ManifestName,
	}
	for src, dst := range files {
		body, err := os.ReadFile(filepath.Join(from, src))
		if err != nil {
			return fmt.Errorf("backup: restore %s: %w", src, err)
		}
		if err := os.WriteFile(filepath.Join(dest, dst), body, 0o600); err != nil {
			return fmt.Errorf("backup: restore %s: %w", dst, err)
		}
	}
	return nil
}

// restoreRestic derives the repository password from the keyring at the target
// root — the chain the recovery card prints — and restores the newest snapshot.
func restoreRestic(ctx context.Context, src Source, work, extracted string) error {
	password := src.Password
	if password == "" {
		keyring, recovery, err := fetchRoot(ctx, src)
		if err != nil {
			return err
		}
		name := src.Name
		if name == "" {
			name = targetNameFromRecovery(recovery)
		}
		if name == "" {
			return errors.New("backup: target name unknown: pass it explicitly, " +
				"or restore from a target whose root holds " + RecoveryName)
		}
		if src.IdentityFile == "" {
			return errors.New("backup: an age identity is required to unwrap " + KeyringName)
		}
		ids, err := ReadIdentities(src.IdentityFile)
		if err != nil {
			return err
		}
		dek, err := UnwrapDEK(keyring, ids)
		if err != nil {
			return err
		}
		defer clear(dek)

		password, err = ResticPassword(dek, name)
		if err != nil {
			return err
		}
	}

	repo, err := resticRepo(Target{BackupTarget: store.BackupTarget{Kind: src.Kind, URI: src.URI}})
	if err != nil {
		return err
	}
	passFile, cleanup, err := writePasswordFile(src.StageRoot, password)
	if err != nil {
		return err
	}
	defer cleanup()

	restored := filepath.Join(work, "restic")
	res, err := runTool(ctx, src.Driver, executor.Spec{
		Kind: "shell",
		Argv: []string{"restic", "-r", repo, "restore", "latest", "--target", restored},
		Env:  resticEnv(src.Env, passFile),
	}, secrets.NewRedactor([][]byte{[]byte(password)}))
	if err != nil {
		return err
	}
	if res.exitCode != 0 {
		return fmt.Errorf("backup: restic restore exited %d: %s", res.exitCode, res.tail(5))
	}

	// restic restores absolute paths under --target, so the staging tree lands
	// somewhere below it; the manifest marks the spot.
	dir, err := findManifestDir(restored)
	if err != nil {
		return err
	}
	return copyDirFiles(dir, extracted)
}

// restoreGit clones the rolling backup ref, decrypts the newest snapshot, and
// unpacks it.
func restoreGit(ctx context.Context, src Source, work, extracted string) error {
	if src.IdentityFile == "" {
		return errors.New("backup: an age identity is required to decrypt a git snapshot")
	}
	ids, err := ReadIdentities(src.IdentityFile)
	if err != nil {
		return err
	}

	clone := filepath.Join(work, "clone")
	res, err := runTool(ctx, src.Driver, executor.Spec{
		Kind: "shell",
		Argv: []string{"git", "clone", "--quiet", "--branch", "backup", "--depth", "1", src.URI, clone},
		Env:  gitEnv(src.Env),
	}, nil)
	if err != nil {
		return err
	}
	if res.exitCode != 0 {
		return fmt.Errorf("backup: git clone exited %d: %s", res.exitCode, res.tail(5))
	}

	snapshot, err := newestSnapshot(clone)
	if err != nil {
		return err
	}
	sealed, err := os.ReadFile(snapshot)
	if err != nil {
		return fmt.Errorf("backup: read %s: %w", filepath.Base(snapshot), err)
	}
	archive, err := ageDecrypt(sealed, ids)
	if err != nil {
		return fmt.Errorf("backup: decrypt %s: %w", filepath.Base(snapshot), err)
	}
	raw, err := decompress(ctx, archive)
	if err != nil {
		return err
	}
	return untar(raw, extracted)
}

func newestSnapshot(dir string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "snapshot-*.age"))
	if err != nil {
		return "", fmt.Errorf("backup: scan snapshots: %w", err)
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("backup: no snapshot-*.age in %s", dir)
	}
	// Snapshot names carry a sortable UTC stamp.
	sort.Strings(matches)
	return matches[len(matches)-1], nil
}

// fetchRoot reads keyring.age and RECOVERY.md from the target root, which sits
// outside the repository.
func fetchRoot(ctx context.Context, src Source) (keyring, recovery []byte, err error) {
	switch src.Kind {
	case KindResticLocal:
		root := localRepoPath(src.URI)
		keyring, err = os.ReadFile(filepath.Join(root, KeyringName))
		if err != nil {
			return nil, nil, fmt.Errorf("backup: read %s at target root: %w", KeyringName, err)
		}
		recovery, _ = os.ReadFile(filepath.Join(root, RecoveryName))
		return keyring, recovery, nil
	case KindResticS3:
		client, err := newS3Client(src.URI, src.Env)
		if err != nil {
			return nil, nil, err
		}
		keyring, err = client.get(ctx, KeyringName)
		if err != nil {
			return nil, nil, err
		}
		recovery, _ = client.get(ctx, RecoveryName)
		return keyring, recovery, nil
	default:
		return nil, nil, fmt.Errorf("backup: kind %q has no target root", src.Kind)
	}
}

func findManifestDir(root string) (string, error) {
	var found string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() == ManifestName {
			found = filepath.Dir(path)
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("backup: scan restored tree: %w", err)
	}
	if found == "" {
		return "", fmt.Errorf("backup: restored tree holds no %s", ManifestName)
	}
	return found, nil
}

func copyDirFiles(from, to string) error {
	entries, err := os.ReadDir(from)
	if err != nil {
		return fmt.Errorf("backup: read restored tree: %w", err)
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		body, err := os.ReadFile(filepath.Join(from, e.Name()))
		if err != nil {
			return fmt.Errorf("backup: read restored %s: %w", e.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(to, e.Name()), body, 0o600); err != nil {
			return fmt.Errorf("backup: stage restored %s: %w", e.Name(), err)
		}
	}
	return nil
}

// VerifyCanary proves a restored data dir end to end: unwrap the DEK from the
// restored keyring with the recovery identity, open the named sealed secret
// with it, and report its size. The plaintext is never returned or printed.
func VerifyCanary(ctx context.Context, dataDir, identityFile, name string) (int, error) {
	ids, err := ReadIdentities(identityFile)
	if err != nil {
		return 0, err
	}
	keyring, err := os.ReadFile(filepath.Join(dataDir, KeyringName))
	if err != nil {
		return 0, fmt.Errorf("backup: read restored %s: %w", KeyringName, err)
	}
	dek, err := UnwrapDEK(keyring, ids)
	if err != nil {
		return 0, err
	}
	defer clear(dek)

	st, err := store.Open(filepath.Join(dataDir, RestoredDBName))
	if err != nil {
		return 0, err
	}
	defer func() { _ = st.Close() }()

	row, err := st.GetSecret(ctx, name)
	if err != nil {
		return 0, err
	}
	pt, err := OpenSealed(dek, row.Name, row.Kind, row.Nonce, row.Ciphertext)
	if err != nil {
		return 0, err
	}
	n := len(pt)
	clear(pt)
	return n, nil
}
