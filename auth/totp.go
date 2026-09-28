package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// TOTP as RFC 6238 has it, with what every authenticator app supports:
// HMAC-SHA1, 6 digits, 30-second steps.
const (
	totpDigits     = 6
	totpPeriod     = 30 // seconds
	totpSecretSize = 20 // bytes: 160 bits, RFC 4226's recommendation
	// totpSkew is how many steps either side of now are accepted, for a
	// phone's clock being a little off.
	totpSkew = 1
	// Issuer names Solstein in the authenticator app.
	Issuer = "Solstein"
)

var totpEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// newTOTPSecret is a random secret, base32 as authenticator apps take it.
func newTOTPSecret() (string, error) {
	secret := make([]byte, totpSecretSize)
	if _, err := rand.Read(secret); err != nil {
		return "", fmt.Errorf("TOTP secret: %w", err)
	}
	return totpEncoding.EncodeToString(secret), nil
}

// totpURI is the otpauth:// link an authenticator app reads from the QR code.
func totpURI(username, secret string) string {
	label := url.PathEscape(Issuer + ":" + username)
	query := url.Values{
		"secret":    {secret},
		"issuer":    {Issuer},
		"algorithm": {"SHA1"},
		"digits":    {fmt.Sprint(totpDigits)},
		"period":    {fmt.Sprint(totpPeriod)},
	}
	return "otpauth://totp/" + label + "?" + query.Encode()
}

// totpStep is the time step a moment falls in.
func totpStep(at time.Time) int64 {
	return at.Unix() / totpPeriod
}

// totpCode is the code for one time step (RFC 4226 section 5.3), with the
// given number of digits.
func totpCode(key []byte, step int64, digits int) string {
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(step))
	mac := hmac.New(sha1.New, key)
	mac.Write(counter[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	value := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	modulus := uint32(1)
	for range digits {
		modulus *= 10
	}
	return fmt.Sprintf("%0*d", digits, value%modulus)
}

// matchTOTP finds the step within totpSkew of now whose code is the one
// given, and returns it; ok is false when none matches or the step isn't
// after lastStep, so the same code (or an older one) can't be used twice.
func matchTOTP(secret, code string, now time.Time, lastStep int64) (step int64, ok bool) {
	key, err := totpEncoding.DecodeString(strings.ToUpper(secret))
	if err != nil {
		return 0, false
	}
	code = strings.Join(strings.Fields(code), "") // "123 456" as apps show it
	if len(code) != totpDigits {
		return 0, false
	}
	current := totpStep(now)
	for candidate := current - totpSkew; candidate <= current+totpSkew; candidate++ {
		if candidate <= lastStep {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(totpCode(key, candidate, totpDigits)), []byte(code)) == 1 {
			return candidate, true
		}
	}
	return 0, false
}
