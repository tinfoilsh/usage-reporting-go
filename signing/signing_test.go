package signing

import "testing"

func TestVerify(t *testing.T) {
	body := []byte(`{"ok":true}`)
	sig := Sign("POST", "/api/internal/usage-reports", "router", "2026-04-16T00:00:00Z", "nonce", body, "secret")
	if !Verify("POST", "/api/internal/usage-reports", "router", "2026-04-16T00:00:00Z", "nonce", body, "secret", sig) {
		t.Fatal("expected signature to verify")
	}
	if Verify("POST", "/api/internal/usage-reports", "router", "2026-04-16T00:00:00Z", "nonce", body, "other", sig) {
		t.Fatal("expected signature verification to fail with wrong secret")
	}
}
