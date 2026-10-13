package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	usagereporting "github.com/tinfoilsh/usage-reporting-go"
)

const (
	defaultFlushInterval       = 2 * time.Second
	defaultMaxBatchSize        = 1000
	defaultMaxBufferedEvents   = 100_000
	defaultMaxDeliveryAttempts = 6
	defaultRetryInitialBackoff = time.Second
	defaultRetryMaxBackoff     = 10 * time.Second
	defaultRetryMaxElapsed     = 30 * time.Second
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
	// MaxDeliveryAttempts caps how many times one batch is sent, counting the
	// first attempt. Transport errors, HTTP 429, and HTTP 5xx are retried with
	// the same delivery ID and events, so a receiver that de-duplicates on
	// (reporter ID, delivery ID) never applies a batch twice; any other non-2xx
	// response is final. Defaults to 6 when unset or non-positive; 1 disables
	// retries.
	MaxDeliveryAttempts int
	// RetryInitialBackoff is the wait before a batch's first retry. It doubles
	// for each further retry up to RetryMaxBackoff; every wait is drawn
	// uniformly from [d/2, d] and is never shorter than a Retry-After header
	// asks for. They default to 1s and 10s when unset or non-positive.
	RetryInitialBackoff time.Duration
	RetryMaxBackoff     time.Duration
	// RetryMaxElapsed bounds how long one batch is retried: no retry starts
	// more than this long after the batch's first attempt, so a failing
	// controlplane holds up a flush for at most this long plus one request
	// timeout. A batch still failing when its attempts or time run out is
	// dropped. Defaults to 30s when unset or non-positive.
	RetryMaxElapsed time.Duration
}

// ReporterClient batches usage events and delivers them to the controlplane
// over signed HTTP. Transient delivery failures are retried for a bounded time
// with the batch's delivery ID unchanged; a batch that is rejected or runs out
// of retries is logged and dropped. Events live only in memory, so callers
// that need durable delivery must layer that on top of this client.
type ReporterClient struct {
	endpoint            string
	reporterID          string
	secret              string
	flushInterval       time.Duration
	httpClient          *http.Client
	maxBatchSize        int
	maxBufferedEvents   int
	maxDeliveryAttempts int
	retryInitialBackoff time.Duration
	retryMaxBackoff     time.Duration
	retryMaxElapsed     time.Duration

	mu sync.Mutex
	// ring is a fixed-capacity ring buffer holding queued events. head points
	// at the oldest event, size tracks how many slots are populated. A full
	// buffer drops the oldest event in O(1) by advancing head.
	ring     []usagereporting.Event
	head     int
	size     int
	dropped  uint64
	notified uint64
	// deliveryFailing is set when a batch is dropped after a transient
	// failure and cleared by the next delivery. Stop skips retries while it
	// is set.
	deliveryFailing bool

	// cancelLoop cancels the context the background loop delivers under.
	cancelLoop context.CancelFunc
	quit       chan struct{}
	stopOnce   sync.Once
	wg         sync.WaitGroup

	stats reporterCounters // Guarded by mu along with the buffer.
}

// reporterCounters holds monotonic event-accounting counters. They exist so a
// caller can export end-to-end delivery accounting (e.g. as Prometheus
// counters): every event handed to AddEvent lands in exactly one of
// enqueued/droppedDisabled, and every enqueued event eventually lands in
// exactly one of deliveredEvents/failedEvents/droppedBufferFull (or remains
// buffered or in flight).
type reporterCounters struct {
	enqueued                   uint64
	droppedDisabled            uint64
	droppedBufferFull          uint64
	deliveredEvents            uint64
	deliveredBatches           uint64
	deliveredAfterRetryEvents  uint64
	deliveredAfterRetryBatches uint64
	failedEvents               uint64
	failedBatches              uint64
	retries                    uint64
}

