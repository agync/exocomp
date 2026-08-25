package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func pushTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "exocomp.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestPushEndpointCRUD(t *testing.T) {
	st := pushTestStore(t)
	ctx := context.Background()

	a, err := st.CreatePushEndpoint(ctx, PushEndpoint{
		TokenID: 1, TokenName: "phone", URL: "https://up.example.com/a", Label: "pixel",
	})
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	if a.ID == 0 || a.CreatedAt.IsZero() {
		t.Fatalf("create a returned %+v, want an id and a created_at", a)
	}
	if _, err := st.CreatePushEndpoint(ctx, PushEndpoint{
		TokenID: 2, TokenName: "laptop", URL: "https://up.example.com/b",
	}); err != nil {
		t.Fatalf("create b: %v", err)
	}

	// list-all sees both; by-token scopes to one.
	all, err := st.ListPushEndpoints(ctx)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("list all = %d rows, want 2", len(all))
	}
	mine, err := st.ListPushEndpointsByToken(ctx, 1)
	if err != nil {
		t.Fatalf("list by token: %v", err)
	}
	if len(mine) != 1 || mine[0].ID != a.ID {
		t.Fatalf("list by token 1 = %+v, want only a", mine)
	}
	n, err := st.CountPushEndpointsForToken(ctx, 1)
	if err != nil || n != 1 {
		t.Fatalf("count token 1 = %d (%v), want 1", n, err)
	}

	// health round-trip: an error then a success clears it.
	if err := st.MarkPushEndpointError(ctx, a.ID, "boom"); err != nil {
		t.Fatalf("mark error: %v", err)
	}
	got, err := st.GetPushEndpoint(ctx, a.ID)
	if err != nil || got.LastError != "boom" {
		t.Fatalf("after error, get = %+v (%v), want last_error boom", got, err)
	}
	now := time.Now().UTC()
	if err := st.MarkPushEndpointSuccess(ctx, a.ID, now); err != nil {
		t.Fatalf("mark success: %v", err)
	}
	got, err = st.GetPushEndpoint(ctx, a.ID)
	if err != nil {
		t.Fatalf("get after success: %v", err)
	}
	if got.LastError != "" {
		t.Fatalf("success did not clear last_error: %+v", got)
	}
	if got.LastSuccessAt == nil {
		t.Fatalf("success did not stamp last_success_at: %+v", got)
	}

	// delete removes the row; a second delete is ErrNotFound.
	if err := st.DeletePushEndpoint(ctx, a.ID); err != nil {
		t.Fatalf("delete a: %v", err)
	}
	if err := st.DeletePushEndpoint(ctx, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("re-delete a err = %v, want ErrNotFound", err)
	}
	if _, err := st.GetPushEndpoint(ctx, a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get deleted a err = %v, want ErrNotFound", err)
	}
}

func TestActivePushEndpoints(t *testing.T) {
	st := pushTestStore(t)
	ctx := context.Background()

	if rows, err := st.ActivePushEndpoints(ctx); err != nil || len(rows) != 0 {
		t.Fatalf("active on empty = %+v (%v), want none", rows, err)
	}
	for _, u := range []string{"https://x/1", "https://x/2"} {
		if _, err := st.CreatePushEndpoint(ctx, PushEndpoint{TokenID: 1, URL: u}); err != nil {
			t.Fatalf("create %s: %v", u, err)
		}
	}
	rows, err := st.ActivePushEndpoints(ctx)
	if err != nil {
		t.Fatalf("active: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("active = %d, want 2", len(rows))
	}
}

func TestCreatePushEndpointRequiresURLAndToken(t *testing.T) {
	st := pushTestStore(t)
	ctx := context.Background()
	if _, err := st.CreatePushEndpoint(ctx, PushEndpoint{TokenID: 1}); err == nil {
		t.Fatal("expected a missing url to be refused")
	}
	if _, err := st.CreatePushEndpoint(ctx, PushEndpoint{URL: "https://x/1"}); err == nil {
		t.Fatal("expected a missing owning token to be refused")
	}
}
