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

type Principal struct {
	Type  string `json:"type,omitempty"`
	ID    string `json:"id,omitempty"`
	OrgID string `json:"org_id,omitempty"`
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
	EventID    string            `json:"event_id"`
	RequestID  string            `json:"request_id,omitempty"`
	OccurredAt time.Time         `json:"occurred_at"`
	Reporter   Reporter          `json:"reporter"`
	Principal  *Principal        `json:"principal,omitempty"`
	Operation  Operation         `json:"operation"`
	APIKey     string            `json:"api_key,omitempty"`
	Meters     []Meter           `json:"meters"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

type Batch struct {
	DeliveryID string  `json:"delivery_id"`
	Events     []Event `json:"events"`
}