// Stats is a point-in-time snapshot of the reporter's monotonic accounting
// counters.
type Stats struct {
	// Enqueued counts events accepted into the buffer.
	Enqueued uint64
	// DroppedDisabled counts events discarded because the reporter was not
	// configured (missing endpoint, secret, or reporter ID).
	DroppedDisabled uint64
	// DroppedBufferFull counts oldest events overwritten on buffer overflow.
	DroppedBufferFull uint64
	// DeliveredEvents / DeliveredBatches count events and batches acknowledged
	// with an HTTP 2xx, including those that needed retries.
	DeliveredEvents  uint64
	DeliveredBatches uint64
	// DeliveredAfterRetryEvents / DeliveredAfterRetryBatches count the subset
	// of delivered events and batches acknowledged only after at least one
	// retry.
	DeliveredAfterRetryEvents  uint64
	DeliveredAfterRetryBatches uint64
	// FailedEvents / FailedBatches count events and batches permanently
	// discarded: rejected with a non-retryable response, or still failing
	// when their retries ran out.
	FailedEvents  uint64
	FailedBatches uint64
	// Retries counts delivery attempts made after a batch's first.
	Retries uint64
}

// Stats returns a snapshot of the reporter's delivery accounting.
func (c *ReporterClient) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Stats{
		Enqueued:                   c.stats.enqueued,
		DroppedDisabled:            c.stats.droppedDisabled,
		DroppedBufferFull:          c.stats.droppedBufferFull,
		DeliveredEvents:            c.stats.deliveredEvents,
		DeliveredBatches:           c.stats.deliveredBatches,
		DeliveredAfterRetryEvents:  c.stats.deliveredAfterRetryEvents,
		DeliveredAfterRetryBatches: c.stats.deliveredAfterRetryBatches,
		FailedEvents:               c.stats.failedEvents,
		FailedBatches:              c.stats.failedBatches,
		Retries:                    c.stats.retries,
	}
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

	maxDeliveryAttempts := cfg.MaxDeliveryAttempts
	if maxDeliveryAttempts <= 0 {
		maxDeliveryAttempts = defaultMaxDeliveryAttempts
	}

	retryInitialBackoff := cfg.RetryInitialBackoff
	if retryInitialBackoff <= 0 {
		retryInitialBackoff = defaultRetryInitialBackoff
	}

	retryMaxBackoff := cfg.RetryMaxBackoff
	if retryMaxBackoff <= 0 {
		retryMaxBackoff = defaultRetryMaxBackoff
	}
	retryMaxBackoff = max(retryMaxBackoff, retryInitialBackoff)

	retryMaxElapsed := cfg.RetryMaxElapsed
	if retryMaxElapsed <= 0 {
		retryMaxElapsed = defaultRetryMaxElapsed
	}

	c := &ReporterClient{
		endpoint:            strings.TrimRight(cfg.Endpoint, "/"),
		reporterID:          cfg.ReporterID,
		secret:              cfg.Secret,
		flushInterval:       interval,
		httpClient:          httpClient,
		maxBatchSize:        maxBatchSize,
		maxBufferedEvents:   maxBufferedEvents,
		maxDeliveryAttempts: maxDeliveryAttempts,
		retryInitialBackoff: retryInitialBackoff,
		retryMaxBackoff:     retryMaxBackoff,
		retryMaxElapsed:     retryMaxElapsed,
		ring:                make([]usagereporting.Event, maxBufferedEvents),
		quit:                make(chan struct{}),
	}

	loopCtx, cancelLoop := context.WithCancel(context.Background())
	c.cancelLoop = cancelLoop
	if c.endpoint != "" && c.secret != "" && c.reporterID != "" {
		c.wg.Add(1)
		go c.loop(loopCtx)
	}

	return c
}

func (c *ReporterClient) Enabled() bool {
	return c.endpoint != "" && c.secret != "" && c.reporterID != ""
}

