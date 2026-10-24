package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// runRootIn runs the root command with stdin, for the verbs that read a value.
func runRootIn(t *testing.T, stdin string, args []string) (string, string, error) {
	t.Helper()
	root := newRootCmd()
	var out, errb bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errb)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), errb.String(), err
}

// openOnDaemon opens a sealed secret on the daemon, to prove what was sealed.
func openOnDaemon(t *testing.T, d *testDaemon, name string) string {
	t.Helper()
	row, err := d.st.GetSecret(context.Background(), name)
	if err != nil {
		t.Fatalf("secret %s on the daemon: %v", name, err)
	}
	pt, err := d.env.OpenSecret(secrets.SealedSecret{Name: row.Name, Kind: row.Kind, Nonce: row.Nonce, Ciphertext: row.Ciphertext})
	if err != nil {
		t.Fatal(err)
	}
	return string(pt)
}

// With the daemon as the default remote, the admin verbs change the daemon's
// store and leave this machine with no data dir, and an ad-hoc run of the
// source then carries the defaults they set.
func TestAdminVerbsChangeTheDaemonStore(t *testing.T) {
	d := startTestDaemon(t)
	d.pin(t, "box", store.ScopeOperator, true)
	t.Setenv(remoteEnv, "")
	root := t.TempDir()
	ctx := context.Background()

	steps := []struct {
		stdin string
		args  []string
		want  string
	}{
		{"", []string{"source", "add", "infra", "--path", root, "--writable"}, `registered writable path source "infra"`},
		{"s3cr3t-value\n", []string{"secrets", "add", "api-token", "--expires", "720h"}, `sealed "api-token" (kind generic, source user, 12 bytes) (expires `},
		{"", []string{"secrets", "add", "minted", "--generate"}, `sealed "minted" (kind generic, source generated, 43 bytes)`},
		{"", []string{"run-secret", "add", "api", "API_TOKEN", "api-token"}, `bound API_TOKEN -> api-token under "api"`},
		{"", []string{"source", "egress", "infra", "443", "8443"}, "source infra default egress ports: [443 8443]"},
		{"", []string{"source", "run-secrets", "infra", "api"}, "source infra default run-secrets: api"},
		{"", []string{"run-secret", "list"}, "api"},
		{"", []string{"run-secret", "list", "api"}, "API_TOKEN"},
		{"", []string{"secrets", "list"}, "api-token"},
		{"n3w-value\n", []string{"secrets", "replace", "api-token"}, `replaced "api-token" (read from stdin, 9 bytes)`},
		{"", []string{"secrets", "replace", "minted"}, `rotated "minted" (regenerated, 43 bytes)`},
	}
	for _, step := range steps {
		out, errOut, err := runRootIn(t, step.stdin, step.args)
		if err != nil {
			t.Fatalf("%v: %v\n%s%s", step.args, err, out, errOut)
		}
		if !strings.Contains(out, step.want) {
			t.Fatalf("%v printed %q, want %q", step.args, out, step.want)
		}
		if step.stdin != "" && strings.Contains(out+errOut, strings.TrimSpace(step.stdin)) {
			t.Fatalf("%v echoed the secret value", step.args)
		}
	}
	assertNoLocalStore(t)

	src, err := d.st.SourceByName(ctx, "infra")
	if err != nil || src.URI != root || !src.Writable {
		t.Fatalf("daemon source = %+v (%v), want a writable path source at %s", src, err, root)
	}
	if got := openOnDaemon(t, d, "api-token"); got != "n3w-value" {
		t.Fatalf("sealed value on the daemon = %q, want the replaced value", got)
	}
	if row, _ := d.st.GetSecret(ctx, "api-token"); row.ExpiresAt == nil {
		t.Error("the expiry did not reach the daemon")
	}

	// The defaults set above apply to a run started from this machine.
	if out, _, err := runRoot(t, []string{"command", "--source", "infra", "--host", "web", "--", "id"}); err != nil {
		t.Fatalf("command: %v\n%s", err, out)
	}
	job := onlyJob(t, d)
	if job.Args.RunSecretBinding != "api" || !slices.Contains(job.Args.EgressPorts, 8443) {
		t.Fatalf("job run-secrets %q egress %v, want the source's defaults", job.Args.RunSecretBinding, job.Args.EgressPorts)
	}

	// A bound secret needs --force or --assume-yes to go.
	if _, _, err := runRoot(t, []string{"secrets", "rm", "api-token"}); err == nil || !strings.Contains(err.Error(), "confirmation") {
		t.Fatalf("rm of a bound secret: want a confirmation refusal, got %v", err)
	}
	for _, args := range [][]string{
		{"source", "run-secrets", "infra"},
		{"run-secret", "rm", "api"},
		{"secrets", "rm", "api-token"},
		{"source", "rm", "infra"},
	} {
		if out, _, err := runRoot(t, args); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	if _, err := d.st.SourceByName(ctx, "infra"); err == nil {
		t.Error("source rm left the daemon's source")
	}
	if _, err := d.st.GetSecret(ctx, "api-token"); err == nil {
		t.Error("secrets rm left the daemon's secret")
	}
	assertNoLocalStore(t)
}

func TestRemoteGitSourceAddReturnsTheDeployKey(t *testing.T) {
	d := startTestDaemon(t)
	d.pin(t, "box", store.ScopeOperator, true)
	t.Setenv(remoteEnv, "")

	out, _, err := runRoot(t, []string{"source", "add", "infra", "--url", "git@forge.invalid:me/infra.git"})
	if err != nil {
		t.Fatalf("source add --url: %v\n%s", err, out)
	}
	if !strings.Contains(out, "ssh-ed25519 ") {
		t.Fatalf("no public deploy key printed:\n%s", out)
	}
	src, err := d.st.SourceByName(context.Background(), "infra")
	if err != nil || src.DeployKeySecret == "" {
		t.Fatalf("daemon source = %+v (%v), want a git source with a deploy key", src, err)
	}
	if key := openOnDaemon(t, d, src.DeployKeySecret); !strings.Contains(key, "PRIVATE KEY") {
		t.Fatal("the deploy key sealed on the daemon is not a private key")
	}
	if strings.Contains(out, "PRIVATE KEY") {
		t.Fatal("the private deploy key left the daemon")
	}
}

func TestRemoteAdminVerbsAsJSON(t *testing.T) {
	d := startTestDaemon(t)
	d.pin(t, "box", store.ScopeOperator, true)
	t.Setenv(remoteEnv, "")

	out, _, err := runRootIn(t, "v\n", []string{"--output", "json", "secrets", "add", "tok"})
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		Kind string         `json:"kind"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil || env.Kind != "secret.sealed" || env.Data["name"] != "tok" {
		t.Fatalf("envelope = %s (%v)", out, err)
	}
	if strings.Contains(out, `"value"`) {
		t.Fatalf("the envelope carries a value field: %s", out)
	}
}

// An admin verb needs an operator token: a readonly or approve token is refused
// before the daemon's store changes.
func TestRemoteAdminVerbsNeedAnOperatorToken(t *testing.T) {
	for _, scope := range []string{store.ScopeReadonly, store.ScopeApprove} {
		t.Run(scope, func(t *testing.T) {
			d := startTestDaemon(t)
			d.pin(t, "box", scope, true)
			t.Setenv(remoteEnv, "")

			for _, step := range []struct {
				stdin string
				args  []string
			}{
				{"", []string{"source", "add", "infra", "--path", "/srv/infra"}},
				{"v\n", []string{"secrets", "add", "tok"}},
				{"", []string{"run-secret", "rm", "api"}},
			} {
				_, _, err := runRootIn(t, step.stdin, step.args)
				if err == nil || !strings.Contains(err.Error(), "operator") {
					t.Fatalf("%v with a %s token: want a refusal naming the operator scope, got %v", step.args, scope, err)
				}
			}
			srcs, _ := d.st.ListSources(context.Background())
			secs, _ := d.st.ListSecrets(context.Background())
			if len(srcs) != 0 || len(secs) != 0 {
				t.Fatalf("a refused verb changed the daemon: %d sources, %d secrets", len(srcs), len(secs))
			}
			if _, err := os.Stat(localDataDir(t)); !os.IsNotExist(err) {
				t.Fatalf("the CLI's local data dir was touched: %v", err)
			}
		})
	}
}

// A binary value reaches the daemon byte for byte.
func TestRemoteSecretKeepsBinaryValues(t *testing.T) {
	d := startTestDaemon(t)
	d.pin(t, "box", store.ScopeOperator, true)
	t.Setenv(remoteEnv, "")

	value := "\x00\xff\xfea"
	if _, _, err := runRootIn(t, value, []string{"secrets", "add", "blob"}); err != nil {
		t.Fatal(err)
	}
	if got := openOnDaemon(t, d, "blob"); got != value {
		t.Fatalf("sealed bytes = %x, want %x", got, value)
	}
}

// Replacing a transport key over a remote is refused before a value is read,
// and a refused rm under --output json is a wire error envelope.
func TestRemoteSecretRefusalsBeforeAndAfterTheDaemon(t *testing.T) {
	d := startTestDaemon(t)
	d.pin(t, "box", store.ScopeOperator, true)
	t.Setenv(remoteEnv, "")
	ctx := context.Background()
	sealed, err := d.env.Seal("fleet/ssh", "fleet-ssh-key", []byte("PRIVATE"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.st.CreateSecret(ctx, store.SecretRow{Name: sealed.Name, Kind: sealed.Kind,
		Nonce: sealed.Nonce, Ciphertext: sealed.Ciphertext, AAD: sealed.Name + "|" + sealed.Kind}); err != nil {
		t.Fatal(err)
	}

	_, _, err = runRootIn(t, "", []string{"secrets", "replace", "fleet/ssh"})
	if err == nil || !strings.Contains(err.Error(), "--local") || strings.Contains(err.Error(), "stdin") {
		t.Fatalf("want a --local refusal before stdin is read, got %v", err)
	}

	for _, args := range [][]string{{"secrets", "add", "tok"}, {"run-secret", "add", "api", "TOKEN", "tok"}} {
		if _, _, err := runRootIn(t, "v\n", args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	out, _, err := runRoot(t, []string{"--output", "json", "secrets", "rm", "tok"})
	if err == nil || !strings.Contains(out, `"kind":"error"`) {
		t.Fatalf("want an error envelope, got err=%v out=%q", err, out)
	}
	if _, err := d.st.GetSecret(ctx, "tok"); err != nil {
		t.Fatal("a refused rm removed the secret")
	}
}
