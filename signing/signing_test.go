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

// TestVerifyNormalizesRootPath guards against a regression where Go's
// net/http exposes root-path URLs with an empty Path while server frameworks
// such as fiber expose them as "/". Both must produce the same signature.
func TestVerifyNormalizesRootPath(t *testing.T) {
	body := []byte(`{"ok":true}`)
	args := []any{"POST", "router", "2026-04-16T00:00:00Z", "nonce", body, "secret"}

	clientSig := Sign("POST", "", "router", "2026-04-16T00:00:00Z", "nonce", body, "secret")
	if !Verify("POST", "/", "router", "2026-04-16T00:00:00Z", "nonce", body, "secret", clientSig) {
		t.Fatalf("empty-path signature should verify against server-seen path %q (args=%v)", "/", args)
	}

	serverSig := Sign("POST", "/", "router", "2026-04-16T00:00:00Z", "nonce", body, "secret")
	if !Verify("POST", "", "router", "2026-04-16T00:00:00Z", "nonce", body, "secret", serverSig) {
		t.Fatalf("server-path signature should verify against client-seen empty path")
	}
}
