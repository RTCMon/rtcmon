package handler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/bcrypt"

	"github.com/RTCMon/rtcmon/internal/session"
)

type CreateOrgRequest struct {
	Name string `json:"name"`
}

type OrgResponse struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

type CreateAppRequest struct {
	Name          string `json:"name"`
	RetentionDays int    `json:"retention_days"`
}

type AppResponse struct {
	ID            int64     `json:"id"`
	OrgID         int64     `json:"org_id"`
	Name          string    `json:"name"`
	RetentionDays int       `json:"retention_days"`
	CreatedAt     time.Time `json:"created_at"`
}

type AppCreatedResponse struct {
	AppResponse
	APIKey string `json:"api_key"`
}

type RotateKeyResponse struct {
	APIKey string `json:"api_key"`
}

// generateClientKey generates a 32-byte random API key as a 64-char hex string.
func generateClientKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// parseOrgParams extracts session + orgId from request, enforcing org membership.
func parseOrgParams(w http.ResponseWriter, r *http.Request) (*session.Data, int64, bool) {
	sess := session.FromContext(r.Context())
	if sess == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
		return nil, 0, false
	}

	orgID, err := strconv.ParseInt(chi.URLParam(r, "orgId"), 10, 64)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid orgId"}`))
		return nil, 0, false
	}

	if sess.OrgID != orgID {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"forbidden"}`))
		return nil, 0, false
	}

	return sess, orgID, true
}

// parseAppParams extends parseOrgParams with appId parsing.
func parseAppParams(w http.ResponseWriter, r *http.Request) (*session.Data, int64, int64, bool) {
	sess, orgID, ok := parseOrgParams(w, r)
	if !ok {
		return nil, 0, 0, false
	}

	appID, err := strconv.ParseInt(chi.URLParam(r, "appId"), 10, 64)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid appId"}`))
		return nil, 0, 0, false
	}

	return sess, orgID, appID, true
}

func HandleCreateOrg(db *pgxpool.Pool, sessions *session.Store, log *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := session.FromContext(r.Context())
		if sess == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}

		var req CreateOrgRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid request body"}`))
			return
		}

		if req.Name == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"name is required"}`))
			return
		}

		ctx := r.Context()
		tx, err := db.Begin(ctx)
		if err != nil {
			log.WithError(err).Error("HandleCreateOrg: db.Begin")
			http.Error(w, "internal service error", http.StatusInternalServerError)
			return
		}
		defer tx.Rollback(ctx)

		var resp OrgResponse
		resp.Name = req.Name
		if err := tx.QueryRow(ctx,
			`INSERT INTO organizations (name) VALUES ($1) RETURNING id, created_at`,
			req.Name,
		).Scan(&resp.ID, &resp.CreatedAt); err != nil {
			log.WithError(err).Error("HandleCreateOrg: insert organization")
			http.Error(w, "internal service error", http.StatusInternalServerError)
			return
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO organization_members (org_id, user_id, role) VALUES ($1, $2, 'admin')`,
			resp.ID, sess.UserID,
		); err != nil {
			log.WithError(err).Error("HandleCreateOrg: insert organization_members")
			http.Error(w, "internal service error", http.StatusInternalServerError)
			return
		}

		if err := tx.Commit(ctx); err != nil {
			log.WithError(err).Error("HandleCreateOrg: tx.Commit")
			http.Error(w, "internal service error", http.StatusInternalServerError)
			return
		}

		// Update session
		oldCookie, err := r.Cookie("session")
		if err == nil && oldCookie != nil {
			_ = sessions.Delete(ctx, oldCookie.Value)
		}

		newSess := *sess
		newSess.OrgID = resp.ID
		newSess.Role = "admin"

		token, err := sessions.Create(ctx, newSess)
		if err != nil {
			log.WithError(err).Error("HandleCreateOrg: session creation")
			http.Error(w, "internal service error", http.StatusInternalServerError)
			return
		}

		setSessionCookie(w, token)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(resp)
	}
}

