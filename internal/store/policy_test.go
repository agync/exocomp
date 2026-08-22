package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

func openPolicyStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestPlanPolicyCRUD(t *testing.T) {
	t.Parallel()
	st := openPolicyStore(t)
	ctx := context.Background()

	created, err := st.CreatePlanPolicy(ctx, store.PlanPolicy{
		Name:      "no-deletes",
		Workspace: "homelab",
		Expr:      `plan.resource_changes.exists(c, c.change.actions.exists(a, a == "delete"))`,
		Action:    store.PolicyDeny,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID == 0 || !created.Enabled || created.CreatedAt.IsZero() {
		t.Fatalf("create returned an incomplete row: %+v", created)
	}

	got, err := st.GetPlanPolicy(ctx, "no-deletes")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Expr != created.Expr || got.Action != store.PolicyDeny || got.Workspace != "homelab" {
		t.Fatalf("get round-trip mismatch: %+v", got)
	}

	// A second workspace's policy and a disabled one, to prove the filters.
	if _, err := st.CreatePlanPolicy(ctx, store.PlanPolicy{
		Name: "gate-prod", Workspace: "prod", Expr: "true", Action: store.PolicyRequireApproval,
	}); err != nil {
		t.Fatalf("create prod: %v", err)
	}

	homelab, err := st.ListPlanPolicies(ctx, "homelab", true)
	if err != nil {
		t.Fatalf("list homelab: %v", err)
	}
	if len(homelab) != 1 || homelab[0].Name != "no-deletes" {
		t.Fatalf("workspace filter returned %+v", homelab)
	}

	all, err := st.ListPlanPolicies(ctx, "", false)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("list all returned %d rows, want 2", len(all))
	}
}

func TestPlanPolicyNameIsUnique(t *testing.T) {
	t.Parallel()
	st := openPolicyStore(t)
	ctx := context.Background()

	p := store.PlanPolicy{Name: "dup", Workspace: "w", Expr: "true", Action: store.PolicyDeny}
	if _, err := st.CreatePlanPolicy(ctx, p); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err := st.CreatePlanPolicy(ctx, p)
	if !errors.Is(err, store.ErrDuplicate) {
		t.Fatalf("second create error = %v, want ErrDuplicate", err)
	}
}

// TestPlanPolicyActionCheck proves the CHECK constraint refuses an action the
// engine does not know how to enforce, so a bad row cannot reach evaluation.
func TestPlanPolicyActionCheck(t *testing.T) {
	t.Parallel()
	st := openPolicyStore(t)
	ctx := context.Background()

	_, err := st.CreatePlanPolicy(ctx, store.PlanPolicy{
		Name: "bogus", Workspace: "w", Expr: "true", Action: "allow",
	})
	if err == nil {
		t.Fatal("create with an unknown action: want an error, got none")
	}
}

func TestPlanPolicyDelete(t *testing.T) {
	t.Parallel()
	st := openPolicyStore(t)
	ctx := context.Background()

	if _, err := st.CreatePlanPolicy(ctx, store.PlanPolicy{
		Name: "temp", Workspace: "w", Expr: "true", Action: store.PolicyDeny,
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.DeletePlanPolicy(ctx, "temp"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := st.DeletePlanPolicy(ctx, "temp"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second delete error = %v, want ErrNotFound", err)
	}
}
