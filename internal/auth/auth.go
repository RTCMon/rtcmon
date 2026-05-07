// Package auth provides stateless HS256 JWT verification and a chi middleware
// for authenticating SDK requests.
//
// Usage:
//
//	r.Use(auth.Authenticate(secret))
//	claims := auth.ClaimsFromContext(r.Context())
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/golang-jwt/jwt/v5"
)

// Claims are the custom JWT claims required in every SDK token.
// All three application-level fields are mandatory; a token missing any one
// of them is rejected by VerifyToken.
type Claims struct {
	jwt.RegisteredClaims

	AppID        string `json:"app_id"`
	ConferenceID string `json:"conference_id"`
	UserID       string `json:"user_id"`
}

// contextKey is an unexported type used as the context key for Claims to avoid
// collisions with keys from other packages.
type contextKey struct{}

// VerifyToken parses and validates a raw HS256 JWT string.
//
// It returns:
//   - (*Claims, nil) on success
//   - a wrapped jwt.ErrTokenExpired when the token has expired
//   - a descriptive error for any other validation failure
//
// The token must carry non-empty AppID, ConferenceID, and UserID claims.
func VerifyToken(tokenString, secret string) (*Claims, error) {
	claims := &Claims{}

	token, err := jwt.ParseWithClaims(
		tokenString,
		claims,
		func(t *jwt.Token) (any, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("auth: unexpected signing method: %v", t.Header["alg"])
			}
			return []byte(secret), nil
		},
		jwt.WithValidMethods([]string{"HS256"}),
	)
	if err != nil {
		// Surface the expired-token error distinctly so middleware can produce
		// the right JSON body.
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, fmt.Errorf("auth: %w", jwt.ErrTokenExpired)
		}
		return nil, fmt.Errorf("auth: parse token: %w", err)
	}

	if !token.Valid {
		return nil, errors.New("auth: token is invalid")
	}

	// Validate required application-level claims.
	if claims.AppID == "" {
		return nil, errors.New("auth: missing required claim: app_id")
	}
	if claims.ConferenceID == "" {
		return nil, errors.New("auth: missing required claim: conference_id")
	}
	if claims.UserID == "" {
		return nil, errors.New("auth: missing required claim: user_id")
	}

	return claims, nil
}

// Authenticate returns a chi-compatible middleware that enforces JWT auth.
//
// On success the validated *Claims are stored in the request context and the
// next handler is called. On failure a 401 JSON response is written and the
// chain is stopped.
//
//	Middleware exit codes:
//	  401 {"error":"missing token"}   — no/malformed Authorization header
//	  401 {"error":"token expired"}   — valid JWT but past exp
//	  401 {"error":"invalid token"}   — any other verification failure
func Authenticate(secret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			header := r.Header.Get("Authorization")
			if header == "" {
				writeAuthError(w, http.StatusUnauthorized, "missing token")
				return
			}

			const prefix = "Bearer "
			if !strings.HasPrefix(header, prefix) {
				// Bare token or wrong scheme — reject.
				writeAuthError(w, http.StatusUnauthorized, "missing token")
				return
			}

			tokenString := strings.TrimPrefix(header, prefix)
			if tokenString == "" {
				writeAuthError(w, http.StatusUnauthorized, "missing token")
				return
			}

			claims, err := VerifyToken(tokenString, secret)
			if err != nil {
				if errors.Is(err, jwt.ErrTokenExpired) {
					writeAuthError(w, http.StatusUnauthorized, "token expired")
					return
				}
				writeAuthError(w, http.StatusUnauthorized, "invalid token")
				return
			}

			ctx := context.WithValue(r.Context(), contextKey{}, claims)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// ClaimsFromContext retrieves the *Claims stored by Authenticate from ctx.
// It returns a zero-value &Claims{} (not nil, never panics) when the context
// does not contain claims.
func ClaimsFromContext(ctx context.Context) *Claims {
	if ctx == nil {
		return &Claims{}
	}
	c, _ := ctx.Value(contextKey{}).(*Claims)
	if c == nil {
		return &Claims{}
	}
	return c
}

// writeAuthError writes a JSON 401 body and sets the Content-Type header.
func writeAuthError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body, _ := json.Marshal(map[string]string{"error": msg})
	_, _ = w.Write(body)
}
