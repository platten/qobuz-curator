// Package security implements password, session, and CSRF primitives.
package security

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/scrypt"
)

const (
	legacyScryptN  = 1 << 14
	currentScryptN = 1 << 17
	scryptR        = 8
	scryptP        = 1
	passwordBytes  = 32
	saltBytes      = 16
)

var readRandom = rand.Read

// HashPassword encodes a password with the current bounded scrypt parameters.
func HashPassword(password string) (string, error) {
	salt := make([]byte, saltBytes)
	if _, e := readRandom(salt); e != nil {
		return "", e
	}
	derived, e := scrypt.Key([]byte(password), salt, currentScryptN, scryptR, scryptP, passwordBytes)
	if e != nil {
		return "", e
	}
	return fmt.Sprintf(
		"scrypt$%d$%d$%d$%s$%s",
		currentScryptN, scryptR, scryptP,
		base64.RawURLEncoding.EncodeToString(salt),
		base64.RawURLEncoding.EncodeToString(derived),
	), nil
}

// VerifyPassword supports both current hashes and the legacy three-part format.
func VerifyPassword(password, encoded string) bool {
	n, r, p, salt, expected, ok := parsePasswordHash(encoded)
	if !ok {
		return false
	}
	actual, err := scrypt.Key([]byte(password), salt, n, r, p, len(expected))
	return err == nil && hmac.Equal(actual, expected)
}

// PasswordHashValid validates the format and fixed resource bounds without
// performing the expensive password derivation.
func PasswordHashValid(encoded string) bool {
	_, _, _, _, _, ok := parsePasswordHash(encoded)
	return ok
}

func parsePasswordHash(encoded string) (int, int, int, []byte, []byte, bool) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 3 && len(parts) != 6 || parts[0] != "scrypt" {
		return 0, 0, 0, nil, nil, false
	}
	n, r, p := legacyScryptN, scryptR, scryptP
	saltPart, hashPart := parts[1], parts[2]
	if len(parts) == 6 {
		var err error
		n, err = strconv.Atoi(parts[1])
		if err != nil {
			return 0, 0, 0, nil, nil, false
		}
		r, err = strconv.Atoi(parts[2])
		if err != nil {
			return 0, 0, 0, nil, nil, false
		}
		p, err = strconv.Atoi(parts[3])
		if err != nil {
			return 0, 0, 0, nil, nil, false
		}
		saltPart, hashPart = parts[4], parts[5]
	}
	// Only known parameter sets are accepted so an edited configuration file
	// cannot turn password verification into an unbounded memory/CPU request.
	if (n != legacyScryptN && n != currentScryptN) || r != scryptR || p != scryptP {
		return 0, 0, 0, nil, nil, false
	}
	salt, err := decodePasswordPart(saltPart)
	if err != nil || len(salt) != saltBytes {
		return 0, 0, 0, nil, nil, false
	}
	expected, err := decodePasswordPart(hashPart)
	if err != nil || len(expected) != passwordBytes {
		return 0, 0, 0, nil, nil, false
	}
	return n, r, p, salt, expected, true
}

func decodePasswordPart(value string) ([]byte, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err == nil {
		return decoded, nil
	}
	return base64.URLEncoding.DecodeString(value)
}

// Session is the small signed value stored in the browser cookie.
type Session struct {
	Authenticated bool   `json:"authenticated"`
	CSRF          string `json:"csrf"`
	ExpiresAt     int64  `json:"expires_at"`
}

// NewSession creates a cryptographically random CSRF token and an expiring
// session. Callers must not continue if the operating system entropy source
// fails.
func NewSession(ttl time.Duration) (Session, error) {
	if ttl <= 0 {
		return Session{}, fmt.Errorf("session TTL must be positive")
	}
	raw := make([]byte, 32)
	if _, err := readRandom(raw); err != nil {
		return Session{}, fmt.Errorf("generate session token: %w", err)
	}
	return Session{
		CSRF:      base64.RawURLEncoding.EncodeToString(raw),
		ExpiresAt: time.Now().Add(ttl).Unix(),
	}, nil
}

// EncodeSession serializes and authenticates a session with HMAC-SHA256.
func EncodeSession(s Session, secret string) string {
	raw, _ := json.Marshal(s)
	payload := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// DecodeSession verifies a session signature, structure, and expiration.
func DecodeSession(value, secret string) (Session, bool) {
	return decodeSessionAt(value, secret, time.Now())
}

func decodeSessionAt(value, secret string, now time.Time) (Session, bool) {
	parts := strings.Split(value, ".")
	if len(parts) != 2 {
		return Session{}, false
	}
	signature, e := base64.RawURLEncoding.DecodeString(parts[1])
	if e != nil {
		return Session{}, false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(parts[0]))
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return Session{}, false
	}
	raw, e := base64.RawURLEncoding.DecodeString(parts[0])
	if e != nil {
		return Session{}, false
	}
	var s Session
	e = json.Unmarshal(raw, &s)
	return s, e == nil && s.CSRF != "" && s.ExpiresAt > now.Unix()
}

// ValidCSRF compares a submitted token in constant time.
func ValidCSRF(s Session, value string) bool {
	return s.CSRF != "" && value != "" && hmac.Equal([]byte(s.CSRF), []byte(value))
}
