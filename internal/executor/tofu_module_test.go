package executor

import (
	"os"
	"path/filepath"
	"testing"
)

func TestModuleCacheDirDerivation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		opts TofuOptions
		want string
	}{
		{
			name: "explicit wins",
			opts: TofuOptions{ModuleCacheDir: "/x/mods", PluginCacheDir: "/p", Workspace: "w"},
			want: "/x/mods",
		},
		{
			name: "derived beside plugin cache",
			opts: TofuOptions{PluginCacheDir: "/data/tofu/plugin-cache", Workspace: "net"},
			want: "/data/tofu/module-cache/net",
		},
		{
			name: "no workspace, no derivation",
			opts: TofuOptions{PluginCacheDir: "/data/tofu/plugin-cache"},
			want: "",
		},
		{
			name: "no plugin cache, no derivation",
			opts: TofuOptions{Workspace: "net"},
			want: "",
		},
		{
			name: "unsafe workspace refused",
			opts: TofuOptions{PluginCacheDir: "/p", Workspace: "../escape"},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := moduleCacheDir(tc.opts); got != tc.want {
				t.Errorf("moduleCacheDir = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestLinkModuleCachePersists proves the modules directory the run writes to is
// the persistent cache: the symlink points there, so what a first init installs
// a second run finds already present.
func TestLinkModuleCachePersists(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "tofu", "module-cache", "net")
	tf := NewTofu(TofuOptions{ModuleCacheDir: cache})

	dataDir := t.TempDir()
	if err := tf.linkModuleCache(dataDir, SinkFunc(func(Event) {})); err != nil {
		t.Fatalf("linkModuleCache: %v", err)
	}

	link := filepath.Join(dataDir, "modules")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("modules is not a symlink: %v", err)
	}
	if target != cache {
		t.Errorf("modules -> %s, want %s", target, cache)
	}

	// A module a prior init left in the cache is visible through a fresh run's
	// symlink — the reuse the cache exists for.
	if err := os.WriteFile(filepath.Join(cache, "modules.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
	nextData := t.TempDir()
	if err := tf.linkModuleCache(nextData, SinkFunc(func(Event) {})); err != nil {
		t.Fatalf("second linkModuleCache: %v", err)
	}
	if _, err := os.Stat(filepath.Join(nextData, "modules", "modules.json")); err != nil {
		t.Errorf("second run does not see the cached module manifest: %v", err)
	}
}

func TestLinkModuleCacheNoopWithoutCache(t *testing.T) {
	tf := NewTofu(TofuOptions{})
	dataDir := t.TempDir()
	if err := tf.linkModuleCache(dataDir, SinkFunc(func(Event) {})); err != nil {
		t.Fatalf("linkModuleCache: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dataDir, "modules")); !os.IsNotExist(err) {
		t.Errorf("modules link created with no cache configured (err = %v)", err)
	}
}
