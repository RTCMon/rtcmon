package auth_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/RTCMon/rtcmon/internal/auth"
)

// ----- GenerateKeyPair -------------------------------------------------------

func TestGenerateKeyPair_Format(t *testing.T) {
	apiKey, secret, err := auth.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair() error: %v", err)
	}

	if len(apiKey) != 64 {
		t.Errorf("apiKey length = %d, want 64", len(apiKey))
	}
	if len(secret) != 64 {
		t.Errorf("secret length = %d, want 64", len(secret))
	}

	if apiKey != strings.ToLower(apiKey) {
		t.Errorf("apiKey is not lowercase: %s", apiKey)
	}
	if secret != strings.ToLower(secret) {
		t.Errorf("secret is not lowercase: %s", secret)
	}

	if _, err := hex.DecodeString(apiKey); err != nil {
		t.Errorf("apiKey is not valid hex: %v", err)
	}
	if _, err := hex.DecodeString(secret); err != nil {
		t.Errorf("secret is not valid hex: %v", err)
	}
}

func TestGenerateKeyPair_Unique(t *testing.T) {
	seen := make(map[string]struct{}, 200)
	for i := range 100 {
		k, s, err := auth.GenerateKeyPair()
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if _, dup := seen[k]; dup {
			t.Fatalf("duplicate apiKey on call %d: %s", i, k)
		}
		if _, dup := seen[s]; dup {
			t.Fatalf("duplicate secret on call %d: %s", i, s)
		}
		seen[k] = struct{}{}
		seen[s] = struct{}{}
	}
}

// ----- EncryptSecret / DecryptSecret -----------------------------------------

// testMasterKey is a 32-byte key used across encryption tests.
var testMasterKey = []byte("12345678901234567890123456789012") // 32 bytes

func TestEncryptDecrypt_Roundtrip(t *testing.T) {
	plaintext := []byte("super-secret-value-1234567890abc")

	enc, err := auth.EncryptSecret(testMasterKey, plaintext)
	if err != nil {
		t.Fatalf("EncryptSecret error: %v", err)
	}

	got, err := auth.DecryptSecret(testMasterKey, enc)
	if err != nil {
		t.Fatalf("DecryptSecret error: %v", err)
	}

	if string(got) != string(plaintext) {
		t.Errorf("roundtrip mismatch: got %q, want %q", got, plaintext)
	}
}

func TestEncryptSecret_DifferentNonceEachTime(t *testing.T) {
	plaintext := []byte("same-plaintext")

	enc1, err := auth.EncryptSecret(testMasterKey, plaintext)
	if err != nil {
		t.Fatalf("first EncryptSecret: %v", err)
	}

	enc2, err := auth.EncryptSecret(testMasterKey, plaintext)
	if err != nil {
		t.Fatalf("second EncryptSecret: %v", err)
	}

	if enc1 == enc2 {
		t.Error("two encryptions of the same plaintext produced identical ciphertexts (nonce reuse)")
	}
}

func TestDecryptSecret_TamperedCiphertext(t *testing.T) {
	plaintext := []byte("tamper-me")

	enc, err := auth.EncryptSecret(testMasterKey, plaintext)
	if err != nil {
		t.Fatalf("EncryptSecret: %v", err)
	}

	// Decode, flip a byte in the ciphertext/tag region, re-encode.
	raw, _ := base64.URLEncoding.DecodeString(enc)
	raw[len(raw)-1] ^= 0xFF
	tampered := base64.URLEncoding.EncodeToString(raw)

	_, err = auth.DecryptSecret(testMasterKey, tampered)
	if err == nil {
		t.Error("DecryptSecret should return error for tampered ciphertext")
	}
}

func TestDecryptSecret_WrongMasterKey(t *testing.T) {
	plaintext := []byte("secret")

	enc, err := auth.EncryptSecret(testMasterKey, plaintext)
	if err != nil {
		t.Fatalf("EncryptSecret: %v", err)
	}

	wrongKey := []byte("aaaabbbbccccddddeeeeffffgggghhhh") // different 32-byte key
	_, err = auth.DecryptSecret(wrongKey, enc)
	if err == nil {
		t.Error("DecryptSecret should return error for wrong master key")
	}
}

