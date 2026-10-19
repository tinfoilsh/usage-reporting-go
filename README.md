# usage-reporting-go

Go module for reporting usage events from Tinfoil edge services (router, tools, buckets) to the control plane over signed HTTP. It defines the wire contract both sides agree on, HMAC signing for batches, and signed request-context propagation so a request that fans out across services is billed once.

## Installation

```bash
go get github.com/tinfoilsh/usage-reporting-go@latest
```

## Sending events

```go
import (
	usagereporting "github.com/tinfoilsh/usage-reporting-go"
	usageclient "github.com/tinfoilsh/usage-reporting-go/client"
)

reporter := usageclient.New(usageclient.Config{
	Endpoint:   "https://controlplane.example.com" + usagereporting.IngestionPath,
	ReporterID: "router-prod-abc123",
	Secret:     sharedSecret,
})
defer reporter.Stop(context.Background())

reporter.AddEvent(usagereporting.Event{
	RequestID: "req_123",
	Operation: usagereporting.Operation{
		Service: usagereporting.ServiceRouter,
		Name:    usagereporting.OperationRouterModelRequest,
	},
	APIKey:           apiKey,
	CustomerRequests: 1,
	Meters: []usagereporting.Meter{
		{Name: usagereporting.MeterInputTokens, Quantity: 120},
		{Name: usagereporting.MeterOutputTokens, Quantity: 48},
	},
})
```

Delivery is fire-and-forget: failed batches are logged and dropped, so callers that need durable delivery must add it themselves.

## Receiving batches

`VerifyBatch` checks only the HMAC signature over the headers returned by `HeaderValues` and the body. Receivers must also reject stale timestamps and any `delivery_id` (the signing nonce) already seen within the allowed skew window; otherwise a captured batch can be replayed and billed twice.

## Development

Run `go test ./...`.

## Reporting Vulnerabilities

Please report security vulnerabilities by either:

- Emailing [security@tinfoil.sh](mailto:security@tinfoil.sh)
- Opening an issue on GitHub on this repository

We aim to respond to (legitimate) security reports within 24 hours.
