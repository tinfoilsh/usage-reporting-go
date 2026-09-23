# usage-reporting-go

Go module for reporting usage events from Tinfoil edge services (router, tools, buckets) to the control plane over signed HTTP. It defines the wire contract both sides agree on, HMAC signing for batches, and signed request-context propagation so a request that fans out across services is billed once.

```bash
go get github.com/tinfoilsh/usage-reporting-go@latest
```

- **`usagereporting`** (root): `Event`, `Operation`, `Meter`, `Batch` types; header and path constants; `SignBatch` / `VerifyBatch`; `SetHeaders` / `FromHeaders` for usage context. Imported by emitters and the control plane.
- **`client/`**: `ReporterClient`, an in-memory batching emitter. Imported only by services that emit events.

## Sending events

```go
package main

import (
	"context"
	"time"

	usagereporting "github.com/tinfoilsh/usage-reporting-go"
	usageclient "github.com/tinfoilsh/usage-reporting-go/client"
)

func main() {
	reporter := usageclient.New(usageclient.Config{
		Endpoint:      "https://controlplane.example.com" + usagereporting.IngestionPath,
		ReporterID:    "router-prod-abc123",
		Secret:        "shared-secret",
		FlushInterval: 2 * time.Second,
	})
	defer reporter.Stop(context.Background())

	// Per-operation pricing: the control plane prices on (Service, Name).
	reporter.AddEvent(usagereporting.Event{
		RequestID: "req_123",
		Operation: usagereporting.Operation{
			Service: usagereporting.ServiceRouter,
			Name:    usagereporting.OperationRouterModelRequest,
		},
		APIKey:           "sk-example",
		CustomerRequests: 1,
		Meters: []usagereporting.Meter{
			{Name: usagereporting.MeterInputTokens, Quantity: 120},
			{Name: usagereporting.MeterOutputTokens, Quantity: 48},
		},
		Attributes: map[string]string{
			"model": "gpt-oss-120b",
			"route": "/v1/chat/completions",
		},
	})

	// Class-based pricing: set Class and the control plane prices on
	// (Service, Class); Name remains the audit label.
	reporter.AddEvent(usagereporting.Event{
		RequestID: "req_456",
		Operation: usagereporting.Operation{
			Service: usagereporting.ServiceBuckets,
			Name:    "put_object",
			Class:   "a",
		},
		APIKey:           "sk-example",
		CustomerRequests: 1,
	})
}
```

`EventID` and `OccurredAt` are filled in when omitted. Delivery is fire-and-forget: events are buffered in memory up to `MaxBufferedEvents`, flushed periodically as signed batches, and dropped with a log line if delivery fails. `Stop` performs a final flush. Callers that need durable delivery must layer it on top.

## Receiving and verifying batches

The library signs and verifies; the receiver must also reject replayed `delivery_id` values within the timestamp skew window.

```go
func handleUsageBatch(r *http.Request, body []byte, sharedSecret string) error {
	reporterID, timestamp, nonce, signature, err := usagereporting.HeaderValues(r.Header)
	if err != nil {
		return err
	}

	if !usagereporting.VerifyBatch(r.Method, r.URL.Path, reporterID, timestamp, nonce, body, sharedSecret, signature) {
		return fmt.Errorf("invalid signature")
	}

	var batch usagereporting.Batch
	return json.Unmarshal(body, &batch)
}
```

## Propagating customer-request context

When a parent service calls a downstream service that also reports usage, attach a signed context so the downstream emits `CustomerRequests: 0` instead of billing a second request:

```go
err := usagereporting.SetHeaders(req.Header, usagereporting.Context{
	RootRequestID:       "req_123",
	ParentService:       usagereporting.ServiceRouter,
	BillCustomerRequest: false,
	IssuedAt:            time.Now().UTC(),
}, contextSigningSecret)
```

The downstream reads it:

```go
ctx, ok, err := usagereporting.FromHeaders(req.Header, contextSigningSecret, time.Now(), 10*time.Minute)
if err != nil {
	// Header present but invalid: reject rather than fall through to direct billing.
}
customerRequests := int64(1)
if ok && !ctx.BillCustomerRequest {
	customerRequests = 0
}
```

A call with no signed header bills as its own customer request.

## Reporting Vulnerabilities

Please report security vulnerabilities by either:

- Emailing [security@tinfoil.sh](mailto:security@tinfoil.sh)
- Opening an issue on GitHub on this repository

We aim to respond to (legitimate) security reports within 24 hours.
