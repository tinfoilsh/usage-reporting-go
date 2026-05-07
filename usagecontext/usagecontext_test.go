package usagecontext

import (
	"net/http"
	"testing"
	"time"
)

func TestSignAndVerify(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	count := int64(0)
	ctx := Context{
		RootRequestID:        "request-1",
		ParentService:        "router",
		CustomerRequestCount: &count,
		IssuedAt:             now,
	}

	encoded, signature, err := Sign(ctx, "secret")
	if err != nil {
		t.Fatalf("sign usage context: %v", err)
	}

	got, err := Verify(encoded, signature, "secret", now, time.Minute)
	if err != nil {
		t.Fatalf("verify usage context: %v", err)
	}
	if got.RootRequestID != ctx.RootRequestID {
		t.Fatalf("root request id mismatch: got %q want %q", got.RootRequestID, ctx.RootRequestID)
	}
	if got.ParentService != ctx.ParentService {
		t.Fatalf("parent service mismatch: got %q want %q", got.ParentService, ctx.ParentService)
	}
	if got.CustomerRequestCount == nil || *got.CustomerRequestCount != 0 {
		t.Fatalf("customer request count mismatch: got %+v want pointer to 0", got.CustomerRequestCount)
	}
}

func TestVerifyRejectsTampering(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	count := int64(1)
	encoded, signature, err := Sign(Context{
		RootRequestID:        "request-1",
		CustomerRequestCount: &count,
		IssuedAt:             now,
	}, "secret")
	if err != nil {
		t.Fatalf("sign usage context: %v", err)
	}

	if _, err := Verify(encoded+"a", signature, "secret", now, time.Minute); err == nil {
		t.Fatal("expected tampered context to fail verification")
	}
	if _, err := Verify(encoded, signature, "wrong-secret", now, time.Minute); err == nil {
		t.Fatal("expected wrong secret to fail verification")
	}
}

func TestVerifyRejectsStaleContext(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	encoded, signature, err := Sign(Context{
		RootRequestID: "request-1",
		IssuedAt:      now.Add(-2 * time.Minute),
	}, "secret")
	if err != nil {
		t.Fatalf("sign usage context: %v", err)
	}

	if _, err := Verify(encoded, signature, "secret", now, time.Minute); err == nil {
		t.Fatal("expected stale context to fail verification")
	}
}

func TestHeadersRoundTrip(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	count := int64(0)
	header := make(http.Header)
	if err := SetHeaders(header, Context{
		RootRequestID:        "request-1",
		ParentService:        "router",
		CustomerRequestCount: &count,
		IssuedAt:             now,
	}, "secret"); err != nil {
		t.Fatalf("set usage context headers: %v", err)
	}

	got, ok, err := FromHeaders(header, "secret", now, time.Minute)
	if err != nil {
		t.Fatalf("read usage context headers: %v", err)
	}
	if !ok {
		t.Fatal("expected usage context headers to be present")
	}
	if got.CustomerRequestCount == nil || *got.CustomerRequestCount != 0 {
		t.Fatalf("customer request count mismatch: got %+v want pointer to 0", got.CustomerRequestCount)
	}
}

func TestFromHeadersMissingContext(t *testing.T) {
	_, ok, err := FromHeaders(http.Header{}, "secret", time.Now(), time.Minute)
	if err != nil {
		t.Fatalf("missing headers should not error: %v", err)
	}
	if ok {
		t.Fatal("missing headers should report ok=false")
	}
}
