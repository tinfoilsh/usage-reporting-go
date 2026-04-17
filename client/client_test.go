package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/tinfoilsh/usage-reporting-go/contract"
)

// TestAddEventSnapshotsReferenceFields guards against a caller mutating
// the Meters slice or Attributes map of an event after AddEvent returns and
// seeing that mutation reflected in the queued telemetry.
func TestAddEventSnapshotsReferenceFields(t *testing.T) {
	c := New(Config{
		Endpoint: "https://example.invalid/usage",
		Reporter: contract.Reporter{ID: "reporter"},
		Secret:   "secret",
	})
	defer c.Stop(context.Background())

	meters := []contract.Meter{{Name: "input_tokens", Quantity: 10}}
	attrs := map[string]string{"model": "gpt-oss-120b"}
	c.AddEvent(contract.Event{
		Operation:  contract.Operation{Service: "router", Name: "model_request"},
		APIKey:     "sk-test",
		Meters:     meters,
		Attributes: attrs,
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
	if got.Attributes["model"] != "gpt-oss-120b" {
		t.Fatalf("attribute was mutated after AddEvent: got %q want %q", got.Attributes["model"], "gpt-oss-120b")
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
		Endpoint: redirector.URL + "/usage",
		Reporter: contract.Reporter{ID: "reporter"},
		Secret:   "secret",
	})
	defer c.Stop(context.Background())

	c.AddEvent(contract.Event{
		Operation: contract.Operation{Service: "router", Name: "model_request"},
		APIKey:    "sk-test",
		Meters:    []contract.Meter{{Name: "input_tokens", Quantity: 1}},
	})

	_ = c.Flush(context.Background())

	if got := attackerHits.Load(); got != 0 {
		t.Fatalf("signed telemetry leaked to redirect target: got %d hits, want 0", got)
	}
}
