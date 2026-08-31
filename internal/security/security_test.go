package security

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/scrypt"
)

func TestPassword(t *testing.T) {
	hash, e := HashPassword("secret")
	if e != nil || !PasswordHashValid(hash) || !VerifyPassword("secret", hash) || VerifyPassword("bad", hash) || VerifyPassword("x", "bad") {
		t.Fatal("password")
	}
}
func TestPasswordMalformed(t *testing.T) {
	for _, value := range []string{
		"argon$x$x", "scrypt$!$!", "scrypt$YQ==$!",
		"scrypt$bad$8$1$x$x", "scrypt$131072$bad$1$x$x", "scrypt$131072$8$bad$x$x",
		"scrypt$262144$8$1$x$x", "scrypt$131072$9$1$x$x", "scrypt$131072$8$2$x$x",
		"scrypt$131072$8$1$YQ$x", "scrypt$131072$8$1$MDEyMzQ1Njc4OWFiY2RlZg$YQ",
	} {
		if VerifyPassword("x", value) {
			t.Fatal(value)
		}
	}
	salt := []byte("0123456789abcdef")
	derived, err := scrypt.Key([]byte("legacy"), salt, legacyScryptN, scryptR, scryptP, passwordBytes)
	if err != nil {
		t.Fatal(err)
	}
	legacy := "scrypt$" + base64.URLEncoding.EncodeToString(salt) + "$" + base64.URLEncoding.EncodeToString(derived)
	if !VerifyPassword("legacy", legacy) || !PasswordHashValid(legacy) {
		t.Fatal("legacy hash rejected")
	}
}
func TestSession(t *testing.T) {
	s, err := NewSession(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	encoded := EncodeSession(s, "secret")
	got, ok := DecodeSession(encoded, "secret")
	if !ok || got.CSRF != s.CSRF || !ValidCSRF(got, s.CSRF) {
		t.Fatal("session")
	}
	for _, v := range []string{"bad", encoded + "x"} {
		if _, ok = DecodeSession(v, "secret"); ok {
			t.Fatal("accepted invalid")
		}
	}
	badJSON := base64.RawURLEncoding.EncodeToString([]byte("{"))
	mac := hmac.New(sha256.New, []byte("secret"))
	_, _ = mac.Write([]byte(badJSON))
	if _, ok := DecodeSession(badJSON+"."+base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), "secret"); ok {
		t.Fatal("json")
	}
	parts := strings.Split(encoded, ".")
	if _, ok := DecodeSession(parts[0]+".!", "secret"); ok {
		t.Fatal("signature")
	}
	if ValidCSRF(s, "") {
		t.Fatal("empty csrf")
	}
	if _, ok := decodeSessionAt(encoded, "secret", time.Unix(s.ExpiresAt+1, 0)); ok {
		t.Fatal("accepted expired session")
	}
	if _, err := NewSession(0); err == nil {
		t.Fatal("accepted invalid TTL")
	}
}

func TestEntropyFailure(t *testing.T) {
	original := readRandom
	readRandom = func([]byte) (int, error) { return 0, errors.New("entropy") }
	defer func() { readRandom = original }()
	if _, err := HashPassword("secret"); err == nil {
		t.Fatal("password entropy")
	}
	if _, err := NewSession(time.Hour); err == nil {
		t.Fatal("session entropy")
	}
}
