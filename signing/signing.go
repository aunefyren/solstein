// Package signing makes and checks the signatures on the feed and episode
// URLs Solstein writes out. A signature covers the URL path, so a leaked URL
// opens only the one feed or episode it points at.
package signing

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
)

// QueryParameter is the query parameter carrying the signature.
const QueryParameter = "sig"

// Signer signs URL paths with a secret key.
type Signer struct {
	key []byte
}

// New returns a Signer for the given key.
func New(key string) Signer {
	return Signer{key: []byte(key)}
}

// Sign returns the signature for path, URL-safe and unpadded so it needs no
// escaping in a query string (ABS runs encodeURI on enclosure URLs).
func (signer Signer) Sign(path string) string {
	mac := hmac.New(sha256.New, signer.key)
	mac.Write([]byte(path))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Verify reports whether signature is valid for path, in constant time.
func (signer Signer) Verify(path, signature string) bool {
	expected := signer.Sign(path)
	return subtle.ConstantTimeCompare([]byte(expected), []byte(signature)) == 1
}
