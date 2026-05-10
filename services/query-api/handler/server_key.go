package handler

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"

	"github.com/RTCMon/rtcmon/internal/auth"
	"github.com/RTCMon/rtcmon/internal/session"
)

// serverKeyRow holds the columns we SELECT from apps for key management.
type serverKeyRow struct {
	ID               int64
	ServerAPIKey     *string
	ServerSecretEnc  *string
	ServerKeyCreated *time.Time
}

// GenerateKeyResponse is the JSON body for 201 (generate) and 200 (rotate).
type GenerateKeyResponse struct {
	APIKey    string    `json:"api_key"`
	APISecret string    `json:"api_secret"`
	CreatedAt time.Time `json:"created_at"`
}

// GetKeyResponse is the JSON body for 200 (metadata only).
type GetKeyResponse struct {
	APIKey    string    `json:"api_key"`
	HasSecret bool      `json:"has_secret"`
	CreatedAt time.Time `json:"created_at"`
}

// HandleGenerateServerKey handles POST /v1/orgs/:orgId/apps/:appId/server-key.
// Admin only. Returns 201 with api_key + api_secret. Fails with 409 if a key
// already exists.
func HandleGenerateServerKey(db *pgxpool.Pool, masterKey []byte, log *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, orgID, appID, ok := extractKeyParams(w, r)
		if !ok {
			return
		}
		if !requireAdmin(w, sess) {
			return
		}
		if len(masterKey) != 32 {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"error": "server key management not configured",
			})
			return
		}

		row, err := fetchAppRow(r, db, appID, orgID)
		if err != nil {
			log.WithError(err).Error("generate server key: fetch app")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		if row == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "app not found"})
			return
		}
		if row.ServerAPIKey != nil {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error": "server key already exists, use rotate-server-key to replace",
			})
			return
		}

		apiKey, secret, createdAt, err := generateAndStore(r, db, masterKey, appID, orgID)
		if err != nil {
			log.WithError(err).Error("generate server key: store")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		writeJSON(w, http.StatusCreated, GenerateKeyResponse{
			APIKey:    apiKey,
			APISecret: secret,
			CreatedAt: createdAt,
		})
	}
}

// HandleGetServerKey handles GET /v1/orgs/:orgId/apps/:appId/server-key.
// Any authenticated org member. Returns metadata only — never the secret.
// @Summary Get Server Key
// @Description Get Server Key endpoint
// @Tags v1
// @Accept json
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Router /v1/orgs/{orgId}/apps/{appId}/server-key [get]
func HandleGetServerKey(db *pgxpool.Pool, log *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, orgID, appID, ok := extractKeyParams(w, r)
		if !ok {
			return
		}

		row, err := fetchAppRow(r, db, appID, orgID)
		if err != nil {
			log.WithError(err).Error("get server key: fetch app")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		if row == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "app not found"})
			return
		}
		if row.ServerAPIKey == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error": "no server key configured for this app",
			})
			return
		}

		writeJSON(w, http.StatusOK, GetKeyResponse{
			APIKey:    *row.ServerAPIKey,
			HasSecret: row.ServerSecretEnc != nil,
			CreatedAt: *row.ServerKeyCreated,
		})
	}
}

