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

	usagereporting "github.com/tinfoilsh/usage-reporting-go"
)

const (
	defaultFlushInterval     = 2 * time.Second
	defaultMaxBatchSize      = 1000
	defaultMaxBufferedEvents = 100_000
)

type Config struct {
	Endpoint      string
	ReporterID    string
	Secret        string
	FlushInterval time.Duration
	HTTPClient    *http.Client
	// MaxBatchSize caps the number of events rolled into a single outbound batch.
	// Defaults to 1000 when unset or non-positive.
	MaxBatchSize int
	// MaxBufferedEvents caps how many events may sit in the in-memory buffer
	// before AddEvent starts dropping the oldest event to make room for a new
	// one. Bounds memory growth when the controlplane is unavailable; once the
	// ceiling is reached, the reporter degrades to "newest events win" rather
	// than growing without limit. Defaults to 100_000 when unset or non-positive.
	MaxBufferedEvents int
}

// ReporterClient batches usage events and delivers them to the controlplane
// over signed HTTP. Delivery is fire-and-forget: if a batch fails, it is
// logged and dropped. Callers that need durable delivery must layer that on
// top of this client.
type ReporterClient struct {
	endpoint          string
	reporterID        string
	secret            string
	flushInterval     time.Duration
	httpClient        *http.Client
	maxBatchSize      int
	maxBufferedEvents int

	mu sync.Mutex
	// ring is a fixed-capacity ring buffer holding queued events. head points
	// at the oldest event, size tracks how many slots are populated. A full
	// buffer drops the oldest event in O(1) by advancing head.
	ring     []usagereporting.Event
	head     int
	size     int
	dropped  uint64
	notified uint64

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

	maxBufferedEvents := cfg.MaxBufferedEvents
	if maxBufferedEvents <= 0 {
		maxBufferedEvents = defaultMaxBufferedEvents
	}

	c := &ReporterClient{
		endpoint:          strings.TrimRight(cfg.Endpoint, "/"),
		reporterID:        cfg.ReporterID,
		secret:            cfg.Secret,
		flushInterval:     interval,
		httpClient:        httpClient,
		maxBatchSize:      maxBatchSize,
		maxBufferedEvents: maxBufferedEvents,
		ring:              make([]usagereporting.Event, maxBufferedEvents),
		quit:              make(chan struct{}),
	}

	if c.endpoint != "" && c.secret != "" && c.reporterID != "" {
		c.wg.Add(1)
		go c.loop()
	}

	return c
}

func (c *ReporterClient) Enabled() bool {
	return c.endpoint != "" && c.secret != "" && c.reporterID != ""
}

func (c *ReporterClient) AddEvent(event usagereporting.Event) {
	if !c.Enabled() {
		return
	}
	if event.EventID == "" {
		event.EventID = uuid.NewString()
	}
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now().UTC()
	}

	// Snapshot reference-typed fields so later caller mutations of the meter
	// slice or attribute map cannot corrupt queued telemetry.
	if len(event.Meters) > 0 {
		meters := make([]usagereporting.Meter, len(event.Meters))
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
	ringCap := len(c.ring)
	if c.size < ringCap {
		c.ring[(c.head+c.size)%ringCap] = event
		c.size++
		c.mu.Unlock()
		return
	}
	// Buffer is full: overwrite the oldest slot and advance head. Fire-and-
	// forget telemetry favours freshness over completeness under backpressure.
	c.ring[c.head] = event
	c.head = (c.head + 1) % ringCap
	c.dropped++
	dropped := c.dropped
	shouldLog := dropped == 1 || dropped-c.notified >= 1000
	if shouldLog {
		c.notified = dropped
	}
	c.mu.Unlock()
	if shouldLog {
		slog.Warn("usage reporter buffer full; dropped oldest event",
			"reporter_id", c.reporterID,
			"max_buffered_events", c.maxBufferedEvents,
			"total_dropped", dropped,
		)
	}
}

// Flush drains buffered events into batches and sends each one. Failed batches
// are logged and discarded; there is no retry queue, so callers cannot react
// to delivery failures.
func (c *ReporterClient) Flush(ctx context.Context) {
	if !c.Enabled() {
		return
	}

	for _, batch := range c.drainBatches() {
		if err := c.sendBatch(ctx, batch); err != nil {
			slog.Warn("usage reporter dropped batch",
				"reporter_id", c.reporterID,
				"delivery_id", batch.DeliveryID,
				"events", len(batch.Events),
				"error", err,
			)
		}
	}
}

func (c *ReporterClient) Stop(ctx context.Context) {
	c.stopOnce.Do(func() {
		close(c.quit)
		c.wg.Wait()
		c.Flush(ctx)
	})
}

func (c *ReporterClient) loop() {
	defer c.wg.Done()

	ticker := time.NewTicker(c.flushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.Flush(context.Background())
		case <-c.quit:
			return
		}
	}
}

// drainBatches moves queued events into one or more outbound batches sized at
// most maxBatchSize, reading them out of the ring buffer in FIFO order.
func (c *ReporterClient) drainBatches() []*usagereporting.Batch {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.size == 0 {
		return nil
	}

	ringCap := len(c.ring)
	total := c.size
	batches := make([]*usagereporting.Batch, 0, (total+c.maxBatchSize-1)/c.maxBatchSize)
	for emitted := 0; emitted < total; {
		remaining := total - emitted
		batchSize := c.maxBatchSize
		if remaining < batchSize {
			batchSize = remaining
		}
		events := make([]usagereporting.Event, batchSize)
		for i := 0; i < batchSize; i++ {
			idx := (c.head + emitted + i) % ringCap
			events[i] = c.ring[idx]
			// Release the slot so any references it holds can be GC'd while
			// the batch is in flight.
			c.ring[idx] = usagereporting.Event{}
		}
		batches = append(batches, &usagereporting.Batch{
			DeliveryID: uuid.NewString(),
			Events:     events,
		})
		emitted += batchSize
	}
	c.head = 0
	c.size = 0
	return batches
}

func (c *ReporterClient) sendBatch(ctx context.Context, batch *usagereporting.Batch) error {
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
	signature := usagereporting.SignBatch(http.MethodPost, req.URL.Path, c.reporterID, timestamp, nonce, body, c.secret)

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(usagereporting.HeaderReporterID, c.reporterID)
	req.Header.Set(usagereporting.HeaderTimestamp, timestamp)
	req.Header.Set(usagereporting.HeaderNonce, nonce)
	req.Header.Set(usagereporting.HeaderSignature, signature)

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