func (c *ReporterClient) AddEvent(event usagereporting.Event) {
	if !c.Enabled() {
		c.mu.Lock()
		c.stats.droppedDisabled++
		c.mu.Unlock()
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
	c.stats.enqueued++
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
	c.stats.droppedBufferFull++
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

// Flush sends buffered events in batches, oldest first, retrying transient
// failures as configured. It returns early, leaving the remaining events
// buffered for the next flush, when ctx is done or when a batch is dropped
// after its retries run out, so one call waits out at most one retry window
// on a failing controlplane. Batches rejected with a non-retryable response
// are logged and discarded.
func (c *ReporterClient) Flush(ctx context.Context) {
	if !c.Enabled() {
		return
	}

	// Drain one batch at a time so events waiting behind an in-flight batch
	// stay in the ring, where MaxBufferedEvents bounds them. Stopping at the
	// count buffered on entry keeps a steady stream of new events from
	// extending the flush indefinitely.
	c.mu.Lock()
	remaining := c.size
	c.mu.Unlock()
	for remaining > 0 && ctx.Err() == nil {
		batch := c.drainBatch(remaining)
		if batch == nil {
			return
		}
		remaining -= len(batch.Events)
		if !c.deliver(ctx, batch, c.maxDeliveryAttempts) {
			return
		}
	}
}

// Stop ends the background flush loop and makes a final attempt to deliver
// everything still buffered. ctx bounds the whole shutdown, including a
// delivery the loop is in the middle of retrying. Without a deadline, Stop
// waits out at most one retry window on a failing controlplane: after one
// batch is dropped with its retries exhausted, the rest get a single attempt
// each. Batches that cannot be delivered count as failed.
func (c *ReporterClient) Stop(ctx context.Context) {
	c.stopOnce.Do(func() {
		stopCancelLoop := context.AfterFunc(ctx, c.cancelLoop)
		close(c.quit)
		c.wg.Wait()
		stopCancelLoop()
		c.cancelLoop()
		c.flushFinal(ctx)
	})
}

func (c *ReporterClient) loop(ctx context.Context) {
	defer c.wg.Done()

	ticker := time.NewTicker(c.flushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.Flush(ctx)
		case <-c.quit:
			return
		}
	}
}

// flushFinal sends everything still buffered once the loop has exited. Unlike
// Flush it never leaves events behind: once delivery is known to be failing,
// here or in the loop's last flush, remaining batches get one attempt each.
func (c *ReporterClient) flushFinal(ctx context.Context) {
	c.mu.Lock()
	remaining := c.size
	attempts := c.maxDeliveryAttempts
	if c.deliveryFailing {
		attempts = 1
	}
	c.mu.Unlock()
	for remaining > 0 {
		batch := c.drainBatch(remaining)
		if batch == nil {
			return
		}
		remaining -= len(batch.Events)
		if !c.deliver(ctx, batch, attempts) {
			attempts = 1
		}
	}
}

// drainBatch moves the oldest queued events, at most limit and at most
// maxBatchSize of them, out of the ring buffer into a new outbound batch. It
// returns nil when nothing is queued.
func (c *ReporterClient) drainBatch(limit int) *usagereporting.Batch {
	c.mu.Lock()
	defer c.mu.Unlock()
	batchSize := min(c.size, c.maxBatchSize, limit)
	if batchSize <= 0 {
		return nil
	}

	ringCap := len(c.ring)
	events := make([]usagereporting.Event, batchSize)
	for i := range batchSize {
		idx := (c.head + i) % ringCap
		events[i] = c.ring[idx]
		// Release the slot so any references it holds can be GC'd while
		// the batch is in flight.
		c.ring[idx] = usagereporting.Event{}
	}
	c.head = (c.head + batchSize) % ringCap
	c.size -= batchSize
	return &usagereporting.Batch{
		DeliveryID: uuid.NewString(),
		Events:     events,
	}
}

// deliver sends batch until it is acknowledged, rejected, or out of retries.
// Every attempt carries the same delivery ID and body, so a receiver that
// de-duplicates on delivery ID acknowledges a batch it already committed
// instead of billing it twice. It reports false when the batch was dropped
// after a transient failure, meaning delivery is currently failing.
func (c *ReporterClient) deliver(ctx context.Context, batch *usagereporting.Batch, maxAttempts int) bool {
	body, err := json.Marshal(batch)
	if err != nil {
		c.recordFailed(batch, 0, &sendFailure{err: fmt.Errorf("marshal usage batch: %w", err)})
		return true
	}

	start := time.Now()
	for attempt := 1; ; attempt++ {
		failure := c.send(ctx, batch.DeliveryID, body)
		if failure == nil {
			c.recordDelivered(batch, attempt)
			return true
		}
		if !failure.transient {
			c.recordFailed(batch, attempt, failure)
			return true
		}
		if !c.awaitRetry(ctx, start, attempt, maxAttempts, failure.retryAfter) {
			c.recordFailed(batch, attempt, failure)
			return false
		}
		c.mu.Lock()
		c.stats.retries++
		c.mu.Unlock()
	}
}

// awaitRetry waits out the backoff after a batch's attempt-th attempt failed
// and reports whether to try again. It returns false without waiting when the
// attempt limit is reached or the wait would end past the retry window, and
// false as soon as ctx is done.
func (c *ReporterClient) awaitRetry(ctx context.Context, start time.Time, attempt, maxAttempts int, retryAfter time.Duration) bool {
	if attempt >= maxAttempts || ctx.Err() != nil {
		return false
	}
	delay := max(c.backoff(attempt), retryAfter)
	if delay > c.retryMaxElapsed-time.Since(start) {
		return false
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// backoff returns the wait before retry n (counting from 1): the initial
// backoff doubled for each earlier retry and capped, then drawn from [d/2, d]
// so reporters that failed together do not retry in lockstep.
func (c *ReporterClient) backoff(n int) time.Duration {
	d := c.retryInitialBackoff
	for i := 1; i < n && d < c.retryMaxBackoff; i++ {
		if d > c.retryMaxBackoff/2 {
			d = c.retryMaxBackoff
		} else {
			d *= 2
		}
	}
	return d/2 + rand.N(d/2+1)
}

func (c *ReporterClient) recordDelivered(batch *usagereporting.Batch, attempts int) {
	events := uint64(len(batch.Events))
	c.mu.Lock()
	c.deliveryFailing = false
	c.stats.deliveredEvents += events
	c.stats.deliveredBatches++
	if attempts > 1 {
		c.stats.deliveredAfterRetryEvents += events
		c.stats.deliveredAfterRetryBatches++
	}
	c.mu.Unlock()
	if attempts > 1 {
		slog.Info("usage reporter delivered batch after retry",
			"reporter_id", c.reporterID,
			"delivery_id", batch.DeliveryID,
			"events", len(batch.Events),
			"attempts", attempts,
		)
	}
}

func (c *ReporterClient) recordFailed(batch *usagereporting.Batch, attempts int, failure *sendFailure) {
	c.mu.Lock()
	if failure.transient {
		c.deliveryFailing = true
	}
	c.stats.failedEvents += uint64(len(batch.Events))
	c.stats.failedBatches++
	c.mu.Unlock()
	slog.Warn("usage reporter dropped batch",
		"reporter_id", c.reporterID,
		"delivery_id", batch.DeliveryID,
		"events", len(batch.Events),
		"attempts", attempts,
		"status", failure.status,
		"error", failure.err,
	)
}

// sendFailure describes a failed delivery attempt.
type sendFailure struct {
	err        error
	status     int           // HTTP status, or 0 when no response arrived
	transient  bool          // a later attempt may succeed
	retryAfter time.Duration // minimum wait the receiver asked for
}

// send makes one signed delivery attempt. It returns nil once the receiver
// acknowledges the batch with a 2xx.
func (c *ReporterClient) send(ctx context.Context, deliveryID string, body []byte) *sendFailure {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return &sendFailure{err: fmt.Errorf("create usage batch request: %w", err)}
	}

	// Sign every attempt afresh: the signature covers the timestamp, and the
	// receiver rejects timestamps outside its skew window.
	timestamp := time.Now().UTC().Format(time.RFC3339Nano)
	signature := usagereporting.SignBatch(http.MethodPost, req.URL.Path, c.reporterID, timestamp, deliveryID, body, c.secret)

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(usagereporting.HeaderReporterID, c.reporterID)
	req.Header.Set(usagereporting.HeaderTimestamp, timestamp)
	req.Header.Set(usagereporting.HeaderNonce, deliveryID)
	req.Header.Set(usagereporting.HeaderSignature, signature)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return &sendFailure{err: fmt.Errorf("send usage batch: %w", err), transient: true}
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	failure := &sendFailure{
		err:    fmt.Errorf("usage batch rejected with status %d", resp.StatusCode),
		status: resp.StatusCode,
	}
	// 429 and 5xx mean the receiver could not take the batch right now. Any
	// other status (bad signature, malformed or oversized body, a blocked
	// redirect) would fail the same way on every attempt.
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode/100 == 5 {
		failure.transient = true
		failure.retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
	}
	return failure
}

// parseRetryAfter reads a Retry-After value given in seconds or as an HTTP
// date. It returns 0 when the value is absent or unparseable.
func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		return time.Duration(min(max(seconds, 0), int64(math.MaxInt64/time.Second))) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		return max(at.Sub(now), 0)
	}
	return 0
}
