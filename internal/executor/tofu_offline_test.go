package executor_test

import (
	"context"
	"encoding/base64"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/api"
	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// TestTofuPlanRunsOfflineFromPluginCache proves the §3.8 offline-DR property
// (the offline-guarantee amendment) for OpenTofu: one online apply warms
// PluginCacheDir with the hashicorp/local provider, and a later plan run
// with OfflineProviders set and no network access at all still succeeds.
//
// The offline half runs in a re-exec'd child under `unshare -rn`, for the
// same reason as the ansible proof (see ansible_offline_test.go): the driver
// exposes no namespace hook, so putting its subprocess inside a
// network-denied namespace means putting the whole process there. Unlike
// ansible, a tofu run always talks to exocomp's own HTTP state backend
// (design/architecture/execution.md), and that backend is loopback-only —
// a namespace's loopback is its own, unreachable from any other namespace —
// so the child brings its network interface up and starts its own instance
// of the backend over the same on-disk store and envelope the parent
// prepared, rather than trying to reach the parent's.
func TestTofuPlanRunsOfflineFromPluginCache(t *testing.T) {
	if testing.Short() {
		t.Skip("integration")
	}
	if os.Getenv(offlineChildVar) == "1" {
		runTofuOfflineChild(t)
		return
	}

	requireOfflineProofTools(t, "unshare", executor.TofuBinary)
	requireTofu(t)
	ctx := context.Background()

	dataDir := t.TempDir()
	tofuDir := t.TempDir()
	cache := t.TempDir()
	const wsName = "offline-proof"
	const resourceValue = "exocomp-tofu-offline-canary"
	config := "resource \"local_file\" \"greeting\" {\n" +
		"  filename = \"${path.module}/greeting.txt\"\n" +
		"  content  = \"" + resourceValue + "\"\n}\n"
	if err := os.WriteFile(filepath.Join(tofuDir, "main.tf"), []byte(config), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	st, err := store.Open(filepath.Join(dataDir, "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	env, _, err := secrets.Init(dataDir, nil)
	if err != nil {
		t.Fatalf("init envelope: %v", err)
	}
	ws, err := st.CreateWorkspace(ctx, store.Workspace{Name: wsName, TofuDir: tofuDir})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	srv, err := api.New(api.Options{Store: st, Tokens: env, Port: 0})
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start backend: %v", err)
	}

	token := api.WorkspaceToken(env, ws.Name)
	stateKey := env.Derive(executor.TofuStatePurpose, ws.Name)
	passphrase := base64.StdEncoding.EncodeToString(stateKey)

	driver := executor.NewTofu(executor.TofuOptions{
		Workspace:       ws.Name,
		BackendURL:      api.StateURL(srv.Addr(), ws.Name),
		BackendToken:    token,
		StatePassphrase: passphrase,
		PrivateDirRoot:  t.TempDir(),
		PluginCacheDir:  cache,
	})
	res, err := driver.Run(ctx, executor.Spec{
		Kind: executor.KindTofu,
		Argv: executor.TofuArgv("apply", "-auto-approve"),
		Dir:  tofuDir,
	}, &collector{})
	if err != nil {
		t.Fatalf("online warm-up apply: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("online warm-up apply exited %d", res.ExitCode)
	}
	written, err := os.ReadFile(filepath.Join(tofuDir, "greeting.txt"))
	if err != nil || string(written) != resourceValue {
		t.Fatalf("warm-up apply did not write the managed file: %q, %v", written, err)
	}

	// Close everything before the child reopens the same files: a clean
	// handoff, not two processes racing the same sqlite database.
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown backend: %v", err)
	}
	if err := env.Close(); err != nil {
		t.Fatalf("close envelope: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	child := exec.Command("unshare", "-rn", "--", self,
		"-test.run", "^TestTofuPlanRunsOfflineFromPluginCache$", "-test.v")
	child.Env = append(os.Environ(),
		offlineChildVar+"=1",
		"EXOCOMP_OFFLINE_DATADIR="+dataDir,
		"EXOCOMP_OFFLINE_TOFUDIR="+tofuDir,
		"EXOCOMP_OFFLINE_CACHE="+cache,
		"EXOCOMP_OFFLINE_WORKSPACE="+wsName,
	)
	out, err := child.CombinedOutput()
	t.Logf("offline child output:\n%s", out)
	if err != nil {
		t.Fatalf("offline run failed: %v", err)
	}
	if !strings.Contains(string(out), "OFFLINE_TOFU_PLAN_OK") {
		t.Fatal("offline child did not report a successful plan")
	}
}

// runTofuOfflineChild is the body that actually runs network-denied.
func runTofuOfflineChild(t *testing.T) {
	if out, err := exec.Command("ip", "link", "set", "lo", "up").CombinedOutput(); err != nil {
		t.Fatalf("bring up loopback inside the namespace: %v: %s", err, out)
	}
	if conn, err := net.DialTimeout("tcp", "registry.opentofu.org:443", 2*time.Second); err == nil {
		_ = conn.Close()
		t.Fatal("registry.opentofu.org is reachable inside the namespace; the offline proof is invalid")
	}

	dataDir := os.Getenv("EXOCOMP_OFFLINE_DATADIR")
	ctx := context.Background()

	st, err := store.Open(filepath.Join(dataDir, "exocomp.db"))
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	env, err := secrets.Open(dataDir)
	if err != nil {
		t.Fatalf("reopen envelope: %v", err)
	}
	ws, err := st.WorkspaceByName(ctx, os.Getenv("EXOCOMP_OFFLINE_WORKSPACE"))
	if err != nil {
		t.Fatalf("workspace by name: %v", err)
	}

	// This namespace's loopback is its own: the backend has to run inside it
	// too, not be reached across the namespace boundary.
	srv, err := api.New(api.Options{Store: st, Tokens: env, Port: 0})
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("start backend: %v", err)
	}

	token := api.WorkspaceToken(env, ws.Name)
	stateKey := env.Derive(executor.TofuStatePurpose, ws.Name)
	passphrase := base64.StdEncoding.EncodeToString(stateKey)

	driver := executor.NewTofu(executor.TofuOptions{
		Workspace:        ws.Name,
		BackendURL:       api.StateURL(srv.Addr(), ws.Name),
		BackendToken:     token,
		StatePassphrase:  passphrase,
		PrivateDirRoot:   t.TempDir(),
		PluginCacheDir:   os.Getenv("EXOCOMP_OFFLINE_CACHE"),
		OfflineProviders: true,
	})
	sink := &collector{}
	res, err := driver.Run(ctx, executor.Spec{
		Kind: executor.KindTofu,
		Argv: executor.TofuArgv("plan"),
		Dir:  os.Getenv("EXOCOMP_OFFLINE_TOFUDIR"),
	}, sink)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("tofu plan exited %d:\n%s", res.ExitCode, strings.Join(sink.msgs(""), "\n"))
	}
	if _, err := os.Stdout.WriteString(strings.Join(sink.msgs(""), "\n") + "\nOFFLINE_TOFU_PLAN_OK\n"); err != nil {
		t.Fatalf("write child output: %v", err)
	}
}
