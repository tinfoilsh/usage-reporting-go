package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	usagereporting "github.com/tinfoilsh/usage-reporting-go"
)

func TestStatsConcurrentSnapshots(t *testing.T) {
	const (
		writers        = 4
		perWriter      = 1024
		bufferCapacity = 16
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	c := New(Config{
		Endpoint:          server.URL,
		ReporterID:        "reporter",
		Secret:            "secret",
		FlushInterval:     time.Hour,
		MaxBatchSize:      1,
		MaxBufferedEvents: bufferCapacity,
	})
	defer c.Stop(context.Background())

	done := make(chan struct{})
	observed := make(chan error, 1)
	go func() {
		var previous Stats
		for {
			s := c.Stats()
			if s.DeliveredEvents != s.DeliveredBatches || s.FailedEvents != s.FailedBatches ||
				s.DeliveredEvents+s.FailedEvents+s.DroppedBufferFull > s.Enqueued {
				observed <- fmt.Errorf("inconsistent snapshot: %+v", s)
				return
			}
			if s.Enqueued < previous.Enqueued || s.DeliveredEvents < previous.DeliveredEvents ||
				s.DeliveredBatches < previous.DeliveredBatches || s.DroppedBufferFull < previous.DroppedBufferFull {
				observed <- fmt.Errorf("counters decreased: previous=%+v current=%+v", previous, s)
				return
			}
			previous = s
			select {
			case <-done:
				observed <- nil
				return
			default:
				runtime.Gosched()
			}
		}
	}()

	var wg sync.WaitGroup
	for writer := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perWriter {
				c.AddEvent(usagereporting.Event{EventID: fmt.Sprintf("%d-%d", writer, i)})
				c.Flush(context.Background())
			}
		}()
	}
	wg.Wait()
	c.Stop(context.Background())
	close(done)
	if err := <-observed; err != nil {
		t.Error(err)
	}
	s := c.Stats()
	if s.Enqueued != writers*perWriter || s.DeliveredEvents+s.DroppedBufferFull != s.Enqueued ||
		s.DeliveredEvents != s.DeliveredBatches || s.FailedEvents != 0 || s.FailedBatches != 0 || s.DroppedDisabled != 0 {
		t.Fatalf("final accounting mismatch: %+v", s)
	}
}

func TestStatsBatchOutcomes(t *testing.T) {
	const (
		eventCount           = 5
		bufferCapacity       = 3
		batchSize            = 2
		batchCount           = 2
		testSecret           = "test-secret"
		invalidTimestampYear = 10000
		lastSuccessStatus    = 299
	)
	for _, tc := range []struct {
		name             string
		status           int
		endpoint         string
		invalidTimestamp bool
		canceled         bool
		closedServer     bool
	}{
		{name: "ok", status: http.StatusOK},
		{name: "no content", status: http.StatusNoContent},
		{name: "last success status", status: lastSuccessStatus},
		{name: "redirect", status: http.StatusMultipleChoices},
		{name: "unauthorized", status: http.StatusUnauthorized},
		{name: "server error", status: http.StatusInternalServerError},
		{name: "marshal error", invalidTimestamp: true},
		{name: "invalid endpoint", endpoint: ":"},
		{name: "canceled context", canceled: true},
		{name: "transport error", closedServer: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var received []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				reporterID, timestamp, nonce, signature, err := usagereporting.HeaderValues(r.Header)
				if err != nil || !usagereporting.VerifyBatch(r.Method, r.URL.Path, reporterID, timestamp, nonce, body, testSecret, signature) {
					t.Error("batch signature did not verify")
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				var batch usagereporting.Batch
				if err := json.Unmarshal(body, &batch); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if len(batch.Events) > batchSize || batch.DeliveryID != nonce {
					t.Errorf("invalid batch: %+v", batch)
				}
				for _, event := range batch.Events {
					received = append(received, event.EventID)
				}
				status := tc.status
				if status == 0 {
					status = http.StatusOK
				}
				w.WriteHeader(status)
			}))
			defer server.Close()
			endpoint := server.URL
			if tc.endpoint != "" {
				endpoint = tc.endpoint
			}
			if tc.closedServer {
				server.Close()
			}
			c := New(Config{
				Endpoint:          endpoint,
				ReporterID:        "reporter",
				Secret:            testSecret,
				FlushInterval:     time.Hour,
				MaxBufferedEvents: bufferCapacity,
				MaxBatchSize:      batchSize,
			})
			defer c.Stop(context.Background())
			if s := c.Stats(); s != (Stats{}) {
				t.Fatalf("initial stats: %+v", s)
			}
			for i := range eventCount {
				event := usagereporting.Event{EventID: fmt.Sprintf("event-%d", i)}
				if tc.invalidTimestamp {
					event.OccurredAt = time.Date(invalidTimestampYear, time.January, 1, 0, 0, 0, 0, time.UTC)
				}
				c.AddEvent(event)
			}
			want := Stats{Enqueued: eventCount, DroppedBufferFull: eventCount - bufferCapacity}
			if s := c.Stats(); s != want {
				t.Fatalf("buffered stats: got %+v want %+v", s, want)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.canceled {
				cancel()
			}
			c.Flush(ctx)
			if tc.status >= http.StatusOK && tc.status <= lastSuccessStatus {
				want.DeliveredEvents = bufferCapacity
				want.DeliveredBatches = batchCount
			} else {
				want.FailedEvents = bufferCapacity
				want.FailedBatches = batchCount
			}
			if s := c.Stats(); s != want {
				t.Fatalf("flushed stats: got %+v want %+v", s, want)
			}
			c.Flush(context.Background())
			c.Stop(context.Background())
			c.Stop(context.Background())
			if s := c.Stats(); s != want {
				t.Fatalf("empty flush or repeated stop changed stats: got %+v want %+v", s, want)
			}
			server.Close()
			if tc.status != 0 {
				wantIDs := []string{"event-2", "event-3", "event-4"}
				if !slices.Equal(received, wantIDs) {
					t.Fatalf("received events: got %v want %v", received, wantIDs)
				}
			} else if len(received) != 0 {
				t.Fatalf("unexpected delivery: %v", received)
			}
		})
	}
}

func TestStatsWhileDeliveryInFlight(t *testing.T) {
	const progressTimeout = 5 * time.Second
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() { close(entered) })
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	c := New(Config{Endpoint: server.URL, ReporterID: "reporter", Secret: "secret", FlushInterval: time.Hour})
	defer c.Stop(context.Background())
	c.AddEvent(usagereporting.Event{})
	flushed := make(chan struct{})
	go func() {
		c.Flush(context.Background())
		close(flushed)
	}()
	defer func() {
		close(release)
		select {
		case <-flushed:
		case <-time.After(progressTimeout):
			t.Error("flush did not finish")
		}
	}()
	select {
	case <-entered:
	case <-time.After(progressTimeout):
		t.Fatal("delivery did not start")
	}
	progress := make(chan Stats, 1)
	go func() {
		c.AddEvent(usagereporting.Event{})
		progress <- c.Stats()
	}()
	select {
	case s := <-progress:
		if s != (Stats{Enqueued: 2}) {
			t.Fatalf("in-flight stats: %+v", s)
		}
	case <-time.After(progressTimeout):
		t.Fatal("buffer and stats blocked on network I/O")
	}
}
