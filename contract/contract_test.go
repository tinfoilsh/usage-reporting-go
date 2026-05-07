package contract

import (
	"encoding/json"
	"testing"
	"time"
)

func TestEventJSONRoundTripsCounters(t *testing.T) {
	original := Event{
		EventID:    "event-1",
		RequestID:  "request-1",
		OccurredAt: time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC),
		Reporter:   Reporter{ID: "reporter", Service: "router"},
		Operation:  Operation{Service: "router", Name: "model_request"},
		APIKey:     "tk_test",
		Meters: []Meter{
			{Name: "requests", Quantity: 1},
		},
		Counters: []Counter{
			{Name: CounterCustomerRequests, Quantity: 1},
		},
		Attributes: map[string]string{"model": "gpt-oss-120b"},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}

	var got Event
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	if len(got.Counters) != 1 {
		t.Fatalf("expected one counter, got %d", len(got.Counters))
	}
	if got.Counters[0].Name != CounterCustomerRequests || got.Counters[0].Quantity != 1 {
		t.Fatalf("unexpected counter: %+v", got.Counters[0])
	}
	if len(got.Meters) != 1 || got.Meters[0].Quantity != 1 {
		t.Fatalf("meter did not round-trip: %+v", got.Meters)
	}
}
