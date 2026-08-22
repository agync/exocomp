package backup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// ErrKeyringDowngrade reports a keyring older than the snapshot being
// restored. An old keyring wraps the DEK to recipients the operator has since
// revoked, so accepting one hands a revoked key its access back — the rollback
// attack the keyring generation exists to catch.
var ErrKeyringDowngrade = errors.New("backup: keyring rollback refused")

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
	Password string
	Env      map[string]string
	// AllowUnauthenticated downgrades a missing manifest tag from a refusal to
	// a loud warning. It exists for the genuinely legacy backup an operator
	// knowingly accepts; the default refuses an untagged snapshot from a target
	// that has authenticated before.
	AllowUnauthenticated bool
	Driver               executor.Driver
	StageRoot            string
	// Warn receives integrity findings that do not stop the restore — a
	// snapshot with no manifest tag above all. Empty logs them.
	Warn func(string)
}

func (s Source) warn(msg string) {
	if s.Warn != nil {
		s.Warn(msg)
		return
	}
	slog.Default().Warn(msg)
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

	var leg legSource
	switch src.Kind {
	case KindResticLocal, KindResticS3:
		leg, err = restoreRestic(ctx, src, work, extracted)
	case KindGit:
		leg, err = restoreGit(ctx, src, work, extracted)
	default:
		err = fmt.Errorf("backup: unknown target kind %q", src.Kind)
	}
	if err != nil {
		return Manifest{}, err
	}
	defer clear(leg.dek)

	manifest, err := ReadManifest(extracted)
	if err != nil {
		return Manifest{}, err
	}
	// Authenticity before integrity: a manifest that is not this envelope's is
	// not a manifest worth hashing files against.
	if err := authenticateSnapshot(extracted, leg.dek, leg.target, src.warn); err != nil {
		return Manifest{}, err
	}
	if err := checkKeyringVersion(extracted, leg.rootVersion, manifest); err != nil {
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
		SnapshotName:       RestoredDBName,
		KeyringName:        KeyringName,
		KeyringVersionName: KeyringVersionName,
		ManifestName:       ManifestName,
	}
	for src, dst := range files {
		body, err := os.ReadFile(filepath.Join(from, src))
		if errors.Is(err, os.ErrNotExist) && src == KeyringVersionName {
			// A snapshot from before the counter: the restored dir is
			// generation 1, which is what a missing file already means.
			continue
		}
		if err != nil {
			return fmt.Errorf("backup: restore %s: %w", src, err)
		}
		if err := os.WriteFile(filepath.Join(dest, dst), body, 0o600); err != nil {
			return fmt.Errorf("backup: restore %s: %w", dst, err)
		}
	}
	return nil
}

// authenticateSnapshot checks the restored manifest against the tag the leg
// wrote beside it. A snapshot with no tag predates manifest authentication:
// it restores, loudly, because refusing it would strand every backup written
// before this shipped.
func authenticateSnapshot(dir string, dek []byte, target string, warn func(string)) error {
	if len(dek) != dekLen || target == "" {
		warn("manifest not authenticated: this restore has no keyring to derive the " +
			ManifestMACPurpose + " subkey from, so only the sha256 digests were checked")
		return nil
	}
	key, err := ManifestMACKey(dek, target)
	if err != nil {
		return err
	}
	defer clear(key)
	return authenticateSnapshotKey(dir, key, target, warn)
}

func authenticateSnapshotKey(dir string, key []byte, target string, warn func(string)) error {
	present, err := VerifyManifestMAC(dir, key, target)
	if err != nil {
		return err
	}
	if !present {
		warn("legacy backup: integrity unverified - this snapshot carries no " + ManifestMACName +
			"; its digests are self-consistent, but nothing proves the manifest was written by your envelope")
	}
	return nil
}

// legSource is what a restore leg learned on its way to the snapshot: the DEK
// it unwrapped, the target name it derived under, and the keyring generation
// sitting at the target root. rootVersion is zero when the leg read no root.
type legSource struct {
	dek         []byte
	target      string
	rootVersion int
}

