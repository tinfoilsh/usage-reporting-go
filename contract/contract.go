package contract

import "time"

const (
	HeaderReporterID = "X-Tinfoil-Reporter-Id"
	HeaderTimestamp  = "X-Tinfoil-Timestamp"
	HeaderNonce      = "X-Tinfoil-Nonce"
	HeaderSignature  = "X-Tinfoil-Signature"
)

type Reporter struct {
	ID      string `json:"id"`
	Service string `json:"service"`
}

type Operation struct {
	Service string `json:"service"`
	Name    string `json:"name"`
}

type Meter struct {
	Name     string `json:"name"`
	Quantity int64  `json:"quantity"`
}

type Event struct {
	EventID    string    `json:"event_id"`
	RequestID  string    `json:"request_id,omitempty"`
	OccurredAt time.Time `json:"occurred_at"`
	Reporter   Reporter  `json:"reporter"`
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
	DeliveryID string  `json:"delivery_id"`
	Events     []Event `json:"events"`
}
