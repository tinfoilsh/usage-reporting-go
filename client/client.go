package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/tinfoilsh/usage-reporting-go/contract"
	"github.com/tinfoilsh/usage-reporting-go/signing"
)

const (
	defaultFlushInterval = 2 * time.Second
	defaultMaxBatchSize  = 1000
)

type Config struct {
	Endpoint      string
	Reporter      contract.Reporter
	Secret        string
	FlushInterval time.Duration
	HTTPClient    *http.Client
	// MaxBatchSize caps the number of events rolled into a single outbound batch.
	// Defaults to 1000 when unset or non-positive.
	MaxBatchSize int
}

// ReporterClient batches usage events and delivers them to the controlplane
// over signed HTTP. Delivery is fire-and-forget: if a batch fails, it is
// logged and dropped. Callers that need durable delivery must layer that on
// top of this client.
type ReporterClient struct {
	endpoint      string
	reporter      contract.Reporter
	secret        string
	flushInterval time.Duration
	httpClient    *http.Client
	maxBatchSize  int

	mu      sync.Mutex
	current []contract.Event

	quit     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

func New(cfg Config) *ReporterClient {
	interval := cfg.FlushInterval
	if interval <= 0 {
		interval = defaultFlushInterval
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout: 10 * time.Second,
			// Block redirects by default. The batch is a signed HMAC against a
			// specific path and host; following a redirect would send signed
			// telemetry (and the reporter identity header) to a different
			// host, which the shared secret cannot authenticate.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}

	maxBatchSize := cfg.MaxBatchSize
	if maxBatchSize <= 0 {
		maxBatchSize = defaultMaxBatchSize
	}

	c := &ReporterClient{
		endpoint:      strings.TrimRight(cfg.Endpoint, "/"),
		reporter:      cfg.Reporter,
		secret:        cfg.Secret,
		flushInterval: interval,
		httpClient:    httpClient,
		maxBatchSize:  maxBatchSize,
		quit:          make(chan struct{}),
	}

	if c.endpoint != "" && c.secret != "" && c.reporter.ID != "" {
		c.wg.Add(1)
		go c.loop()
	}

	return c
}

func (c *ReporterClient) Enabled() bool {
	return c.endpoint != "" && c.secret != "" && c.reporter.ID != ""
}

func (c *ReporterClient) AddEvent(event contract.Event) {
	if !c.Enabled() {
		return
	}
	if event.EventID == "" {
		event.EventID = uuid.NewString()
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now().UTC()
	}
	if event.Reporter.ID == "" {
		event.Reporter = c.reporter
	}

	// Snapshot reference-typed fields so later caller mutations of the meter
	// slice or attribute map cannot corrupt queued telemetry.
	if len(event.Meters) > 0 {
		meters := make([]contract.Meter, len(event.Meters))
		copy(meters, event.Meters)
		event.Meters = meters
	}
	if event.Attributes != nil {
		attrs := make(map[string]string, len(event.Attributes))
		for k, v := range event.Attributes {
			attrs[k] = v
		}
		event.Attributes = attrs
	}

	c.mu.Lock()
	c.current = append(c.current, event)
	c.mu.Unlock()
}

// Flush drains buffered events into batches and sends each one. Failed batches
// are logged and discarded; there is no retry queue.
func (c *ReporterClient) Flush(ctx context.Context) error {
	if !c.Enabled() {
		return nil
	}

	for _, batch := range c.drainBatches() {
		if err := c.sendBatch(ctx, batch); err != nil {
			slog.Warn("usage reporter dropped batch",
				"reporter_id", c.reporter.ID,
				"delivery_id", batch.DeliveryID,
				"events", len(batch.Events),
				"error", err,
			)
		}
	}
	return nil
}

func (c *ReporterClient) Stop(ctx context.Context) error {
	var err error
	c.stopOnce.Do(func() {
		close(c.quit)
		c.wg.Wait()
		err = c.Flush(ctx)
	})
	return err
}

func (c *ReporterClient) loop() {
	defer c.wg.Done()

	ticker := time.NewTicker(c.flushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			_ = c.Flush(context.Background())
		case <-c.quit:
			return
		}
	}
}

// drainBatches moves the current event buffer into one or more outbound
// batches sized at most maxBatchSize.
func (c *ReporterClient) drainBatches() []*contract.Batch {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.current) == 0 {
		return nil
	}

	batches := make([]*contract.Batch, 0, (len(c.current)+c.maxBatchSize-1)/c.maxBatchSize)
	for start := 0; start < len(c.current); start += c.maxBatchSize {
		end := start + c.maxBatchSize
		if end > len(c.current) {
			end = len(c.current)
		}
		events := make([]contract.Event, end-start)
		copy(events, c.current[start:end])
		batches = append(batches, &contract.Batch{
			DeliveryID: uuid.NewString(),
			Events:     events,
		})
	}
	c.current = nil
	return batches
}

func (c *ReporterClient) sendBatch(ctx context.Context, batch *contract.Batch) error {
	body, err := json.Marshal(batch)
	if err != nil {
		return fmt.Errorf("marshal usage batch: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create usage batch request: %w", err)
	}

	timestamp := time.Now().UTC().Format(time.RFC3339Nano)
	nonce := batch.DeliveryID
	signature := signing.Sign(http.MethodPost, req.URL.Path, c.reporter.ID, timestamp, nonce, body, c.secret)

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(contract.HeaderReporterID, c.reporter.ID)
	req.Header.Set(contract.HeaderTimestamp, timestamp)
	req.Header.Set(contract.HeaderNonce, nonce)
	req.Header.Set(contract.HeaderSignature, signature)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send usage batch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("usage batch rejected with status %d", resp.StatusCode)
	}

	return nil
}