// restoreRestic derives the repository password from the keyring at the target
// root — the chain the recovery card prints — and restores the newest snapshot.
func restoreRestic(ctx context.Context, src Source, work, extracted string) (leg legSource, err error) {
	password := src.Password
	leg.target = src.Name
	if password == "" {
		root, err := fetchRoot(ctx, src)
		if err != nil {
			return legSource{}, err
		}
		leg.rootVersion = root.version
		if leg.target == "" {
			leg.target = targetNameFromRecovery(root.recovery)
		}
		if leg.target == "" {
			return legSource{}, errors.New("backup: target name unknown: pass it explicitly, " +
				"or restore from a target whose root holds " + RecoveryName)
		}
		if src.IdentityFile == "" {
			return legSource{}, errors.New("backup: an age identity is required to unwrap " + KeyringName)
		}
		ids, err := ReadIdentities(src.IdentityFile)
		if err != nil {
			return legSource{}, err
		}
		leg.dek, err = UnwrapDEK(root.keyring, ids)
		if err != nil {
			return legSource{}, err
		}

		password, err = ResticPassword(leg.dek, leg.target)
		if err != nil {
			clear(leg.dek)
			return legSource{}, err
		}
	}
	defer func() {
		if err != nil {
			clear(leg.dek)
		}
	}()

	repo, err := resticRepo(Target{BackupTarget: store.BackupTarget{Kind: src.Kind, URI: src.URI}})
	if err != nil {
		return legSource{}, err
	}
	passFile, cleanup, err := writePasswordFile(src.StageRoot, password)
	if err != nil {
		return legSource{}, err
	}
	defer cleanup()

	restored := filepath.Join(work, "restic")
	res, err := runTool(ctx, src.Driver, executor.Spec{
		Kind: "shell",
		Argv: []string{"restic", "-r", repo, "restore", "latest", "--target", restored},
		Env:  resticEnv(src.Env, passFile),
	}, secrets.NewRedactor([][]byte{[]byte(password)}))
	if err != nil {
		return legSource{}, err
	}
	if res.exitCode != 0 {
		return legSource{}, fmt.Errorf("backup: restic restore exited %d: %s", res.exitCode, res.tail(5))
	}

	// restic restores absolute paths under --target, so the staging tree lands
	// somewhere below it; the manifest marks the spot.
	dir, err := findManifestDir(restored)
	if err != nil {
		return legSource{}, err
	}
	if err := copyDirFiles(dir, extracted); err != nil {
		return legSource{}, err
	}
	return leg, nil
}

// restoreGit clones the rolling backup ref, decrypts the newest snapshot, and
// unpacks it. The keyring committed in the clone is this leg's root: it gives
// both the DEK the tags are checked with and the generation it belongs to.
func restoreGit(ctx context.Context, src Source, work, extracted string) (leg legSource, err error) {
	if src.IdentityFile == "" {
		return legSource{}, errors.New("backup: an age identity is required to decrypt a git snapshot")
	}
	ids, err := ReadIdentities(src.IdentityFile)
	if err != nil {
		return legSource{}, err
	}

	clone := filepath.Join(work, "clone")
	res, err := runTool(ctx, src.Driver, executor.Spec{
		Kind: "shell",
		Argv: []string{"git", "clone", "--quiet", "--branch", "backup", "--depth", "1", src.URI, clone},
		Env:  gitEnv(src.Env),
	}, nil)
	if err != nil {
		return legSource{}, err
	}
	if res.exitCode != 0 {
		return legSource{}, fmt.Errorf("backup: git clone exited %d: %s", res.exitCode, res.tail(5))
	}

	leg.target = src.Name
	if leg.target == "" {
		notes, _ := os.ReadFile(filepath.Join(clone, RecoveryName))
		leg.target = targetNameFromRecovery(notes)
	}
	if keyring, kerr := os.ReadFile(filepath.Join(clone, KeyringName)); kerr == nil {
		leg.dek, _ = UnwrapDEK(keyring, ids)
		version, verr := secrets.ReadKeyringVersion(clone)
		if verr != nil {
			return legSource{}, verr
		}
		leg.rootVersion = version.Version
	}
	defer func() {
		if err != nil {
			clear(leg.dek)
		}
	}()

	snapshot, err := newestSnapshot(clone)
	if err != nil {
		return legSource{}, err
	}
	manifest, partsBody, err := readPartsManifest(clone, snapshot)
	if err != nil {
		return legSource{}, err
	}
	if err := authenticateParts(clone, partsBody, snapshot, leg.dek, leg.target,
		src.AllowUnauthenticated, src.warn); err != nil {
		return legSource{}, err
	}
	sealed, err := reassembleSnapshot(clone, manifest)
	if err != nil {
		return legSource{}, err
	}
	archive, err := ageDecrypt(sealed, ids)
	if err != nil {
		return legSource{}, fmt.Errorf("backup: decrypt %s: %w", snapshot, err)
	}
	raw, err := decompress(ctx, archive)
	if err != nil {
		return legSource{}, err
	}
	if err := untar(raw, extracted); err != nil {
		return legSource{}, err
	}
	return leg, nil
}

// checkKeyringVersion refuses a keyring older than the snapshot it is being
// used with, at the target root and inside the snapshot alike. A manifest with
// no generation predates the counter and cannot be judged.
func checkKeyringVersion(extracted string, rootVersion int, m Manifest) error {
	if m.KeyringVersion == 0 {
		return nil
	}
	snapshot, err := secrets.ReadKeyringVersion(extracted)
	if err != nil {
		return err
	}
	for _, found := range []struct {
		where   string
		version int
	}{
		{"the target root", rootVersion},
		{"the restored snapshot", snapshot.Version},
	} {
		if found.version == 0 || found.version >= m.KeyringVersion {
			continue
		}
		return fmt.Errorf("%w: %s holds keyring v%d, this snapshot was written under v%d; "+
			"fetch the current %s, or restore a snapshot from that generation",
			ErrKeyringDowngrade, found.where, found.version, m.KeyringVersion, KeyringName)
	}
	return nil
}

