package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	usagereporting "github.com/tinfoilsh/usage-reporting-go"
)

const (
	retryTestSecret = "retry-test-secret"
	// controlplaneTimestampSkew mirrors usageReportTimestampSkew in the
	// controlplane's usage-report handler.
	controlplaneTimestampSkew = 10 * time.Minute
)

// reply scripts the receiver's answer to one attempt. The zero value is a
// 200; err simulates a transport failure instead of a response.
type reply struct {
	status     int
	retryAfter string
	err        error
}

type attempt struct {
	at         time.Time
	deliveryID string
	eventIDs   []string
}

// receiver stands in for the controlplane's usage-report endpoint inside a
// synctest bubble. It authenticates each attempt the way the controlplane's
// verifyRequest does (signature plus timestamp window), records it, and
// answers with respond(n) for the n-th attempt, counting from 0.
type receiver struct {
	t       *testing.T
	respond func(n int) reply

	mu       sync.Mutex
	attempts []attempt
}

func newReceiver(t *testing.T, respond func(n int) reply) *receiver {
	return &receiver{t: t, respond: respond}
}

func alwaysReply(r reply) func(int) reply {
	return func(int) reply { return r }
}

func (r *receiver) RoundTrip(req *http.Request) (*http.Response, error) {
	// Like http.Transport, send nothing once the request's context is done.
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	reporterID, timestamp, nonce, signature, err := usagereporting.HeaderValues(req.Header)
	if err != nil || !usagereporting.VerifyBatch(req.Method, req.URL.Path, reporterID, timestamp, nonce, body, retryTestSecret, signature) {
		r.t.Errorf("attempt failed signature verification")
		return response(http.StatusUnauthorized, ""), nil
	}
	sentAt, err := time.Parse(time.RFC3339Nano, timestamp)
	if now := time.Now(); err != nil || sentAt.Before(now.Add(-controlplaneTimestampSkew)) || sentAt.After(now.Add(controlplaneTimestampSkew)) {
		r.t.Errorf("attempt timestamp %q is stale at %s", timestamp, now.UTC().Format(time.RFC3339))
		return response(http.StatusUnauthorized, ""), nil
	}
	var batch usagereporting.Batch
	if err := json.Unmarshal(body, &batch); err != nil {
		return nil, err
	}
	if batch.DeliveryID != nonce {
		r.t.Errorf("delivery id %q differs from signed nonce %q", batch.DeliveryID, nonce)
	}
	eventIDs := make([]string, 0, len(batch.Events))
	for _, event := range batch.Events {
		eventIDs = append(eventIDs, event.EventID)
	}

	r.mu.Lock()
	n := len(r.attempts)
	r.attempts = append(r.attempts, attempt{at: time.Now(), deliveryID: batch.DeliveryID, eventIDs: eventIDs})
	r.mu.Unlock()

	rep := r.respond(n)
	if rep.err != nil {
		return nil, rep.err
	}
	return response(rep.status, rep.retryAfter), nil
}

func (r *receiver) got() []attempt {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.attempts)
}

func response(status int, retryAfter string) *http.Response {
	if status == 0 {
		status = http.StatusOK
	}
	header := make(http.Header)
	if retryAfter != "" {
		header.Set("Retry-After", retryAfter)
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(bytes.NewReader(nil))}
}

// newRetryClient builds a reporter that delivers to r. The flush loop is
// effectively off unless cfg sets FlushInterval.
func newRetryClient(r *receiver, cfg Config) *ReporterClient {
	cfg.Endpoint = "https://controlplane.test" + usagereporting.IngestionPath
	cfg.ReporterID = "reporter"
	cfg.Secret = retryTestSecret
	cfg.HTTPClient = &http.Client{Transport: r}
	if cfg.FlushInterval == 0 {
		cfg.FlushInterval = time.Hour
	}
	return New(cfg)
}

func addEvents(c *ReporterClient, ids ...string) {
	for _, id := range ids {
		c.AddEvent(usagereporting.Event{EventID: id})
	}
}

func describe(attempts []attempt) string {
	var b strings.Builder
	for _, a := range attempts {
		fmt.Fprintf(&b, "\n  %s delivery=%s events=%v", a.at.UTC().Format(time.TimeOnly), a.deliveryID, a.eventIDs)
	}
	return b.String()
}