func HandleListApps(db *pgxpool.Pool, log *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, orgID, ok := parseOrgParams(w, r)
		if !ok {
			return
		}

		rows, err := db.Query(r.Context(),
			`SELECT id, org_id, name, retention_days, created_at 
			 FROM apps WHERE org_id = $1 ORDER BY created_at ASC`,
			orgID,
		)
		if err != nil {
			log.WithError(err).Error("HandleListApps: db.Query")
			http.Error(w, "internal service error", http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		apps := make([]AppResponse, 0)
		for rows.Next() {
			var app AppResponse
			if err := rows.Scan(&app.ID, &app.OrgID, &app.Name, &app.RetentionDays, &app.CreatedAt); err != nil {
				log.WithError(err).Error("HandleListApps: rows.Scan")
				http.Error(w, "internal service error", http.StatusInternalServerError)
				return
			}
			apps = append(apps, app)
		}

		if err := rows.Err(); err != nil {
			log.WithError(err).Error("HandleListApps: rows.Err")
			http.Error(w, "internal service error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": apps})
	}
}

func HandleCreateApp(db *pgxpool.Pool, log *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, orgID, ok := parseOrgParams(w, r)
		if !ok {
			return
		}

		var req CreateAppRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid request body"}`))
			return
		}

		if req.Name == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"name is required"}`))
			return
		}

		if req.RetentionDays <= 0 {
			req.RetentionDays = 90
		}

		apiKey, err := generateClientKey()
		if err != nil {
			log.WithError(err).Error("HandleCreateApp: generateClientKey")
			http.Error(w, "internal service error", http.StatusInternalServerError)
			return
		}

		hash, err := bcrypt.GenerateFromPassword([]byte(apiKey), 12)
		if err != nil {
			log.WithError(err).Error("HandleCreateApp: bcrypt.GenerateFromPassword")
			http.Error(w, "internal service error", http.StatusInternalServerError)
			return
		}

		var resp AppCreatedResponse
		resp.Name = req.Name
		resp.OrgID = orgID
		resp.RetentionDays = req.RetentionDays
		resp.APIKey = apiKey

		if err := db.QueryRow(r.Context(),
			`INSERT INTO apps (org_id, name, api_key_hash, retention_days) 
			 VALUES ($1, $2, $3, $4) RETURNING id, created_at`,
			orgID, req.Name, string(hash), req.RetentionDays,
		).Scan(&resp.ID, &resp.CreatedAt); err != nil {
			log.WithError(err).Error("HandleCreateApp: insert app")
			http.Error(w, "internal service error", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(resp)
	}
}

func HandleDeleteApp(db *pgxpool.Pool, log *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, orgID, appID, ok := parseAppParams(w, r)
		if !ok {
			return
		}

		var id int64
		if err := db.QueryRow(r.Context(), `SELECT id FROM apps WHERE id = $1 AND org_id = $2`, appID, orgID).Scan(&id); err != nil {
			if err == pgx.ErrNoRows {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"app not found"}`))
				return
			}
			log.WithError(err).Error("HandleDeleteApp: check existence")
			http.Error(w, "internal service error", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusAccepted)

		go func(pool *pgxpool.Pool, id int64) {
			if _, err := pool.Exec(context.Background(), `DELETE FROM apps WHERE id = $1`, id); err != nil {
				log.WithError(err).WithField("app_id", id).Error("HandleDeleteApp: background delete failed")
			}
		}(db, appID)
	}
}

func HandleRotateKey(db *pgxpool.Pool, log *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, orgID, appID, ok := parseAppParams(w, r)
		if !ok {
			return
		}

		var id int64
		if err := db.QueryRow(r.Context(), `SELECT id FROM apps WHERE id = $1 AND org_id = $2`, appID, orgID).Scan(&id); err != nil {
			if err == pgx.ErrNoRows {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"app not found"}`))
				return
			}
			log.WithError(err).Error("HandleRotateKey: check existence")
			http.Error(w, "internal service error", http.StatusInternalServerError)
			return
		}

		newKey, err := generateClientKey()
		if err != nil {
			log.WithError(err).Error("HandleRotateKey: generateClientKey")
			http.Error(w, "internal service error", http.StatusInternalServerError)
			return
		}

		hash, err := bcrypt.GenerateFromPassword([]byte(newKey), 12)
		if err != nil {
			log.WithError(err).Error("HandleRotateKey: bcrypt.GenerateFromPassword")
			http.Error(w, "internal service error", http.StatusInternalServerError)
			return
		}

		if _, err := db.Exec(r.Context(),
			`UPDATE apps SET api_key_hash = $1 WHERE id = $2 AND org_id = $3`,
			string(hash), appID, orgID,
		); err != nil {
			log.WithError(err).Error("HandleRotateKey: update api_key_hash")
			http.Error(w, "internal service error", http.StatusInternalServerError)
			return
		}

		var resp RotateKeyResponse
		resp.APIKey = newKey

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(resp)
	}
}
