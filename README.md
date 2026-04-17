# usage-reporting-go

`usage-reporting-go` is a small Go module for reporting internal usage events to a control plane over signed HTTP requests.

It provides:

- a shared event contract in `contract`
- HMAC signing helpers in `signing`
- a batching background reporter in `client`

## Install

```bash
go get github.com/tinfoilsh/usage-reporting-go@latest
```

## Packages

### `contract`

The `contract` package defines the wire format:

- `Reporter`: the service sending events
- `Principal`: optional end-user or org attribution
- `Operation`: the action being measured
- `Meter`: a named counter such as `input_tokens`, `output_tokens`, or `requests`
- `Event`: one usage record
- `Batch`: a delivery envelope containing multiple events

It also defines the signing header names used on outbound requests.

### `signing`

The `signing` package creates and verifies HMAC-SHA256 signatures for a request body plus request metadata:

- HTTP method
- request path
- reporter ID
- timestamp
- nonce
- SHA-256 body hash

Use it on the sender to sign requests and on the receiver to verify them.

### `client`

The `client` package provides `ReporterClient`, which:

- buffers events in memory
- periodically flushes them as batches
- signs each batch request
- retries naturally by leaving failed batches pending
- flushes remaining events on `Stop`

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
		Endpoint: "https://controlplane.example.com/api/internal/usage-reports",
		Reporter: contract.Reporter{
			ID:      "router",
			Service: "router",
		},
		Secret:        "shared-secret",
		FlushInterval: 2 * time.Second,
	})
	defer reporter.Stop(context.Background())

	reporter.AddEvent(contract.Event{
		RequestID: "req_123",
		Operation: contract.Operation{
			Service: "router",
			Name:    "model_request",
		},
		APIKey: "sk-example",
		Meters: []contract.Meter{
			{Name: "input_tokens", Quantity: 120},
			{Name: "output_tokens", Quantity: 48},
			{Name: "requests", Quantity: 1},
		},
		Attributes: map[string]string{
			"model": "gpt-oss-120b",
			"route": "/v1/chat/completions",
		},
	})
}
```

If `EventID`, `OccurredAt`, or `Reporter` are omitted, `ReporterClient` fills them in automatically.

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

	return nil
}
```

## Delivery model

- batching is in-memory
- each flushed batch gets a `DeliveryID`
- failed deliveries remain pending and are retried on the next flush
- `Stop` flushes current and pending events before returning

This module does not prescribe server-side storage or deduplication strategy; receivers typically use `DeliveryID` or `EventID` depending on the guarantees they want.
