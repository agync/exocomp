package store_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"exocomp.dev/exocomp/internal/store"
)

func newWorkspace(t *testing.T, s *store.Store, name string) store.Workspace {
	t.Helper()
	ws, err := s.CreateWorkspace(context.Background(), store.Workspace{
		Name:    name,
		TofuDir: "/srv/tofu/" + name,
	})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	return ws
}

func TestWorkspaceCRUD(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()

	created, err := s.CreateWorkspace(ctx, store.Workspace{
		Name:    "edge",
		TofuDir: "/srv/tofu/edge",
	})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if created.ID == 0 || created.Serial != 0 {
		t.Fatalf("created = %+v", created)
	}

	byName, err := s.WorkspaceByName(ctx, "edge")
	if err != nil {
		t.Fatalf("workspace by name: %v", err)
	}
	if !reflect.DeepEqual(byName, created) {
		t.Errorf("by name = %+v, want %+v", byName, created)
	}

	byID, err := s.GetWorkspace(ctx, created.ID)
	if err != nil {
		t.Fatalf("get workspace: %v", err)
	}
	if !reflect.DeepEqual(byID, created) {
		t.Errorf("by id = %+v, want %+v", byID, created)
	}

	newWorkspace(t, s, "core")
	list, err := s.ListWorkspaces(ctx)
	if err != nil {
		t.Fatalf("list workspaces: %v", err)
	}
	if len(list) != 2 || list[0].Name != "core" || list[1].Name != "edge" {
		t.Errorf("list = %+v, want core then edge", list)
	}

	_, err = s.CreateWorkspace(ctx, store.Workspace{Name: "edge", TofuDir: "/elsewhere"})
	if !errors.Is(err, store.ErrDuplicate) {
		t.Errorf("duplicate name error = %v, want ErrDuplicate", err)
	}

	if _, err := s.WorkspaceByName(ctx, "absent"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing workspace error = %v, want ErrNotFound", err)
	}
}

func TestCreateWorkspaceRejectsUnsafeName(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()

	for _, name := range []string{"../..", "a/b/c", ".", "", "evil\x00name"} {
		if _, err := s.CreateWorkspace(ctx, store.Workspace{Name: name, TofuDir: "/srv/tofu/x"}); err == nil {
			t.Errorf("CreateWorkspace(%q) = nil error, want a validation error", name)
		}
	}
}

func TestTFStateKeepsEverySerial(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	ws := newWorkspace(t, s, "edge")

	if _, err := s.LatestTFState(ctx, ws.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("latest before any push = %v, want ErrNotFound", err)
	}

	for i := 1; i <= 3; i++ {
		pushed, err := s.AppendTFState(ctx, store.NewTFState{
			WorkspaceID: ws.ID,
			SerialHint:  int64(i),
			Lineage:     "1e5f",
			Ciphertext:  []byte(fmt.Sprintf(`{"serial":%d}`, i)),
		})
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		if pushed.Serial != int64(i) {
			t.Errorf("serial = %d, want %d", pushed.Serial, i)
		}
	}

	versions, err := s.ListTFStates(ctx, ws.ID, 0)
	if err != nil {
		t.Fatalf("list tfstates: %v", err)
	}
	if len(versions) != 3 {
		t.Fatalf("kept %d versions, want 3", len(versions))
	}
	if versions[0].Serial != 3 {
		t.Errorf("newest serial = %d, want 3", versions[0].Serial)
	}

	latest, err := s.LatestTFState(ctx, ws.ID)
	if err != nil {
		t.Fatalf("latest tfstate: %v", err)
	}
	if string(latest.Ciphertext) != `{"serial":3}` {
		t.Errorf("latest ciphertext = %s", latest.Ciphertext)
	}

	reread, err := s.GetWorkspace(ctx, ws.ID)
	if err != nil {
		t.Fatalf("get workspace: %v", err)
	}
	if reread.Serial != 3 {
		t.Errorf("workspace serial = %d, want 3", reread.Serial)
	}
}

