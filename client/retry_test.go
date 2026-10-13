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
	events     json.RawMessage // the events array exactly as sent
}

// receiver stands in for the controlplane's usage-report endpoint inside a
// synctest bubble. It authenticates each attempt the way the controlplane's
// verifyRequest does (signature plus timestamp window), rejects a replayed
// nonce as the library's contract asks of receivers, records the attempt, and
// answers with respond(n) for the n-th attempt, counting from 0.
type receiver struct {
	t       *testing.T
	respond func(n int) reply

	mu       sync.Mutex
	attempts []attempt
	nonces   map[string]bool
}

func newReceiver(t *testing.T, respond func(n int) reply) *receiver {
	return &receiver{t: t, respond: respond, nonces: make(map[string]bool)}
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
	var batch struct {
		DeliveryID string          `json:"delivery_id"`
		Events     json.RawMessage `json:"events"`
	}
	var events []usagereporting.Event
	if err := json.Unmarshal(body, &batch); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(batch.Events, &events); err != nil {
		return nil, err
	}
	if batch.DeliveryID != nonce {
		r.t.Errorf("delivery id %q differs from signed nonce %q", batch.DeliveryID, nonce)
	}
	eventIDs := make([]string, 0, len(events))
	for _, event := range events {
		eventIDs = append(eventIDs, event.EventID)
	}

	r.mu.Lock()
	replayed := r.nonces[nonce]
	r.nonces[nonce] = true
	n := len(r.attempts)
	if !replayed {
		r.attempts = append(r.attempts, attempt{at: time.Now(), deliveryID: batch.DeliveryID, eventIDs: eventIDs, events: batch.Events})
	}
	r.mu.Unlock()
	if replayed {
		r.t.Errorf("nonce %q was replayed", nonce)
		return response(http.StatusUnauthorized, ""), nil
	}

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

// checkResentEvents fails unless every attempt resent the first attempt's
// events byte for byte under a delivery id of its own.
func checkResentEvents(t *testing.T, got []attempt) {
	t.Helper()
	seen := make(map[string]bool)
	for _, a := range got {
		if seen[a.deliveryID] {
			t.Fatalf("delivery id %s reused across attempts:%s", a.deliveryID, describe(got))
		}
		seen[a.deliveryID] = true
		if !bytes.Equal(a.events, got[0].events) {
			t.Fatalf("attempts sent different events:%s", describe(got))
		}
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
// transient error is retried until it lands, resending the same events (same
// event IDs, order, and content) so the controlplane's de-duplication on
// event ID can acknowledge events whose response was lost instead of billing
// them twice. Each attempt is a new delivery: its own delivery ID, which the
// receiver checks against the signed nonce and rejects if replayed. The waits
// exceed the controlplane's timestamp window, so a retry that reused the
// first attempt's signature would also be rejected as stale.
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
				// The second event's ID is filled in by AddEvent, before any
				// attempt, so every attempt must carry the same one.
				addEvents(c, "event-0", "")

				c.Flush(context.Background())

				got := r.got()
				if len(got) != 3 {
					t.Fatalf("got %d attempts, want 3:%s", len(got), describe(got))
				}
				checkResentEvents(t, got)
				if ids := got[0].eventIDs; len(ids) != 2 || ids[0] != "event-0" || ids[1] == "" {
					t.Fatalf("unexpected event ids %q", ids)
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
		if len(got) != 3 {
			t.Fatalf("got %d attempts, want 3:%s", len(got), describe(got))
		}
		checkResentEvents(t, got)
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

// eventAttempts counts attempts per event ID.
func eventAttempts(attempts []attempt) map[string]int {
	counts := make(map[string]int)
	for _, a := range attempts {
		for _, id := range a.eventIDs {
			counts[id]++
		}
	}
	return counts
}

// TestStopDuringOutage checks that shutdown makes a best effort within the
// caller's context and the retry bounds, then gives up and counts every
// undelivered event as failed rather than leaving it buffered.
func TestStopDuringOutage(t *testing.T) {
	const window = 30 * time.Second
	for _, tc := range []struct {
		name     string
		deadline time.Duration // 0 stops with context.Background()
		outage   time.Duration // 0 never recovers
		check    func(t *testing.T, s Stats, attempts map[string]int, took time.Duration)
	}{
		{
			name:     "caller deadline",
			deadline: 5 * time.Second,
			check: func(t *testing.T, s Stats, attempts map[string]int, took time.Duration) {
				if took > 5*time.Second {
					t.Errorf("stop took %s, past the caller's 5s deadline", took)
				}
				if attempts["event-1"] != 0 || attempts["event-2"] != 0 {
					t.Errorf("batches were sent after the deadline: %v", attempts)
				}
				if want := (Stats{Enqueued: 3, FailedEvents: 3, FailedBatches: 3, Retries: s.Retries}); s != want {
					t.Errorf("stats: got %+v want %+v", s, want)
				}
			},
		},
		{
			name: "no deadline",
			check: func(t *testing.T, s Stats, attempts map[string]int, took time.Duration) {
				if took > window {
					t.Errorf("stop took %s, more than one %s retry window", took, window)
				}
				if attempts["event-0"] < 2 || attempts["event-1"] != 1 || attempts["event-2"] != 1 {
					t.Errorf("want retries for the first batch and one attempt for each other: %v", attempts)
				}
				if want := (Stats{Enqueued: 3, FailedEvents: 3, FailedBatches: 3, Retries: s.Retries}); s != want {
					t.Errorf("stats: got %+v want %+v", s, want)
				}
			},
		},
		{
			name:   "outage within retry window",
			outage: 4 * time.Second,
			check: func(t *testing.T, s Stats, attempts map[string]int, took time.Duration) {
				if s.DeliveredEvents != 3 || s.FailedEvents != 0 {
					t.Errorf("stop lost events to a 4s outage: %+v", s)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				r := newReceiver(t, func(int) reply {
					if tc.outage == 0 || time.Since(start) < tc.outage {
						return reply{status: http.StatusServiceUnavailable}
					}
					return reply{}
				})
				c := newRetryClient(r, Config{MaxBatchSize: 1, RetryMaxElapsed: window})
				addEvents(c, "event-0", "event-1", "event-2")

				ctx := context.Background()
				if tc.deadline > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, tc.deadline)
					defer cancel()
				}
				c.Stop(ctx)

				tc.check(t, c.Stats(), eventAttempts(r.got()), time.Since(start))
			})
		})
	}
}

// TestStopBoundsLoopRetryByCallerContext checks that the caller's deadline
// also cuts short a delivery the background loop is retrying when Stop is
// called, instead of Stop blocking until that batch's retry window ends.
func TestStopBoundsLoopRetryByCallerContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const flushInterval = time.Second
		r := newReceiver(t, alwaysReply(reply{status: http.StatusServiceUnavailable}))
		c := newRetryClient(r, Config{FlushInterval: flushInterval})
		addEvents(c, "event-0")

		time.Sleep(flushInterval)
		synctest.Wait()
		if got := r.got(); len(got) != 1 {
			t.Fatalf("want the loop waiting to retry after one attempt:%s", describe(got))
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		start := time.Now()
		c.Stop(ctx)

		if took := time.Since(start); took > 3*time.Second {
			t.Fatalf("stop took %s, past the caller's 3s deadline", took)
		}
		if s := c.Stats(); s.FailedBatches != 1 || s.DeliveredBatches != 0 {
			t.Fatalf("stats: %+v", s)
		}
	})
}

