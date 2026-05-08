package contract

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestEventJSONRoundTripsCustomerRequests(t *testing.T) {
	original := Event{
		EventID:          "event-1",
		RequestID:        "request-1",
		OccurredAt:       time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC),
		Operation:        Operation{Service: ServiceRouter, Name: OperationRouterModelRequest},
		APIKey:           "tk_test",
		CustomerRequests: 1,
		Meters: []Meter{
			{Name: MeterInputTokens, Quantity: 10},
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
	if got.CustomerRequests != 1 {
		t.Fatalf("customer_requests did not round-trip: got %d", got.CustomerRequests)
	}
	if len(got.Meters) != 1 || got.Meters[0].Name != MeterInputTokens || got.Meters[0].Quantity != 10 {
		t.Fatalf("meter did not round-trip: %+v", got.Meters)
	}
}

func TestEventJSONOmitsCustomerRequestsWhenZero(t *testing.T) {
	event := Event{
		EventID:    "event-1",
		OccurredAt: time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC),
		Operation:  Operation{Service: ServiceWebsearch, Name: OperationWebsearchSession},
	}
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	if bytes := string(data); strings.Contains(bytes, "customer_requests") {
		t.Fatalf("expected customer_requests to be omitted when zero, got %s", bytes)
	}
}