func TestDecryptSecret_TruncatedBlob(t *testing.T) {
	// 10 raw bytes → only 10 bytes when decoded, well below nonce(12)+tag(16)=28.
	short := make([]byte, 10)
	encoded := base64.URLEncoding.EncodeToString(short)

	_, err := auth.DecryptSecret(testMasterKey, encoded)
	if err == nil {
		t.Error("DecryptSecret should return error for truncated blob, got nil")
	}
}

// ----- BuildCanonical --------------------------------------------------------

func TestBuildCanonical_Format(t *testing.T) {
	body := []byte("hello world")
	sum := sha256.Sum256(body)
	wantHash := hex.EncodeToString(sum[:])
	want := fmt.Sprintf("POST\n/v1/events\n1234567890\n%s", wantHash)

	got := auth.BuildCanonical("POST", "/v1/events", 1234567890, body)
	if got != want {
		t.Errorf("BuildCanonical mismatch:\n got  %q\n want %q", got, want)
	}
}

func TestBuildCanonical_BodyHashIsLowercaseHex(t *testing.T) {
	// SHA256("hello") is a well-known value.
	body := []byte("hello")
	canonical := auth.BuildCanonical("GET", "/path", 0, body)
	parts := strings.Split(canonical, "\n")
	if len(parts) != 4 {
		t.Fatalf("expected 4 parts, got %d: %q", len(parts), canonical)
	}

	hashPart := parts[3]
	if hashPart != strings.ToLower(hashPart) {
		t.Errorf("hash is not lowercase: %s", hashPart)
	}

	// Known SHA256("hello").
	expected := "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if hashPart != expected {
		t.Errorf("hash = %s, want %s", hashPart, expected)
	}
}

// ----- VerifyHMAC ------------------------------------------------------------

func buildSignature(secret []byte, method, path string, ts int64, body []byte) string {
	canonical := auth.BuildCanonical(method, path, ts, body)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(canonical))
	return hex.EncodeToString(mac.Sum(nil))
}

func TestVerifyHMAC_Valid(t *testing.T) {
	secret := []byte("my-super-secret-key")
	body := []byte(`{"conference_id":"abc"}`)
	sig := buildSignature(secret, "POST", "/v1/server/events", 1700000000, body)

	if !auth.VerifyHMAC(secret, "POST", "/v1/server/events", 1700000000, body, sig) {
		t.Error("VerifyHMAC should return true for a correctly signed request")
	}
}

func TestVerifyHMAC_WrongBody(t *testing.T) {
	secret := []byte("my-super-secret-key")
	body := []byte(`{"conference_id":"abc"}`)
	sig := buildSignature(secret, "POST", "/v1/server/events", 1700000000, body)

	tamperedBody := []byte(`{"conference_id":"xyz"}`)
	if auth.VerifyHMAC(secret, "POST", "/v1/server/events", 1700000000, tamperedBody, sig) {
		t.Error("VerifyHMAC should return false for a tampered body")
	}
}

func TestVerifyHMAC_WrongSecret(t *testing.T) {
	secret := []byte("my-super-secret-key")
	body := []byte(`{"data":"value"}`)
	sig := buildSignature(secret, "POST", "/v1/server/events", 1700000000, body)

	wrongSecret := []byte("different-secret-key")
	if auth.VerifyHMAC(wrongSecret, "POST", "/v1/server/events", 1700000000, body, sig) {
		t.Error("VerifyHMAC should return false for wrong secret")
	}
}

func TestVerifyHMAC_WrongPath(t *testing.T) {
	secret := []byte("my-super-secret-key")
	body := []byte(`{"data":"value"}`)
	sig := buildSignature(secret, "POST", "/v1/server/events", 1700000000, body)

	if auth.VerifyHMAC(secret, "POST", "/v1/other/path", 1700000000, body, sig) {
		t.Error("VerifyHMAC should return false for modified path")
	}
}

func TestVerifyHMAC_WrongTimestamp(t *testing.T) {
	secret := []byte("my-super-secret-key")
	body := []byte(`{"data":"value"}`)
	ts := int64(1700000000)
	sig := buildSignature(secret, "POST", "/v1/server/events", ts, body)

	if auth.VerifyHMAC(secret, "POST", "/v1/server/events", ts+1, body, sig) {
		t.Error("VerifyHMAC should return false for ts+1")
	}
	if auth.VerifyHMAC(secret, "POST", "/v1/server/events", ts-1, body, sig) {
		t.Error("VerifyHMAC should return false for ts-1")
	}
}
