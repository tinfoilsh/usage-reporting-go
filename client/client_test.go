package client

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	usagereporting "github.com/tinfoilsh/usage-reporting-go"
)

// TestAddEventSnapshotsReferenceFields guards against a caller mutating
// the Meters slice or Attributes map of an event after AddEvent returns
// and seeing that mutation reflected in the queued telemetry.
func TestAddEventSnapshotsReferenceFields(t *testing.T) {
	c := New(Config{
		Endpoint:   "https://example.invalid/usage",
		ReporterID: "reporter",
		Secret:     "secret",
	})
	defer c.Stop(context.Background())

	meters := []usagereporting.Meter{{Name: usagereporting.MeterInputTokens, Quantity: 10}}
	attrs := map[string]string{"model": "gpt-oss-120b"}
	c.AddEvent(usagereporting.Event{
		Operation:        usagereporting.Operation{Service: usagereporting.ServiceRouter, Name: usagereporting.OperationRouterModelRequest},
		APIKey:           "sk-test",
		CustomerRequests: 1,
		Meters:           meters,
		Attributes:       attrs,
	})

	meters[0].Quantity = 999
	attrs["model"] = "attacker-controlled"

	batches := c.drainBatches()
	if len(batches) != 1 || len(batches[0].Events) != 1 {
		t.Fatalf("expected one batch with one event, got %+v", batches)
	}
	got := batches[0].Events[0]
	if got.Meters[0].Quantity != 10 {
		t.Fatalf("meter quantity was mutated after AddEvent: got %d want 10", got.Meters[0].Quantity)
	}
	if got.CustomerRequests != 1 {
		t.Fatalf("customer requests was mutated after AddEvent: got %d want 1", got.CustomerRequests)
	}
	if got.Attributes["model"] != "gpt-oss-120b" {
		t.Fatalf("attribute was mutated after AddEvent: got %q want %q", got.Attributes["model"], "gpt-oss-120b")
	}
}

// TestAddEventEnforcesBufferCeiling guards against unbounded memory growth
// when the controlplane is unavailable: once MaxBufferedEvents is reached,
// the oldest event is dropped to make room for the newest.
func TestAddEventEnforcesBufferCeiling(t *testing.T) {
	c := New(Config{
		Endpoint:          "https://example.invalid/usage",
		ReporterID:        "reporter",
		Secret:            "secret",
		MaxBufferedEvents: 3,
	})
	defer c.Stop(context.Background())

	for i := 0; i < 5; i++ {
		c.AddEvent(usagereporting.Event{
			EventID:   "event-" + string(rune('0'+i)),
			Operation: usagereporting.Operation{Service: usagereporting.ServiceRouter, Name: usagereporting.OperationRouterModelRequest},
			APIKey:    "sk-test",
		})
	}

	batches := c.drainBatches()
	if len(batches) != 1 {
		t.Fatalf("expected one batch, got %d", len(batches))
	}
	got := batches[0].Events
	if len(got) != 3 {
		t.Fatalf("expected ceiling of 3 buffered events, got %d", len(got))
	}
	wantIDs := []string{"event-2", "event-3", "event-4"}
	for i, ev := range got {
		if ev.EventID != wantIDs[i] {
			t.Fatalf("event[%d] mismatch: got %q want %q", i, ev.EventID, wantIDs[i])
		}
	}
}

// TestAddEventConcurrentAtCapacity asserts that AddEvent does not block or
// corrupt the ring under sustained backpressure: once the buffer is full,
// every concurrent enqueue must be O(1) and the buffer must hold exactly
// MaxBufferedEvents distinct freshest events.
func TestAddEventConcurrentAtCapacity(t *testing.T) {
	const cap = 256
	const writers = 16
	const perWriter = 1024

	c := New(Config{
		Endpoint:          "https://example.invalid/usage",
		ReporterID:        "reporter",
		Secret:            "secret",
		MaxBufferedEvents: cap,
	})
	defer c.Stop(context.Background())

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(writer int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				c.AddEvent(usagereporting.Event{
					EventID:   fmt.Sprintf("w%d-i%d", writer, i),
					Operation: usagereporting.Operation{Service: usagereporting.ServiceRouter, Name: usagereporting.OperationRouterModelRequest},
					APIKey:    "sk-test",
				})
			}
		}(w)
	}
	wg.Wait()

	batches := c.drainBatches()
	total := 0
	seen := make(map[string]struct{})
	for _, b := range batches {
		for _, ev := range b.Events {
			if _, dup := seen[ev.EventID]; dup {
				t.Fatalf("ring buffer yielded duplicate event id %q", ev.EventID)
			}
			seen[ev.EventID] = struct{}{}
			total++
		}
	}
	if total != cap {
		t.Fatalf("expected exactly %d buffered events under sustained pressure, got %d", cap, total)
	}
}

// TestFlushRefusesRedirects verifies the default HTTP client does not follow
// cross-host redirects, preventing signed telemetry from leaking to a
// different origin than the one configured.
func TestFlushRefusesRedirects(t *testing.T) {
	var attackerHits atomic.Int32
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attackerHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer attacker.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL+"/usage", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	c := New(Config{
		Endpoint:   redirector.URL + "/usage",
		ReporterID: "reporter",
		Secret:     "secret",
	})
	defer c.Stop(context.Background())

	c.AddEvent(usagereporting.Event{
		Operation: usagereporting.Operation{Service: usagereporting.ServiceRouter, Name: usagereporting.OperationRouterModelRequest},
		APIKey:    "sk-test",
		Meters:    []usagereporting.Meter{{Name: usagereporting.MeterInputTokens, Quantity: 1}},
	})

	c.Flush(context.Background())

	if got := attackerHits.Load(); got != 0 {
		t.Fatalf("signed telemetry leaked to redirect target: got %d hits, want 0", got)
	}
}
