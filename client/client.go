package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/tinfoilsh/usage-reporting-go/contract"
	"github.com/tinfoilsh/usage-reporting-go/signing"
)

const defaultFlushInterval = 2 * time.Second

type Config struct {
	Endpoint      string
	Reporter      contract.Reporter
	Secret        string
	FlushInterval time.Duration
	HTTPClient    *http.Client
}

type ReporterClient struct {
	endpoint      string
	reporter      contract.Reporter
	secret        string
	flushInterval time.Duration
	httpClient    *http.Client

	mu      sync.Mutex
	current []contract.Event
	pending []*contract.Batch

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
		httpClient = &http.Client{Timeout: 10 * time.Second}
	}

	c := &ReporterClient{
		endpoint:      strings.TrimRight(cfg.Endpoint, "/"),
		reporter:      cfg.Reporter,
		secret:        cfg.Secret,
		flushInterval: interval,
		httpClient:    httpClient,
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

	c.mu.Lock()
	c.current = append(c.current, event)
	c.mu.Unlock()
}

func (c *ReporterClient) Flush(ctx context.Context) error {
	if !c.Enabled() {
		return nil
	}

	c.rollCurrentBatch()

	for {
		batch := c.nextPending()
		if batch == nil {
			return nil
		}
		if err := c.sendBatch(ctx, batch); err != nil {
			return err
		}
		c.popPending(batch.DeliveryID)
	}
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

func (c *ReporterClient) rollCurrentBatch() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.current) == 0 {
		return
	}
	events := make([]contract.Event, len(c.current))
	copy(events, c.current)
	c.pending = append(c.pending, &contract.Batch{
		DeliveryID: uuid.NewString(),
		Events:     events,
	})
	c.current = nil
}

func (c *ReporterClient) nextPending() *contract.Batch {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) == 0 {
		return nil
	}
	return c.pending[0]
}

func (c *ReporterClient) popPending(deliveryID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) == 0 {
		return
	}
	if c.pending[0].DeliveryID == deliveryID {
		c.pending = c.pending[1:]
	}
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
