// Package session provides a Redis-backed HTTP session store for the dashboard.
// Each session is stored as a JSON-encoded Data blob under the key
// "session:{token}" with a configurable TTL.
package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

type ctxKey struct{}

// FromContext retrieves the session Data placed by middleware. Returns nil
// when no session is present (unauthenticated request).
func FromContext(ctx context.Context) *Data {
	v, _ := ctx.Value(ctxKey{}).(*Data)
	return v
}

// WithContext returns a copy of ctx with data stored under the session key.
func WithContext(ctx context.Context, data *Data) context.Context {
	return context.WithValue(ctx, ctxKey{}, data)
}

const keyPrefix = "session:"

// Data holds the fields stored server-side for an authenticated dashboard user.
type Data struct {
	UserID int64  `json:"user_id"`
	OrgID  int64  `json:"org_id"`
	Role   string `json:"role"`
	Email  string `json:"email"`
	Name   string `json:"name"`
}

// Store manages sessions in Redis.
type Store struct {
	rdb *redis.Client
	ttl time.Duration
}

// NewStore creates a Store with the given TTL in seconds. A zero or negative
// value falls back to the 8-hour default.
func NewStore(rdb *redis.Client, ttlSeconds int) *Store {
	if ttlSeconds <= 0 {
		ttlSeconds = 8 * 3600
	}
	return &Store{rdb: rdb, ttl: time.Duration(ttlSeconds) * time.Second}
}

// Create stores data in Redis under a new random token and returns the token.
// The token is a 64-character lowercase hex string (32 bytes of crypto/rand).
func (s *Store) Create(ctx context.Context, data Data) (string, error) {
	token, err := generateToken()
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(data)
	if err != nil {
		return "", fmt.Errorf("session: marshal: %w", err)
	}
	if err := s.rdb.Set(ctx, keyPrefix+token, b, s.ttl).Err(); err != nil {
		return "", fmt.Errorf("session: create: %w", err)
	}
	return token, nil
}

// Get retrieves the session associated with token. Returns (nil, nil) when the
// token does not exist or has expired.
func (s *Store) Get(ctx context.Context, token string) (*Data, error) {
	b, err := s.rdb.Get(ctx, keyPrefix+token).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		return nil, fmt.Errorf("session: get: %w", err)
	}
	var data Data
	if err := json.Unmarshal(b, &data); err != nil {
		return nil, fmt.Errorf("session: unmarshal: %w", err)
	}
	return &data, nil
}

// Delete removes the session for the given token. Deleting a non-existent
// token is a no-op and returns nil.
func (s *Store) Delete(ctx context.Context, token string) error {
	if err := s.rdb.Del(ctx, keyPrefix+token).Err(); err != nil {
		return fmt.Errorf("session: delete: %w", err)
	}
	return nil
}

func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("session: generate token: %w", err)
	}
	return hex.EncodeToString(b), nil
}
