package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// GenerateKeyPair returns a new random API key and secret as 64-character
// lowercase hex strings (32 random bytes each). Both values are
// cryptographically random; two consecutive calls are statistically guaranteed
// to be distinct.
func GenerateKeyPair() (apiKey, secret string, err error) {
	keyBytes := make([]byte, 32)
	if _, err = io.ReadFull(rand.Reader, keyBytes); err != nil {
		return "", "", fmt.Errorf("apikey: generate key: %w", err)
	}

	secretBytes := make([]byte, 32)
	if _, err = io.ReadFull(rand.Reader, secretBytes); err != nil {
		return "", "", fmt.Errorf("apikey: generate secret: %w", err)
	}

	return hex.EncodeToString(keyBytes), hex.EncodeToString(secretBytes), nil
}

// EncryptSecret encrypts plaintext using AES-256-GCM with a random 12-byte
// nonce. masterKey must be exactly 32 bytes.
//
// Output format (base64url-encoded): nonce[12] || ciphertext || gcm_tag[16].
// Two calls with identical inputs produce different ciphertexts because the
// nonce is regenerated each time.
func EncryptSecret(masterKey, plaintext []byte) (string, error) {
	if len(masterKey) != 32 {
		return "", fmt.Errorf("apikey: master key must be 32 bytes, got %d", len(masterKey))
	}

	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return "", fmt.Errorf("apikey: create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("apikey: create GCM: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize()) // 12 bytes
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("apikey: generate nonce: %w", err)
	}

	// Seal appends ciphertext+tag to nonce, producing nonce||ciphertext||tag.
	sealed := gcm.Seal(nonce, nonce, plaintext, nil)
	return base64.URLEncoding.EncodeToString(sealed), nil
}

// DecryptSecret decrypts a blob produced by EncryptSecret. Returns an error if
// the blob is truncated, the GCM tag fails (tampered ciphertext or wrong key),
// or the base64 is malformed.
func DecryptSecret(masterKey []byte, encoded string) ([]byte, error) {
	raw, err := base64.URLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("apikey: base64 decode: %w", err)
	}

	// Minimum: nonce(12) + at least one ciphertext byte + tag(16) = 29, but
	// gcm.Overhead() == 16 and NonceSize() == 12; an empty plaintext is valid
	// so the floor is 12+16 = 28.
	const minLen = 12 + 16
	if len(raw) < minLen {
		return nil, fmt.Errorf("apikey: blob too short: %d bytes", len(raw))
	}

	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, fmt.Errorf("apikey: create cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("apikey: create GCM: %w", err)
	}

	nonce, ciphertext := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("apikey: decrypt: %w", err)
	}

	return plaintext, nil
}

// BuildCanonical constructs the canonical request string used for HMAC signing.
//
// Format:
//
//	METHOD\nPATH\nTIMESTAMP\nSHA256_HEX(body)
//
// SHA256_HEX is a lowercase hex-encoded SHA-256 digest of the raw request body.
func BuildCanonical(method, path string, ts int64, body []byte) string {
	sum := sha256.Sum256(body)
	return strings.Join([]string{
		method,
		path,
		strconv.FormatInt(ts, 10),
		hex.EncodeToString(sum[:]),
	}, "\n")
}

// VerifyHMAC verifies that sigHex is the correct HMAC-SHA256 signature for the
// given request parameters using secret. Returns false for any mismatch,
// including a tampered body, wrong secret, modified path, or modified timestamp.
//
// The comparison is performed with hmac.Equal to prevent timing attacks.
func VerifyHMAC(secret []byte, method, path string, ts int64, body []byte, sigHex string) bool {
	canonical := BuildCanonical(method, path, ts, body)

	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(canonical))
	expected := mac.Sum(nil)

	got, err := hex.DecodeString(sigHex)
	if err != nil {
		return false
	}

	return hmac.Equal(expected, got)
}
