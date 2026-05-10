package handler

import (
	"encoding/json"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/bcrypt"

	"github.com/RTCMon/rtcmon/internal/session"
)

const bcryptCost = 12

// RegisterRequest is the body for POST /auth/register.
type RegisterRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Name     string `json:"name"`
}

// LoginRequest is the body for POST /auth/login.
type LoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// PatchMeRequest is the body for PATCH /auth/me.
type PatchMeRequest struct {
	Name     *string `json:"name"`
	Password *string `json:"password"`
}

// HandleRegister is the first-run endpoint: creates the first admin user.
// Returns 409 if any user already exists.
// @Summary Register
// @Description Register endpoint
// @Tags auth
// @Accept json
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Router /auth/register [post]
func HandleRegister(db *pgxpool.Pool, sessions *session.Store, log *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req RegisterRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}
		if req.Email == "" || req.Password == "" || req.Name == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "email, password, and name are required"})
			return
		}

		// First-run check: fail if any user already exists.
		var count int
		if err := db.QueryRow(r.Context(), `SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
			log.WithError(err).Error("register: count users")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		if count > 0 {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "registration disabled"})
			return
		}

		hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcryptCost)
		if err != nil {
			log.WithError(err).Error("register: bcrypt")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		tx, err := db.Begin(r.Context())
		if err != nil {
			log.WithError(err).Error("register: begin tx")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		defer tx.Rollback(r.Context()) //nolint:errcheck

		var userID int64
		if err := tx.QueryRow(r.Context(),
			`INSERT INTO users (email, name, password_hash) VALUES ($1, $2, $3) RETURNING id`,
			req.Email, req.Name, string(hash),
		).Scan(&userID); err != nil {
			log.WithError(err).Error("register: insert user")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		var orgID int64
		if err := tx.QueryRow(r.Context(),
			`INSERT INTO organizations (name) VALUES ($1) RETURNING id`,
			req.Name+"'s Organization",
		).Scan(&orgID); err != nil {
			log.WithError(err).Error("register: insert org")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		if _, err := tx.Exec(r.Context(),
			`INSERT INTO organization_members (org_id, user_id, role) VALUES ($1, $2, 'admin')`,
			orgID, userID,
		); err != nil {
			log.WithError(err).Error("register: insert org_member")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		if err := tx.Commit(r.Context()); err != nil {
			log.WithError(err).Error("register: commit")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		token, err := sessions.Create(r.Context(), session.Data{
			UserID: userID,
			OrgID:  orgID,
			Role:   "admin",
			Email:  req.Email,
			Name:   req.Name,
		})
		if err != nil {
			log.WithError(err).Error("register: create session")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		setSessionCookie(w, token)
		writeJSON(w, http.StatusCreated, map[string]any{
			"id":    userID,
			"email": req.Email,
			"name":  req.Name,
			"role":  "admin",
		})
	}
}

// HandleLogin authenticates a user and sets a session cookie.
// @Summary Login
// @Description Login endpoint
// @Tags auth
// @Accept json
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Router /auth/login [post]
func HandleLogin(db *pgxpool.Pool, sessions *session.Store, log *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req LoginRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}

		var (
			userID       int64
			name         string
			passwordHash string
			orgID        int64
			role         string
		)
		err := db.QueryRow(r.Context(), `
			SELECT u.id, u.name, u.password_hash, om.org_id, om.role
			FROM users u
			JOIN organization_members om ON om.user_id = u.id
			WHERE u.email = $1
			LIMIT 1
		`, req.Email).Scan(&userID, &name, &passwordHash, &orgID, &role)
		if err != nil {
			// Use same error message to prevent email enumeration.
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid credentials"})
			return
		}

		if err := bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(req.Password)); err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid credentials"})
			return
		}

		token, err := sessions.Create(r.Context(), session.Data{
			UserID: userID,
			OrgID:  orgID,
			Role:   role,
			Email:  req.Email,
			Name:   name,
		})
		if err != nil {
			log.WithError(err).Error("login: create session")
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}

		setSessionCookie(w, token)
		writeJSON(w, http.StatusOK, map[string]any{
			"id":    userID,
			"email": req.Email,
			"name":  name,
			"role":  role,
		})
	}
}

// HandleLogout clears the session cookie and deletes the session from Redis.
// @Summary Logout
// @Description Logout endpoint
// @Tags auth
// @Accept json
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Router /auth/logout [post]
func HandleLogout(sessions *session.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("session")
		if err == nil {
			_ = sessions.Delete(r.Context(), cookie.Value)
		}
		clearSessionCookie(w)
		w.WriteHeader(http.StatusOK)
	}
}

// HandleMe returns the current user's info from the session context.
// @Summary Me
// @Description Me endpoint
// @Tags auth
// @Accept json
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Router /auth/me [get]
func HandleMe() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := session.FromContext(r.Context())
		if sess == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id":    sess.UserID,
			"email": sess.Email,
			"name":  sess.Name,
			"role":  sess.Role,
		})
	}
}

// HandlePatchMe updates the authenticated user's name and/or password.
func HandlePatchMe(db *pgxpool.Pool, sessions *session.Store, log *logrus.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := session.FromContext(r.Context())
		if sess == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}

		var req PatchMeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
			return
		}

		if req.Name != nil {
			if _, err := db.Exec(r.Context(),
				`UPDATE users SET name = $1 WHERE id = $2`,
				*req.Name, sess.UserID,
			); err != nil {
				log.WithError(err).Error("patch me: update name")
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
				return
			}
		}

		if req.Password != nil {
			hash, err := bcrypt.GenerateFromPassword([]byte(*req.Password), bcryptCost)
			if err != nil {
				log.WithError(err).Error("patch me: bcrypt")
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
				return
			}
			if _, err := db.Exec(r.Context(),
				`UPDATE users SET password_hash = $1 WHERE id = $2`,
				string(hash), sess.UserID,
			); err != nil {
				log.WithError(err).Error("patch me: update password")
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
				return
			}
		}

		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}
}

// setSessionCookie writes the HttpOnly session cookie onto the response.
func setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
}

// writeJSON encodes v as JSON and writes it with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

