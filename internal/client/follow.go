package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"exocomp.dev/exocomp/internal/wire"
)

// This file is the live-follow surface: GET /api/v1/jobs/{id}/stream as SSE,
// which is how an operator on a laptop watches a job the remote daemon is
// running. The stream carries the SAME already-redacted job_event rows the
// events endpoint projects (internal/web/stream.go), so following adds no new
// disclosure path.

// maxEventLine caps one SSE data line. A job event is one redacted log line;
// anything larger is a misbehaving endpoint, not a legitimate event.
const maxEventLine = 1 << 20

// followReconnects is how many times a dropped stream is re-established. ONE:
// a tailnet blip should not end a watched apply, but a stream that keeps
// dropping is a fault the operator must see rather than a loop that hides it.
const followReconnects = 1

// Event is one job event as the stream carries it — identical in shape to
// wire.JobEventView, values-free and already redacted.
type Event struct {
	Seq   int64  `json:"seq"`
	Ts    string `json:"ts"`
	Level string `json:"level"`
	Msg   string `json:"msg"`
}

// FollowJob streams one job's events to fn until the job is terminal, calling fn
// once per event in sequence order. since>0 resumes after that Seq, so an agent
// re-attaching does not re-read what it already has.
//
// The daemon's stream is authoritative only from the moment it attaches: it
// starts at whatever the tail was when the request arrived and replays nothing
// before it. So every attach is followed by a backfill read of the events
// endpoint from the last Seq — issued AFTER the response headers arrive, hence
// after the server fixed its own cursor, which is what closes the gap rather
// than narrowing it. Events are then deduped by Seq, so an event that arrives
// on both paths is delivered once.
//
// It returns nil when the job settles, ctx.Err() on cancellation, and an error
// when the stream fails and the one reconnect has been spent.
func (c *Client) FollowJob(ctx context.Context, id int64, since int64, fn func(Event) error) error {
	last := since
	for attempt := 0; ; attempt++ {
		resp, err := c.openStream(ctx, id, last)
		if err != nil {
			return err
		}

		next, berr := c.backfill(ctx, id, last, fn)
		last = next
		if berr != nil {
			_ = resp.Body.Close()
			return unwrapCallback(berr)
		}

		done, next, serr := consumeSSE(resp.Body, last, fn)
		_ = resp.Body.Close()
		last = next

		switch {
		case done:
			return nil
		case ctx.Err() != nil:
			return ctx.Err()
		case serr != nil && isCallback(serr):
			return unwrapCallback(serr)
		case attempt >= followReconnects:
			if serr == nil {
				return fmt.Errorf("client: follow job %d: stream ended before the job settled", id)
			}
			return fmt.Errorf("client: follow job %d: %w", id, serr)
		}
	}
}

// openStream attaches to the job's SSE route. A non-2xx answer is read as a wire
// error envelope so a follow fails with the same typed APIError a read would.
func (c *Client) openStream(ctx context.Context, id int64, since int64) (*http.Response, error) {
	u := *c.base
	setPath(&u, "/api/v1/jobs/"+strconv.FormatInt(id, 10)+"/stream")
	if since > 0 {
		q := url.Values{}
		q.Set("since", strconv.FormatInt(since, 10))
		u.RawQuery = q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("client: build stream request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.tok)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	if since > 0 {
		// The SSE-standard resume header, alongside ?since: whichever the daemon
		// honours, the client's own Seq dedup makes the result the same.
		req.Header.Set("Last-Event-ID", strconv.FormatInt(since, 10))
	}

	resp, err := c.stream.Do(req)
	if err != nil {
		var pin *PinMismatchError
		if errors.As(err, &pin) {
			return nil, pin
		}
		return nil, fmt.Errorf("client: follow job %d: %w", id, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		_ = resp.Body.Close()
		_, _, derr := decodeEnvelope(resp.StatusCode, raw)
		if derr == nil {
			derr = statusError(resp.StatusCode, strings.TrimSpace(string(raw)))
		}
		return nil, derr
	}
	return resp, nil
}

// backfill delivers the events appended before the stream attached, reading them
// from the cursor-paged events endpoint. It returns the new high-water Seq.
func (c *Client) backfill(ctx context.Context, id int64, last int64, fn func(Event) error) (int64, error) {
	rows, err := c.JobEvents(ctx, id, last)
	if err != nil {
		return last, err
	}
	for _, e := range rows {
		if e.Seq <= last {
			continue
		}
		last = e.Seq
		if cerr := fn(eventOf(e)); cerr != nil {
			return last, &callbackError{err: cerr}
		}
	}
	return last, nil
}

func eventOf(e wire.JobEventView) Event {
	return Event{Seq: e.Seq, Ts: e.Ts, Level: e.Level, Msg: e.Msg}
}

// consumeSSE parses the text/event-stream body, invoking fn for every `line`
// event past last, and reports done=true when the `done` event arrives (the
// daemon sends it once the job is terminal). A dropped connection comes back as
// a non-nil error with done=false, which is what earns a reconnect.
func consumeSSE(body io.Reader, last int64, fn func(Event) error) (bool, int64, error) {
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64<<10), maxEventLine)

	var name string
	var data []byte
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			// A blank line dispatches the event accumulated so far.
			if name == "done" {
				return true, last, nil
			}
			if name == "line" && len(data) > 0 {
				var ev Event
				if err := json.Unmarshal(data, &ev); err != nil {
					return false, last, fmt.Errorf("decode stream event: %w", err)
				}
				if ev.Seq > last {
					last = ev.Seq
					if cerr := fn(ev); cerr != nil {
						return false, last, &callbackError{err: cerr}
					}
				}
			}
			name, data = "", nil
		case strings.HasPrefix(line, ":"):
			// A comment / keepalive.
		case strings.HasPrefix(line, "event:"):
			name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")...)
		}
	}
	if err := sc.Err(); err != nil {
		return false, last, err
	}
	return false, last, nil
}

// callbackError marks a failure the CALLER's fn raised, not the stream's. It
// must end the follow immediately: reconnecting would re-deliver events to a
// consumer that already refused one.
type callbackError struct{ err error }

func (e *callbackError) Error() string { return e.err.Error() }
func (e *callbackError) Unwrap() error { return e.err }

func isCallback(err error) bool {
	var c *callbackError
	return errors.As(err, &c)
}

func unwrapCallback(err error) error {
	var c *callbackError
	if errors.As(err, &c) {
		return c.err
	}
	return err
}
