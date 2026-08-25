package web

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/store"
	"exocomp.dev/exocomp/internal/wire"
)

// waitJobEvent polls until an event whose Msg contains want exists, returning
// the full tail at that point. A long-running shell job's only automatic
// event is the "running ..." system line emitted when its worker goroutine
// starts (queue.execute) — this waits past that race instead of guessing a
// Seq number for it.
func waitJobEvent(t *testing.T, h *harness, id int64, want string, within time.Duration) []store.JobEvent {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		events, err := h.store.ListJobEvents(context.Background(), id)
		if err != nil {
			t.Fatalf("ListJobEvents: %v", err)
		}
		for _, e := range events {
			if strings.Contains(e.Msg, want) {
				return events
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %d never emitted an event containing %q", id, want)
		}
		time.Sleep(15 * time.Millisecond)
	}
}

// TestJobStreamSinceBackfillsMissedEvents is the fix-1 regression test: the
// cursor used to start at whatever was newest at connect time, so events
// written between the page's render and the SSE connect (running -> error ->
// terminal, all inside one second) were skipped — the client jumped straight
// to "done" and never saw them. The client now passes ?since=<last-rendered
// seq>; the stream must deliver every event after that point, including ones
// that already existed in the table before the connection opened.
func TestJobStreamSinceBackfillsMissedEvents(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()

	job, err := h.queue.Enqueue(ctx, queue.Spec{
		Runbook: "deploy-app", Kind: "shell", Trigger: "manual", Argv: []string{"/bin/sleep", "30"},
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	// The point the page's own render would have last seen — the "running
	// ..." system line the worker emits at start, nothing more for a sleep.
	rendered := waitJobEvent(t, h, job.ID, "running", 3*time.Second)
	since := rendered[len(rendered)-1].Seq

	// Both land before the client connects at all — simulating the gap
	// between page-render and SSE-connect where the old cursor skipped
	// everything.
	if _, err := h.store.AppendJobEvent(ctx, store.JobEvent{JobID: job.ID, Level: store.LevelInfo, Msg: "missed-first"}); err != nil {
		t.Fatalf("AppendJobEvent: %v", err)
	}
	if _, err := h.store.AppendJobEvent(ctx, store.JobEvent{JobID: job.ID, Level: store.LevelInfo, Msg: "missed-second"}); err != nil {
		t.Fatalf("AppendJobEvent: %v", err)
	}

	token := h.mint(t, "op", "operator")
	client := login(t, ts.URL, token)
	resp, err := client.Get(ts.URL + "/jobs/" + itoa64(job.ID) + "/stream?since=" + strconv.FormatInt(since, 10))
	if err != nil {
		t.Fatalf("GET stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200", resp.StatusCode)
	}

	scan := bufio.NewScanner(resp.Body)
	lines := make(chan string, 64)
	go func() {
		defer close(lines)
		for scan.Scan() {
			lines <- scan.Text()
		}
	}()

	if !waitFor(lines, "missed-first", 3*time.Second) {
		t.Fatal("stream did not backfill an event that existed before connect")
	}
	if !waitFor(lines, "missed-second", 3*time.Second) {
		t.Fatal("stream did not backfill the second pre-existing event")
	}
}

// TestJobStreamNoSinceKeepsOldBehavior: the API follow route and any older
// client omit ?since= — the cursor must still fall back to latest-at-connect
// so that path is unchanged.
func TestJobStreamNoSinceKeepsOldBehavior(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()

	job, err := h.queue.Enqueue(ctx, queue.Spec{
		Runbook: "deploy-app", Kind: "shell", Trigger: "manual", Argv: []string{"/bin/sleep", "30"},
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	waitJobEvent(t, h, job.ID, "running", 3*time.Second)

	token := h.mint(t, "op", "operator")
	client := login(t, ts.URL, token)
	resp, err := client.Get(ts.URL + "/jobs/" + itoa64(job.ID) + "/stream")
	if err != nil {
		t.Fatalf("GET stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d, want 200", resp.StatusCode)
	}

	scan := bufio.NewScanner(resp.Body)
	lines := make(chan string, 64)
	go func() {
		defer close(lines)
		for scan.Scan() {
			lines <- scan.Text()
		}
	}()

	if waitFor(lines, "running", 500*time.Millisecond) {
		t.Fatal("a stream with no since must not replay events already at the tail when it connected")
	}
	if _, err := h.store.AppendJobEvent(ctx, store.JobEvent{JobID: job.ID, Level: store.LevelInfo, Msg: "after-connect"}); err != nil {
		t.Fatalf("AppendJobEvent: %v", err)
	}
	if !waitFor(lines, "after-connect", 3*time.Second) {
		t.Fatal("stream with no since stopped delivering new events")
	}
}

// TestJobStreamTsMatchesRESTFormat is the fix-1 regression test: the SSE
// `line` event used to format ts with clock() ("2006-01-02 15:04:05Z"), a
// different shape than the REST job/event views (RFC3339 via wire's rfc3339).
// A phone client parsing both surfaces needs one timestamp parser, so the SSE
// ts for an event must equal what /api/v1/jobs/{id}/events reports for that
// exact same event.
func TestJobStreamTsMatchesRESTFormat(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()

	job, err := h.queue.Enqueue(ctx, queue.Spec{
		Runbook: "deploy-app", Kind: "shell", Trigger: "manual", Argv: []string{"/bin/true"},
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	h.waitJobState(t, job.ID, store.StateSucceeded, 5*time.Second)

	token := h.mint(t, "agent", "readonly")

	_, _, restBody := bearerGet(t, ts.URL, "/api/v1/jobs/"+itoa64(job.ID)+"/events", token)
	var restEnv struct {
		Data []wire.JobEventView `json:"data"`
	}
	if err := json.Unmarshal([]byte(restBody), &restEnv); err != nil {
		t.Fatalf("REST events body is not JSON: %v (%s)", err, restBody)
	}
	if len(restEnv.Data) == 0 {
		t.Fatal("job produced no events to compare")
	}
	want := restEnv.Data[0]

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/jobs/"+itoa64(job.ID)+"/stream?since=0", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET stream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	scan := bufio.NewScanner(resp.Body)
	var gotTs string
	for scan.Scan() {
		line := scan.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var evt struct {
			Seq int64  `json:"seq"`
			Ts  string `json:"ts"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &evt); err != nil {
			continue
		}
		if evt.Seq == want.Seq {
			gotTs = evt.Ts
			break
		}
	}
	if gotTs == "" {
		t.Fatalf("stream never emitted seq %d", want.Seq)
	}
	if gotTs != want.Ts {
		t.Fatalf("SSE ts = %q, REST ts = %q, want equal", gotTs, want.Ts)
	}
}

// TestJobDetailPassesLastRenderedSeq: the job-detail page must thread the
// server-rendered tail's last Seq into the stream script as ?since=, or the
// backfill this fix relies on has nothing to key off.
func TestJobDetailPassesLastRenderedSeq(t *testing.T) {
	h := newHarness(t)
	ts := httptest.NewServer(h.server.Handler())
	defer ts.Close()
	ctx := context.Background()

	job, err := h.queue.Enqueue(ctx, queue.Spec{
		Runbook: "deploy-app", Kind: "shell", Trigger: "manual", Argv: []string{"/bin/sleep", "30"},
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	rendered := waitJobEvent(t, h, job.ID, "running", 3*time.Second)
	last := rendered[len(rendered)-1].Seq

	op := h.mint(t, "laptop", "operator")
	client := login(t, ts.URL, op)
	_, body := getBody(t, client, ts.URL+"/jobs/"+itoa64(job.ID))
	want := `data-since="` + strconv.FormatInt(last, 10) + `"`
	if !strings.Contains(body, want) {
		t.Errorf("job detail page missing %q (the stream's since cursor)", want)
	}
}