// TestDeliveryRetriesTransientFailures checks that a batch failing with a
// transient error is retried until it lands, as the same batch: every attempt
// carries the same delivery ID and events, so the controlplane's
// de-duplication can acknowledge a batch whose response was lost instead of
// billing it twice. The waits exceed the controlplane's timestamp window, so a
// retry that reused the first attempt's signature would be rejected as stale.
func TestDeliveryRetriesTransientFailures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure reply
	}{
		{name: "server error", failure: reply{status: http.StatusServiceUnavailable}},
		{name: "rate limited", failure: reply{status: http.StatusTooManyRequests}},
		{name: "transport error", failure: reply{err: errors.New("connection reset by peer")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := newReceiver(t, func(n int) reply {
					if n < 2 {
						return tc.failure
					}
					return reply{}
				})
				c := newRetryClient(r, Config{
					RetryInitialBackoff: 3 * controlplaneTimestampSkew,
					RetryMaxBackoff:     3 * controlplaneTimestampSkew,
					RetryMaxElapsed:     24 * time.Hour,
				})
				defer c.Stop(context.Background())
				addEvents(c, "event-0", "event-1")

				c.Flush(context.Background())

				got := r.got()
				if len(got) != 3 {
					t.Fatalf("got %d attempts, want 3:%s", len(got), describe(got))
				}
				for _, a := range got {
					if a.deliveryID != got[0].deliveryID || !slices.Equal(a.eventIDs, []string{"event-0", "event-1"}) {
						t.Fatalf("retries changed the batch:%s", describe(got))
					}
				}
				want := Stats{
					Enqueued:                   2,
					DeliveredEvents:            2,
					DeliveredBatches:           1,
					DeliveredAfterRetryEvents:  2,
					DeliveredAfterRetryBatches: 1,
					Retries:                    2,
				}
				if s := c.Stats(); s != want {
					t.Fatalf("stats: got %+v want %+v", s, want)
				}
			})
		})
	}
}

// TestDefaultRetryPolicyRidesOutShortOutage pins what the defaults are for: a
// batch first sent just as the database goes away (the weekly ~4s Timescale
// restart, a deadlock, a short out-of-memory event) is still delivered if the
// controlplane recovers within 10 seconds, whatever the jitter.
func TestDefaultRetryPolicyRidesOutShortOutage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const outage = 10 * time.Second
		start := time.Now()
		r := newReceiver(t, func(int) reply {
			if time.Since(start) < outage {
				return reply{status: http.StatusServiceUnavailable}
			}
			return reply{}
		})
		c := newRetryClient(r, Config{})
		defer c.Stop(context.Background())
		addEvents(c, "event-0")

		c.Flush(context.Background())

		if s := c.Stats(); s.DeliveredAfterRetryBatches != 1 || s.FailedBatches != 0 {
			t.Fatalf("batch did not survive a %s outage: %+v%s", outage, s, describe(r.got()))
		}
	})
}

func TestDeliveryStopsAtAttemptLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newReceiver(t, alwaysReply(reply{status: http.StatusBadGateway}))
		c := newRetryClient(r, Config{MaxDeliveryAttempts: 3})
		defer c.Stop(context.Background())
		addEvents(c, "event-0")

		c.Flush(context.Background())

		got := r.got()
		if len(got) != 3 || got[1].deliveryID != got[0].deliveryID || got[2].deliveryID != got[0].deliveryID {
			t.Fatalf("want 3 attempts of one batch:%s", describe(got))
		}
		want := Stats{Enqueued: 1, FailedEvents: 1, FailedBatches: 1, Retries: 2}
		if s := c.Stats(); s != want {
			t.Fatalf("stats: got %+v want %+v", s, want)
		}
	})
}

// TestDeliveryStopsAtRetryWindow checks that no retry starts later than
// RetryMaxElapsed after a batch's first attempt, and that the reporter gives
// up as soon as the next wait would cross that bound rather than sleeping
// into it first.
func TestDeliveryStopsAtRetryWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const window = 10 * time.Second
		r := newReceiver(t, alwaysReply(reply{status: http.StatusServiceUnavailable}))
		c := newRetryClient(r, Config{
			MaxDeliveryAttempts: 100,
			RetryInitialBackoff: 4 * time.Second,
			RetryMaxBackoff:     4 * time.Second,
			RetryMaxElapsed:     window,
		})
		defer c.Stop(context.Background())
		addEvents(c, "event-0")

		start := time.Now()
		c.Flush(context.Background())
		returned := time.Since(start)

		got := r.got()
		// Each wait falls in [2s, 4s], so the window admits two to five retries.
		if len(got) < 3 || len(got) > 6 {
			t.Fatalf("got %d attempts in a %s window:%s", len(got), window, describe(got))
		}
		last := got[len(got)-1].at.Sub(start)
		if last > window {
			t.Fatalf("last attempt started %s after the first, past the %s window", last, window)
		}
		if returned != last {
			t.Fatalf("flush returned %s after the first attempt, last attempt was at %s", returned, last)
		}
		want := Stats{Enqueued: 1, FailedEvents: 1, FailedBatches: 1, Retries: uint64(len(got) - 1)}
		if s := c.Stats(); s != want {
			t.Fatalf("stats: got %+v want %+v", s, want)
		}
	})
}

func TestDeliveryHonoursRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		name       string
		retryAfter func(now time.Time) string
		// wantGap is the wait before the second attempt; zero means the
		// reporter must give up at once because the wait exceeds its window.
		wantGap time.Duration
	}{
		{name: "delay seconds", retryAfter: func(time.Time) string { return "7" }, wantGap: 7 * time.Second},
		{
			name:       "http date",
			retryAfter: func(now time.Time) string { return now.Add(7 * time.Second).UTC().Format(http.TimeFormat) },
			wantGap:    7 * time.Second,
		},
		{name: "beyond retry window", retryAfter: func(time.Time) string { return "120" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := newReceiver(t, func(n int) reply {
					if n == 0 {
						return reply{status: http.StatusTooManyRequests, retryAfter: tc.retryAfter(time.Now())}
					}
					return reply{}
				})
				c := newRetryClient(r, Config{
					RetryInitialBackoff: time.Millisecond,
					RetryMaxBackoff:     time.Millisecond,
					RetryMaxElapsed:     30 * time.Second,
				})
				defer c.Stop(context.Background())
				addEvents(c, "event-0")

				start := time.Now()
				c.Flush(context.Background())

				got := r.got()
				if tc.wantGap == 0 {
					if len(got) != 1 || time.Since(start) != 0 || c.Stats().FailedBatches != 1 {
						t.Fatalf("want one attempt and an immediate drop, flush took %s:%s", time.Since(start), describe(got))
					}
					return
				}
				if len(got) != 2 || got[1].at.Sub(got[0].at) != tc.wantGap || c.Stats().DeliveredAfterRetryBatches != 1 {
					t.Fatalf("want a retry exactly %s after the first attempt:%s", tc.wantGap, describe(got))
				}
			})
		})
	}
}

func TestFlushStopsRetryingWhenContextEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newReceiver(t, alwaysReply(reply{status: http.StatusServiceUnavailable}))
		c := newRetryClient(r, Config{})
		defer c.Stop(context.Background())
		addEvents(c, "event-0")

		ctx, cancel := context.WithCancel(context.Background())
		start := time.Now()
		flushed := make(chan struct{})
		go func() {
			c.Flush(ctx)
			close(flushed)
		}()
		synctest.Wait()
		if got := r.got(); len(got) != 1 {
			t.Fatalf("want flush waiting after one attempt:%s", describe(got))
		}
		cancel()
		<-flushed
		if waited := time.Since(start); waited != 0 {
			t.Fatalf("flush kept waiting %s after its context ended", waited)
		}
		want := Stats{Enqueued: 1, FailedEvents: 1, FailedBatches: 1}
		if s := c.Stats(); s != want {
			t.Fatalf("stats after cancellation: got %+v want %+v", s, want)
		}

		// A flush whose context has already ended leaves events buffered
		// rather than draining them into a batch it cannot send.
		addEvents(c, "event-1")
		c.Flush(ctx)
		want.Enqueued = 2
		if s := c.Stats(); s != want || len(r.got()) != 1 {
			t.Fatalf("flush with a done context sent or dropped events: %+v", s)
		}
	})
}

// TestFlushLeavesBacklogBufferedAfterGivingUp checks that one flush waits out
// at most one retry window on a failing controlplane: once a batch is dropped
// with its retries exhausted, the batches behind it stay buffered and unsent
// for the next flush instead of each waiting out a window of its own.
func TestFlushLeavesBacklogBufferedAfterGivingUp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var healthy atomic.Bool
		r := newReceiver(t, func(int) reply {
			if healthy.Load() {
				return reply{}
			}
			return reply{status: http.StatusServiceUnavailable}
		})
		c := newRetryClient(r, Config{MaxBatchSize: 1, MaxDeliveryAttempts: 3})
		defer c.Stop(context.Background())
		addEvents(c, "event-0", "event-1", "event-2")

		c.Flush(context.Background())
		got := r.got()
		if len(got) != 3 {
			t.Fatalf("want three attempts of the first batch only:%s", describe(got))
		}
		for _, a := range got {
			if !slices.Equal(a.eventIDs, []string{"event-0"}) {
				t.Fatalf("flush moved on after giving up:%s", describe(got))
			}
		}
		want := Stats{Enqueued: 3, FailedEvents: 1, FailedBatches: 1, Retries: 2}
		if s := c.Stats(); s != want {
			t.Fatalf("stats after outage: got %+v want %+v", s, want)
		}

		healthy.Store(true)
		c.Flush(context.Background())
		got = r.got()[3:]
		if len(got) != 2 || !slices.Equal(got[0].eventIDs, []string{"event-1"}) || !slices.Equal(got[1].eventIDs, []string{"event-2"}) {
			t.Fatalf("recovery flush did not deliver the backlog:%s", describe(got))
		}
		want.DeliveredEvents, want.DeliveredBatches = 2, 2
		if s := c.Stats(); s != want {
			t.Fatalf("stats after recovery: got %+v want %+v", s, want)
		}
	})
}