// A pusher that reuses a serial must not collide with the version already
// stored under it: the workspace's own counter wins.
func TestTFStateAdvancesPastAReusedSerial(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	ws := newWorkspace(t, s, "edge")

	for i := 0; i < 2; i++ {
		if _, err := s.AppendTFState(ctx, store.NewTFState{
			WorkspaceID: ws.ID,
			SerialHint:  1,
			Ciphertext:  []byte(`{"serial":1}`),
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	versions, err := s.ListTFStates(ctx, ws.ID, 0)
	if err != nil {
		t.Fatalf("list tfstates: %v", err)
	}
	if len(versions) != 2 || versions[0].Serial != 2 {
		t.Errorf("versions = %+v, want two with newest serial 2", versions)
	}
}

func TestAppendTFStateVerifiesLockHolder(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	ws := newWorkspace(t, s, "edge")

	if _, err := s.AcquireTFLock(ctx, store.TFLock{
		WorkspaceID: ws.ID,
		LockID:      "held",
		Who:         "operator@host",
		Operation:   "OperationTypeApply",
	}); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	push := func(lockID string) error {
		_, err := s.AppendTFState(ctx, store.NewTFState{
			WorkspaceID: ws.ID,
			LockID:      lockID,
			SerialHint:  1,
			Ciphertext:  []byte(`{"serial":1}`),
		})
		return err
	}

	if err := push("other"); !errors.Is(err, store.ErrLocked) {
		t.Errorf("push under a foreign lock = %v, want ErrLocked", err)
	}
	if err := push(""); !errors.Is(err, store.ErrLocked) {
		t.Errorf("push with no lock while locked = %v, want ErrLocked", err)
	}
	if err := push("held"); err != nil {
		t.Errorf("push by the holder = %v, want success", err)
	}

	if err := s.ReleaseTFLock(ctx, ws.ID, "held"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := push("stale"); !errors.Is(err, store.ErrLocked) {
		t.Errorf("push naming a lock nobody holds = %v, want ErrLocked", err)
	}
}

func TestTFLockAcquireIsAtomic(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	ws := newWorkspace(t, s, "edge")

	const racers = 8
	var (
		start   sync.WaitGroup
		done    sync.WaitGroup
		mu      sync.Mutex
		winners []string
		losers  []store.TFLock
	)
	start.Add(1)
	done.Add(racers)

	for i := 0; i < racers; i++ {
		go func(id string) {
			defer done.Done()
			start.Wait()
			holder, err := s.AcquireTFLock(ctx, store.TFLock{
				WorkspaceID: ws.ID,
				LockID:      id,
				Who:         id + "@host",
				Operation:   "OperationTypeApply",
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				winners = append(winners, id)
			case errors.Is(err, store.ErrLocked):
				losers = append(losers, holder)
			default:
				t.Errorf("acquire %s: %v", id, err)
			}
		}(fmt.Sprintf("racer-%d", i))
	}
	start.Done()
	done.Wait()

	if len(winners) != 1 {
		t.Fatalf("winners = %v, want exactly one", winners)
	}
	if len(losers) != racers-1 {
		t.Fatalf("losers = %d, want %d", len(losers), racers-1)
	}
	// Every loser must be told who beat it, not merely that it lost.
	for _, holder := range losers {
		if holder.LockID != winners[0] {
			t.Errorf("loser saw holder %q, want %q", holder.LockID, winners[0])
		}
		if holder.Who != winners[0]+"@host" || holder.Operation != "OperationTypeApply" {
			t.Errorf("loser saw holder %+v, want the winner's details", holder)
		}
	}

	held, err := s.GetTFLock(ctx, ws.ID)
	if err != nil {
		t.Fatalf("get tflock: %v", err)
	}
	if held.LockID != winners[0] {
		t.Errorf("stored lock = %q, want %q", held.LockID, winners[0])
	}
}

func TestReleaseTFLockChecksLockID(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	ws := newWorkspace(t, s, "edge")

	if _, err := s.AcquireTFLock(ctx, store.TFLock{WorkspaceID: ws.ID, LockID: "held"}); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := s.ReleaseTFLock(ctx, ws.ID, "other"); !errors.Is(err, store.ErrLocked) {
		t.Errorf("release by a non-holder = %v, want ErrLocked", err)
	}
	if err := s.ReleaseTFLock(ctx, ws.ID, "held"); err != nil {
		t.Fatalf("release by the holder: %v", err)
	}
	// A retried unlock must not fail a job that already finished.
	if err := s.ReleaseTFLock(ctx, ws.ID, "held"); err != nil {
		t.Errorf("second release = %v, want success", err)
	}
	if _, err := s.GetTFLock(ctx, ws.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("lock after release = %v, want ErrNotFound", err)
	}
}

// TestStaleTFLocksNamesWhatNoJobWillRelease: a lock's holder is a job, and a
// job that ended will never send UNLOCK. Nothing else in the system moves that
// row, so it has to be findable (ADR 0004).
func TestStaleTFLocksNamesWhatNoJobWillRelease(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()

	live := newWorkspace(t, s, "live")
	dead := newWorkspace(t, s, "dead")
	orphan := newWorkspace(t, s, "orphan")

	running, err := s.CreateJob(ctx, store.NewJob{Runbook: "workspace:live", Kind: "tofu", Trigger: "manual"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := s.ClaimJob(ctx, running.ID); err != nil {
		t.Fatalf("claim job: %v", err)
	}
	finished, err := s.CreateJob(ctx, store.NewJob{Runbook: "workspace:dead", Kind: "tofu", Trigger: "manual"})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := s.ClaimJob(ctx, finished.ID); err != nil {
		t.Fatalf("claim job: %v", err)
	}
	if err := s.TransitionJob(ctx, finished.ID, store.StateRunning, store.StateInterrupted); err != nil {
		t.Fatalf("interrupt job: %v", err)
	}

	for _, tc := range []struct {
		ws    store.Workspace
		jobID *int64
	}{
		{live, &running.ID},
		{dead, &finished.ID},
		{orphan, nil},
	} {
		if _, err := s.AcquireTFLock(ctx, store.TFLock{
			WorkspaceID: tc.ws.ID, LockID: "lock-" + tc.ws.Name,
			Who: "operator@host", Operation: "OperationTypeApply", JobID: tc.jobID,
		}); err != nil {
			t.Fatalf("acquire lock on %s: %v", tc.ws.Name, err)
		}
	}

	stale, err := s.StaleTFLocks(ctx)
	if err != nil {
		t.Fatalf("stale locks: %v", err)
	}
	held := map[int64]bool{}
	for _, l := range stale {
		held[l.WorkspaceID] = true
	}
	if held[live.ID] {
		t.Error("a lock whose job is still running was called stale")
	}
	if !held[dead.ID] {
		t.Error("a lock whose job ended was not called stale")
	}
	if !held[orphan.ID] {
		t.Error("a lock naming no job was not called stale")
	}
}

func TestForceReleaseTFLock(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	ws := newWorkspace(t, s, "edge")

	if _, err := s.ForceReleaseTFLock(ctx, ws.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("force release of a free workspace = %v, want ErrNotFound", err)
	}

	if _, err := s.AcquireTFLock(ctx, store.TFLock{
		WorkspaceID: ws.ID, LockID: "held", Who: "gone@host", Operation: "OperationTypeApply",
	}); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// Breaking it takes no lock id: the holder is what cannot be asked.
	broken, err := s.ForceReleaseTFLock(ctx, ws.ID)
	if err != nil {
		t.Fatalf("force release: %v", err)
	}
	if broken.LockID != "held" || broken.Who != "gone@host" {
		t.Errorf("broken = %+v, want the lock that was held", broken)
	}
	if _, err := s.GetTFLock(ctx, ws.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("get after force release = %v, want ErrNotFound", err)
	}
	if _, err := s.AcquireTFLock(ctx, store.TFLock{
		WorkspaceID: ws.ID, LockID: "next", Who: "apply@host", Operation: "OperationTypeApply",
	}); err != nil {
		t.Errorf("a later apply could not take the freed lock: %v", err)
	}
}
