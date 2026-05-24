package usagereporting

import "testing"

func TestVerifyBatch(t *testing.T) {
	body := []byte(`{"ok":true}`)
	sig := SignBatch("POST", "/api/internal/usage-reports", "router", "2026-04-16T00:00:00Z", "nonce", body, "secret")
	if !VerifyBatch("POST", "/api/internal/usage-reports", "router", "2026-04-16T00:00:00Z", "nonce", body, "secret", sig) {
		t.Fatal("expected signature to verify")
	}
	if VerifyBatch("POST", "/api/internal/usage-reports", "router", "2026-04-16T00:00:00Z", "nonce", body, "other", sig) {
		t.Fatal("expected signature verification to fail with wrong secret")
	}
}

// TestVerifyBatchNormalizesRootPath guards against a regression where Go's
// net/http exposes root-path URLs with an empty Path while server frameworks
// such as fiber expose them as "/". Both must produce the same signature.
func TestVerifyBatchNormalizesRootPath(t *testing.T) {
	body := []byte(`{"ok":true}`)
	args := []any{"POST", "router", "2026-04-16T00:00:00Z", "nonce", body, "secret"}

	clientSig := SignBatch("POST", "", "router", "2026-04-16T00:00:00Z", "nonce", body, "secret")
	if !VerifyBatch("POST", "/", "router", "2026-04-16T00:00:00Z", "nonce", body, "secret", clientSig) {
		t.Fatalf("empty-path signature should verify against server-seen path %q (args=%v)", "/", args)
	}

	serverSig := SignBatch("POST", "/", "router", "2026-04-16T00:00:00Z", "nonce", body, "secret")
	if !VerifyBatch("POST", "", "router", "2026-04-16T00:00:00Z", "nonce", body, "secret", serverSig) {
		t.Fatalf("server-path signature should verify against client-seen empty path")
	}
}