// TestStopSkipsRetriesWhileDeliveryIsFailing checks that when the background
// loop has just dropped a batch with its retries exhausted, Stop does not wait
// out another retry window: each remaining batch gets a single attempt. A
// delivery since then clears that state, and Stop retries again.
func TestStopSkipsRetriesWhileDeliveryIsFailing(t *testing.T) {
	const flushInterval = time.Second
	for _, tc := range []struct {
		name             string
		recoverInBetween bool
		wantAttempts     int // for event-2, sent by Stop
	}{
		{name: "loop gave up", wantAttempts: 1},
		{name: "loop delivered since", recoverInBetween: true, wantAttempts: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var healthy atomic.Bool
				r := newReceiver(t, func(int) reply {
					if healthy.Load() {
						return reply{}
					}
					return reply{status: http.StatusServiceUnavailable}
				})
				c := newRetryClient(r, Config{FlushInterval: flushInterval, MaxBatchSize: 1, MaxDeliveryAttempts: 2})
				addEvents(c, "event-0")

				// The loop sends event-0 at 1s and gives up after one retry,
				// by 2s. Sleeping to half-seconds keeps clear of the ticks.
				time.Sleep(3*flushInterval + flushInterval/2)
				synctest.Wait()
				if s := c.Stats(); s.FailedBatches != 1 {
					t.Fatalf("want the loop to have given up on event-0: %+v", s)
				}
				if tc.recoverInBetween {
					healthy.Store(true)
					addEvents(c, "event-1")
					time.Sleep(flushInterval)
					synctest.Wait()
					healthy.Store(false)
					if s := c.Stats(); s.DeliveredBatches != 1 {
						t.Fatalf("want the loop to have delivered event-1: %+v", s)
					}
				}
				addEvents(c, "event-2")
				start := time.Now()
				c.Stop(context.Background())

				attempts := eventAttempts(r.got())
				if attempts["event-2"] != tc.wantAttempts {
					t.Fatalf("stop made %d attempts for event-2, want %d", attempts["event-2"], tc.wantAttempts)
				}
				if took := time.Since(start); (took == 0) != (tc.wantAttempts == 1) {
					t.Fatalf("stop took %s for %d attempts", took, tc.wantAttempts)
				}
			})
		})
	}
}
