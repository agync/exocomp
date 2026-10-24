package service_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/service"
	"exocomp.dev/exocomp/internal/store"
)

// ResolvePlainSourcePlay returns the confined playbook, the checkout root and
// the source's posture: read-only by default, writable only when the source is
// registered writable.
func TestResolvePlainSourcePlay(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	// The resolver returns symlink-resolved paths, so compare against the same.
	checkout, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp dir: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(checkout, "plays"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for name, writable := range map[string]bool{"ro": false, "rw": true} {
		if _, err := st.CreateSource(ctx, store.Source{
			Name: name, Kind: store.SourcePath, URI: checkout, Writable: writable, Enabled: true,
		}); err != nil {
			t.Fatalf("CreateSource %s: %v", name, err)
		}
	}
	svc := service.New(st, t.TempDir(), nil)

	for name, wantReadOnly := range map[string]bool{"ro": true, "rw": false} {
		abs, root, readOnly, err := svc.ResolvePlainSourcePlay(ctx, name, "plays/site.yml")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if want := filepath.Join(checkout, "plays", "site.yml"); abs != want {
			t.Errorf("%s: playbook = %q, want %q", name, abs, want)
		}
		if root != checkout || readOnly != wantReadOnly {
			t.Errorf("%s: root/readOnly = %q/%v, want %q/%v", name, root, readOnly, checkout, wantReadOnly)
		}
	}
}

// The refusals a schedule relies on when it fires: an unknown source, an
// absolute or escaping playbook, and a hooked source never resolve.
func TestResolvePlainSourcePlayRefuses(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	checkout := t.TempDir()
	if _, err := st.CreateSource(ctx, store.Source{Name: "ops", Kind: store.SourcePath, URI: checkout, Enabled: true}); err != nil {
		t.Fatalf("CreateSource: %v", err)
	}
	svc := service.New(st, t.TempDir(), nil)

	for _, tc := range []struct{ source, playbook, want string }{
		{"ghost", "site.yml", "no source"},
		{"ops", "/etc/passwd", "relative to the checkout"},
		{"ops", "nested/../../escape.yml", "outside the source checkout"},
	} {
		_, _, _, err := svc.ResolvePlainSourcePlay(ctx, tc.source, tc.playbook)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("(%s, %s) err = %v, want one containing %q", tc.source, tc.playbook, err, tc.want)
		}
	}

	if err := os.MkdirAll(filepath.Join(checkout, ".exocomp"), 0o700); err != nil {
		t.Fatalf("mkdir hooks: %v", err)
	}
	if err := os.WriteFile(filepath.Join(checkout, ".exocomp", "hooks.yml"),
		[]byte("pre_run:\n  - name: ping\n    run: echo hi\n"), 0o600); err != nil {
		t.Fatalf("write hooks: %v", err)
	}
	if _, _, _, err := svc.ResolvePlainSourcePlay(ctx, "ops", "site.yml"); err == nil ||
		!strings.Contains(err.Error(), "hooks or a gate") {
		t.Errorf("hooked source err = %v, want the hooks refusal", err)
	}
}
