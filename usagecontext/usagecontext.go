package usagecontext

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	HeaderContext   = "X-Tinfoil-Usage-Context"
	HeaderSignature = "X-Tinfoil-Usage-Context-Signature"

	signatureDomain = "tinfoil-usage-context-v1:"
)

// Context describes the customer-facing request a downstream service is being
// invoked inside of. BillCustomerRequest tells the downstream whether to count
// the call as its own customer-billable request: parents that have already
// counted the request set it false; direct customer-facing callers set it true.
type Context struct {
	ContextID           string    `json:"context_id,omitempty"`
	RootRequestID       string    `json:"root_request_id,omitempty"`
	ParentService       string    `json:"parent_service,omitempty"`
	APIKeyHash          string    `json:"api_key_hash,omitempty"`
	Depth               int       `json:"depth,omitempty"`
	BillCustomerRequest bool      `json:"bill_customer_request,omitempty"`
	IssuedAt            time.Time `json:"issued_at"`
}

func Sign(ctx Context, secret string) (encoded, signature string, err error) {
	if strings.TrimSpace(secret) == "" {
		return "", "", fmt.Errorf("usage context secret is empty")
	}
	body, err := json.Marshal(ctx)
	if err != nil {
		return "", "", fmt.Errorf("marshal usage context: %w", err)
	}
	encoded = base64.RawURLEncoding.EncodeToString(body)
	return encoded, signEncoded(encoded, secret), nil
}

func Verify(encoded, signature, secret string, now time.Time, maxSkew time.Duration) (Context, error) {
	if strings.TrimSpace(secret) == "" {
		return Context{}, fmt.Errorf("usage context secret is empty")
	}
	encoded = strings.TrimSpace(encoded)
	signature = strings.TrimSpace(signature)
	if encoded == "" || signature == "" {
		return Context{}, fmt.Errorf("usage context and signature are required")
	}
	expected := signEncoded(encoded, secret)
	if subtle.ConstantTimeCompare([]byte(expected), []byte(signature)) != 1 {
		return Context{}, fmt.Errorf("invalid usage context signature")
	}

	body, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return Context{}, fmt.Errorf("decode usage context: %w", err)
	}
	var ctx Context
	if err := json.Unmarshal(body, &ctx); err != nil {
		return Context{}, fmt.Errorf("unmarshal usage context: %w", err)
	}
	if err := validateIssuedAt(ctx.IssuedAt, now, maxSkew); err != nil {
		return Context{}, err
	}
	return ctx, nil
}

func SetHeaders(header http.Header, ctx Context, secret string) error {
	encoded, signature, err := Sign(ctx, secret)
	if err != nil {
		return err
	}
	header.Set(HeaderContext, encoded)
	header.Set(HeaderSignature, signature)
	return nil
}

func FromHeaders(header http.Header, secret string, now time.Time, maxSkew time.Duration) (Context, bool, error) {
	encoded := strings.TrimSpace(header.Get(HeaderContext))
	signature := strings.TrimSpace(header.Get(HeaderSignature))
	if encoded == "" && signature == "" {
		return Context{}, false, nil
	}
	if encoded == "" || signature == "" {
		return Context{}, true, fmt.Errorf("usage context and signature must both be present")
	}
	ctx, err := Verify(encoded, signature, secret, now, maxSkew)
	return ctx, true, err
}

func HashAPIKey(apiKey string) string {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(apiKey))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func VerifyAPIKeyHash(apiKey, expectedHash string) bool {
	expectedHash = strings.TrimSpace(expectedHash)
	if expectedHash == "" {
		return true
	}
	actualHash := HashAPIKey(apiKey)
	return subtle.ConstantTimeCompare([]byte(actualHash), []byte(expectedHash)) == 1
}

func signEncoded(encoded, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = io.WriteString(mac, signatureDomain)
	_, _ = io.WriteString(mac, encoded)
	return hex.EncodeToString(mac.Sum(nil))
}

func validateIssuedAt(issuedAt time.Time, now time.Time, maxSkew time.Duration) error {
	if issuedAt.IsZero() {
		return fmt.Errorf("usage context issued_at is required")
	}
	if maxSkew <= 0 {
		return nil
	}
	now = now.UTC()
	issuedAt = issuedAt.UTC()
	if issuedAt.Before(now.Add(-maxSkew)) || issuedAt.After(now.Add(maxSkew)) {
		return fmt.Errorf("usage context issued_at is outside allowed skew")
	}
	return nil
}
