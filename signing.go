package usagereporting

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
)

func BodyHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func CanonicalString(method, path, reporterID, timestamp, nonce string, body []byte) string {
	return strings.Join([]string{
		strings.ToUpper(method),
		canonicalPath(path),
		reporterID,
		timestamp,
		nonce,
		BodyHash(body),
	}, "\n")
}

// canonicalPath ensures clients and servers agree on the path portion of the
// signature regardless of which HTTP stack they use. Go's net/http exposes
// root-path URLs as "" via req.URL.Path, while common server frameworks
// expose them as "/". Normalizing here keeps both sides in lockstep.
func canonicalPath(path string) string {
	if path == "" {
		return "/"
	}
	return path
}

// SignBatch produces the HMAC signature for a usage-report batch request.
// Paired with VerifyBatch on the controlplane side.
func SignBatch(method, path, reporterID, timestamp, nonce string, body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = io.WriteString(mac, CanonicalString(method, path, reporterID, timestamp, nonce, body))
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyBatch is the constant-time companion to SignBatch.
func VerifyBatch(method, path, reporterID, timestamp, nonce string, body []byte, secret, signature string) bool {
	expected := SignBatch(method, path, reporterID, timestamp, nonce, body, secret)
	return subtle.ConstantTimeCompare([]byte(expected), []byte(signature)) == 1
}

func HeaderValues(header http.Header) (reporterID, timestamp, nonce, signature string, err error) {
	reporterID = strings.TrimSpace(header.Get(HeaderReporterID))
	timestamp = strings.TrimSpace(header.Get(HeaderTimestamp))
	nonce = strings.TrimSpace(header.Get(HeaderNonce))
	signature = strings.TrimSpace(header.Get(HeaderSignature))
	if reporterID == "" || timestamp == "" || nonce == "" || signature == "" {
		return "", "", "", "", fmt.Errorf("missing signing headers")
	}
	return reporterID, timestamp, nonce, signature, nil
}