// HandleDeleteServerKey handles DELETE /v1/orgs/:orgId/apps/:appId/server-key.
// Admin only. Sets all three key columns to NULL. Returns 204.
func HandleDeleteServerKey(db *pgxpool.Pool, log *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, orgID, appID, ok := extractKeyParams(w, r)
		if !ok {
			return
		}
		if !requireAdmin(w, sess) {
			return
		}

		row, err := fetchAppRow(r, db, appID, orgID)
		if err != nil {
			log.WithError(err).Error("delete server key: fetch app")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		if row == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "app not found"})
			return
		}
		if row.ServerAPIKey == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error": "no server key configured for this app",
			})
			return
		}

		_, err = db.Exec(r.Context(), `
			UPDATE apps
			SET server_api_key        = NULL,
			    server_api_secret_enc = NULL,
			    server_key_created_at = NULL
			WHERE id = $1 AND org_id = $2
		`, appID, orgID)
		if err != nil {
			log.WithError(err).Error("delete server key: update")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

// HandleRotateServerKey handles POST /v1/orgs/:orgId/apps/:appId/rotate-server-key.
// Admin only. Generates a new key pair atomically. Returns 404 if no key exists yet.
// @Summary Rotate Server Key
// @Description Rotate Server Key endpoint
// @Tags v1
// @Accept json
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Router /v1/orgs/{orgId}/apps/{appId}/rotate-server-key [post]
func HandleRotateServerKey(db *pgxpool.Pool, masterKey []byte, log *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, orgID, appID, ok := extractKeyParams(w, r)
		if !ok {
			return
		}
		if !requireAdmin(w, sess) {
			return
		}
		if len(masterKey) != 32 {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"error": "server key management not configured",
			})
			return
		}

		row, err := fetchAppRow(r, db, appID, orgID)
		if err != nil {
			log.WithError(err).Error("rotate server key: fetch app")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		if row == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "app not found"})
			return
		}
		if row.ServerAPIKey == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{
				"error": "no server key to rotate, use POST server-key to create one",
			})
			return
		}

		apiKey, secret, createdAt, err := generateAndStore(r, db, masterKey, appID, orgID)
		if err != nil {
			log.WithError(err).Error("rotate server key: store")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		writeJSON(w, http.StatusOK, GenerateKeyResponse{
			APIKey:    apiKey,
			APISecret: secret,
			CreatedAt: createdAt,
		})
	}
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// extractKeyParams reads the session, validates org ownership, and parses
// orgId/appId from the URL. Writes the appropriate error response and returns
// false when any check fails.
func extractKeyParams(w http.ResponseWriter, r *http.Request) (sess *session.Data, orgID, appID int64, ok bool) {
	sess = session.FromContext(r.Context())
	if sess == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return nil, 0, 0, false
	}

	orgID, err := strconv.ParseInt(chi.URLParam(r, "orgId"), 10, 64)
	if err != nil || orgID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid orgId"})
		return nil, 0, 0, false
	}
	if orgID != sess.OrgID {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
		return nil, 0, 0, false
	}

	appID, err = strconv.ParseInt(chi.URLParam(r, "appId"), 10, 64)
	if err != nil || appID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid appId"})
		return nil, 0, 0, false
	}

	return sess, orgID, appID, true
}

// requireAdmin checks that the session role is "admin". Writes 403 and returns
// false if not.
func requireAdmin(w http.ResponseWriter, sess *session.Data) bool {
	if sess == nil || sess.Role != "admin" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin role required"})
		return false
	}
	return true
}

// fetchAppRow queries the apps table for the given app+org combination.
// Returns (nil, nil) when no row is found (not an error — caller should 404).
func fetchAppRow(r *http.Request, db *pgxpool.Pool, appID, orgID int64) (*serverKeyRow, error) {
	row := &serverKeyRow{ID: appID}
	err := db.QueryRow(r.Context(), `
		SELECT server_api_key, server_api_secret_enc, server_key_created_at
		FROM apps
		WHERE id = $1 AND org_id = $2
	`, appID, orgID).Scan(&row.ServerAPIKey, &row.ServerSecretEnc, &row.ServerKeyCreated)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return row, nil
}

// generateAndStore creates a new key pair, encrypts the secret, and persists
// both to the apps table. Returns the plaintext key, plaintext secret, and
// server_key_created_at timestamp from the DB.
func generateAndStore(r *http.Request, db *pgxpool.Pool, masterKey []byte, appID, orgID int64) (apiKey, secret string, createdAt time.Time, err error) {
	apiKey, secret, err = auth.GenerateKeyPair()
	if err != nil {
		return
	}

	secretEnc, err := auth.EncryptSecret(masterKey, []byte(secret))
	if err != nil {
		return
	}

	err = db.QueryRow(r.Context(), `
		UPDATE apps
		SET server_api_key        = $1,
		    server_api_secret_enc = $2,
		    server_key_created_at = now()
		WHERE id = $3 AND org_id = $4
		RETURNING server_key_created_at
	`, apiKey, secretEnc, appID, orgID).Scan(&createdAt)
	return
}
