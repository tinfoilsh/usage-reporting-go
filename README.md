# usage-reporting-go

`usage-reporting-go` is a small Go module for reporting internal usage events from edge services to a control plane over signed HTTP.

It provides:

- a shared event contract in `contract`
- HMAC signing helpers in `signing`
- a batching background reporter in `client`
- signed request-context propagation in `usagecontext`

## Install

```bash
go get github.com/tinfoilsh/usage-reporting-go@latest
```

## Packages

### `contract`

Defines the wire format and the well-known identifiers used across reporters and the controlplane:

- `Operation`: the action being measured, scoped by service (`Service` + `Name`).
- `Meter`: a named integer quantity such as `input_tokens` or `output_tokens`.
- `Event`: one usage record. Carries `CustomerRequests` (how many customer-billable requests this event represents), zero or more `Meters`, and free-form `Attributes`.
- `Batch`: a delivery envelope containing multiple events.
- Header constants for signed-batch transport (`X-Tinfoil-Reporter-Id`, etc.).
- `IngestionPath`: the controlplane HTTP path that accepts signed batches.
- Service, operation, and meter name constants (`ServiceRouter`, `OperationRouterModelRequest`, `MeterInputTokens`, ...).

### `signing`

Creates and verifies HMAC-SHA256 signatures over a canonical string built from the request method, path, reporter ID, timestamp, nonce, and SHA-256 body hash. Use it on the sender to sign batch deliveries and on the receiver to verify them.

### `client`

Provides `ReporterClient`, which:

- buffers events in memory
- periodically flushes them as batches
- signs each outbound batch
- drops any batch whose delivery fails (fire-and-forget)
- flushes remaining events on `Stop`

### `usagecontext`

Lets a service propagate a signed `Context` (root request ID, parent service, customer-request-count) to a downstream service over HTTP headers. Downstream services verify the signature and use the context to decide whether to bill the call as its own customer request or treat it as part of the parent's already-counted request. This is what prevents double-counting when a router fans out to a tool service.

## Sending events

```go
package main

import (
	"context"
	"time"

	usageclient "github.com/tinfoilsh/usage-reporting-go/client"
	"github.com/tinfoilsh/usage-reporting-go/contract"
)

func main() {
	reporter := usageclient.New(usageclient.Config{
		Endpoint:      "https://controlplane.example.com" + contract.IngestionPath,
		ReporterID:    "router-prod-abc123",
		Secret:        "shared-secret",
		FlushInterval: 2 * time.Second,
	})
	defer reporter.Stop(context.Background())

	reporter.AddEvent(contract.Event{
		RequestID: "req_123",
		Operation: contract.Operation{
			Service: contract.ServiceRouter,
			Name:    contract.OperationRouterModelRequest,
		},
		APIKey:           "sk-example",
		CustomerRequests: 1,
		Meters: []contract.Meter{
			{Name: contract.MeterInputTokens, Quantity: 120},
			{Name: contract.MeterOutputTokens, Quantity: 48},
		},
		Attributes: map[string]string{
			"model": "gpt-oss-120b",
			"route": "/v1/chat/completions",
		},
	})
}
```

If `EventID` or `OccurredAt` are omitted, `ReporterClient` fills them in automatically.

## Receiving and verifying batches

On the receiving side:

1. read the raw request body
2. extract `X-Tinfoil-*` signing headers
3. verify the signature with the shared secret
4. unmarshal the body into `contract.Batch`
5. process the batch atomically if you need retry-safe ingestion

```go
package main

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/tinfoilsh/usage-reporting-go/contract"
	"github.com/tinfoilsh/usage-reporting-go/signing"
)

func handleUsageBatch(r *http.Request, body []byte, sharedSecret string) error {
	reporterID, timestamp, nonce, signature, err := signing.HeaderValues(r.Header)
	if err != nil {
		return err
	}

	if !signing.Verify(
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

	var batch contract.Batch
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
import "github.com/tinfoilsh/usage-reporting-go/usagecontext"

count := int64(0)
err := usagecontext.SetHeaders(req.Header, usagecontext.Context{
	RootRequestID:        "req_123",
	ParentService:        contract.ServiceRouter,
	CustomerRequestCount: &count,
	IssuedAt:             time.Now().UTC(),
}, contextSigningSecret)
```

The downstream verifies and reads it:

```go
ctx, ok, err := usagecontext.FromHeaders(req.Header, contextSigningSecret, time.Now(), 10*time.Minute)
if err != nil {
	// header was present but invalid; reject the request rather than fall
	// through to the direct-billing default.
}
customerRequests := int64(1)
if ok && ctx.CustomerRequestCount != nil && *ctx.CustomerRequestCount >= 0 {
	customerRequests = *ctx.CustomerRequestCount
}
```

A direct customer-facing call (no signed header) bills as its own request. A call dispatched by a parent service bills under the parent.

## Delivery model

- batching is in-memory
- each flushed batch gets a `DeliveryID`, which is also used as the signing nonce
- delivery is fire-and-forget: if a batch fails to send it is logged and dropped
- `Stop` performs a final flush of buffered events before returning

Callers that need durable delivery must layer that on top of this client.
