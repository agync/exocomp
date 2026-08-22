package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// planBody stands in for a saved plan: the attribute in it is the kind of value
// a plan carries in the clear and the database never holds.
const planBody = "tfplan\x00db_password = \"hunter2-in-the-plan\"\n"

// TestPlanArtifactIsUnreadableAtRest walks the whole path: a plan a run wrote
// is sealed before it is recorded, its bytes on disk carry none of its content,
// and the apply path reads it back.
func TestPlanArtifactIsUnreadableAtRest(t *testing.T) {
	dir, _ := initForKit(t)

	env, err := secrets.Open(dir)
	if err != nil {
		t.Fatalf("secrets.Open: %v", err)
	}
	defer func() { _ = env.Close() }()

	dirs, err := tofuDirs(dir)
	if err != nil {
		t.Fatalf("tofuDirs: %v", err)
	}
	plan := filepath.Join(dirs.artifacts, "homelab-1.tfplan")
	if err := os.WriteFile(plan, []byte(planBody), 0o600); err != nil {
		t.Fatalf("write plan: %v", err)
	}

	st, err := openStore()
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	defer func() { _ = st.Close() }()
	job, err := st.CreateJob(t.Context(), store.NewJob{
		Runbook: "workspace:homelab", Kind: "tofu", Trigger: "manual",
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	var out bytes.Buffer
	if err := recordPlan(t.Context(), st, env, &out, job.ID, plan); err != nil {
		t.Fatalf("recordPlan: %v", err)
	}

	artifacts, err := st.ListArtifacts(t.Context(), job.ID)
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	if len(artifacts) != 1 {
		t.Fatalf("recordPlan left %d artifacts, want 1", len(artifacts))
	}
	saved := artifacts[0]
	if !strings.HasSuffix(saved.Path, secrets.SealedSuffix) {
		t.Fatalf("artifact path %s is not sealed", saved.Path)
	}
	if _, err := os.Stat(plan); !os.IsNotExist(err) {
		t.Errorf("the plaintext plan is still on disk (stat err = %v)", err)
	}

	raw, err := os.ReadFile(saved.Path)
	if err != nil {
		t.Fatalf("read sealed plan: %v", err)
	}
	if bytes.Contains(raw, []byte("hunter2-in-the-plan")) {
		t.Error("the sealed plan holds its attribute values in the clear")
	}
	if !bytes.HasPrefix(raw, []byte("age-encryption.org/")) {
		t.Errorf("the sealed plan is not an age file: % x", raw[:min(len(raw), 24)])
	}
	if sum, size, err := fileDigest(saved.Path); err != nil {
		t.Fatalf("digest: %v", err)
	} else if sum != saved.SHA256 || size != saved.Size {
		t.Error("the artifact row records a digest that is not the file on disk")
	}

	// The approval and apply path: readable again, for the length of one run.
	readable, release, err := openPlanForRun(env, dirs.private, saved.Path)
	if err != nil {
		t.Fatalf("openPlanForRun: %v", err)
	}
	body, err := os.ReadFile(readable)
	if err != nil {
		t.Fatalf("read decrypted plan: %v", err)
	}
	if string(body) != planBody {
		t.Errorf("decrypted plan = %q, want %q", body, planBody)
	}
	release()
	if _, err := os.Stat(readable); !os.IsNotExist(err) {
		t.Errorf("the decrypted plan outlived the run (stat err = %v)", err)
	}
}

// TestOpenPlanForRunPassesThroughPlaintext keeps an operator who points --plan
// at a plan file of their own working: it is not sealed, and it is not touched.
func TestOpenPlanForRunPassesThroughPlaintext(t *testing.T) {
	dir, _ := initForKit(t)
	env, err := secrets.Open(dir)
	if err != nil {
		t.Fatalf("secrets.Open: %v", err)
	}
	defer func() { _ = env.Close() }()

	plan := filepath.Join(t.TempDir(), "own.tfplan")
	if err := os.WriteFile(plan, []byte(planBody), 0o600); err != nil {
		t.Fatalf("write plan: %v", err)
	}

	readable, release, err := openPlanForRun(env, dir, plan)
	if err != nil {
		t.Fatalf("openPlanForRun: %v", err)
	}
	defer release()
	if readable != plan {
		t.Errorf("openPlanForRun rewrote an unsealed plan to %s", readable)
	}
}

// TestSealFileIsIdempotent: a plan recorded twice must not end up sealed twice,
// which would leave a file nothing reads in one pass.
func TestSealFileIsIdempotent(t *testing.T) {
	dir, _ := initForKit(t)
	env, err := secrets.Open(dir)
	if err != nil {
		t.Fatalf("secrets.Open: %v", err)
	}
	defer func() { _ = env.Close() }()

	plan := filepath.Join(t.TempDir(), "plan.tfplan")
	if err := os.WriteFile(plan, []byte(planBody), 0o600); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	sealed, err := env.SealFile(plan)
	if err != nil {
		t.Fatalf("SealFile: %v", err)
	}
	again, err := env.SealFile(sealed)
	if err != nil {
		t.Fatalf("SealFile twice: %v", err)
	}
	if again != sealed {
		t.Fatalf("sealing a sealed file produced %s", again)
	}
	body, err := env.OpenSealedFile(sealed)
	if err != nil {
		t.Fatalf("OpenSealedFile: %v", err)
	}
	if string(body) != planBody {
		t.Errorf("round trip returned %q", body)
	}
}
