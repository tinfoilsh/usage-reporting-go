package usagereporting

import "time"

const (
	HeaderReporterID = "X-Tinfoil-Reporter-Id"
	HeaderTimestamp  = "X-Tinfoil-Timestamp"
	HeaderNonce      = "X-Tinfoil-Nonce"
	HeaderSignature  = "X-Tinfoil-Signature"
)

// IngestionPath is the controlplane HTTP path that accepts signed batches.
const IngestionPath = "/api/internal/usage-reports"

// Service identifiers used in Operation.Service for Tinfoil-owned reporters.
// Other services may use their own stable service and operation strings; the
// controlplane pricing registry decides whether an emitted operation is priced.
const (
	ServiceRouter    = "router"
	ServiceWebsearch = "websearch"
	ServiceBuckets   = "buckets"
)

// Operation names that the controlplane recognizes.
// An op belongs here iff the controlplane needs to branch on it
const (
	OperationRouterModelRequest = "model_request"
	OperationWebsearchSession   = "session"
)

// Meter names. Pricing for a meter is keyed by (service, operation, name)
// in the controlplane.
const (
	MeterInputTokens  = "input_tokens"
	MeterOutputTokens = "output_tokens"
)

type Operation struct {
	Service string `json:"service"`
	Name    string `json:"name"`
	Class   string `json:"class,omitempty"` // Optional, tier-based
}

type Meter struct {
	Name     string `json:"name"`
	Quantity int64  `json:"quantity"`
}

type Event struct {
	// EventID is globally unique for this specific usage event and is the
	// natural idempotency key once receivers persist accepted events.
	EventID string `json:"event_id"`
	// RequestID identifies the customer-facing request or session that caused
	// the event. For the originating service it should match the usage-context
	// RootRequestID propagated to downstream services.
	RequestID  string    `json:"request_id,omitempty"`
	OccurredAt time.Time `json:"occurred_at"`
	Operation  Operation `json:"operation"`
	APIKey     string    `json:"api_key,omitempty"`
	// CustomerRequests records how many customer-billable requests this event
	// represents. Leaf services invoked inside another service's request (for
	// example a tool call dispatched by the router) emit 0 so the parent
	// request is counted exactly once at the edge.
	CustomerRequests int64             `json:"customer_requests,omitempty"`
	Meters           []Meter           `json:"meters"`
	Attributes       map[string]string `json:"attributes,omitempty"`
}

type Batch struct {
	// DeliveryID is unique for one flush attempt and is used as the signing
	// nonce. Retried events should keep their EventID but use a new DeliveryID.
	DeliveryID string  `json:"delivery_id"`
	Events     []Event `json:"events"`
}