// authenticateParts checks the parts manifest before a byte of it is trusted:
// it lists the digests every part is checked against, so an unauthenticated
// parts manifest makes those digests worth nothing.
func authenticateParts(dir string, body []byte, snapshot string, dek []byte, target string,
	allowUnauthenticated bool, warn func(string)) error {
	name := partsManifestName(snapshot)
	if len(dek) != dekLen || target == "" {
		warn("parts manifest not authenticated: no keyring in the clone to derive the " +
			ManifestMACPurpose + " subkey from")
		return nil
	}
	key, err := ManifestMACKey(dek, target)
	if err != nil {
		return err
	}
	defer clear(key)

	present, err := VerifyMACFile(filepath.Join(dir, name+macSuffix), key, body, target)
	if err != nil {
		return err
	}
	if present {
		return nil
	}

	// No tag. If this target has authenticated before it carries a marker at
	// its root, and a snapshot that drops the tag there is a downgrade — the
	// git forgery this refusal exists to stop. A forged or wrong-target marker
	// surfaces as ErrManifestTampered from the verify.
	marked, err := VerifyAuthMarker(dir, key, target)
	if err != nil {
		return err
	}
	if marked && !allowUnauthenticated {
		return fmt.Errorf("%w: %s carries no tag, but %s records this target as authenticating its "+
			"snapshots — a git snapshot binds to your keyring only through that tag; restore with "+
			"--allow-unauthenticated only if you are certain this snapshot is yours",
			ErrManifestUnauthenticated, name, AuthMarkerName)
	}
	warn("legacy backup: integrity unverified - " + name + " carries no tag; " +
		"the part digests are self-consistent, but nothing proves they are yours")
	return nil
}

// newestSnapshot returns the base name of the newest chunked snapshot in dir
// — e.g. "snapshot-20260301T020000Z.tar.zst.age" — found by its parts
// manifest rather than its (no longer single) archive file. Snapshot names
// carry a sortable UTC stamp.
func newestSnapshot(dir string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "snapshot-*"+partsSuffix))
	if err != nil {
		return "", fmt.Errorf("backup: scan snapshots: %w", err)
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("backup: no snapshot-*%s in %s", partsSuffix, dir)
	}
	sort.Strings(matches)
	newest := filepath.Base(matches[len(matches)-1])
	return strings.TrimSuffix(newest, partsSuffix), nil
}

// rootFiles are the files a target keeps outside its repository: the keyring
// that unwraps everything, the generation it belongs to, and the notes naming
// the target.
type rootFiles struct {
	keyring  []byte
	recovery []byte
	version  int
}

// fetchRoot reads keyring.age, keyring.version and RECOVERY.md from the target
// root, which sits outside the repository.
func fetchRoot(ctx context.Context, src Source) (rootFiles, error) {
	var out rootFiles
	switch src.Kind {
	case KindResticLocal:
		root := localRepoPath(src.URI)
		keyring, err := os.ReadFile(filepath.Join(root, KeyringName))
		if err != nil {
			return rootFiles{}, fmt.Errorf("backup: read %s at target root: %w", KeyringName, err)
		}
		version, err := secrets.ReadKeyringVersion(root)
		if err != nil {
			return rootFiles{}, err
		}
		out.keyring, out.version = keyring, version.Version
		out.recovery, _ = os.ReadFile(filepath.Join(root, RecoveryName))
		return out, nil
	case KindResticS3:
		client, err := newS3Client(src.URI, src.Env)
		if err != nil {
			return rootFiles{}, err
		}
		out.keyring, err = client.get(ctx, KeyringName)
		if err != nil {
			return rootFiles{}, err
		}
		out.version, err = fetchRootVersion(ctx, client)
		if err != nil {
			return rootFiles{}, err
		}
		out.recovery, _ = client.get(ctx, RecoveryName)
		return out, nil
	default:
		return rootFiles{}, fmt.Errorf("backup: kind %q has no target root", src.Kind)
	}
}

// fetchRootVersion reads the generation beside a bucket's keyring. A target
// written before the counter existed has no object there, which is
// generation 1 — the same rule a local root follows.
func fetchRootVersion(ctx context.Context, client *s3Client) (int, error) {
	body, err := client.get(ctx, KeyringVersionName)
	if err != nil {
		return 1, nil
	}
	version, err := secrets.ParseKeyringVersion(body)
	if err != nil {
		return 0, err
	}
	return version.Version, nil
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
