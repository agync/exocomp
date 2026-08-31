package reactive_test

import (
	"context"
	"crypto/sha256"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"exocomp.dev/exocomp/internal/executor"
	"exocomp.dev/exocomp/internal/notify"
	"exocomp.dev/exocomp/internal/queue"
	"exocomp.dev/exocomp/internal/reactive"
	"exocomp.dev/exocomp/internal/secrets"
	"exocomp.dev/exocomp/internal/store"
)

// testDeriver stands in for the secrets envelope. It is deterministic so a
// token minted in one part of a test verifies in another, exactly as the real
// envelope's HKDF subkeys do.
type testDeriver struct{ seed string }

func (d testDeriver) Derive(purpose, id string) []byte {
	sum := sha256.Sum256([]byte(d.seed + "|" + purpose + ":" + id))
	return sum[:]
}

// capturingTransport records what would have been pushed.
type capturingTransport struct{ sent []notify.Notification }

func (c *capturingTransport) Name() string { return "capture" }

func (c *capturingTransport) Publish(_ context.Context, n notify.Notification) error {
	c.sent = append(c.sent, n)
	return nil
}

type harness struct {
	store    *store.Store
	queue    *queue.Queue
	engine   *reactive.Engine
	deriver  testDeriver
	sent     *capturingTransport
	dbPath   string
	nowValue time.Time
}

type harnessOptions struct {
	dedupWindow time.Duration
	approvalTTL time.Duration
	baseURL     string
	dbPath      string
	// known are the secret values the engine's redactor must scrub out of a
	// stored body.
	known [][]byte
}

func newHarness(t *testing.T, opts harnessOptions) *harness {
	t.Helper()

	if testing.Short() {
		t.Skip("reactive harness integration")
	}

	if opts.dbPath == "" {
		opts.dbPath = filepath.Join(t.TempDir(), "exocomp.db")
	}
	st, err := store.Open(opts.dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	q := queue.New(st, executor.NewShell(), queue.Options{
		Workers:      2,
		PollInterval: 10 * time.Millisecond,
		Logger:       slog.New(slog.DiscardHandler),
	})

	h := &harness{
		store:    st,
		queue:    q,
		deriver:  testDeriver{seed: "reactive-test"},
		sent:     &capturingTransport{},
		dbPath:   opts.dbPath,
		nowValue: time.Now().UTC(),
	}

	var redactor func() reactive.Redactor
	if len(opts.known) > 0 {
		redactor = func() reactive.Redactor { return secrets.NewRedactor(opts.known) }
	}

	engine, err := reactive.New(reactive.Options{
		Store:       st,
		Queue:       q,
		Deriver:     h.deriver,
		Redactor:    redactor,
		Notifier:    notify.New(slog.New(slog.DiscardHandler), h.sent),
		BaseURL:     opts.baseURL,
		DedupWindow: opts.dedupWindow,
		ApprovalTTL: opts.approvalTTL,
		Clock:       func() time.Time { return h.nowValue },
		Logger:      slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	h.engine = engine
	return h
}

// advance moves the harness clock, which is how a test reaches a cooldown's
// far side or an approval's expiry without sleeping.
func (h *harness) advance(d time.Duration) { h.nowValue = h.nowValue.Add(d) }

func (h *harness) addRule(t *testing.T, n store.NewRule) store.Rule {
	t.Helper()
	if _, err := reactive.Compile(n.CELExpr); err != nil {
		t.Fatalf("rule %q does not compile: %v", n.Name, err)
	}
	rule, err := h.store.CreateRule(context.Background(), n)
	if err != nil {
		t.Fatalf("create rule %q: %v", n.Name, err)
	}
	return rule
}

// shellRule is the rule most tests use: matches DiskFull, runs /bin/true.
func shellRule(name string) store.NewRule {
	return store.NewRule{
		Name:       name,
		CELExpr:    `event.name == "DiskFull" && event.status == "firing"`,
		ActionKind: store.ActionShell,
		Action:     store.RuleAction{Argv: []string{"/bin/true"}},
	}
}

// event normalizes a body against the harness clock, which is what the
// ingress does with the engine's clock in production.
func (h *harness) event(t *testing.T, name, host, status string) reactive.Event {
	t.Helper()
	events, err := reactive.Normalize(reactive.SourceAlertmanager,
		alertmanagerBody(name, host, status), h.nowValue)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	return events[0]
}

func (h *harness) firing(t *testing.T, name, host string) reactive.Event {
	t.Helper()
	return h.event(t, name, host, "firing")
}

func alertmanagerBody(alertname, instance, status string) []byte {
	return []byte(`{
	  "version": "4",
	  "status": "` + status + `",
	  "receiver": "exocomp",
	  "commonLabels": {"alertname": "` + alertname + `", "severity": "critical"},
	  "alerts": [
	    {
	      "status": "` + status + `",
	      "labels": {"alertname": "` + alertname + `", "instance": "` + instance + `:9100",
	                 "severity": "critical", "job": "node"},
	      "annotations": {"summary": "the disk is full"},
	      "fingerprint": "fp-` + alertname + `-` + instance + `"
	    }
	  ]
	}`)
}

// serve mounts the ingress on an httptest server, which is what an
// Alertmanager instance and a phone both talk to.
func (h *harness) serve(t *testing.T, allow string) *httptest.Server {
	t.Helper()

	prefixes, err := reactive.ParseAllow(allow)
	if err != nil {
		t.Fatalf("parse allow %q: %v", allow, err)
	}
	ingress, err := reactive.NewIngress(reactive.IngressOptions{
		Engine:  h.engine,
		Deriver: h.deriver,
		Allow:   prefixes,
		Logger:  slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatalf("new ingress: %v", err)
	}

	mux := http.NewServeMux()
	for pattern, handler := range ingress.Mounts() {
		mux.Handle(pattern, handler)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, client *http.Client, url string, body []byte, headers map[string]string) (int, string) {
	t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytesReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp.StatusCode, string(raw)
}

func get(t *testing.T, client *http.Client, url string) (int, string) {
	t.Helper()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp.StatusCode, string(raw)
}

func bytesReader(b []byte) *readerOnly { return &readerOnly{b: b} }

// readerOnly keeps the request body a plain stream, so the handler's
// MaxBytesReader path is the one under test rather than a known-length body.
type readerOnly struct {
	b []byte
	i int
}

func (r *readerOnly) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.i:])
	r.i += n
	return n, nil
}

func lastEvent(t *testing.T, st *store.Store) store.Event {
	t.Helper()
	events, err := st.ListEvents(context.Background(), store.EventFilter{Limit: 1})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("no events recorded")
	}
	return events[0]
}

func waitForJobState(t *testing.T, st *store.Store, jobID int64, want store.JobState) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		job, err := st.GetJob(context.Background(), jobID)
		if err != nil {
			t.Fatalf("get job %d: %v", jobID, err)
		}
		if job.State == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %d never reached %s", jobID, want)
}
