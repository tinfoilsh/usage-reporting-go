package usagecontext

import (
	"net/http"
	"testing"
	"time"

	"github.com/tinfoilsh/usage-reporting-go/contract"
)

func TestSignAndVerify(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	ctx := Context{
		RootRequestID:       "request-1",
		ParentService:       contract.ServiceRouter,
		BillCustomerRequest: false,
		IssuedAt:            now,
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
	if got.BillCustomerRequest {
		t.Fatalf("bill_customer_request mismatch: got true want false")
	}
}

func TestVerifyRejectsTampering(t *testing.T) {
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	encoded, signature, err := Sign(Context{
		RootRequestID:       "request-1",
		BillCustomerRequest: true,
		IssuedAt:            now,
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
	header := make(http.Header)
	if err := SetHeaders(header, Context{
		RootRequestID:       "request-1",
		ParentService:       contract.ServiceRouter,
		BillCustomerRequest: false,
		IssuedAt:            now,
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
	if got.BillCustomerRequest {
		t.Fatalf("bill_customer_request mismatch: got true want false")
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
