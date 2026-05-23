# usage-reporting-go

`usage-reporting-go` is a small Go module for reporting internal usage events from edge services to a control plane over signed HTTP.

It provides:

- the wire contract — event/operation/batch types, header names, ingestion path
- HMAC signing helpers (`SignBatch` / `VerifyBatch`)
- signed request-context propagation (`SignContext` / `VerifyContext`)
- a batching background reporter in the `client` subpackage

## Layout

The module is split into two packages:

- root `usagereporting` — wire surface. Types both sides agree on (`Event`, `Operation`, `Batch`, `Meter`), header constants, signing helpers, usage-context machinery.
- `client/` — the emitter (`ReporterClient`, `Config`, `New`).

## Install

```bash
go get github.com/tinfoilsh/usage-reporting-go@latest
```

## Billing models

The controlplane supports two pricing modes per event, chosen via `Operation`:

1. **Per-operation pricing** — controlplane prices on `(service, name)`. The default.
2. **Class-based pricing** — set `Operation.Class` to opt in. Controlplane prices on `(service, class)` and ignores `Name` for pricing. `Name` remains the granular audit label persisted with every event.

## Packages

### root — `usagereporting`

The wire surface. Imported by both emitters and the controlplane.

**Contract** — the wire format and the well-known identifiers used across reporters and the controlplane:

- `Operation`: the action being measured, scoped by service (`Service` + `Name`, with optional `Class` for tier pricing).
- `Meter`: a named integer quantity such as `input_tokens` or `output_tokens`.
- `Event`: one usage record. Carries `CustomerRequests` (how many customer-billable requests this event represents), zero or more `Meters`, and free-form `Attributes`.
- `Batch`: a delivery envelope containing multiple events.
- Header constants for signed-batch transport (`X-Tinfoil-Reporter-Id`, etc.).
- `IngestionPath`: the controlplane HTTP path that accepts signed batches.
- Service, operation, and meter name constants (`ServiceRouter`, `OperationRouterModelRequest`, `MeterInputTokens`, ...).

**Signing** — `SignBatch` / `VerifyBatch` produce and verify HMAC-SHA256 signatures over a canonical string built from method, path, reporter ID, timestamp, nonce, and SHA-256 body hash. `HeaderValues` extracts the four signing headers from an incoming request.

**Usage context** — `SignContext` / `VerifyContext` and `SetHeaders` / `FromHeaders` propagate a signed `Context` (root request ID, parent service, billing decision) to a downstream service over HTTP headers. Downstream services verify the signature and use it to decide whether to bill the call as their own customer request or treat it as part of the parent's already-counted request — this is what prevents double-counting when a router fans out to a tool service.

### `client/`

The in-process batching emitter. Imported only by services that emit events; the controlplane never imports this.

`ReporterClient` (via `New(Config{...})`) buffers events in memory, periodically flushes them as signed batches, drops any batch whose delivery fails (fire-and-forget), and performs a final flush on `Stop`.

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

	// Per-operation pricing (token-metered model request).
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

	// Class-based pricing (flat per-request, class shared across many ops).
	reporter.AddEvent(usagereporting.Event{
		RequestID: "req_456",
		Operation: usagereporting.Operation{
			Service: usagereporting.ServiceBuckets,
			Name:    "put_object", // granular audit label, emitter-defined
			Class:   "a",          // billing key
		},
		APIKey:           "sk-example",
		CustomerRequests: 1,
	})
}
```

If `EventID` or `OccurredAt` are omitted, `ReporterClient` fills them in automatically.

## Receiving and verifying batches

On the receiving side:

1. read the raw request body
2. extract `X-Tinfoil-*` signing headers
3. verify the signature with the shared secret
4. enforce timestamp skew and reject any batch whose `delivery_id` (used as the signing nonce) has already been seen, to prevent replay
5. unmarshal the body into `usagereporting.Batch`
6. process the batch atomically if you need retry-safe ingestion

The library only signs and verifies; the receiver is responsible for tracking recently-seen `delivery_id` values within the timestamp skew window.

```go
package main

import (
	"encoding/json"
	"fmt"
	"net/http"

	usagereporting "github.com/tinfoilsh/usage-reporting-go"
)

func handleUsageBatch(r *http.Request, body []byte, sharedSecret string) error {
	reporterID, timestamp, nonce, signature, err := usagereporting.HeaderValues(r.Header)
	if err != nil {
		return err
	}

	if !usagereporting.VerifyBatch(
		r.Method,
		r.URL.Path,
		reporterID,
		timestamp,
		nonce,
		body,
		sharedSecret,
		signature,
	) {
		return fmt.Errorf("invalid signature")
	}

	var batch usagereporting.Batch
	if err := json.Unmarshal(body, &batch); err != nil {
		return err
	}

	_ = batch
	return nil
}
```

## Propagating customer-request context

When a parent service (for example a router) calls a downstream tool service that also reports usage, set a signed context on the outgoing request so the downstream emits `customer_requests = 0` and avoids double-counting:

```go
err := usagereporting.SetHeaders(req.Header, usagereporting.Context{
	RootRequestID:       "req_123",
	ParentService:       usagereporting.ServiceRouter,
	BillCustomerRequest: false,
	IssuedAt:            time.Now().UTC(),
}, contextSigningSecret)
```

The downstream verifies and reads it:

```go
ctx, ok, err := usagereporting.FromHeaders(req.Header, contextSigningSecret, time.Now(), 10*time.Minute)
if err != nil {
	// header was present but invalid; reject the request rather than fall
	// through to the direct-billing default.
}
customerRequests := int64(1)
if ok && !ctx.BillCustomerRequest {
	customerRequests = 0
}
```

A direct customer-facing call (no signed header) bills as its own request. A call dispatched by a parent service bills under the parent.

## Delivery model

- batching is in-memory, with a configurable `MaxBufferedEvents` ceiling; once full, the oldest event is dropped to make room for the newest and a warning is logged.
- each flushed batch gets a `DeliveryID`, which is also used as the signing nonce.
- delivery is fire-and-forget: if a batch fails to send it is logged and dropped. `Flush` does not surface delivery errors, since there is no retry queue for callers to react against.
- `Stop` performs a final flush of buffered events before returning.

Callers that need durable delivery must layer that on top of this client.
